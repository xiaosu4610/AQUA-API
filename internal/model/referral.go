// 本文件定义「邀请返利 + 每日签到」领域模型与仓储接口。
//
// 意图（Why）：
//
//	本站的增长与留存靠两张底牌：邀请返利（拉新）与每日签到（留存）。
//	把它们的领域对象独立成文件，是为了让三件容易出错、后果又是"资损"的事
//	有明确且唯一的归属：
//	  1) 幂等——同一笔充值订单只能给邀请人返一次利（由唯一索引 + 先插记录再加额度保证）；
//	  2) 一天一次——同一用户北京时间当天只能签一次（由 (user_id, date) 唯一约束保证）；
//	  3) 邀请码规范——统一生成规则（去掉 0/O/1/I 等易混字符），避免"抄错码"的无效沟通。
//
//	奖励一律用「整数额度」表示，且只做加法：算不出正数就不发，宁可少给不可多给。
//
// 流转（Flow）：
//
//	注册：handler_auth.handleRegister → UserIDByInviteCode → BindInviter
//	      → GrantReward(kind=register)
//	充值：入账成功 → rewardReferralOnRecharge → InviterID → GrantReward(kind=recharge)
//	门户：GET /api/user/referral → EnsureInviteCode + CountInvitees + TotalRewardQuota
//	      + CheckinSummary；POST /api/user/checkin → Checkin（当天唯一）
//
// 扩展（Extend）：
//
//	新增奖励类型（如"邀请满 N 人额外奖励"）：在 ReferralKind 增加常量，
//	并在 store/referral_repo.go 的 GrantReward 中沿用同一幂等写法（新增唯一键维度）；
//	新增签到策略（连续签到递增）：改 CheckinSummary 的连续天数计算与 server 层发奖口径，
//	表结构无需变动。
package model

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 邀请 / 签到相关的领域错误。
var (
	// ErrInviteCodeNotFound 表示按邀请码未找到对应邀请人。
	ErrInviteCodeNotFound = errors.New("model: 邀请码不存在")
	// ErrCheckinAlreadyDone 表示今天已经签过到（仅作语义标识，实际由唯一约束判定）。
	ErrCheckinAlreadyDone = errors.New("model: 今日已签到")
)

