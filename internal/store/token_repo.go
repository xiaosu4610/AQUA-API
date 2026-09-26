// 本文件是 model.TokenRepository 接口的 SQL 实现。
//
// 意图（Why）：
//
//	把令牌数据落到数据库，并承担两项与安全强相关的职责：
//	  1) 写入时同时保存 key_hash（SHA-256 摘要，供鉴权查找）与 key_enc（密文，供后台展示）；
//	  2) 读取时解密 key_enc 还原明文供业务使用。
//	关键设计：鉴权走 key_hash 唯一索引，避免"解密全表比对"——后者在大规模令牌下
//	既慢（CPU 与 IO 双重开销）又危险（内存中出现全部明文）。
//
// 流转（Flow）：
//
//	main.go 装配 → NewTokenRepository(db, cipher)
//	  ├─ Create/Update：Validate → 计算摘要与密文 → 写库
//	  └─ GetByKey/GetByID/List：读库 → 解密 → 返回领域对象
//
// 扩展（Extend）：
//
//	新增字段：先新建迁移脚本加列，再同步更新本文件的 tokenColumns / insert / update / scanToken。
//	（三处必须一起改，否则会出现"写了但读不出"或"读不到新字段"的问题。）
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

// 令牌列表查询的条数约束（与渠道列表同样的保护思路：避免一次性拉全表）。
const (
	defaultTokenListLimit = 100
	maxTokenListLimit     = 1000
)

// tokenColumns 集中定义查询列，顺序必须与 scanToken 的扫描顺序严格一致。
const tokenColumns = `id, owner_id, name, key_hash, key_enc, status, expires_at, remain_quota, unlimited_quota, used_quota, models, created_at, updated_at, last_used_at`

// tokenRepository 是 model.TokenRepository 的 SQL 实现，并发安全。
type tokenRepository struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

// NewTokenRepository 创建令牌仓储。
//
// 参数 cipher 用于加解密令牌 KEY，不可为 nil。
func NewTokenRepository(db *sql.DB, cipher *crypto.Cipher) model.TokenRepository {
	return &tokenRepository{db: db, cipher: cipher}
}

// Create 新增令牌并回填数据库生成的 ID 与时间戳。
func (r *tokenRepository) Create(ctx context.Context, t *model.Token) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("store: 令牌数据非法: %w", err)
	}

	keyHash := crypto.SHA256Hex(t.Key)
	keyEnc, err := r.cipher.Encrypt(t.Key)
	if err != nil {
		return fmt.Errorf("store: 加密令牌 KEY 失败: %w", err)
	}

	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO tokens
			(owner_id, name, key_hash, key_enc, status, expires_at, remain_quota, unlimited_quota,
			 used_quota, models, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.OwnerID, t.Name, keyHash, keyEnc, int(t.Status), expiresAtToUnix(t.ExpiresAt),
		t.RemainQuota, boolToInt(t.UnlimitedQuota), t.UsedQuota, encodeModels(t.Models),
		t.CreatedAt.Unix(), t.UpdatedAt.Unix(),
	)
	if err != nil {
		// 唯一索引冲突通常意味着令牌 KEY 重复（随机生成几乎不可能，多为手工指定）
		return fmt.Errorf("store: 新增令牌失败（KEY 是否重复？）: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增令牌的自增 ID 失败: %w", err)
	}
	t.ID = uint64(id)
	return nil
}

// GetByID 按主键查询令牌，不存在时返回 model.ErrTokenNotFound。
func (r *tokenRepository) GetByID(ctx context.Context, id uint64) (*model.Token, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+tokenColumns+" FROM tokens WHERE id = ?", id)

	t, err := r.scanToken(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrTokenNotFound
		}
		return nil, err
	}
	return t, nil
}

// GetByKey 按令牌明文查询。
//
// 实现要点：先计算明文摘要，再用摘要走唯一索引精确匹配——
// 这样数据库无需存储可用于比对的明文，也不需要全表解密。
func (r *tokenRepository) GetByKey(ctx context.Context, key string) (*model.Token, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+tokenColumns+" FROM tokens WHERE key_hash = ?", crypto.SHA256Hex(key))

	t, err := r.scanToken(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrTokenNotFound
		}
		return nil, err
	}
	return t, nil
}

