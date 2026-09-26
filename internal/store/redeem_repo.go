// 本文件是 model.RedeemCodeRepository 的 SQL 实现（兑换码）。
//
// 意图（Why）：
//
//	兑换码的核心风险是"同一张码被重复领取"（资损）与"码已置为已使用但额度没到账"
//	（用户付了活动成本却拿不到额度）。因此本文件把两件事做扎实：
//	  1) 兑换在【单个事务】内完成，且用"条件更新 + 受影响行数"判定是否抢到码——
//	     这是并发下"一码一用"的唯一可靠保证（唯一索引只能防重复码，防不了重复领取）；
//	  2) 先置码为已使用、再给用户加额度，全程同事务：任一步失败即整体回滚，
//	     码回到未使用，不会出现"码废了、额度没了"的中间态。
//
// 流转（Flow）：
//
//	NewRedeemCodeRepository(db)
//	  ├─ 后台维护：server → CreateBatch / List / UpdateStatus / UpdateRemark / Delete / DeleteInvalid
//	  └─ 用户兑换：server → Redeem（事务：条件更新抢码 → 累加用户额度）
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 redeemCodeColumns / scanRedeemCode /
//	CreateBatch 列清单 / buildRedeemWhere 四处。
//	新增失败语义：在 model 层补哨兵错误，并在 Redeem 的失败归类逻辑中补充分支。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// redeemCodeColumns 集中定义查询列，顺序必须与 scanRedeemCode 的扫描顺序严格一致。
const redeemCodeColumns = `id, code, quota, status, expires_at, used_by, used_at, batch_no, remark, created_at`

// 兑换码列表的分页参数。上限取 200，防止异常请求拉出超大响应。
const (
	defaultRedeemPageSize = 50
	maxRedeemPageSize     = 200
)

// redeemCodeRepository 是 model.RedeemCodeRepository 的 SQL 实现，并发安全。
type redeemCodeRepository struct {
	db *sql.DB
}

// NewRedeemCodeRepository 创建兑换码仓储。
func NewRedeemCodeRepository(db *sql.DB) model.RedeemCodeRepository {
	return &redeemCodeRepository{db: db}
}

// CreateBatch 批量写入兑换码。
//
// 用事务是刻意的：一批码要么全部落库、要么全部不落。若不做事务，
// 中途失败会留下"半批"数据，管理员既不知道生成了几张，也无法按批次清理。
func (r *redeemCodeRepository) CreateBatch(ctx context.Context, codes []*model.RedeemCode) error {
	if len(codes) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启兑换码批量写入事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	for _, item := range codes {
		item.Code = normalizeRedeemCode(item.Code)
		if item.Status == 0 {
			item.Status = model.RedeemStatusUnused
		}
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if err := item.Validate(); err != nil {
			return fmt.Errorf("store: 兑换码非法: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO redeem_codes (code, quota, status, expires_at, used_by, used_at, batch_no, remark, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.Code, item.Quota, int(item.Status), unixOrZeroLocal(item.ExpiresAt),
			item.UsedBy, unixOrZeroLocal(item.UsedAt), item.BatchNo, item.Remark, item.CreatedAt.Unix(),
		)
		if err != nil {
			return fmt.Errorf("store: 写入兑换码 %s 失败: %w", item.Code, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: 读取新增兑换码的 ID 失败: %w", err)
		}
		item.ID = uint64(id)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交兑换码批量写入事务失败: %w", err)
	}
	return nil
}

// List 按条件分页查询兑换码，同时返回符合条件的总数。
func (r *redeemCodeRepository) List(ctx context.Context, query model.RedeemCodeQuery) ([]*model.RedeemCode, int, error) {
	where, args := buildRedeemWhere(query)

	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM redeem_codes"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计兑换码数量失败: %w", err)
	}

	limit := normalizeLimit(query.Limit, defaultRedeemPageSize, maxRedeemPageSize)
	offset := normalizeOffset(query.Offset)

	// 按 id 倒序：最新生成的批次排在最前，符合后台"刚生成一批码要马上看到"的直觉
	sqlText := "SELECT " + redeemCodeColumns + " FROM redeem_codes" + where +
		" ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询兑换码列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]*model.RedeemCode, 0, limit)
	for rows.Next() {
		item, err := scanRedeemCode(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: 遍历兑换码列表失败: %w", err)
	}
	return items, total, nil
}

// GetByCode 按兑换码查询。
func (r *redeemCodeRepository) GetByCode(ctx context.Context, code string) (*model.RedeemCode, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+redeemCodeColumns+" FROM redeem_codes WHERE code = ?", normalizeRedeemCode(code))

	item, err := scanRedeemCode(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrRedeemCodeNotFound
		}
		return nil, err
	}
	return item, nil
}

