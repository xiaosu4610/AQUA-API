// 本文件是 model.ChannelKeyRepository 接口的 SQL 实现。
//
// 意图（Why）：
//
//	把渠道密钥池落到数据库，并承担三件事：
//	  1) 密钥加解密（明文只在内存，落库一律密文）；
//	  2) 用摘要（key_hash）做去重与存在性判断，避免解密整池密钥只为比对；
//	  3) 记录每把密钥的连续失败次数与状态，支撑"失效自动摘除"。
//
// 流转（Flow）：
//
//	main.go → NewChannelKeyRepository(db, cipher)
//	  ├─ 后台导入：ReplaceAll（事务内做差集增删）
//	  ├─ 转发路径：ListUsable → PickKey → MarkUsed / MarkSuccess / MarkFailure
//	  └─ 列表页：Summary（一次查询拿全部渠道的计数，避免 N+1）
//
// 扩展（Extend）：
//
//	新增密钥字段：先建迁移加列，再同步本文件的 channelKeyColumns / scanChannelKey /
//	insert 三处（列顺序必须与扫描顺序一致，否则会静默错位）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// channelKeyColumns 集中定义查询列，顺序必须与 scanChannelKey 的扫描顺序严格一致。
const channelKeyColumns = `id, channel_id, kind, key_enc, label, status, fail_count, last_used_at, last_error, created_at, ` +
	`refresh_token_enc, access_token_enc, expires_at, account_hint, provider`

// channelKeyRepository 是 model.ChannelKeyRepository 的 SQL 实现，并发安全。
type channelKeyRepository struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

// NewChannelKeyRepository 创建渠道密钥池仓储。
//
// 参数 cipher 用于密钥加解密，不可为 nil —— 没有加密能力就不应允许写入密钥。
func NewChannelKeyRepository(db *sql.DB, cipher *crypto.Cipher) model.ChannelKeyRepository {
	return &channelKeyRepository{db: db, cipher: cipher}
}

// ReplaceAll 用给定 API Key 集合替换渠道的 api_key 类型凭据。
//
// 实现委托给 ReplaceCredentials：两类凭据的差集增删逻辑完全一致，
// 只是类型不同。这样避免同一套逻辑存在两份、日后改一处漏一处。
func (r *channelKeyRepository) ReplaceAll(ctx context.Context, channelID uint64, keys, labels []string) (int, int, error) {
	inputs := make([]model.CredentialInput, 0, len(keys))
	for i, key := range keys {
		label := ""
		if i < len(labels) {
			label = strings.TrimSpace(labels[i])
		}
		inputs = append(inputs, model.CredentialInput{
			Kind:   model.CredentialKindAPIKey,
			APIKey: key,
			Label:  label,
		})
	}
	return r.ReplaceCredentials(ctx, channelID, inputs)
}