// 邀请码生成参数。
const (
	// inviteCodeLength 是邀请码长度。
	//
	// 取 8：字符集大小 32，熵约 40 bit（32^8），在注册限流下暴力枚举他人邀请码
	// 不现实；同时足够短，便于用户口头转述或手抄。
	inviteCodeLength = 8
	// inviteCodeCharset 是邀请码字符集（大写字母 + 数字）。
	//
	// 刻意剔除 0/O、1/I 四个易混字符：邀请码常被转述/手抄，
	// 字形相近会带来大量"码明明没错却提示不存在"的沟通成本。
	// 共 32 个字符（24 个字母 + 8 个数字），恰好整除 256，取模无偏。
	inviteCodeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// GenerateInviteCode 生成一个邀请码。
//
// 使用 crypto/rand 而非 math/rand：邀请码是"能建立邀请关系并触发额度奖励"的凭证，
// 可预测的伪随机序列会让人能够枚举他人的邀请码并冒领奖励。
//
// 无需拒绝采样：字符集长度 32 恰好整除 256，直接取模即等概率
// （对比兑换码：字符集 31 不整除 256，故那边做了拒绝采样，见 redeem.go）。
func GenerateInviteCode() (string, error) {
	buf := make([]byte, inviteCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("model: 生成邀请码随机数失败: %w", err)
	}
	const charsetLen = len(inviteCodeCharset)
	for i, b := range buf {
		buf[i] = inviteCodeCharset[int(b)%charsetLen]
	}
	return string(buf), nil
}

// NormalizeInviteCode 归一化邀请码：去首尾空白并转大写。
//
// 用户输入常带空格或小写（链接被自动转义、手抄时顺手小写），
// 统一在此规整，避免"看着一样的码查不到"。
func NormalizeInviteCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// ReferralKind 表示邀请奖励的类型。
//
// 用字符串枚举而非整数：它会直接落库（referral_rewards.kind）并出现在排查语句里，
// "register"/"recharge" 一眼可读，远胜于 1/2。
type ReferralKind string

const (
	// ReferralKindRegister 邀请注册奖：被邀请人成功注册时发给邀请人。
	ReferralKindRegister ReferralKind = "register"
	// ReferralKindRecharge 充值返利：被邀请人充值入账成功时，按比例发给邀请人。
	ReferralKindRecharge ReferralKind = "recharge"
)

// IsValid 判断奖励类型是否合法。
func (k ReferralKind) IsValid() bool {
	return k == ReferralKindRegister || k == ReferralKindRecharge
}

// String 返回奖励类型的中文名（用于界面与日志）。
func (k ReferralKind) String() string {
	switch k {
	case ReferralKindRegister:
		return "邀请注册奖"
	case ReferralKindRecharge:
		return "充值返利"
	default:
		return fmt.Sprintf("未知(%s)", string(k))
	}
}

// ReferralReward 表示一条邀请奖励台账记录。
type ReferralReward struct {
	ID           uint64       // 主键
	InviterID    uint64       // 获得奖励的邀请人
	InviteeID    uint64       // 触发奖励的被邀请人
	Kind         ReferralKind // 奖励类型
	Quota        int64        // 本次奖励额度（正整数）
	OrderTradeNo string       // 充值返利对应订单号；注册奖为空串
	CreatedAt    time.Time    // 发放时间
}

// CheckinRecord 表示一条签到记录。
type CheckinRecord struct {
	ID          uint64    // 主键
	UserID      uint64    // 签到用户
	CheckinDate string    // 北京时间日期（YYYY-MM-DD）
	Quota       int64     // 本次发放额度
	CreatedAt   time.Time // 记录时间
}

// CheckinStats 是签到汇总，供门户一次性展示。
type CheckinStats struct {
	// CheckedToday 表示北京时间今天是否已签到。
	CheckedToday bool
	// TotalDays 是累计签到天数。
	TotalDays int64
	// TotalQuota 是累计签到获得额度。
	TotalQuota int64
	// StreakDays 是当前连续签到天数（详见 store 实现中的口径说明）。
	StreakDays int
}

// ReferralRepository 定义邀请返利与签到的持久化操作。
//
// 幂等与并发约定（实现必须遵守，这是防资损的关键）：
//   - EnsureInviteCode：用条件更新（WHERE invite_code <> 已有值）保证并发下只写一次；
//   - GrantReward：先 INSERT 台账（撞唯一约束即视为已发过、直接跳过），再加额度；
//   - Checkin：依赖 (user_id, checkin_date) 唯一约束保证"每人每天一次"。
type ReferralRepository interface {
	// EnsureInviteCode 返回用户的邀请码；若为空则懒生成并落库。
	//
	// 并发安全：用"条件更新 + 受影响行数"判定——受影响行为 0 说明已被其他请求写入，
	// 此时回读既有值返回，绝不覆盖。用户不存在时返回 ErrUserNotFound。
	EnsureInviteCode(ctx context.Context, userID uint64) (string, error)

	// UserIDByInviteCode 按邀请码反查用户 id；邀请码不存在时返回 ErrInviteCodeNotFound。
	UserIDByInviteCode(ctx context.Context, code string) (uint64, error)

	// InviterID 返回某用户的邀请人 id；无邀请人时返回 0（不是错误）。
	InviterID(ctx context.Context, inviteeID uint64) (uint64, error)

	// BindInviter 建立邀请关系（被邀请人 → 邀请人）。
	//
	// 幂等且"只绑一次"：仅当被邀请人当前 inviter_id 为 0 时才写入，
	// 从而不会覆盖已被别的邀请码建立的关系。双方 id 相同视为非法，返回错误。
	BindInviter(ctx context.Context, inviteeID, inviterID uint64) error

	// GrantReward 幂等发放一笔邀请奖励，返回"本次是否真正发放"。
	//
	// 返回值 semantics（与 CreditOrder 一致）：true 表示本次完成发放；
	// false 表示此前已就同一 (kind, invitee, order) 发过，本次未做任何变更。
	// quota <= 0 时直接返回 (false, nil)：不发"零或负数"的奖励。
	GrantReward(ctx context.Context, reward *ReferralReward) (bool, error)

	// CountInvitees 统计某邀请人成功邀请的人数（含尚未产生奖励的）。
	CountInvitees(ctx context.Context, inviterID uint64) (int64, error)

	// TotalRewardQuota 统计某邀请人累计已获得的邀请奖励额度。
	TotalRewardQuota(ctx context.Context, inviterID uint64) (int64, error)

	// Checkin 记录一次签到并发放额度（同一事务），返回是否签到成功。
	//
	// 返回 false 表示"北京时间今天已签到过"（唯一约束冲突），不视为错误。
	Checkin(ctx context.Context, userID uint64, date string, quota int64) (bool, error)

	// CheckinSummary 返回签到汇总（今日是否已签、累计天数/额度、连续天数）。
	//
	// date 为北京时间今天（YYYY-MM-DD）；连续天数以此为锚点计算。
	CheckinSummary(ctx context.Context, userID uint64, date string) (*CheckinStats, error)
}
