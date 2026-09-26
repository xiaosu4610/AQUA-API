// 本文件是 model.QuotaRepository 的 SQL 实现（额度预扣台账）。
//
// 意图（Why）：
//
//	把"额度从响应后扣"变成"请求前预扣 + 响应后结算/退还"，堵住并发超支。
//	本文件承担三项与资损强相关的职责：
//	  1) 预留：在【单事务】里插入在途记录并原子扣减用户与令牌额度；
//	  2) 结算：按实际用量多退少补；退还：请求失败时全额退还；
//	  3) 回收：清理进程崩溃留下的超时在途记录。
//
// 原子性怎么保证（关键，防资损）：
//
//	所有扣减都用「条件更新 + 受影响行数」判定，绝不"先读出来再写回"：
//	  · 预留扣减用户额度：UPDATE ... SET used_quota = used_quota + ?
//	                        WHERE id = ? AND (quota < 0 OR quota - used_quota >= ?)
//	    受影响行数为 0 即表示额度不足（含"用户不存在"），整体回滚返回 ErrQuotaInsufficient；
//	  · 结算/释放用 `WHERE request_id = ? AND status = 在途` 作为幂等闸门，
//	    只有把状态从"在途"改成终态的【那一次】调用才会去加减额度。
//	事务的第一条语句都是【写】语句，避免 SQLite 下"先读后写"的锁升级死锁。
//
// 流转（Flow）：
//
//	NewQuotaRepository(db)
//	  ├─ 鉴权：Reserve（事务：写入在途 + 扣用户额度 + 扣令牌额度）
//	  ├─ 转发结束：Settle（多退少补）/ Release（全额退还）
//	  └─ 启动或后台：CleanupExpired（回收超时在途并退还）
//
// 扩展（Extend）：
//
//	新增额度维度（如按渠道单独限额）时：先在迁移里给本表加列，
//	再同步 quotaReservationColumns / scanQuotaReservation / Reserve 的列清单三处。
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

// quotaReservationColumns 集中定义查询列，顺序必须与 scanQuotaReservation 的扫描顺序严格一致。
const quotaReservationColumns = `id, request_id, user_id, token_id, reserved, settled, status, created_at, expires_at`

// defaultReservationTTL 是预留的在途有效期（调用方未指定时使用）。
//
// 取值 15 分钟：必须大于上游首字节超时（relay.UpstreamTimeout = 300 秒）并留足余量，
// 否则一个"慢但正常"的请求会在结算前就被当成陈旧预留回收，造成账目错乱。
const defaultReservationTTL = 15 * time.Minute

// quotaRepository 是 model.QuotaRepository 的 SQL 实现，并发安全。
type quotaRepository struct {
	db *sql.DB
}

// NewQuotaRepository 创建额度预留台账仓储。
func NewQuotaRepository(db *sql.DB) model.QuotaRepository {
	return &quotaRepository{db: db}
}

