// 本文件是 model.ReferralRepository 的 SQL 实现（邀请返利 + 每日签到）。
//
// 意图（Why）：
//
//	邀请返利与签到都会"给用户凭空加额度"，一旦不幂等就是直接资损，
//	一旦不并发安全就会出现"同一天签两次""同一订单返两次"。因此本文件的两条铁律是：
//	  1) 幂等：GrantReward 先 INSERT 台账（撞唯一约束即视为已发过、直接跳过），
//	     再加额度；顺序绝不能反——反过来会重复发奖。Checkin 依赖唯一约束保证一天一次。
//	  2) 并发安全：邀请码懒生成用"条件更新 + 受影响行数"判定，
//	     绝不用"先查后写"（那有竞态窗口）。
//
// 流转（Flow）：
//
//	NewReferralRepository(db)
//	  ├─ 注册：handler_auth → UserIDByInviteCode / BindInviter / GrantReward(register)
//	  ├─ 充值：handler_payment 入账成功 → InviterID / GrantReward(recharge)
//	  └─ 门户：handler_referral → EnsureInviteCode / CountInvitees / TotalRewardQuota
//	                          / Checkin / CheckinSummary
//
// 扩展（Extend）：
//
//	新增奖励类型：在 model.ReferralKind 增加常量即可复用 GrantReward
//	（其唯一键为 kind+invitee+order，天然区分不同类型）；表结构无需变动。
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

// checkinStreakLookback 是计算连续签到时最多回看的签到记录条数。
//
// 设上限的目的：签到表会随天数长期增长，而计算连续天数只需"最近的连续段"。
// 回看 400 条已足以覆盖任何现实中的连续天数（一年也才 365 条），
// 同时避免每次查询都把全表历史拉进内存。
const checkinStreakLookback = 400

// referralRepository 是 model.ReferralRepository 的 SQL 实现，并发安全。
type referralRepository struct {
	db *sql.DB
}

// NewReferralRepository 创建邀请返利 / 签到仓储。
func NewReferralRepository(db *sql.DB) model.ReferralRepository {
	return &referralRepository{db: db}
}

// EnsureInviteCode 返回用户邀请码；为空时懒生成并落库。
//
// 并发安全的关键（务必理解）：
//
//	条件更新 `WHERE id = ? AND invite_code = ''` 的受影响行数只有两种结果：
//	  · 1 —— 本次写入成功，返回自己生成的码；
//	  · 0 —— 要么用户不存在，要么已被并发的另一个请求抢先写入。
//	受影响行为 0 时回读一次：读到非空码就返回它（不覆盖），读不到说明用户不存在。
//	唯一索引还会在极端并发下兜底：两个请求即便都通过了条件，写第二个也会撞唯一约束，
//	此时换个码重试（continue）即可。
func (r *referralRepository) EnsureInviteCode(ctx context.Context, userID uint64) (string, error) {
	current, err := r.currentInviteCode(ctx, userID)
	if err != nil {
		return "", err
	}
	if current != "" {
		return current, nil
	}

	for attempt := 0; attempt < maxInviteCodeAttempts; attempt++ {
		code, err := model.GenerateInviteCode()
		if err != nil {
			return "", fmt.Errorf("store: 生成邀请码失败: %w", err)
		}

		res, err := r.db.ExecContext(ctx, `
			UPDATE users SET invite_code = ?, updated_at = ?
			WHERE id = ? AND invite_code = ''`,
			code, time.Now().Unix(), userID)
		if err != nil {
			if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
				continue // 与并发写入撞码，换一个再试
			}
			return "", fmt.Errorf("store: 为用户 %d 写入邀请码失败: %w", userID, err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return "", fmt.Errorf("store: 读取邀请码写入影响行数失败: %w", err)
		}
		if affected == 1 {
			return code, nil
		}

		// 受影响 0 行：回读——被并发写入则返回既有值，否则用户不存在。
		current, err = r.currentInviteCode(ctx, userID)
		if err != nil {
			return "", err
		}
		if current != "" {
			return current, nil
		}
		return "", model.ErrUserNotFound
	}
	return "", fmt.Errorf("store: 为用户 %d 生成邀请码连续 %d 次撞码", userID, maxInviteCodeAttempts)
}

// currentInviteCode 读取用户当前邀请码（可能为空串）。
func (r *referralRepository) currentInviteCode(ctx context.Context, userID uint64) (string, error) {
	var code string
	err := r.db.QueryRowContext(ctx, "SELECT invite_code FROM users WHERE id = ?", userID).Scan(&code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", model.ErrUserNotFound
		}
		return "", fmt.Errorf("store: 读取用户 %d 邀请码失败: %w", userID, err)
	}
	return code, nil
}

