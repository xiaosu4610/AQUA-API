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
const channelKeyColumns = `id, channel_id, key_enc, label, status, fail_count, last_used_at, last_error, created_at`

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

// ReplaceAll 用给定集合整体替换密钥池（差集增删，幂等）。
//
// 实现要点：所有增删在【单个事务】内完成。若不加事务，删一半失败会留下
// "旧密钥已删、新密钥没进来"的空池状态，渠道会瞬间不可用。
func (r *channelKeyRepository) ReplaceAll(ctx context.Context, channelID uint64, keys, labels []string) (int, int, error) {
	if channelID == 0 {
		return 0, 0, errors.New("store: 替换密钥池时渠道 ID 不能为 0")
	}

	// 目标集合：摘要 → 索引（用于取回对应的备注）
	desired := make(map[string]string, len(keys))
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		label := ""
		if i < len(labels) {
			label = strings.TrimSpace(labels[i])
		}
		hash := crypto.SHA256Hex(k)
		if _, dup := desired[hash]; dup {
			continue
		}
		desired[hash] = label
	}

	// 现有摘要集合（只查摘要，不解密：解密整池密钥毫无必要且更慢）
	existing, err := r.listHashes(ctx, channelID)
	if err != nil {
		return 0, 0, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("store: 开启密钥池事务失败: %w", err)
	}
	// 失败时回滚；成功后 Commit，此调用变为无操作
	defer func() { _ = tx.Rollback() }()

	// 1) 删除不在目标集合中的密钥
	removed := 0
	for hash := range existing {
		if _, keep := desired[hash]; keep {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM channel_keys WHERE channel_id = ? AND key_hash = ?", channelID, hash); err != nil {
			return 0, 0, fmt.Errorf("store: 删除陈旧密钥失败: %w", err)
		}
		removed++
	}

	// 2) 新增目标集合中缺失的密钥
	now := time.Now().Unix()
	added := 0
	for hash, label := range desired {
		if _, ok := existing[hash]; ok {
			continue // 已存在：保留其状态与失败统计，不重置
		}
		// 从目标集合反查明文：desired 只存了 hash → label，故这里需要另一张表
		plain := keyPlaintext(keys, hash)
		if plain == "" {
			continue
		}
		encrypted, err := r.cipher.Encrypt(plain)
		if err != nil {
			return 0, 0, fmt.Errorf("store: 加密密钥失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_keys (channel_id, key_enc, key_hash, label, status, fail_count, last_used_at, last_error, created_at)
			VALUES (?, ?, ?, ?, ?, 0, 0, '', ?)`,
			channelID, encrypted, hash, label, int(model.ChannelKeyStatusEnabled), now); err != nil {
			return 0, 0, fmt.Errorf("store: 新增密钥失败: %w", err)
		}
		added++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: 提交密钥池变更失败: %w", err)
	}
	return added, removed, nil
}

// keyPlaintext 在原始密钥列表中按摘要反查明文。
//
// 为什么用线性查找：ReplaceAll 是低频后台操作（导入/编辑时触发），
// 而这里的规模是几百条，线性扫描的开销可忽略；相比之下额外维护一张
// "hash → 明文"的映射会让代码多一份状态、多一处出错可能。
func keyPlaintext(keys []string, hash string) string {
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" && crypto.SHA256Hex(k) == hash {
			return k
		}
	}
	return ""
}

// listHashes 返回某渠道已有密钥的摘要集合。
func (r *channelKeyRepository) listHashes(ctx context.Context, channelID uint64) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT key_hash FROM channel_keys WHERE channel_id = ?", channelID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询已有密钥摘要失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]struct{})
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("store: 读取密钥摘要失败: %w", err)
		}
		result[hash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历密钥摘要失败: %w", err)
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

// scanChannelKey 把一行数据映射为密钥对象，并完成解密。
func (r *channelKeyRepository) scanChannelKey(sc rowScanner) (*model.ChannelKey, error) {
	var (
		id         uint64
		channelID  uint64
		encoded    string
		label      string
		status     int
		failCount  int
		lastUsedAt int64
		lastError  string
		createdAt  int64
	)

	if err := sc.Scan(&id, &channelID, &encoded, &label, &status,
		&failCount, &lastUsedAt, &lastError, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取密钥字段失败: %w", err)
	}

	plain, err := r.cipher.Decrypt(encoded)
	if err != nil {
		return nil, fmt.Errorf("store: 解密密钥 %d 失败（AQUA_APP_KEY 是否变更？）: %w", id, err)
	}

	return &model.ChannelKey{
		ID:         id,
		ChannelID:  channelID,
		Key:        plain,
		Label:      label,
		Status:     model.ChannelKeyStatus(status),
		FailCount:  failCount,
		LastUsedAt: unixToExpiresAt(lastUsedAt),
		LastError:  lastError,
		CreatedAt:  time.Unix(createdAt, 0),
	}, nil
}