// UpdateStatus 按 ID 更新状态。
func (r *redeemCodeRepository) UpdateStatus(ctx context.Context, id uint64, status model.RedeemStatus) error {
	if !status.IsValid() {
		return fmt.Errorf("store: 兑换码状态非法: %d", int(status))
	}
	res, err := r.db.ExecContext(ctx,
		"UPDATE redeem_codes SET status = ? WHERE id = ?", int(status), id)
	if err != nil {
		return fmt.Errorf("store: 更新兑换码 %d 状态失败: %w", id, err)
	}
	return redeemAffectedOrNotFound(res, id)
}

// UpdateRemark 按 ID 更新备注。
func (r *redeemCodeRepository) UpdateRemark(ctx context.Context, id uint64, remark string) error {
	res, err := r.db.ExecContext(ctx,
		"UPDATE redeem_codes SET remark = ? WHERE id = ?", strings.TrimSpace(remark), id)
	if err != nil {
		return fmt.Errorf("store: 更新兑换码 %d 备注失败: %w", id, err)
	}
	return redeemAffectedOrNotFound(res, id)
}

// Delete 按 ID 删除。
func (r *redeemCodeRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM redeem_codes WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除兑换码 %d 失败: %w", id, err)
	}
	return redeemAffectedOrNotFound(res, id)
}

// DeleteInvalid 清理"已使用"与"已过期且未使用"的兑换码，返回清理条数。
//
// 保留"已作废"的码：作废往往是人工干预的痕迹，保留一段时间便于追查，
// 确需彻底清理时管理员可按批次删除。
func (r *redeemCodeRepository) DeleteInvalid(ctx context.Context) (int, error) {
	now := time.Now().Unix()
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM redeem_codes
		WHERE status = ?
		   OR (status = ? AND expires_at > 0 AND expires_at <= ?)`,
		int(model.RedeemStatusUsed), int(model.RedeemStatusUnused), now)
	if err != nil {
		return 0, fmt.Errorf("store: 清理失效兑换码失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取清理条数失败: %w", err)
	}
	return int(affected), nil
}

// Redeem 兑换：校验状态与有效期 → 置为已使用 → 给用户加额度，全程单事务。
//
// 原子性保证（关键，防资损）：
//
//	事务的【第一条语句】就是带 `WHERE status = 未使用` 的条件更新，
//	它既充当"抢码"，也充当"校验"。并发时数据库只允许一个写事务，
//	其余请求会在此串行等待；等到它们执行时，码已是"已使用"，条件不成立，
//	受影响行数为 0 —— 据此判定"没抢到"，返回 ErrRedeemCodeUsed。
//	因此即使不做 SELECT ... FOR UPDATE，也能保证同一张码只被成功兑换一次。
//
// 失败归类：抢码失败（受影响行数 0）时才回查一次该码的真实状态，
// 以便区分"不存在 / 已使用 / 已过期 / 已作废"，让上层给出精确提示。
func (r *redeemCodeRepository) Redeem(ctx context.Context, code string, userID int64) (int64, error) {
	code = normalizeRedeemCode(code)
	if code == "" {
		return 0, model.ErrRedeemCodeNotFound
	}
	now := time.Now().Unix()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: 开启兑换事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 第一步：条件更新（事务内第一条语句，必须是写语句，见上方说明）。
	// 过期条件与状态条件并列，保证"已过期"的码也抢不到。
	res, err := tx.ExecContext(ctx, `
		UPDATE redeem_codes
		SET status = ?, used_by = ?, used_at = ?
		WHERE code = ? AND status = ? AND (expires_at = 0 OR expires_at > ?)`,
		int(model.RedeemStatusUsed), userID, now,
		code, int(model.RedeemStatusUnused), now)
	if err != nil {
		return 0, fmt.Errorf("store: 兑换码 %s 置为已使用失败: %w", code, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取兑换影响行数失败: %w", err)
	}
	if affected == 0 {
		// 没抢到：回查真实状态给出精确错误（此分支会随 defer 回滚）
		return 0, r.classifyRedeemFailure(ctx, tx, code, now)
	}

	// 第二步：读回本次额度（条件更新成功后该行必然存在）
	var quota int64
	if err := tx.QueryRowContext(ctx,
		"SELECT quota FROM redeem_codes WHERE code = ?", code).Scan(&quota); err != nil {
		return 0, fmt.Errorf("store: 读取兑换码 %s 额度失败: %w", code, err)
	}

	// 第三步：给用户加额度。
	// quota != 不限额度 条件：不限额度账户加数字会把它变成有限额度，
	// 属于最不该发生的资损（见 user_repo.AddQuota 的同款处理）。
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET quota = quota + ?, updated_at = ?
		WHERE id = ? AND quota != ?`,
		quota, now, userID, model.QuotaUnlimited); err != nil {
		return 0, fmt.Errorf("store: 累加用户 %d 额度失败: %w", userID, err)
	}

	// 校验用户确实存在：用户不存在时上面的 UPDATE 影响 0 行（无限额条件下也会 0 行），
	// 若不显式发现就会把码置为已使用却没给任何人加额度。此处发现即回滚。
	var userExists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(1) FROM users WHERE id = ?", userID).Scan(&userExists); err != nil {
		return 0, fmt.Errorf("store: 校验用户 %d 存在性失败: %w", userID, err)
	}
	if userExists == 0 {
		return 0, fmt.Errorf("store: 兑换码 %s 归属用户 %d 不存在，无法入账", code, userID)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: 提交兑换事务失败: %w", err)
	}
	return quota, nil
}

