// 本文件定义「兑换码」领域模型与仓储接口。
//
// 意图（Why）：
//
//	兑换码是运营最常用的"低成本发额度"手段：管理员批量生成一批码，
//	通过活动/社群分发，用户在门户输入码即可领取额度。相比"直接给账号充值"，
//	它把「分发」与「绑定用户」解耦（先发码、后兑换），天然带有效期与批次，
//	适合限时活动与小额批量发放。
//
//	把兑换码提升为独立领域对象，是为了让三件关键约束有明确的归属：
//	  1) 一码一用——由仓储层在事务内以条件更新保证（并发安全，防资损）；
//	  2) 状态语义——未使用 / 已使用 / 已作废，用哨兵错误精确区分失败原因；
//	  3) 生成规则——用 crypto/rand 生成、剔除易混字符，避免"抄错码"的投诉。
//
// 流转（Flow）：
//
//	后台：RedeemCodesView → Repository.CreateBatch / List / UpdateStatus / Delete / DeleteInvalid
//	门户：用户输入码 → Repository.Redeem（事务内校验 + 置已使用 + 给用户加额度）
//
// 扩展（Extend）：
//
//	新增字段（如"限定分组可用""单用户限领次数"）时：
//	在本结构体加字段 + 建新迁移加列 + 同步 store/redeem_repo.go 的列清单与
//	扫描逻辑三处；若涉及新的失败语义，在下方补哨兵错误并在 server 层补充映射。
package model

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 兑换码相关的领域错误。
//
// 为什么要细分成四个哨兵错误（而不是统一报"无效"）：
//
//	用户输入失败时，提示"不存在""已使用""已过期""已作废"对应的处置完全不同
//	（重输 / 换一张 / 找客服换新），含糊其辞只会让用户反复重试并投诉。
var (
	// ErrRedeemCodeNotFound 表示兑换码不存在。
	ErrRedeemCodeNotFound = errors.New("model: 兑换码不存在")
	// ErrRedeemCodeUsed 表示兑换码已被使用（含并发下被他人抢先兑换）。
	ErrRedeemCodeUsed = errors.New("model: 兑换码已被使用")
	// ErrRedeemCodeExpired 表示兑换码已过期。
	ErrRedeemCodeExpired = errors.New("model: 兑换码已过期")
	// ErrRedeemCodeVoid 表示兑换码已被作废。
	ErrRedeemCodeVoid = errors.New("model: 兑换码已作废")
)

// RedeemStatus 表示兑换码状态。
type RedeemStatus int

const (
	// RedeemStatusUnused 未使用：可被兑换。
	RedeemStatusUnused RedeemStatus = 1
	// RedeemStatusUsed 已使用：已被某用户兑换（终态）。
	RedeemStatusUsed RedeemStatus = 2
	// RedeemStatusVoid 已作废：被管理员主动废弃，不再可兑换（终态）。
	RedeemStatusVoid RedeemStatus = 3
)

