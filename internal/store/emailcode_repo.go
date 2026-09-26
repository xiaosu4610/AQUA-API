// 本文件是 model.EmailCodeRepository 的 SQL 实现。
//
// 意图（Why）：
//
//	把邮箱验证码落到数据库。本层只做"存取"，不实现任何业务判定
//	（过期、冷却、尝试次数上限等规则都在 model / server 层），
//	原因是这类规则一旦写进 SQL，就变成"散落在字符串里的业务逻辑"，
//	既难测试也容易在改动时出现两处不一致。
//
// 流转（Flow）：
//
//	NewEmailCodeRepository(db)
//	  ├─ 申请验证码：CountByEmailSince / CountByIPSince（限流）→ Create
//	  ├─ 校验验证码：LatestActive → 比对 → Consume / IncreaseAttempts
//	  └─ 定期维护：DeleteExpired
//
// 扩展（Extend）：
//
//	新增用途（purpose）无需改本文件：所有查询都按 (email, purpose) 成对过滤。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// emailCodeColumns 集中定义查询列，顺序必须与 scanEmailCode 严格一致。
const emailCodeColumns = `id, email, purpose, code_hash, attempts, expires_at, consumed_at, request_ip, created_at`

// emailCodeRepository 是 model.EmailCodeRepository 的 SQL 实现，并发安全。
type emailCodeRepository struct {
	db *sql.DB
}

// NewEmailCodeRepository 创建邮箱验证码仓储。
func NewEmailCodeRepository(db *sql.DB) model.EmailCodeRepository {
	return &emailCodeRepository{db: db}
}

// Create 新增验证码记录。
func (r *emailCodeRepository) Create(ctx context.Context, code *model.EmailCode) error {
	if code.Email == "" {
		return errors.New("store: 验证码必须关联邮箱")
	}
	if code.CodeHash == "" {
		return errors.New("store: 验证码摘要不能为空")
	}
	if code.Purpose == "" {
		return errors.New("store: 验证码必须指定用途")
	}

	code.CreatedAt = time.Now()

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO email_codes (email, purpose, code_hash, attempts, expires_at, consumed_at, request_ip, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		code.Email, code.Purpose, code.CodeHash, code.Attempts,
		code.ExpiresAt.Unix(), unixOrZero(code.ConsumedAt), code.RequestIP, code.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: 保存邮箱验证码失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取验证码 ID 失败: %w", err)
	}
	code.ID = uint64(id)
	return nil
}

// LatestActive 取指定邮箱与用途下最新的一条未消费记录。
//
// 说明：这里只保证"未消费"，是否过期由调用方判定——
// 过期判定需要"当前时间"，而数据库与业务的时间源应保持一致（都由 Go 提供），
// 避免时区或时钟漂移导致两边判断不同。
func (r *emailCodeRepository) LatestActive(ctx context.Context, email, purpose string) (*model.EmailCode, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+emailCodeColumns+" FROM email_codes"+
			" WHERE email = ? AND purpose = ? AND consumed_at = 0"+
			" ORDER BY id DESC LIMIT 1",
		email, purpose)

	code, err := scanEmailCode(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrEmailCodeNotFound
		}
		return nil, err
	}
	return code, nil
}

// IncreaseAttempts 累加失败次数。
//
// 用 SQL 原子自增而非"读出-加一-写回"：并发提交错误验证码时，
// 后者会互相覆盖，导致次数统计偏小、攻击者实际尝试次数远超上限。
func (r *emailCodeRepository) IncreaseAttempts(ctx context.Context, id uint64) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE email_codes SET attempts = attempts + 1 WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: 累加验证码失败次数失败: %w", err)
	}
	return nil
}

// Consume 标记验证码已使用。
//
// 条件里带 consumed_at = 0：这样即使两个请求同时用同一个验证码注册，
// 也只有一条 UPDATE 会生效（第二条影响行数为 0），从而实现"一次性"语义。
func (r *emailCodeRepository) Consume(ctx context.Context, id uint64, at time.Time) error {
	res, err := r.db.ExecContext(ctx,
		"UPDATE email_codes SET consumed_at = ? WHERE id = ? AND consumed_at = 0", at.Unix(), id)
	if err != nil {
		return fmt.Errorf("store: 消费验证码失败: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if affected == 0 {
		// 已被其他请求消费：视为失效，调用方应拒绝本次注册
		return model.ErrEmailCodeNotFound
	}
	return nil
}

// CountByEmailSince 统计某邮箱在指定时间之后申请的次数。
func (r *emailCodeRepository) CountByEmailSince(ctx context.Context, email string, since time.Time) (int, error) {
	return r.countSince(ctx, "SELECT COUNT(1) FROM email_codes WHERE email = ? AND created_at >= ?", email, since)
}

// CountByIPSince 统计某来源 IP 在指定时间之后申请的次数。
func (r *emailCodeRepository) CountByIPSince(ctx context.Context, ip string, since time.Time) (int, error) {
	return r.countSince(ctx, "SELECT COUNT(1) FROM email_codes WHERE request_ip = ? AND created_at >= ?", ip, since)
}

// countSince 是计数查询的公共实现，避免两处重复写 Scan 错误处理。
func (r *emailCodeRepository) countSince(ctx context.Context, query string, key string, since time.Time) (int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx, query, key, since.Unix()).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计验证码申请次数失败: %w", err)
	}
	return total, nil
}

// DeleteExpired 清理过期记录。
//
// 注意判据是 expires_at 而非 created_at：保留"未过期但较早创建"的记录，
// 否则会出现"用户刚收到的验证码被清理任务删掉"的诡异现象。
func (r *emailCodeRepository) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, "DELETE FROM email_codes WHERE expires_at < ?", before.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期验证码失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取清理条数失败: %w", err)
	}
	return affected, nil
}

// scanEmailCode 把一行数据映射为验证码对象。
func scanEmailCode(sc rowScanner) (*model.EmailCode, error) {
	var (
		id         uint64
		email      string
		purpose    string
		codeHash   string
		attempts   int
		expiresAt  int64
		consumedAt int64
		requestIP  string
		createdAt  int64
	)

	if err := sc.Scan(&id, &email, &purpose, &codeHash, &attempts,
		&expiresAt, &consumedAt, &requestIP, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取验证码字段失败: %w", err)
	}

	return &model.EmailCode{
		ID:         id,
		Email:      email,
		Purpose:    purpose,
		CodeHash:   codeHash,
		Attempts:   attempts,
		ExpiresAt:  time.Unix(expiresAt, 0),
		ConsumedAt: timeFromUnix(consumedAt),
		RequestIP:  requestIP,
		CreatedAt:  time.Unix(createdAt, 0),
	}, nil
}

// unixOrZero 把时间转为 Unix 秒；零值转为 0（表示"未消费"）。
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// timeFromUnix 把 Unix 秒还原为时间；0 返回零值（表示"未消费"）。
func timeFromUnix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
