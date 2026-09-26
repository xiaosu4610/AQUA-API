// 本文件是 model.PaymentOrderRepository 的 SQL 实现（充值订单）。
//
// 意图（Why）：
//
//	支付回调天然会重复投递（第三方普遍"重试直到收到 success"），
//	因此"入账"必须幂等。本文件把这个不变量下沉到数据库：
//	MarkPaid 使用条件更新（WHERE status = 待支付），
//	并用 RowsAffected 告诉调用方"是不是我这一枪打中的"。
//	只有打中的那一次才允许给用户加额度。
//
// 流转（Flow）：
//
//	NewPaymentOrderRepository(db)
//	  ├─ 下单：server → Create
//	  ├─ 回调：internal/payment 验签 → MarkPaid（幂等）→ 首次跃迁才加额度
//	  ├─ 查询：用户/后台列表 → List / Count
//	  └─ 维护：启动时 CloseExpired 关闭超时未付订单
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 orderColumns / scanPaymentOrder /
//	Create 列清单三处。
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

// orderColumns 集中定义查询列，顺序必须与 scanPaymentOrder 的扫描顺序严格一致。
const orderColumns = `id, trade_no, user_id, amount, currency, quota, method, sub_method,
	status, credited, pay_url, provider_trade_no, notify_payload, remark,
	created_at, updated_at, paid_at, credited_at, expires_at`

// 订单列表的分页参数。
const (
	defaultOrderPageSize = 20
	maxOrderPageSize     = 200
)

// paymentOrderRepository 是 model.PaymentOrderRepository 的 SQL 实现，并发安全。
type paymentOrderRepository struct {
	db *sql.DB
}

// NewPaymentOrderRepository 创建充值订单仓储。
func NewPaymentOrderRepository(db *sql.DB) model.PaymentOrderRepository {
	return &paymentOrderRepository{db: db}
}

// Create 创建订单。
func (r *paymentOrderRepository) Create(ctx context.Context, order *model.PaymentOrder) error {
	if err := order.Validate(); err != nil {
		return fmt.Errorf("store: 订单非法: %w", err)
	}

	now := time.Now()
	order.CreatedAt = now
	order.UpdatedAt = now
	if strings.TrimSpace(order.Currency) == "" {
		order.Currency = "CNY"
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO payment_orders
			(trade_no, user_id, amount, currency, quota, method, sub_method, status, credited,
			 pay_url, provider_trade_no, notify_payload, remark,
			 created_at, updated_at, paid_at, credited_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		order.TradeNo, order.UserID, order.Amount, order.Currency, order.Quota,
		order.Method, order.SubMethod, int(order.Status), boolToInt(order.IsCredited()),
		order.PayURL, order.ProviderTradeNo, order.NotifyPayload, order.Remark,
		order.CreatedAt.Unix(), order.UpdatedAt.Unix(), unixOrZero(order.PaidAt),
		unixOrZero(order.CreditedAt), unixOrZero(order.ExpiresAt),
	)
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return fmt.Errorf("store: 订单号重复（%s）", order.TradeNo)
		}
		return fmt.Errorf("store: 创建订单失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增订单的 ID 失败: %w", err)
	}
	order.ID = uint64(id)
	return nil
}

// GetByTradeNo 按订单号查询。
func (r *paymentOrderRepository) GetByTradeNo(ctx context.Context, tradeNo string) (*model.PaymentOrder, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+orderColumns+" FROM payment_orders WHERE trade_no = ?", strings.TrimSpace(tradeNo))

	order, err := scanPaymentOrder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrPaymentOrderNotFound
		}
		return nil, err
	}
	return order, nil
}

// List 查询订单列表（按创建时间倒序）。
func (r *paymentOrderRepository) List(ctx context.Context, query model.PaymentOrderQuery) ([]*model.PaymentOrder, error) {
	where, args := buildOrderWhere(query)

	limit := normalizeLimit(query.Limit, defaultOrderPageSize, maxOrderPageSize)
	offset := normalizeOffset(query.Offset)

	sqlText := "SELECT " + orderColumns + " FROM payment_orders" + where +
		" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询订单列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	orders := make([]*model.PaymentOrder, 0, 32)
	for rows.Next() {
		order, err := scanPaymentOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历订单列表失败: %w", err)
	}
	return orders, nil
}

// Count 统计符合条件的订单数。
func (r *paymentOrderRepository) Count(ctx context.Context, query model.PaymentOrderQuery) (int64, error) {
	where, args := buildOrderWhere(query)

	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM payment_orders"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计订单数失败: %w", err)
	}
	return total, nil
}