// Reserve 预扣额度（幂等）。
//
// 单事务内完成：
//  1. 插入在途记录（request_id 唯一；冲突即说明已预留过，返回既有记录，不重复扣减）；
//  2. 原子扣减用户额度（条件更新 + 受影响行数，不足则整体回滚）；
//  3. 原子扣减令牌额度（同为条件更新；不限额度令牌跳过剩余额度校验）。
//
// 事务第一条语句是 INSERT（写），避免 SQLite 下"先读后写"的锁升级死锁。
func (r *quotaRepository) Reserve(ctx context.Context, req model.ReserveRequest) (*model.QuotaReservation, error) {
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return nil, errors.New("store: 预留 request_id 不能为空")
	}
	if req.Amount < 0 {
		return nil, fmt.Errorf("store: 预留额度不能为负: %d", req.Amount)
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultReservationTTL
	}

	now := time.Now()
	expiresAt := now.Add(ttl)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 开启预留事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 第 1 步：插入在途记录。唯一索引冲突 = 该 request_id 已预留过。
	res, err := tx.ExecContext(ctx, `
		INSERT INTO quota_reservations
			(request_id, user_id, token_id, reserved, settled, status, created_at, expires_at)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?)`,
		requestID, req.UserID, req.TokenID, req.Amount,
		int(model.ReservationInFlight), now.Unix(), expiresAt.Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			// 幂等：重复预留直接返回既有记录（不重复扣减）。
			// 注意必须回滚当前事务，否则会与下面的查询争用连接。
			_ = tx.Rollback()
			return r.GetByRequestID(ctx, requestID)
		}
		return nil, fmt.Errorf("store: 写入预留记录失败: %w", err)
	}

	// 第 2 步：原子扣减用户额度。
	// 条件 `quota < 0 OR quota - used_quota >= amount` 一次表达两件事：
	//   · quota < 0 覆盖"不限额度"（QuotaUnlimited = -1）；
	//   · 其余情况要求"剩余额度足额"，不足则受影响 0 行。
	if req.UserID > 0 {
		res, err := tx.ExecContext(ctx, `
			UPDATE users SET used_quota = used_quota + ?, updated_at = ?
			WHERE id = ? AND (quota < 0 OR quota - used_quota >= ?)`,
			req.Amount, now.Unix(), req.UserID, req.Amount)
		if err != nil {
			return nil, fmt.Errorf("store: 预留扣减用户 %d 额度失败: %w", req.UserID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("store: 读取预留影响行数失败: %w", err)
		}
		if affected == 0 {
			// 额度不足（或用户不存在，属于鉴权阶段就该暴露的异常）：
			// 统一按"额度不足"处理，整体回滚；此处刻意不做额外查询，
			// 以免在高并发抢占（大量失败）时把失败路径变成数据库放大点。
			return nil, model.ErrQuotaInsufficient
		}
	}

	// 第 3 步：原子扣减令牌额度。
	// 不限额度令牌只需累加已用；有限令牌要求剩余额度足额（不足则受影响 0 行）。
	if req.TokenID > 0 {
		res, err := tx.ExecContext(ctx, `
			UPDATE tokens SET
				used_quota   = used_quota + ?,
				remain_quota = CASE WHEN unlimited_quota = 1 THEN remain_quota
				                    ELSE remain_quota - ? END,
				last_used_at = ?
			WHERE id = ? AND (unlimited_quota = 1 OR remain_quota >= ?)`,
			req.Amount, req.Amount, now.Unix(), req.TokenID, req.Amount)
		if err != nil {
			return nil, fmt.Errorf("store: 预留扣减令牌 %d 额度失败: %w", req.TokenID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("store: 读取预留影响行数失败: %w", err)
		}
		if affected == 0 {
			return nil, model.ErrQuotaInsufficient
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交预留事务失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: 读取预留记录 ID 失败: %w", err)
	}
	return &model.QuotaReservation{
		ID:        uint64(id),
		RequestID: requestID,
		UserID:    req.UserID,
		TokenID:   req.TokenID,
		Reserved:  req.Amount,
		Settled:   0,
		Status:    model.ReservationInFlight,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}, nil
}

// Settle 结算（幂等）：把在途预留改为已结算，并按 reserved−actualQuota 多退少补。
//
// 幂等闸门：先以 `status = 在途` 为条件把状态改为"已结算"。
// 只有受影响 1 行的那次调用才继续做加减；重复调用或对非在途记录调用会读到 0 行，
// 直接返回既有记录，不产生任何副作用。
//
// 补扣口径：实际用量超过预留时为"少补"，补扣量以实际可用额度为上限
// （不把用户扣成负数）；返回记录的 Settled 反映【真实入账额度】，
// 调用方据此即可识别"补扣受限"的缺口。
func (r *quotaRepository) Settle(ctx context.Context, requestID string, actualQuota int64) (*model.QuotaReservation, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("store: 结算 request_id 不能为空")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 开启结算事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 幂等闸门：只有"在途"能被结算。
	res, err := tx.ExecContext(ctx, `
		UPDATE quota_reservations SET status = ?
		WHERE request_id = ? AND status = ?`,
		int(model.ReservationSettled), requestID, int(model.ReservationInFlight))
	if err != nil {
		return nil, fmt.Errorf("store: 结算预留 %s 失败: %w", requestID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: 读取结算影响行数失败: %w", err)
	}
	if affected == 0 {
		// 已结算/已释放/不存在：无副作用，返回既有记录（不存在则明确报错）。
		return r.readReservationInTx(ctx, tx, requestID)
	}

	// 此时本事务已持有写锁，随后的读取是稳定值。
	var (
		reserved int64
		userID   uint64
		tokenID  uint64
	)
	if err := tx.QueryRowContext(ctx,
		"SELECT reserved, user_id, token_id FROM quota_reservations WHERE request_id = ?", requestID).
		Scan(&reserved, &userID, &tokenID); err != nil {
		return nil, fmt.Errorf("store: 读取预留 %s 明细失败: %w", requestID, err)
	}

	applied := actualQuota
	if applied < 0 {
		// QuotaUnknown：未取得用量，按预留量收取（不退成 0）。
		applied = reserved
	}

	now := time.Now()
	switch {
	case applied < reserved:
		// 多退：退还差额。
		if err := refundQuota(ctx, tx, userID, tokenID, reserved-applied, now); err != nil {
			return nil, err
		}
	case applied > reserved:
		// 少补：按可用额度扣减，不足则只扣得动多少扣多少。
		chargedUser, err := chargeUserQuotaUpTo(ctx, tx, userID, applied-reserved, now)
		if err != nil {
			return nil, err
		}
		chargedToken, err := chargeTokenQuotaUpTo(ctx, tx, tokenID, applied-reserved, now)
		if err != nil {
			return nil, err
		}
		// 以用户侧入账为准（用户是计费主体）；无用户时退化为令牌侧。
		if userID > 0 {
			applied = reserved + chargedUser
		} else {
			applied = reserved + chargedToken
		}
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE quota_reservations SET settled = ? WHERE request_id = ?", applied, requestID); err != nil {
		return nil, fmt.Errorf("store: 写入预留 %s 结算额度失败: %w", requestID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交结算事务失败: %w", err)
	}

	return &model.QuotaReservation{
		RequestID: requestID,
		UserID:    userID,
		TokenID:   tokenID,
		Reserved:  reserved,
		Settled:   applied,
		Status:    model.ReservationSettled,
	}, nil
}

// Release 全额退还预留（幂等）。
//
// 幂等闸门与 Settle 相同：只有把状态从"在途"改成"已释放"的那次调用会退还额度。
// 重复调用、或对已结算/已释放的记录调用，都读到 0 行并安全返回。
func (r *quotaRepository) Release(ctx context.Context, requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return errors.New("store: 释放 request_id 不能为空")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启释放事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE quota_reservations SET status = ?
		WHERE request_id = ? AND status = ?`,
		int(model.ReservationReleased), requestID, int(model.ReservationInFlight))
	if err != nil {
		return fmt.Errorf("store: 释放预留 %s 失败: %w", requestID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取释放影响行数失败: %w", err)
	}
	if affected == 0 {
		// 已结算/已释放/不存在：幂等，直接成功返回。
		return nil
	}

	var (
		reserved int64
		userID   uint64
		tokenID  uint64
	)
	if err := tx.QueryRowContext(ctx,
		"SELECT reserved, user_id, token_id FROM quota_reservations WHERE request_id = ?", requestID).
		Scan(&reserved, &userID, &tokenID); err != nil {
		return fmt.Errorf("store: 读取预留 %s 明细失败: %w", requestID, err)
	}

	if err := refundQuota(ctx, tx, userID, tokenID, reserved, time.Now()); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交释放事务失败: %w", err)
	}
	return nil
}

// GetByRequestID 按幂等键查询预留记录。
func (r *quotaRepository) GetByRequestID(ctx context.Context, requestID string) (*model.QuotaReservation, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+quotaReservationColumns+" FROM quota_reservations WHERE request_id = ?",
		strings.TrimSpace(requestID))

	item, err := scanQuotaReservation(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrReservationNotFound
		}
		return nil, err
	}
	return item, nil
}

// PendingAmount 返回某用户在途预留的合计额度。
func (r *quotaRepository) PendingAmount(ctx context.Context, userID uint64) (int64, error) {
	if userID == 0 {
		return 0, nil
	}
	var total sql.NullInt64
	if err := r.db.QueryRowContext(ctx,
		"SELECT SUM(reserved) FROM quota_reservations WHERE user_id = ? AND status = ?",
		userID, int(model.ReservationInFlight)).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计用户 %d 在途预留失败: %w", userID, err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// CleanupExpired 回收"在途且已过期"的陈旧预留并退还额度，返回处理条数。
//
// 逐条以 `status = 在途` 为条件回收：与某个正常请求的结算/释放并发时，
// 只有一方会拿到受影响 1 行，另一方读到 0 行，因此不会重复退还。
func (r *quotaRepository) CleanupExpired(ctx context.Context, now time.Time) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: 开启回收事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先读后写：先把待回收记录读入内存（含一条写语句的事务中读取稳定，
	// 且必须先关闭结果集再做后续写操作，避免同一连接上结果集与写语句交叉）。
	rows, err := tx.QueryContext(ctx, `
		SELECT id, user_id, token_id, reserved FROM quota_reservations
		WHERE status = ? AND expires_at > 0 AND expires_at < ?`,
		int(model.ReservationInFlight), now.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: 查询超时预留失败: %w", err)
	}

	type stale struct {
		id              uint64
		userID, tokenID uint64
		reserved        int64
	}
	var items []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.userID, &s.tokenID, &s.reserved); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: 读取超时预留失败: %w", err)
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("store: 遍历超时预留失败: %w", err)
	}
	_ = rows.Close()

	cleaned := 0
	for _, s := range items {
		res, err := tx.ExecContext(ctx, `
			UPDATE quota_reservations SET status = ?
			WHERE id = ? AND status = ?`,
			int(model.ReservationReleased), s.id, int(model.ReservationInFlight))
		if err != nil {
			return cleaned, fmt.Errorf("store: 回收预留 %d 失败: %w", s.id, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return cleaned, fmt.Errorf("store: 读取回收影响行数失败: %w", err)
		}
		if affected == 0 {
			// 已被正常结算/释放抢先处理：跳过，避免重复退还
			continue
		}
		if err := refundQuota(ctx, tx, s.userID, s.tokenID, s.reserved, now); err != nil {
			return cleaned, err
		}
		cleaned++
	}

	if err := tx.Commit(); err != nil {
		return cleaned, fmt.Errorf("store: 提交回收事务失败: %w", err)
	}
	return cleaned, nil
}

// readReservationInTx 在事务内按幂等键读取预留记录，不存在时返回 ErrReservationNotFound。
func (r *quotaRepository) readReservationInTx(ctx context.Context, tx *sql.Tx, requestID string) (*model.QuotaReservation, error) {
	item, err := scanQuotaReservation(tx.QueryRowContext(ctx,
		"SELECT "+quotaReservationColumns+" FROM quota_reservations WHERE request_id = ?", requestID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrReservationNotFound
		}
		return nil, err
	}
	return item, nil
}

// refundQuota 退还额度（用户与令牌同时退），必须在事务内调用。
//
// 用户侧用 MAX(used_quota - ?, 0) 兜底，避免历史脏数据导致"已用"变成负数；
// 令牌侧不限额度时只回退已用、不动剩余额度（与 ConsumeQuota 的口径一致）。
func refundQuota(ctx context.Context, tx *sql.Tx, userID, tokenID uint64, amount int64, now time.Time) error {
	if amount <= 0 {
		return nil
	}
	if userID > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET used_quota = MAX(used_quota - ?, 0), updated_at = ?
			WHERE id = ?`,
			amount, now.Unix(), userID); err != nil {
			return fmt.Errorf("store: 退还用户 %d 额度失败: %w", userID, err)
		}
	}
	if tokenID > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tokens SET
				used_quota   = MAX(used_quota - ?, 0),
				remain_quota = CASE WHEN unlimited_quota = 1 THEN remain_quota
				                    ELSE remain_quota + ? END,
				last_used_at = ?
			WHERE id = ?`,
			amount, amount, now.Unix(), tokenID); err != nil {
			return fmt.Errorf("store: 退还令牌 %d 额度失败: %w", tokenID, err)
		}
	}
	return nil
}

// chargeUserQuotaUpTo 结算补扣：尽可能多地扣减用户额度，最多扣到"总额度"为止。
//
// 返回实际扣减额。必须在【已持有写事务】时调用：先用一条写语句拿到写锁，
// 再读取额度，读到的值不会再被其他写事务改动。
func chargeUserQuotaUpTo(ctx context.Context, tx *sql.Tx, userID uint64, amount int64, now time.Time) (int64, error) {
	if userID == 0 || amount <= 0 {
		return 0, nil
	}
	var quota, used int64
	err := tx.QueryRowContext(ctx, "SELECT quota, used_quota FROM users WHERE id = ?", userID).Scan(&quota, &used)
	if errors.Is(err, sql.ErrNoRows) {
		// 用户已被删除：无从补扣，按 0 处理（不阻断结算）。
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: 读取用户 %d 额度失败: %w", userID, err)
	}

	charged := amount
	if quota >= 0 {
		available := quota - used
		if available < 0 {
			available = 0
		}
		if charged > available {
			charged = available
		}
	}
	if charged == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET used_quota = used_quota + ?, updated_at = ? WHERE id = ?",
		charged, now.Unix(), userID); err != nil {
		return 0, fmt.Errorf("store: 补扣用户 %d 额度失败: %w", userID, err)
	}
	return charged, nil
}

// chargeTokenQuotaUpTo 结算补扣：尽可能多地扣减令牌额度，最多扣到"剩余额度"为止。
//
// 返回实际扣减额；不限额度令牌不受剩余额度限制（只累加已用）。
func chargeTokenQuotaUpTo(ctx context.Context, tx *sql.Tx, tokenID uint64, amount int64, now time.Time) (int64, error) {
	if tokenID == 0 || amount <= 0 {
		return 0, nil
	}
	var (
		remain    int64
		unlimited int
	)
	err := tx.QueryRowContext(ctx,
		"SELECT remain_quota, unlimited_quota FROM tokens WHERE id = ?", tokenID).Scan(&remain, &unlimited)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: 读取令牌 %d 额度失败: %w", tokenID, err)
	}

	charged := amount
	if unlimited == 0 && charged > remain {
		charged = remain
	}
	if charged == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tokens SET
			used_quota   = used_quota + ?,
			remain_quota = CASE WHEN unlimited_quota = 1 THEN remain_quota
			                    ELSE remain_quota - ? END,
			last_used_at = ?
		WHERE id = ?`,
		charged, charged, now.Unix(), tokenID); err != nil {
		return 0, fmt.Errorf("store: 补扣令牌 %d 额度失败: %w", tokenID, err)
	}
	return charged, nil
}

// scanQuotaReservation 把一行数据映射为预留记录。
func scanQuotaReservation(sc rowScanner) (*model.QuotaReservation, error) {
	var (
		id        uint64
		requestID string
		userID    uint64
		tokenID   uint64
		reserved  int64
		settled   int64
		status    int
		createdAt int64
		expiresAt int64
	)
	if err := sc.Scan(&id, &requestID, &userID, &tokenID, &reserved, &settled,
		&status, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取预留字段失败: %w", err)
	}

	return &model.QuotaReservation{
		ID:        id,
		RequestID: requestID,
		UserID:    userID,
		TokenID:   tokenID,
		Reserved:  reserved,
		Settled:   settled,
		Status:    model.ReservationStatus(status),
		CreatedAt: time.Unix(createdAt, 0),
		// 0 表示不回收：转成 Go 零值时间，便于领域层用 IsZero 判断
		ExpiresAt: unixToTimeOrZero(expiresAt),
	}, nil
}

// isUniqueViolation 判断错误是否为唯一索引冲突。
//
// 依赖驱动错误文本而非具体错误类型：实现层只用了纯 Go SQLite 驱动，
// 其错误类型未导出可判断的哨兵；文本包含 "UNIQUE" 是稳定判据。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}