// List 按条件查询令牌列表，按 ID 升序返回（便于后台稳定分页）。
func (r *tokenRepository) List(ctx context.Context, q model.TokenQuery) ([]*model.Token, error) {
	where, args := buildTokenWhere(q)

	var sb strings.Builder
	sb.WriteString("SELECT " + tokenColumns + " FROM tokens")
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}
	sb.WriteString(" ORDER BY id ASC")

	limit := q.Limit
	if limit <= 0 {
		limit = defaultTokenListLimit
	}
	if limit > maxTokenListLimit {
		limit = maxTokenListLimit
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	sb.WriteString(" LIMIT ? OFFSET ?")
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询令牌列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tokens := make([]*model.Token, 0, limit)
	for rows.Next() {
		t, err := r.scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历令牌结果集失败: %w", err)
	}
	return tokens, nil
}

// Update 按 ID 更新令牌，不存在时返回 model.ErrTokenNotFound。
func (r *tokenRepository) Update(ctx context.Context, t *model.Token) error {
	if t.ID == 0 {
		return errors.New("store: 更新令牌时 ID 不能为 0")
	}
	if err := t.Validate(); err != nil {
		return fmt.Errorf("store: 令牌数据非法: %w", err)
	}

	keyHash := crypto.SHA256Hex(t.Key)
	keyEnc, err := r.cipher.Encrypt(t.Key)
	if err != nil {
		return fmt.Errorf("store: 加密令牌 KEY 失败: %w", err)
	}

	t.UpdatedAt = time.Now()

	// 刻意不更新 created_at：创建时间应保持不可变，便于审计
	res, err := r.db.ExecContext(ctx, `
		UPDATE tokens SET
			owner_id = ?, name = ?, key_hash = ?, key_enc = ?, status = ?, expires_at = ?,
			remain_quota = ?, unlimited_quota = ?, used_quota = ?, models = ?, updated_at = ?
		WHERE id = ?`,
		t.OwnerID, t.Name, keyHash, keyEnc, int(t.Status), expiresAtToUnix(t.ExpiresAt),
		t.RemainQuota, boolToInt(t.UnlimitedQuota), t.UsedQuota, encodeModels(t.Models),
		t.UpdatedAt.Unix(), t.ID,
	)
	if err != nil {
		return fmt.Errorf("store: 更新令牌 %d 失败: %w", t.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrTokenNotFound
	}
	return nil
}

// Delete 按 ID 物理删除令牌，不存在时返回 model.ErrTokenNotFound。
func (r *tokenRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM tokens WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除令牌 %d 失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取删除影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrTokenNotFound
	}
	return nil
}

// buildTokenWhere 构造令牌查询的 WHERE 子句与参数。
//
// 抽成独立函数让列表与计数共用同一筛选逻辑，避免分页总数与实际列表不一致。
func buildTokenWhere(q model.TokenQuery) (string, []any) {
	var (
		conditions []string
		args       []any
	)
	if q.OwnerID != nil {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, *q.OwnerID)
	}
	if q.Status != nil {
		conditions = append(conditions, "status = ?")
		args = append(args, int(*q.Status))
	}
	return strings.Join(conditions, " AND "), args
}

// Count 返回符合条件的令牌总数。
func (r *tokenRepository) Count(ctx context.Context, q model.TokenQuery) (int, error) {
	where, args := buildTokenWhere(q)

	sb := strings.Builder{}
	sb.WriteString("SELECT COUNT(1) FROM tokens")
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}

	var total int
	if err := r.db.QueryRowContext(ctx, sb.String(), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计令牌数失败: %w", err)
	}
	return total, nil
}

