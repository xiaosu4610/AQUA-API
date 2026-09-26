// 本文件定义「邮箱验证码」领域模型与仓储接口。
//
// 意图（Why）：
//
//	开放注册的站点最怕的不是"有人注册"，而是"有人拿脚本批量注册"——
//	后果是垃圾账号、刷光免费额度、发信通道被投诉封禁。
//	因此引入"邮箱验证码"作为注册的前置条件，把"拥有一个可用邮箱"变成注册门槛。
//
// 安全设计（逐条说明为什么这样取值）：
//
//  1. 只存哈希不存明文：数据库泄露时攻击者也无法直接读出验证码；
//     哈希以「邮箱 + 用途」为盐，防止同一个码在不同邮箱间被撞出规律。
//  2. 有效期 5 分钟：足够正常人收信并填写，又不足以支撑离线爆破。
//  3. 单邮箱 60 秒冷却 + 每小时次数上限：防止把本站当成"短信/邮件轰炸机"。
//  4. 单 IP 每小时上限：防止换邮箱绕过邮箱维度限流。
//  5. 校验失败次数上限（5 次）：6 位数字共 100 万种可能，
//     不限次数时理论上可在线穷举；限次后穷举成功的概率可忽略。
//  6. 一次性消费（ConsumedAt）：校验通过即作废，杜绝同一验证码重复注册。
//
// 流转（Flow）：
//
//	POST /api/auth/email-code → 生成码 → Create（存哈希）
//	  → 用户收到邮件 → POST /api/auth/register 带上 code
//	  → LatestActive 取出记录 → 比对哈希 → Consume（标记已用）→ 建用户
//
// 扩展（Extend）：
//
//	新增用途（如找回密码）时：新增 Purpose 常量即可，
//	仓储按 (email, purpose) 维度隔离，互不干扰；无需改表结构。
package model

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// 验证码业务常量。
const (
	// EmailCodePurposeRegister 注册用途。
	EmailCodePurposeRegister = "register"

	// EmailCodeLength 验证码位数。
	//
	// 取 6 位而非 4 位：4 位仅 1 万种组合，配合"5 次尝试上限"仍偏弱；
	// 6 位（100 万种）在同样尝试上限下被猜中的概率约为百万分之五，足够安全。
	EmailCodeLength = 6

	// EmailCodeTTL 验证码有效期。
	EmailCodeTTL = 5 * time.Minute

	// EmailCodeResendCooldown 同一邮箱的重发冷却时间。
	EmailCodeResendCooldown = 60 * time.Second

	// EmailCodeMaxPerEmailPerHour 同一邮箱每小时最多请求次数。
	EmailCodeMaxPerEmailPerHour = 5

	// EmailCodeMaxPerIPPerHour 同一来源 IP 每小时最多请求次数。
	//
	// 取值高于邮箱维度：同一办公网络/校园网可能有多人同时注册（出口 IP 相同），
	// 限得太严会误伤正常用户。
	EmailCodeMaxPerIPPerHour = 20

	// EmailCodeMaxAttempts 单个验证码允许的最大校验失败次数。
	EmailCodeMaxAttempts = 5

	// maxEmailLength 邮箱最大长度（RFC 5321 对邮件地址路径的限制）。
	maxEmailLength = 254
)

// 验证码相关领域错误。
var (
	// ErrEmailCodeNotFound 表示不存在可用的验证码（未申请、已用、已过期或已被作废）。
	ErrEmailCodeNotFound = errors.New("model: 邮箱验证码不存在或已失效")
	// ErrEmailCodeMismatch 表示验证码不匹配。
	ErrEmailCodeMismatch = errors.New("model: 邮箱验证码错误")
	// ErrEmailCodeTooManyAttempts 表示该验证码的失败次数已达上限。
	ErrEmailCodeTooManyAttempts = errors.New("model: 邮箱验证码尝试次数过多")
	// ErrInvalidEmail 表示邮箱格式非法。
	ErrInvalidEmail = errors.New("model: 邮箱格式非法")
)

// EmailCode 表示一条邮箱验证码记录。
type EmailCode struct {
	ID         uint64    // 主键
	Email      string    // 收件邮箱（已规范化为小写）
	Purpose    string    // 用途，见 EmailCodePurpose* 常量
	CodeHash   string    // 验证码摘要（十六进制），禁止存明文
	Attempts   int       // 已失败的校验次数
	ExpiresAt  time.Time // 过期时间
	ConsumedAt time.Time // 消费时间；零值表示尚未使用
	RequestIP  string    // 申请时的来源 IP，用于风控追溯
	CreatedAt  time.Time // 创建时间
}

// IsConsumed 判断验证码是否已被使用。
func (c *EmailCode) IsConsumed() bool {
	return !c.ConsumedAt.IsZero()
}