// MarkPaid 把订单置为已支付（幂等）。
//
// 关键：WHERE 中的 status = 待支付 就是"入账只发生一次"的落地形式。
// 回调重复到达时，第二次的 RowsAffected 为 0，调用方据此跳过加额度。
func (r *paymentOrderRepository) MarkPaid(ctx context.Context, tradeNo, providerTradeNo, payload string, paidAt time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE payment_orders SET
			status = ?,
			provider_trade_no = CASE WHEN ? = '' THEN provider_trade_no ELSE ? END,
			notify_payload = CASE WHEN ? = '' THEN notify_payload ELSE ? END,
			paid_at = ?,
			updated_at = ?
		WHERE trade_no = ? AND status = ?`,
		int(model.PaymentStatusPaid), providerTradeNo, providerTradeNo, payload, payload,
		paidAt.Unix(), time.Now().Unix(), tradeNo, int(model.PaymentStatusPending),
	)
	if err != nil {
		return false, fmt.Errorf("store: 标记订单 %s 已支付失败: %w", tradeNo, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	return affected > 0, nil
}

// UpdateStatus 修改订单状态。
//
// 仅允许"待支付 → 已关闭"与"已支付 → 已退款"两种流转之外的调用，
// 由上层业务判断；这里只保证不会把终态改回待支付。
func (r *paymentOrderRepository) UpdateStatus(ctx context.Context, tradeNo string, status model.PaymentStatus) error {
	res, err := r.db.ExecContext(ctx,
		"UPDATE payment_orders SET status = ?, updated_at = ? WHERE trade_no = ?",
		int(status), time.Now().Unix(), strings.TrimSpace(tradeNo))
	if err != nil {
		return fmt.Errorf("store: 更新订单 %s 状态失败: %w", tradeNo, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrPaymentOrderNotFound
	}
	return nil
}

// CreditOrder 给订单入账：在同一事务内标记已入账并把额度加到用户账户。
//
// 事务内的执行顺序（顺序本身就是正确性的一部分）：
//  1. 读出订单，确认"已支付且未入账"——否则直接返回 false（幂等出口）；
//  2. 确认订单归属用户仍存在（被删除则整体回滚，留下未入账记录供人工处理）；
//  3. 累加用户总额度；
//  4. 标记订单已入账；
//  5. 提交。
//
// 为什么先加额度、后标记：若顺序反过来，一旦加额度失败，订单已被标记入账，
// 就再也无法通过"未入账"这个线索把它找出来了——用户的钱等于凭空消失。
// 现在的顺序保证"要么全成，要么订单仍是未入账状态，可供补偿"。
func (r *paymentOrderRepository) CreditOrder(ctx context.Context, tradeNo string, at time.Time) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: 开启入账事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		userID   uint64
		quota    int64
		status   int
		credited int
	)
	err = tx.QueryRowContext(ctx,
		"SELECT user_id, quota, status, credited FROM payment_orders WHERE trade_no = ?",
		tradeNo).Scan(&userID, &quota, &status, &credited)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, model.ErrPaymentOrderNotFound
		}
		return false, fmt.Errorf("store: 读取订单 %s 失败: %w", tradeNo, err)
	}
	if model.PaymentStatus(status) != model.PaymentStatusPaid || credited != 0 {
		// 未支付，或已经入账过：都属于"本次无事可做"
		return false, nil
	}

	// 用户被删除时不能静默跳过：额度无处可加，必须让事务回滚并报错，
	// 以便运营介入（恢复用户或改为退款），而不是让钱消失在一笔"成功"的订单里。
	var userExists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(1) FROM users WHERE id = ?", userID).Scan(&userExists); err != nil {
		return false, fmt.Errorf("store: 校验订单归属用户失败: %w", err)
	}
	if userExists == 0 {
		return false, fmt.Errorf("store: 订单 %s 归属用户 %d 不存在，无法入账", tradeNo, userID)
	}

	// quota != -1 条件：不限额度账户加数字会把它变成有限额度，属于资损
	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET quota = quota + ?, updated_at = ? WHERE id = ? AND quota != ?",
		quota, at.Unix(), userID, model.QuotaUnlimited); err != nil {
		return false, fmt.Errorf("store: 累加用户 %d 额度失败: %w", userID, err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE payment_orders SET credited = 1, credited_at = ?, updated_at = ? WHERE trade_no = ?",
		at.Unix(), at.Unix(), tradeNo); err != nil {
		return false, fmt.Errorf("store: 标记订单 %s 已入账失败: %w", tradeNo, err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: 提交入账事务失败: %w", err)
	}
	return true, nil
}

// ListPaidUncredited 列出"已支付但未入账"的订单。
func (r *paymentOrderRepository) ListPaidUncredited(ctx context.Context, limit int) ([]*model.PaymentOrder, error) {
	if limit <= 0 {
		limit = defaultOrderPageSize
	}

	rows, err := r.db.QueryContext(ctx,
		"SELECT "+orderColumns+" FROM payment_orders WHERE status = ? AND credited = 0 ORDER BY paid_at ASC LIMIT ?",
		int(model.PaymentStatusPaid), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询未入账订单失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	orders := make([]*model.PaymentOrder, 0, limit)
	for rows.Next() {
		order, err := scanPaymentOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历未入账订单失败: %w", err)
	}
	return orders, nil
}

// CloseExpired 关闭在 before 之前到期且仍未支付的订单。
//
// 为什么必须关单：不关的话，用户可以在支付页面停留任意久，
// 而订单长期处于"待支付"会让"待处理订单"这一指标失去意义。
func (r *paymentOrderRepository) CloseExpired(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"UPDATE payment_orders SET status = ?, updated_at = ? WHERE status = ? AND expires_at > 0 AND expires_at < ?",
		int(model.PaymentStatusClosed), time.Now().Unix(),
		int(model.PaymentStatusPending), before.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: 关闭超时订单失败: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取关闭影响行数失败: %w", err)
	}
	return affected, nil
}

// SumPaidQuota 统计某用户已支付订单的额度合计。
func (r *paymentOrderRepository) SumPaidQuota(ctx context.Context, userID uint64) (int64, error) {
	var total sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		"SELECT SUM(quota) FROM payment_orders WHERE user_id = ? AND status = ?",
		userID, int(model.PaymentStatusPaid)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: 统计充值额度失败: %w", err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// buildOrderWhere 依据查询条件拼装 WHERE 子句与参数。
func buildOrderWhere(query model.PaymentOrderQuery) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)

	if query.UserID > 0 {
		conditions = append(conditions, "user_id = ?")
		args = append(args, query.UserID)
	}
	if method := strings.TrimSpace(query.Method); method != "" {
		conditions = append(conditions, "method = ?")
		args = append(args, method)
	}
	if query.Status != nil {
		conditions = append(conditions, "status = ?")
		args = append(args, int(*query.Status))
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// scanPaymentOrder 把一行数据映射为订单对象。
func scanPaymentOrder(sc rowScanner) (*model.PaymentOrder, error) {
	var (
		id              uint64
		tradeNo         string
		userID          uint64
		amount          int64
		currency        string
		quota           int64
		method          string
		subMethod       string
		status          int
		credited        int
		payURL          string
		providerTradeNo string
		notifyPayload   string
		remark          string
		createdAt       int64
		updatedAt       int64
		paidAt          int64
		creditedAt      int64
		expiresAt       int64
	)

	if err := sc.Scan(&id, &tradeNo, &userID, &amount, &currency, &quota, &method, &subMethod,
		&status, &credited, &payURL, &providerTradeNo, &notifyPayload, &remark,
		&createdAt, &updatedAt, &paidAt, &creditedAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取订单字段失败: %w", err)
	}

	order := &model.PaymentOrder{
		ID:              id,
		TradeNo:         tradeNo,
		UserID:          userID,
		Amount:          amount,
		Currency:        currency,
		Quota:           quota,
		Method:          method,
		SubMethod:       subMethod,
		Status:          model.PaymentStatus(status),
		PayURL:          payURL,
		ProviderTradeNo: providerTradeNo,
		NotifyPayload:   notifyPayload,
		Remark:          remark,
		CreatedAt:       time.Unix(createdAt, 0),
		UpdatedAt:       time.Unix(updatedAt, 0),
	}
	if paidAt > 0 {
		order.PaidAt = time.Unix(paidAt, 0)
	}
	if creditedAt > 0 {
		order.CreditedAt = time.Unix(creditedAt, 0)
	} else if credited != 0 {
		// 异常数据兜底：标记了已入账却没有时间戳。
		// 用订单更新时间兜底而不是留零值——零值会被 IsCredited() 判为"未入账"，
		// 进而导致重复入账（重复给钱），这是必须杜绝的方向。
		order.CreditedAt = order.UpdatedAt
	}
	if expiresAt > 0 {
		order.ExpiresAt = time.Unix(expiresAt, 0)
	}
	return order, nil
}