// UserIDByInviteCode 按邀请码反查用户 id。
//
// 附加 `invite_code <> ”` 条件：空串是"尚未生成"的占位，绝不能被当作有效邀请码匹配上，
// 否则传入空邀请码就会命中某个倒霉用户、凭空建立邀请关系。
func (r *referralRepository) UserIDByInviteCode(ctx context.Context, code string) (uint64, error) {
	normalized := model.NormalizeInviteCode(code)
	if normalized == "" {
		return 0, model.ErrInviteCodeNotFound
	}

	var id uint64
	err := r.db.QueryRowContext(ctx,
		"SELECT id FROM users WHERE invite_code = ? AND invite_code <> ''", normalized).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, model.ErrInviteCodeNotFound
		}
		return 0, fmt.Errorf("store: 按邀请码查询用户失败: %w", err)
	}
	return id, nil
}

// InviterID 返回某用户的邀请人 id；无邀请人时返回 0。
func (r *referralRepository) InviterID(ctx context.Context, inviteeID uint64) (uint64, error) {
	var inviterID uint64
	err := r.db.QueryRowContext(ctx, "SELECT inviter_id FROM users WHERE id = ?", inviteeID).Scan(&inviterID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, model.ErrUserNotFound
		}
		return 0, fmt.Errorf("store: 读取用户 %d 邀请人失败: %w", inviteeID, err)
	}
	return inviterID, nil
}

// BindInviter 建立邀请关系；仅当被邀请人尚未绑定邀请人时写入。
//
// 用条件更新（WHERE inviter_id = 0）表达"只绑一次"：重复调用或并发调用都不会
// 覆盖已有关系。受影响行数为 0 不报错（可能是重复绑定，也可能是用户不存在），
// 因为绑定失败不应中断注册主流程。
func (r *referralRepository) BindInviter(ctx context.Context, inviteeID, inviterID uint64) error {
	if inviteeID == 0 || inviterID == 0 {
		return errors.New("store: 绑定邀请关系需要双方 id 均非 0")
	}
	if inviteeID == inviterID {
		return errors.New("store: 用户不能邀请自己")
	}

	if _, err := r.db.ExecContext(ctx, `
		UPDATE users SET inviter_id = ?, updated_at = ?
		WHERE id = ? AND inviter_id = 0`,
		inviterID, time.Now().Unix(), inviteeID); err != nil {
		return fmt.Errorf("store: 为用户 %d 绑定邀请人失败: %w", inviteeID, err)
	}
	return nil
}

// GrantReward 幂等发放一笔邀请奖励。
//
// 顺序（关键，注释即防呆）：
//
//	第 1 步 INSERT referral_rewards —— 撞唯一约束 (kind, invitee_id, order_trade_no)
//	        即表示"这笔奖励此前已发过"，直接返回 (false, nil)，绝不进入第 2 步；
//	第 2 步 给邀请人加额度 —— 只有第 1 步真正插入成功才会执行。
//
// 为什么必须是这个顺序：若先加额度再插台账，重复调用会在"插入撞约束"之前
// 就已经把额度重复加出去了，台账反而成了"事后发现重复"的记录，无法防资损。
//
// 不限额度账户（quota = -1）只记台账、不改额度（给"不限"加数字会把它变成有限额度）。
func (r *referralRepository) GrantReward(ctx context.Context, reward *model.ReferralReward) (bool, error) {
	if reward == nil {
		return false, errors.New("store: 奖励记录不能为空")
	}
	if reward.InviterID == 0 || reward.InviteeID == 0 {
		return false, errors.New("store: 奖励必须同时指定邀请人与被邀请人")
	}
	if !reward.Kind.IsValid() {
		return false, fmt.Errorf("store: 奖励类型非法: %q", string(reward.Kind))
	}
	if reward.Quota <= 0 {
		// 0 或负数没有任何发放意义，直接视为"无需发放"（不是错误）
		return false, nil
	}
	if reward.CreatedAt.IsZero() {
		reward.CreatedAt = time.Now()
	}
	reward.OrderTradeNo = strings.TrimSpace(reward.OrderTradeNo)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: 开启邀请奖励事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 第 1 步：插入台账。唯一约束冲突 = 已发放过 → 幂等跳过。
	res, err := tx.ExecContext(ctx, `
		INSERT INTO referral_rewards (inviter_id, invitee_id, kind, quota, order_trade_no, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		reward.InviterID, reward.InviteeID, string(reward.Kind), reward.Quota,
		reward.OrderTradeNo, reward.CreatedAt.Unix())
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return false, nil
		}
		return false, fmt.Errorf("store: 写入邀请奖励台账失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, fmt.Errorf("store: 读取邀请奖励 id 失败: %w", err)
	}
	reward.ID = uint64(id)

	// 第 2 步：给邀请人加额度（不限额度账户跳过，见方法注释）。
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET quota = quota + ?, updated_at = ?
		WHERE id = ? AND quota != ?`,
		reward.Quota, time.Now().Unix(), reward.InviterID, model.QuotaUnlimited); err != nil {
		return false, fmt.Errorf("store: 给邀请人 %d 加奖励额度失败: %w", reward.InviterID, err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: 提交邀请奖励事务失败: %w", err)
	}
	return true, nil
}

// CountInvitees 统计某邀请人成功邀请的人数。
func (r *referralRepository) CountInvitees(ctx context.Context, inviterID uint64) (int64, error) {
	var count int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(1) FROM users WHERE inviter_id = ?", inviterID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: 统计邀请人数失败: %w", err)
	}
	return count, nil
}