// ReplaceCredentials 用给定凭据集合整体替换渠道的凭据池（差集增删，幂等）。
//
// 实现要点：
//  1. 所有增删在【单个事务】内完成。若不加事务，删一半失败会留下
//     "旧凭据已删、新凭据没进来"的空池状态，渠道会瞬间不可用；
//  2. 只增删「输入中出现的类型」的凭据，其他类型保持不变。
//     例如导入 API Key 时不会误删该渠道已有的 OAuth 订阅账号；
//     反之亦然。若一股脑全删，管理员只想补一批 Key 却把订阅账号清空，
//     会造成难以察觉的线上故障；
//  3. 去重标识统一存在 key_hash 列：api_key 用密钥摘要，
//     oauth 用 refresh_token 摘要（它才是账号的长期唯一标识）。
func (r *channelKeyRepository) ReplaceCredentials(ctx context.Context, channelID uint64, inputs []model.CredentialInput) (int, int, error) {
	if channelID == 0 {
		return 0, 0, errors.New("store: 替换凭据池时渠道 ID 不能为 0")
	}

	desired := make(map[string]model.CredentialInput, len(inputs))
	touchedKinds := make(map[model.CredentialKind]struct{}, 2)
	for _, input := range inputs {
		if err := input.Validate(); err != nil {
			return 0, 0, fmt.Errorf("store: 凭据非法: %w", err)
		}
		touchedKinds[input.Kind] = struct{}{}
		hash := input.IdentityHash(crypto.SHA256Hex)
		if _, dup := desired[hash]; dup {
			continue
		}
		desired[hash] = input
	}

	existing, err := r.listHashByKind(ctx, channelID)
	if err != nil {
		return 0, 0, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("store: 开启凭据池事务失败: %w", err)
	}
	// 失败时回滚；成功后 Commit，此调用变为无操作
	defer func() { _ = tx.Rollback() }()

	// 1) 删除：仅限"本次涉及的类型"中不再出现的凭据
	removed := 0
	for kind := range touchedKinds {
		for hash := range existing[kind] {
			if _, keep := desired[hash]; keep {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM channel_keys WHERE channel_id = ? AND kind = ? AND key_hash = ?",
				channelID, string(kind), hash); err != nil {
				return 0, 0, fmt.Errorf("store: 删除陈旧凭据失败: %w", err)
			}
			removed++
		}
	}

	// 2) 新增目标集合中缺失的凭据
	now := time.Now().Unix()
	added := 0
	for hash, input := range desired {
		if _, ok := existing[input.Kind][hash]; ok {
			continue // 已存在：保留其状态与失败统计，不重置
		}

		keyEnc, err := r.encryptField(input.APIKey)
		if err != nil {
			return 0, 0, err
		}
		refreshEnc, err := r.encryptField(input.RefreshToken)
		if err != nil {
			return 0, 0, err
		}
		accessEnc, err := r.encryptField(input.AccessToken)
		if err != nil {
			return 0, 0, err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_keys
				(channel_id, kind, key_enc, key_hash, label, status, fail_count, last_used_at, last_error,
				 created_at, refresh_token_enc, access_token_enc, expires_at, account_hint, provider)
			VALUES (?, ?, ?, ?, ?, ?, 0, 0, '', ?, ?, ?, ?, ?, ?)`,
			channelID, string(input.Kind), keyEnc, hash, strings.TrimSpace(input.Label),
			int(model.ChannelKeyStatusEnabled), now,
			refreshEnc, accessEnc, unixOrZero(input.ExpiresAt),
			strings.TrimSpace(input.AccountHint), strings.TrimSpace(input.Provider)); err != nil {
			return 0, 0, fmt.Errorf("store: 新增凭据失败: %w", err)
		}
		added++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: 提交凭据池变更失败: %w", err)
	}
	return added, removed, nil
}

// UpdateTokens 回写刷新后的 OAuth 令牌。
//
// 三个细节：
//  1. refreshToken 为空时保留原值——部分平台刷新后不回传新 refresh_token，
//     若按空值写入，等于永久丢失该账号；
//  2. 刷新成功即把失败计数清零：能刷出令牌说明该账号本身是好的，
//     之前的失败多半是令牌过期所致，不该继续累计；
//  3. 空 accessToken 视为非法（调用方应当只在刷新成功时回写）。
func (r *channelKeyRepository) UpdateTokens(ctx context.Context, id uint64, accessToken string, expiresAt time.Time, refreshToken string) error {
	if strings.TrimSpace(accessToken) == "" {
		return errors.New("store: 回写的 access_token 不能为空")
	}

	accessEnc, err := r.encryptField(accessToken)
	if err != nil {
		return err
	}

	var res sql.Result
	if strings.TrimSpace(refreshToken) != "" {
		refreshEnc, encErr := r.encryptField(refreshToken)
		if encErr != nil {
			return encErr
		}
		res, err = r.db.ExecContext(ctx, `
			UPDATE channel_keys SET
				access_token_enc = ?, expires_at = ?, refresh_token_enc = ?,
				fail_count = 0, last_error = ''
			WHERE id = ?`,
			accessEnc, unixOrZero(expiresAt), refreshEnc, id)
	} else {
		res, err = r.db.ExecContext(ctx, `
			UPDATE channel_keys SET
				access_token_enc = ?, expires_at = ?,
				fail_count = 0, last_error = ''
			WHERE id = ?`,
			accessEnc, unixOrZero(expiresAt), id)
	}
	if err != nil {
		return fmt.Errorf("store: 回写令牌失败: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrChannelKeyNotFound
	}
	return nil
}

// encryptField 加密单个字段；空值返回空串（数据库里"未使用"就是空串）。
func (r *channelKeyRepository) encryptField(plain string) (string, error) {
	if strings.TrimSpace(plain) == "" {
		return "", nil
	}
	encrypted, err := r.cipher.Encrypt(plain)
	if err != nil {
		return "", fmt.Errorf("store: 加密凭据字段失败: %w", err)
	}
	return encrypted, nil
}

// listHashByKind 返回某渠道已有凭据的摘要集合，按类型分组。
//
// 分组的必要性：替换时要"只动本次涉及的类型"，
// 因此必须先知道每一类里已有哪些凭据。
func (r *channelKeyRepository) listHashByKind(ctx context.Context, channelID uint64) (map[model.CredentialKind]map[string]struct{}, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT kind, key_hash FROM channel_keys WHERE channel_id = ?", channelID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询已有凭据摘要失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[model.CredentialKind]map[string]struct{}, 2)
	for rows.Next() {
		var (
			kind string
			hash string
		)
		if err := rows.Scan(&kind, &hash); err != nil {
			return nil, fmt.Errorf("store: 读取凭据摘要失败: %w", err)
		}
		credentialKind := model.CredentialKind(kind)
		if !credentialKind.IsValid() {
			credentialKind = model.CredentialKindAPIKey
		}
		if result[credentialKind] == nil {
			result[credentialKind] = make(map[string]struct{})
		}
		result[credentialKind][hash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历凭据摘要失败: %w", err)
	}
	return result, nil
}

// ListByChannel 列出某渠道的全部密钥（含已禁用与已摘除）。
func (r *channelKeyRepository) ListByChannel(ctx context.Context, channelID uint64) ([]*model.ChannelKey, error) {
	return r.list(ctx,
		"SELECT "+channelKeyColumns+" FROM channel_keys WHERE channel_id = ? ORDER BY id ASC",
		channelID)
}

// ListUsable 列出某渠道中可参与轮询的密钥。
func (r *channelKeyRepository) ListUsable(ctx context.Context, channelID uint64) ([]*model.ChannelKey, error) {
	return r.list(ctx,
		"SELECT "+channelKeyColumns+" FROM channel_keys WHERE channel_id = ? AND status = ? ORDER BY id ASC",
		channelID, int(model.ChannelKeyStatusEnabled))
}

// list 是列表查询的公共实现（两个入口只差 WHERE 条件）。
func (r *channelKeyRepository) list(ctx context.Context, query string, args ...any) ([]*model.ChannelKey, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询密钥列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	keys := make([]*model.ChannelKey, 0, 32)
	for rows.Next() {
		key, err := r.scanChannelKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历密钥列表失败: %w", err)
	}
	return keys, nil
}

// Summary 批量统计多个渠道的密钥池概览。
//
// 实现方式：一条 GROUP BY 查询取回全部渠道的计数，再在内存里归类。
// 这样渠道列表页（可能几十个渠道）只需一次数据库往返。
func (r *channelKeyRepository) Summary(ctx context.Context, channelIDs []uint64) (map[uint64]model.KeyPoolSummary, error) {
	result := make(map[uint64]model.KeyPoolSummary, len(channelIDs))
	if len(channelIDs) == 0 {
		return result, nil
	}

	// 用 IN 查询限定范围；渠道数量受分页上限约束（<=1000），不会触到 SQLite 参数上限
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(channelIDs)), ",")
	args := make([]any, 0, len(channelIDs))
	for _, id := range channelIDs {
		args = append(args, id)
	}

	query := "SELECT channel_id, status, COUNT(1) FROM channel_keys WHERE channel_id IN (" +
		placeholders + ") GROUP BY channel_id, status"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 统计密钥池失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			channelID uint64
			status    int
			count     int
		)
		if err := rows.Scan(&channelID, &status, &count); err != nil {
			return nil, fmt.Errorf("store: 读取密钥池统计失败: %w", err)
		}
		summary := result[channelID]
		summary.ChannelID = channelID
		summary.Total += count
		switch model.ChannelKeyStatus(status) {
		case model.ChannelKeyStatusEnabled:
			summary.Enabled += count
		case model.ChannelKeyStatusDisabled:
			summary.Disabled += count
		case model.ChannelKeyStatusAutoRemoved:
			summary.AutoRemoved += count
		}
		result[channelID] = summary
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历密钥池统计失败: %w", err)
	}
	return result, nil
}

// DeleteByChannel 删除某渠道的全部密钥（渠道删除时级联清理）。
func (r *channelKeyRepository) DeleteByChannel(ctx context.Context, channelID uint64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM channel_keys WHERE channel_id = ?", channelID); err != nil {
		return fmt.Errorf("store: 删除渠道 %d 的密钥池失败: %w", channelID, err)
	}
	return nil
}

// MarkUsed 记录密钥被选中使用的时间。
func (r *channelKeyRepository) MarkUsed(ctx context.Context, id uint64, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE channel_keys SET last_used_at = ? WHERE id = ?", at.Unix(), id); err != nil {
		return fmt.Errorf("store: 记录密钥使用时间失败: %w", err)
	}
	return nil
}

// MarkSuccess 记录一次成功：清零连续失败计数。
//
// 注意这里【不】改状态：被手动禁用的密钥不应因一次"恰好被选中并成功"而启用
// （实际上它根本不会被选中）。保持状态与统计分离，语义更清晰。
func (r *channelKeyRepository) MarkSuccess(ctx context.Context, id uint64) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE channel_keys SET fail_count = 0, last_error = '' WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: 重置密钥失败计数失败: %w", err)
	}
	return nil
}

// MarkFailure 记录一次失败：累加连续失败计数，达到阈值时自动摘除。
//
// 用 SQL 表达式在一条语句内完成"自增 + 判断阈值 + 改状态"，
// 避免"先读再判断再写"在并发下的计数丢失。
func (r *channelKeyRepository) MarkFailure(ctx context.Context, id uint64, reason string) error {
	// 截断错误描述：上游可能返回很长的响应体，直接入库会撑大数据库
	if len(reason) > 200 {
		reason = reason[:200]
	}

	if _, err := r.db.ExecContext(ctx, `
		UPDATE channel_keys SET
			fail_count = fail_count + 1,
			last_error = ?,
			status = CASE WHEN fail_count + 1 >= ? THEN ? ELSE status END
		WHERE id = ?`,
		reason, model.KeyAutoRemoveThreshold, int(model.ChannelKeyStatusAutoRemoved), id); err != nil {
		return fmt.Errorf("store: 记录密钥失败失败: %w", err)
	}
	return nil
}

// UpdateStatus 手动修改密钥状态。
//
// 典型用法：把误杀（连续失败被自动摘除）的密钥重新启用，
// 或临时禁用一个正在被上游限流的密钥。
func (r *channelKeyRepository) UpdateStatus(ctx context.Context, id uint64, status model.ChannelKeyStatus) error {
	if !status.IsValid() {
		return fmt.Errorf("store: 密钥状态非法: %d", int(status))
	}
	res, err := r.db.ExecContext(ctx,
		"UPDATE channel_keys SET status = ? WHERE id = ?", int(status), id)
	if err != nil {
		return fmt.Errorf("store: 更新密钥状态失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrChannelKeyNotFound
	}
	return nil
}

// scanChannelKey 把一行数据映射为凭据对象，并完成解密。
func (r *channelKeyRepository) scanChannelKey(sc rowScanner) (*model.ChannelKey, error) {
	var (
		id           uint64
		channelID    uint64
		kind         string
		encryptedKey string
		label        string
		status       int
		failCount    int
		lastUsedAt   int64
		lastError    string
		createdAt    int64
		refreshEnc   string
		accessEnc    string
		expiresAt    int64
		accountHint  string
		provider     string
	)

	if err := sc.Scan(&id, &channelID, &kind, &encryptedKey, &label, &status,
		&failCount, &lastUsedAt, &lastError, &createdAt,
		&refreshEnc, &accessEnc, &expiresAt, &accountHint, &provider); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取凭据字段失败: %w", err)
	}

	credentialKind := model.CredentialKind(kind)
	if !credentialKind.IsValid() {
		// 数据库里出现未知类型：不阻断读取（管理员仍应能在界面上看到这条异常数据），
		// 但按 api_key 处理并保留原始 kind 字符串用于排查
		credentialKind = model.CredentialKindAPIKey
	}

	plainKey, err := r.decryptField(encryptedKey, id, "密钥")
	if err != nil {
		return nil, err
	}
	refreshToken, err := r.decryptField(refreshEnc, id, "refresh_token")
	if err != nil {
		return nil, err
	}
	accessToken, err := r.decryptField(accessEnc, id, "access_token")
	if err != nil {
		return nil, err
	}

	return &model.ChannelKey{
		ID:           id,
		ChannelID:    channelID,
		Kind:         credentialKind,
		Key:          plainKey,
		Label:        label,
		Status:       model.ChannelKeyStatus(status),
		FailCount:    failCount,
		RefreshToken: refreshToken,
		AccessToken:  accessToken,
		ExpiresAt:    unixToExpiresAt(expiresAt),
		AccountHint:  accountHint,
		Provider:     provider,
		LastUsedAt:   unixToExpiresAt(lastUsedAt),
		LastError:    lastError,
		CreatedAt:    time.Unix(createdAt, 0),
	}, nil
}

// decryptField 解密单个字段。
//
// 空字符串直接返回空：数据库里未使用的字段是空串，对它调用解密会报错，
// 而"空"本身是合法状态（例如 api_key 类型的凭据没有 refresh_token）。
func (r *channelKeyRepository) decryptField(encoded string, id uint64, fieldName string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	plain, err := r.cipher.Decrypt(encoded)
	if err != nil {
		return "", fmt.Errorf("store: 解密凭据 %d 的 %s 失败（AQUA_APP_KEY 是否变更？）: %w", id, fieldName, err)
	}
	return plain, nil
}