// IsExpired 判断验证码是否已过期。
func (c *EmailCode) IsExpired(now time.Time) bool {
	return !now.Before(c.ExpiresAt)
}

// EmailCodeRepository 定义邮箱验证码的持久化操作。
//
// 注意：仓储只做存取，不实现"过期/冷却/次数"等业务判定——
// 这些规则属于领域逻辑，集中放在本文件与 server 层，避免规则散落在 SQL 里难以审计。
type EmailCodeRepository interface {
	// Create 新增一条验证码记录。
	Create(ctx context.Context, code *EmailCode) error

	// LatestActive 取指定邮箱与用途下"最新且未被消费"的记录。
	// 不存在时返回 ErrEmailCodeNotFound。
	LatestActive(ctx context.Context, email, purpose string) (*EmailCode, error)

	// IncreaseAttempts 累加失败次数，用于达到上限后作废该验证码。
	IncreaseAttempts(ctx context.Context, id uint64) error

	// Consume 标记验证码已使用（一次性）。
	Consume(ctx context.Context, id uint64, at time.Time) error

	// CountByEmailSince 统计某邮箱在指定时间之后申请的次数（用于冷却与小时上限）。
	CountByEmailSince(ctx context.Context, email string, since time.Time) (int, error)

	// CountByIPSince 统计某来源 IP 在指定时间之后申请的次数。
	CountByIPSince(ctx context.Context, ip string, since time.Time) (int, error)

	// DeleteExpired 清理过期记录，避免表无限增长。
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// GenerateEmailCode 生成一个数字验证码。
//
// 使用 crypto/rand 而非 math/rand：后者是可预测的伪随机，
// 攻击者通过观察若干次验证码即可推算出后续值，等同于没有验证码。
func GenerateEmailCode() (string, error) {
	// 上限 10^EmailCodeLength，取 [0, 上限) 的均匀随机数
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(EmailCodeLength), nil)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", fmt.Errorf("model: 生成邮箱验证码失败: %w", err)
	}
	// 左侧补零，保证长度恒定（如 000123），避免"长度不同"泄露信息
	return fmt.Sprintf("%0*d", EmailCodeLength, n.Int64()), nil
}

// NormalizeEmail 规范化邮箱：去空白 + 转小写。
//
// 为什么统一小写：邮箱的本地部分（@ 之前）理论上区分大小写，
// 但实际所有主流邮件服务商都按不区分大小写处理；
// 若不归一化，同一邮箱用大小写变体即可绕过"单邮箱限流"。
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmailFormat 校验邮箱格式。
//
// 刻意保持"足够严格但不追求 RFC 完备"：邮箱格式的完整正则极其复杂且仍有争议，
// 真正的可用性验证由"能否收到验证码"完成——这是最可靠的验证。
func ValidateEmailFormat(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return ErrInvalidEmail
	}
	if len(email) > maxEmailLength {
		return ErrInvalidEmail
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return ErrInvalidEmail
	}

	at := strings.LastIndex(email, "@")
	// @ 不能在首尾，且必须唯一存在
	if at <= 0 || at == len(email)-1 || strings.Count(email, "@") != 1 {
		return ErrInvalidEmail
	}

	local, domain := email[:at], email[at+1:]
	if local == "" || domain == "" {
		return ErrInvalidEmail
	}
	// 域名必须含点且点不在首尾（拒绝 "user@localhost" 这类无法收信的形式）
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return ErrInvalidEmail
	}
	return nil
}

// HashEmailCode 计算验证码摘要。
//
// 为什么以「邮箱 + 用途」为盐：验证码只有 6 位数字，若不加上下文，
// 攻击者拿到哈希后可用一张预计算表（10^6 条）瞬间反查出明文。
// 加盐后必须针对每个邮箱单独计算，成本被显著抬高。
func HashEmailCode(email, purpose, code string) string {
	sum := sha256.Sum256([]byte(NormalizeEmail(email) + "|" + purpose + "|" + strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

// VerifyEmailCode 比对验证码是否匹配记录中的摘要。
//
// 使用固定时间比较（constant-time）而非 == ：
// 字符串比较会"遇到第一个不同字符即返回"，攻击者可通过响应时间差逐位试探。
// 对 6 位数字而言这种攻击虽然实践上很难，但成本极低，没有理由不做。
func VerifyEmailCode(record *EmailCode, code string) bool {
	expected := record.CodeHash
	actual := HashEmailCode(record.Email, record.Purpose, code)
	if len(expected) != len(actual) {
		return false
	}
	var diff byte
	for i := 0; i < len(expected); i++ {
		diff |= expected[i] ^ actual[i]
	}
	return diff == 0
}