// classifyRedeemFailure 在抢码失败（受影响行数为 0）后回查真实状态，返回精确的哨兵错误。
func (r *redeemCodeRepository) classifyRedeemFailure(ctx context.Context, tx *sql.Tx, code string, now int64) error {
	var (
		status    int
		expiresAt int64
	)
	err := tx.QueryRowContext(ctx,
		"SELECT status, expires_at FROM redeem_codes WHERE code = ?", code).Scan(&status, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ErrRedeemCodeNotFound
		}
		return fmt.Errorf("store: 查询兑换码 %s 状态失败: %w", code, err)
	}

	switch model.RedeemStatus(status) {
	case model.RedeemStatusUsed:
		return model.ErrRedeemCodeUsed
	case model.RedeemStatusVoid:
		return model.ErrRedeemCodeVoid
	case model.RedeemStatusUnused:
		// 状态未使用却抢码失败，只可能是已过期
		if expiresAt > 0 && expiresAt <= now {
			return model.ErrRedeemCodeExpired
		}
		return model.ErrRedeemCodeNotFound
	default:
		return model.ErrRedeemCodeNotFound
	}
}

// buildRedeemWhere 依据查询条件拼装 WHERE 子句与参数（全部参数化，杜绝 SQL 注入）。
func buildRedeemWhere(query model.RedeemCodeQuery) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 4)

	if query.Status != nil {
		conditions = append(conditions, "status = ?")
		args = append(args, int(*query.Status))
	}
	if keyword := strings.TrimSpace(query.Keyword); keyword != "" {
		// 模糊匹配兑换码或备注；转义 % 与 _ 避免用户输入被当作通配符
		pattern := "%" + escapeLike(keyword) + "%"
		conditions = append(conditions, "(code LIKE ? ESCAPE '\\' OR remark LIKE ? ESCAPE '\\')")
		args = append(args, pattern, pattern)
	}
	if batchNo := strings.TrimSpace(query.BatchNo); batchNo != "" {
		conditions = append(conditions, "batch_no = ?")
		args = append(args, batchNo)
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// scanRedeemCode 把一行数据映射为兑换码对象。
func scanRedeemCode(sc rowScanner) (*model.RedeemCode, error) {
	var (
		id        uint64
		code      string
		quota     int64
		status    int
		expiresAt int64
		usedBy    uint64
		usedAt    int64
		batchNo   string
		remark    string
		createdAt int64
	)

	if err := sc.Scan(&id, &code, &quota, &status, &expiresAt, &usedBy, &usedAt,
		&batchNo, &remark, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取兑换码字段失败: %w", err)
	}

	return &model.RedeemCode{
		ID:     id,
		Code:   code,
		Quota:  quota,
		Status: model.RedeemStatus(status),
		// 0 表示永不过期/未使用：转成 Go 零值时间，便于领域层用 IsZero 判断
		ExpiresAt: unixToTimeOrZero(expiresAt),
		UsedBy:    usedBy,
		UsedAt:    unixToTimeOrZero(usedAt),
		BatchNo:   batchNo,
		Remark:    remark,
		CreatedAt: time.Unix(createdAt, 0),
	}, nil
}

// redeemAffectedOrNotFound 依据受影响行数判断操作是否命中记录。
func redeemAffectedOrNotFound(res sql.Result, id uint64) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrRedeemCodeNotFound
	}
	return nil
}

// normalizeRedeemCode 归一化兑换码：去空白并转大写（用户输入常带空格或小写）。
func normalizeRedeemCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// unixOrZeroLocal 把时间转为 Unix 秒；零值时间返回 0（表示永不过期/未使用）。
func unixOrZeroLocal(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// unixToTimeOrZero 把 Unix 秒转回时间；0 返回零值时间。
func unixToTimeOrZero(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