// StatusCounts 按状态分组统计令牌数量。
func (r *tokenRepository) StatusCounts(ctx context.Context) (map[model.TokenStatus]int, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT status, COUNT(1) FROM tokens GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("store: 统计令牌状态失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[model.TokenStatus]int, 4)
	for rows.Next() {
		var (
			status int
			count  int
		)
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("store: 读取令牌状态统计失败: %w", err)
		}
		counts[model.TokenStatus(status)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历令牌状态统计失败: %w", err)
	}
	return counts, nil
}

// RecordUsage 记录令牌最近一次使用时间。
//
// 说明：只更新一个字段，避免整行写回造成的并发覆盖。
func (r *tokenRepository) RecordUsage(ctx context.Context, id uint64, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE tokens SET last_used_at = ? WHERE id = ?", at.Unix(), id)
	if err != nil {
		return fmt.Errorf("store: 记录令牌 %d 使用时间失败: %w", id, err)
	}
	// 令牌可能已被删除（如用户在被调用期间删除了它），
	// 此时无记录可更新属于正常情况，不应让转发流程因统计失败而报错。
	return nil
}

// ConsumeQuota 调整令牌额度：amount 为正表示扣减，为负表示退还。
//
// 实现要点（逐条说明为什么这样写）：
//  1. 单条 SQL 完成"累加已用 + 自减剩余"，杜绝并发下的计数丢失；
//  2. 用 CASE WHEN unlimited_quota 保证"不限额度"令牌的剩余额度不被扣成负数；
//  3. 用 SQLite 的标量 MAX(x, 0) 兜底，防止管理员误设的余额被扣穿；
//  4. amount == 0 时直接返回：调用方可能传入 0（未定价模型），
//     此时不应产生一次无意义的写操作。
//
// 关于退还（amount < 0）：SQL 中的减法在负数入参下自然变成加法
// （remain_quota - (-x) = remain_quota + x），因此退还与扣减共用同一段语句。
// 退还量由调用方保证不超过当初扣减的量（见 relay.Billing.Refund 的说明），
// 因此 remain_quota 不会被加到不合理的高度。
func (r *tokenRepository) ConsumeQuota(ctx context.Context, id uint64, amount int64, at time.Time) error {
	if amount == 0 {
		return nil
	}

	_, err := r.db.ExecContext(ctx, `
		UPDATE tokens SET
			used_quota   = used_quota + ?,
			remain_quota = CASE WHEN unlimited_quota = 1 THEN remain_quota
			                    ELSE MAX(remain_quota - ?, 0) END,
			last_used_at = ?
		WHERE id = ?`,
		amount, amount, at.Unix(), id)
	if err != nil {
		return fmt.Errorf("store: 扣减令牌 %d 额度失败: %w", id, err)
	}
	// 令牌可能已被删除：属于正常情况，不让计费失败影响转发主流程
	return nil
}

// scanToken 把一行数据映射为领域对象，并解密 KEY。
func (r *tokenRepository) scanToken(sc rowScanner) (*model.Token, error) {
	var (
		id             uint64
		ownerID        uint64
		name           string
		keyHash        string // 取出但不使用：摘要在鉴权路径已用于匹配，无需回传业务
		keyEnc         string
		status         int
		expiresAt      int64
		remainQuota    int64
		unlimitedQuota int
		usedQuota      int64
		modelsCSV      string
		createdAt      int64
		updatedAt      int64
		lastUsedAt     int64
	)

	if err := sc.Scan(&id, &ownerID, &name, &keyHash, &keyEnc, &status, &expiresAt,
		&remainQuota, &unlimitedQuota, &usedQuota, &modelsCSV, &createdAt, &updatedAt,
		&lastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取令牌字段失败: %w", err)
	}

	key, err := r.cipher.Decrypt(keyEnc)
	if err != nil {
		return nil, fmt.Errorf("store: 解密令牌 %d 的 KEY 失败（AQUA_APP_KEY 是否变更？）: %w", id, err)
	}

	return &model.Token{
		ID:             id,
		OwnerID:        ownerID,
		Name:           name,
		Key:            key,
		Status:         model.TokenStatus(status),
		ExpiresAt:      unixToExpiresAt(expiresAt),
		RemainQuota:    remainQuota,
		UnlimitedQuota: unlimitedQuota != 0,
		UsedQuota:      usedQuota,
		Models:         decodeModels(modelsCSV),
		CreatedAt:      time.Unix(createdAt, 0),
		UpdatedAt:      time.Unix(updatedAt, 0),
		LastUsedAt:     unixToExpiresAt(lastUsedAt), // 复用"0 表示零值时间"的转换
	}, nil
}

// expiresAtToUnix 把过期时间转为 Unix 秒；零值时间转为 0（表示永不过期）。
//
// 为什么要单独转换：time.Time 的零值（公元 1 年）直接取 Unix() 会得到一个大负数，
// 落库后再读出来会造成语义混乱。统一用 0 表示"永不过期"更直观。
func expiresAtToUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// unixToExpiresAt 把 Unix 秒还原为过期时间；0 还原为零值时间（永不过期）。
func unixToExpiresAt(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

// boolToInt 把布尔值转成数据库使用的 0/1（SQLite 无原生布尔类型）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