// TotalRewardQuota 统计某邀请人累计获得的邀请奖励额度。
func (r *referralRepository) TotalRewardQuota(ctx context.Context, inviterID uint64) (int64, error) {
	var total int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(quota), 0) FROM referral_rewards WHERE inviter_id = ?", inviterID).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计邀请奖励额度失败: %w", err)
	}
	return total, nil
}

// Checkin 记录一次签到并发放额度，返回是否签到成功。
//
// 一天一次的判定完全交给 (user_id, checkin_date) 唯一约束：
// 并发下只有一个请求能成功插入，其余请求撞唯一约束返回 (false, nil)。
// 这比"先查是否已签再插入"可靠得多——后者存在竞态窗口。
//
// 插入记录与加额度在同一事务内：任一步失败即整体回滚，
// 不会出现"记录写了但额度没到"或"额度到了却没有记录"的中间态。
func (r *referralRepository) Checkin(ctx context.Context, userID uint64, date string, quota int64) (bool, error) {
	if userID == 0 {
		return false, errors.New("store: 签到时用户 id 不能为 0")
	}
	date = strings.TrimSpace(date)
	if date == "" {
		return false, errors.New("store: 签到时日期不能为空")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: 开启签到事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO checkin_records (user_id, checkin_date, quota, created_at)
		VALUES (?, ?, ?, ?)`,
		userID, date, quota, now)
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return false, nil // 今天已签到
		}
		return false, fmt.Errorf("store: 写入签到记录失败: %w", err)
	}
	if _, err := res.LastInsertId(); err != nil {
		return false, fmt.Errorf("store: 读取签到记录 id 失败: %w", err)
	}

	// 发放签到额度（quota <= 0 时不加：允许"只记天数、不发额度"的配置）
	if quota > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET quota = quota + ?, updated_at = ?
			WHERE id = ? AND quota != ?`,
			quota, now, userID, model.QuotaUnlimited); err != nil {
			return false, fmt.Errorf("store: 为用户 %d 发放签到额度失败: %w", userID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: 提交签到事务失败: %w", err)
	}
	return true, nil
}

// CheckinSummary 返回签到汇总。
//
// 连续天数的口径（明确定义，避免各处理解不一致）：
//
//	以"今天"为锚点；若今天已签到则从今天起向前数；
//	若今天还没签，但昨天签了，则把昨天当作锚点继续数
//	—— 这样"今天还没签但昨天还在连签"的用户看到的仍是当前连续天数，
//	而不是忽然归零（归零会让连签体验很挫败）。今天与昨天都没签则连续天数为 0。
func (r *referralRepository) CheckinSummary(ctx context.Context, userID uint64, date string) (*model.CheckinStats, error) {
	stats := &model.CheckinStats{}

	row := r.db.QueryRowContext(ctx, `
		SELECT COUNT(1), COALESCE(SUM(quota), 0)
		FROM checkin_records WHERE user_id = ?`, userID)
	if err := row.Scan(&stats.TotalDays, &stats.TotalQuota); err != nil {
		return nil, fmt.Errorf("store: 统计签到记录失败: %w", err)
	}

	// 取最近的签到日期用于计算连续天数（今天是否签到也从这里判定）。
	rows, err := r.db.QueryContext(ctx, `
		SELECT checkin_date FROM checkin_records
		WHERE user_id = ?
		ORDER BY checkin_date DESC
		LIMIT ?`, userID, checkinStreakLookback)
	if err != nil {
		return nil, fmt.Errorf("store: 查询签到日期失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	dates := make(map[string]struct{}, 32)
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			return nil, fmt.Errorf("store: 读取签到日期失败: %w", err)
		}
		dates[item] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历签到日期失败: %w", err)
	}

	today, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, fmt.Errorf("store: 签到日期格式非法（应为 YYYY-MM-DD）: %w", err)
	}
	stats.CheckedToday = false
	if _, ok := dates[today.Format("2006-01-02")]; ok {
		stats.CheckedToday = true
	}

	stats.StreakDays = countStreak(dates, today)
	return stats, nil
}

// countStreak 计算以 today 为锚点的连续签到天数（口径见 CheckinSummary 注释）。
func countStreak(dates map[string]struct{}, today time.Time) int {
	anchor := today
	if _, ok := dates[anchor.Format("2006-01-02")]; !ok {
		anchor = today.AddDate(0, 0, -1)
	}
	if _, ok := dates[anchor.Format("2006-01-02")]; !ok {
		return 0
	}

	streak := 0
	for cursor := anchor; ; cursor = cursor.AddDate(0, 0, -1) {
		if _, ok := dates[cursor.Format("2006-01-02")]; !ok {
			break
		}
		streak++
	}
	return streak
}