// String 返回状态的中文描述（用于界面展示与错误提示）。
func (s RedeemStatus) String() string {
	switch s {
	case RedeemStatusUnused:
		return "未使用"
	case RedeemStatusUsed:
		return "已使用"
	case RedeemStatusVoid:
		return "已作废"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

// IsValid 判断状态取值是否合法。
func (s RedeemStatus) IsValid() bool {
	return s == RedeemStatusUnused || s == RedeemStatusUsed || s == RedeemStatusVoid
}

// 兑换码生成参数。
const (
	// redeemCodeLength 是兑换码长度。
	//
	// 取 16：字符集大小 32，熵约 80 bit（32^16），暴力猜码在实际限流下不可行；
	// 同时不超过用户愿意手工输入的舒适长度。
	redeemCodeLength = 16
	// redeemCodeMaxLen 是兑换码允许的最大长度（校验用，防御超长输入）。
	redeemCodeMaxLen = 64
)

// redeemCodeCharset 是兑换码字符集。
//
// 刻意剔除 0/O、1/I/L 等易混字符：兑换码常被用户手工抄写或口述，
// 字形相近会带来大量"码明明没错却兑换失败"的无效沟通。
// 共 31 个字符（26 个字母剔除 I/O/L + 数字 2-9）。
const redeemCodeCharset = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// RedeemCode 表示一张兑换码。
type RedeemCode struct {
	ID     uint64       // 主键
	Code   string       // 兑换码明文（大写字母数字）
	Quota  int64        // 可兑换额度（正整数）
	Status RedeemStatus // 状态
	// ExpiresAt 为过期时间；零值表示永不过期（落库为 0）。
	ExpiresAt time.Time
	UsedBy    uint64 // 领取用户 id（未使用为 0）
	// UsedAt 为领取时间；零值表示未使用（落库为 0）。
	UsedAt    time.Time
	BatchNo   string // 批次号
	Remark    string // 备注
	CreatedAt time.Time
}

// IsExpired 判断兑换码在给定时刻是否已过期。
func (r *RedeemCode) IsExpired(now time.Time) bool {
	if r == nil || r.ExpiresAt.IsZero() {
		return false
	}
	return now.After(r.ExpiresAt)
}

// IsUsable 判断兑换码当前是否可兑换（状态未使用且未过期）。
func (r *RedeemCode) IsUsable(now time.Time) bool {
	return r != nil && r.Status == RedeemStatusUnused && !r.IsExpired(now)
}

// Validate 校验兑换码的必要字段。
func (r *RedeemCode) Validate() error {
	code := strings.TrimSpace(r.Code)
	if code == "" {
		return errors.New("兑换码不能为空")
	}
	if code != strings.ToUpper(code) {
		return fmt.Errorf("兑换码只能使用大写字母（当前为 %q）", code)
	}
	if len(code) > redeemCodeMaxLen {
		return fmt.Errorf("兑换码最多 %d 个字符，当前 %d", redeemCodeMaxLen, len(code))
	}
	// 额度必须为正：0 或负数会产生"兑换成功但没得到任何额度"的荒诞结果
	if r.Quota <= 0 {
		return fmt.Errorf("可兑换额度必须大于 0，当前 %d", r.Quota)
	}
	if !r.Status.IsValid() {
		return fmt.Errorf("兑换码状态非法: %d", int(r.Status))
	}
	return nil
}

// RedeemCodeQuery 是兑换码列表的查询条件。
type RedeemCodeQuery struct {
	// Status 非空时按状态过滤（后台最常用的筛选）。
	Status *RedeemStatus
	// Keyword 按兑换码或备注模糊匹配。
	Keyword string
	// BatchNo 非空时只返回该批次的码。
	BatchNo string
	Limit   int
	Offset  int
}

// RedeemCodeRepository 定义兑换码的持久化操作。
type RedeemCodeRepository interface {
	// CreateBatch 批量写入兑换码（同一批次一次性落库，保证原子）。
	CreateBatch(ctx context.Context, codes []*RedeemCode) error

	// List 按条件分页查询，返回当页数据与符合条件的总数。
	List(ctx context.Context, query RedeemCodeQuery) ([]*RedeemCode, int, error)

	// GetByCode 按兑换码查询，不存在时返回 ErrRedeemCodeNotFound。
	GetByCode(ctx context.Context, code string) (*RedeemCode, error)

	// UpdateStatus 按 ID 更新状态，不存在时返回 ErrRedeemCodeNotFound。
	UpdateStatus(ctx context.Context, id uint64, status RedeemStatus) error

	// UpdateRemark 按 ID 更新备注，不存在时返回 ErrRedeemCodeNotFound。
	UpdateRemark(ctx context.Context, id uint64, remark string) error

	// Delete 按 ID 删除，不存在时返回 ErrRedeemCodeNotFound。
	Delete(ctx context.Context, id uint64) error

	// DeleteInvalid 清理"已使用/已过期"的兑换码，返回清理条数。
	DeleteInvalid(ctx context.Context) (int, error)

	// Redeem 兑换：校验未使用且未过期 → 置为已使用 → 给用户加额度。
	//
	// 必须原子：同一张码在并发兑换时只能成功一次。失败时返回对应的哨兵错误
	// （NotFound / Used / Expired / Void），供上层映射成精确的提示语。
	// 返回值为本次获得的额度。
	Redeem(ctx context.Context, code string, userID int64) (quota int64, err error)
}

// GenerateRedeemCode 生成一个兑换码。
//
// 使用 crypto/rand 而非 math/rand：兑换码等价于"可兑换额度"的凭据，
// 可预测的伪随机序列会让人能够推算/枚举出未分发的码，直接造成资损。
//
// 随机数到字符集的映射采用【拒绝采样】：字符集长度 31 不能整除 256，
// 若直接取模会让靠前的字符出现概率略高（可被统计推断）。做法是丢弃
// 落在 [248, 256) 的字节（248 = 31×8），保证每个字符等概率出现。
func GenerateRedeemCode() (string, error) {
	const (
		charsetLen = len(redeemCodeCharset)
		// maxAcceptable 是"可安全取模"的上界：>= 它的字节一律丢弃
		maxAcceptable = 256 - (256 % charsetLen)
	)

	buf := make([]byte, 0, redeemCodeLength)
	random := make([]byte, 1)
	for len(buf) < redeemCodeLength {
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("model: 生成兑换码随机数失败: %w", err)
		}
		if int(random[0]) >= maxAcceptable {
			continue // 落在无偏区间之外，丢弃后重新采样
		}
		buf = append(buf, redeemCodeCharset[int(random[0])%charsetLen])
	}
	return string(buf), nil
}

// GenerateRedeemBatchNo 生成一个批次号。
//
// 格式：R + 时间戳（到秒）+ 4 位随机大写十六进制，
// 形如 R20260927153012A3F1。带时间戳是为了让管理员一眼看出批次生成时间，
// 随机后缀用于避免同一秒内多次生成撞批次号。
func GenerateRedeemBatchNo() (string, error) {
	suffix := make([]byte, 2)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("model: 生成批次号随机数失败: %w", err)
	}
	const hexUpper = "0123456789ABCDEF"
	for i, b := range suffix {
		suffix[i] = hexUpper[int(b)%len(hexUpper)]
	}
	return "R" + time.Now().Format("20060102150405") + string(suffix), nil
}
