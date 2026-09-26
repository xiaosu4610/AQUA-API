// 本文件定义「充值订单」领域模型与仓储接口。
//
// 意图（Why）：
//
//	网关的额度体系需要一个"钱从哪来"的入口：用户充值 → 获得额度 → 调用模型。
//	没有它，额度只能靠管理员手工调整，无法对外运营。
//
//	为什么必须把订单落库、且状态机写在领域层：
//	  1) 支付回调可能重复到达（第三方普遍采用"重试直到收到 success"的投递语义），
//	     因此"入账"必须幂等——判据只能是数据库中的状态跃迁，不能靠内存标记；
//	  2) 金额与额度的换算必须唯一：订单一旦创建就把 quota 固化下来，
//	     之后管理员改兑换比例也不影响历史订单（否则用户会对不上账）。
//
// 金额口径（重要）：
//
//	Amount 一律用【分】为单位的 int64，绝不使用浮点。
//	"0.1 + 0.2 != 0.3" 在支付系统里等于账目错乱，无法用任何补偿机制挽回信任。
//
// 流转（Flow）：
//
//	用户下单：POST /api/user/orders → 计算 quota → Create(status=待支付) → 返回支付地址
//	第三方回调：POST /api/payments/{method}/notify
//	  └─ 验签 → MarkPaid（条件更新，返回是否首次跃迁）
//	       └─ 首次跃迁才给用户加额度（幂等入账）
//	订单查询：GET /api/user/orders 与后台 GET /api/admin/orders
//
// 扩展（Extend）：
//
//	新增支付通道：在 internal/payment 实现 Provider 并在注册表登记，
//	本文件与 store 层无需改动（method 是字符串而非枚举）。
package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 支付相关领域错误。
var (
	// ErrPaymentOrderNotFound 表示订单不存在。
	ErrPaymentOrderNotFound = errors.New("model: 充值订单不存在")
	// ErrPaymentOrderNotPending 表示订单不处于"待支付"，无法执行该操作。
	ErrPaymentOrderNotPending = errors.New("model: 订单不处于待支付状态")
)

// PaymentStatus 表示订单状态。
//
// 取值与数据库字段 payment_orders.status 一一对应，禁止随意改动数值。
type PaymentStatus int

const (
	// PaymentStatusPending 待支付：已下单，等待第三方回调或管理员确认。
	PaymentStatusPending PaymentStatus = 1
	// PaymentStatusPaid 已支付：额度已入账（终态）。
	PaymentStatusPaid PaymentStatus = 2
	// PaymentStatusClosed 已关闭：超时未支付或被管理员关闭（终态）。
	PaymentStatusClosed PaymentStatus = 3
	// PaymentStatusRefunded 已退款：额度已扣回（终态）。
	PaymentStatusRefunded PaymentStatus = 4
)

// String 返回状态中文名，便于日志与界面展示。
func (s PaymentStatus) String() string {
	switch s {
	case PaymentStatusPending:
		return "待支付"
	case PaymentStatusPaid:
		return "已支付"
	case PaymentStatusClosed:
		return "已关闭"
	case PaymentStatusRefunded:
		return "已退款"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

// IsTerminal 判断是否为终态。
func (s PaymentStatus) IsTerminal() bool {
	return s != PaymentStatusPending
}

// 支付方式名常量。
//
// 与 internal/payment 中注册的 Provider 名字对应；用字符串而非枚举，
// 是为了让"新增通道"不需要改动模型层（只需注册一个 Provider）。
const (
	// PaymentMethodEPay 第三方聚合支付（易支付协议，覆盖支付宝/微信等）。
	PaymentMethodEPay = "epay"
	// PaymentMethodStripe Stripe Checkout。
	PaymentMethodStripe = "stripe"
	// PaymentMethodAlipay 支付宝官方（电脑网站支付 / 当面付）。
	//
	// 与易支付通道里的子方式 "alipay" 无关：那是经由聚合网关转发的支付宝，
	// 本常量指资金直接进自己支付宝商户的官方通道，两者的商户号、密钥体系完全不同。
	PaymentMethodAlipay = "alipay"
	// PaymentMethodWeChatPay 微信支付官方（APIv3）。
	PaymentMethodWeChatPay = "wechatpay"
	// PaymentMethodManual 人工确认（无支付通道时由管理员手动入账）。
	PaymentMethodManual = "manual"
)

// PaymentOrder 表示一笔充值订单。
type PaymentOrder struct {
	ID      uint64 // 主键
	TradeNo string // 对外订单号（唯一）
	UserID  uint64 // 归属用户

	// Amount 是实付金额，单位为【分】。
	Amount int64
	// Currency 是货币代码（如 CNY / USD）。
	Currency string
	// Quota 是本单入账的额度，下单时固化，之后改兑换比例不影响历史订单。
	Quota int64

	// Method 是支付通道（epay / stripe / manual）。
	Method string
	// SubMethod 是通道内的具体方式（如 alipay / wxpay / card）。
	SubMethod string

	Status PaymentStatus
	// CreditedAt 是额度入账时间；零值表示尚未入账。
	//
	// 为什么与 Status 分开：回调重试、进程崩溃都可能在"订单已标记支付"
	// 与"额度已到账"之间留下缺口。分开记录后既能保证入账幂等，
	// 也能在启动时查出"已支付但未入账"的订单做补偿。
	CreditedAt time.Time
	// PayURL 是第三方返回的收银台地址（manual 通道为空）。
	PayURL string
	// ProviderTradeNo 是第三方订单号，用于对账。
	ProviderTradeNo string
	// NotifyPayload 保存回调原文，便于事后核对"到底收到了什么"。
	NotifyPayload string
	// Remark 是订单备注（如"充值 10 元"）。
	Remark string

	CreatedAt time.Time
	UpdatedAt time.Time
	// PaidAt 为支付完成时间；零值表示尚未支付。
	PaidAt time.Time
	// ExpiresAt 为支付截止时间；超时后订单应被关闭。
	ExpiresAt time.Time
}

// Validate 校验订单的必要字段。
func (o *PaymentOrder) Validate() error {
	if strings.TrimSpace(o.TradeNo) == "" {
		return errors.New("订单号不能为空")
	}
	if o.UserID == 0 {
		return errors.New("订单必须归属某个用户")
	}
	if o.Amount <= 0 {
		return fmt.Errorf("订单金额必须大于 0（单位：分），当前 %d", o.Amount)
	}
	if o.Quota < 0 {
		return fmt.Errorf("入账额度不能为负数，当前 %d", o.Quota)
	}
	if strings.TrimSpace(o.Method) == "" {
		return errors.New("必须指定支付通道")
	}
	if o.Status == 0 {
		o.Status = PaymentStatusPending
	}
	return nil
}

// IsExpired 判断订单在给定时刻是否已过支付时限。
func (o *PaymentOrder) IsExpired(now time.Time) bool {
	if o.ExpiresAt.IsZero() {
		return false
	}
	return now.After(o.ExpiresAt)
}

// IsCredited 判断额度是否已入账。
func (o *PaymentOrder) IsCredited() bool {
	return !o.CreditedAt.IsZero()
}

// AmountYuan 返回金额的"元"表示，仅用于展示。
//
// 用整数运算拼出两位小数，避免浮点误差（如 0.1+0.2）。
func (o *PaymentOrder) AmountYuan() string {
	return FormatCents(o.Amount)
}

// FormatCents 把"分"格式化为带两位小数的字符串。
func FormatCents(cents int64) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	result := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if negative {
		return "-" + result
	}
	return result
}

// PaymentOrderQuery 是订单列表的查询条件。
type PaymentOrderQuery struct {
	// UserID > 0 时只查该用户的订单；0 表示不限（管理员视角）。
	UserID uint64
	Method string
	Status *PaymentStatus
	Limit  int
	Offset int
}

// PaymentOrderRepository 定义充值订单的持久化操作。
type PaymentOrderRepository interface {
	// Create 创建订单。
	Create(ctx context.Context, order *PaymentOrder) error

	// GetByTradeNo 按订单号查询，不存在时返回 ErrPaymentOrderNotFound。
	GetByTradeNo(ctx context.Context, tradeNo string) (*PaymentOrder, error)

	// List 查询订单列表（按创建时间倒序）。
	List(ctx context.Context, query PaymentOrderQuery) ([]*PaymentOrder, error)

	// Count 统计符合条件的订单数。
	Count(ctx context.Context, query PaymentOrderQuery) (int64, error)

	// MarkPaid 把订单置为已支付。
	//
	// 返回值 paid 表示"本次调用是否真正完成了状态跃迁"：
	//   - true  ：本次由待支付 → 已支付，调用方【应该】给用户加额度；
	//   - false ：订单早已是已支付（回调重复到达），调用方【不得】重复加额度。
	//
	// 为什么把幂等判据放在 SQL 层：回调可能并发到达，
	// 用"先查状态再更新"存在竞态窗口，两次回调都可能通过检查。
	// 条件更新（WHERE status = 待支付）由数据库保证只有一个赢家。
	MarkPaid(ctx context.Context, tradeNo, providerTradeNo, payload string, paidAt time.Time) (bool, error)

	// UpdateStatus 修改订单状态（用于关闭、退款等由管理员触发的流转）。
	UpdateStatus(ctx context.Context, tradeNo string, status PaymentStatus) error

	// CreditOrder 给订单入账：在【同一事务】内标记已入账并把额度加到用户账户。
	//
	// 返回值 credited 表示本次是否真正入账：
	//   - true  ：本次完成了入账（调用方无需再做任何事）；
	//   - false ：该订单此前已入账（回调重复到达），本次未做任何修改。
	//
	// 为什么必须是单个方法 + 单个事务：
	//   "标记已入账"与"加额度"若分两次调用，中间崩溃就会出现
	//   "已标记但钱没到"或"钱到了但能再次入账（重复加额度）"。
	//   资金类操作不允许存在这种窗口。
	//
	// 实现约定：订单必须处于"已支付"状态；额度加在订单归属用户身上；
	// 若该用户为不限额度（quota = -1），则只标记入账、不修改额度。
	CreditOrder(ctx context.Context, tradeNo string, at time.Time) (bool, error)

	// ListPaidUncredited 列出"已支付但未入账"的订单，用于启动补偿。
	//
	// 存在这类订单的唯一原因是"标记支付"与"入账"之间的极端中断，
	// 数量应为 0；一旦出现必须补偿，否则就是用户付了钱没到账。
	ListPaidUncredited(ctx context.Context, limit int) ([]*PaymentOrder, error)

	// CloseExpired 关闭在 before 之前到期且仍未支付的订单，返回关闭条数。
	CloseExpired(ctx context.Context, before time.Time) (int64, error)

	// SumPaidQuota 统计某用户已支付的额度合计（用于充值记录展示）。
	SumPaidQuota(ctx context.Context, userID uint64) (int64, error)
}

// GenerateTradeNo 生成对外订单号。
//
// 形态：pay + YYYYMMDDHHMMSS + 6 位十六进制随机。
//
// 为什么前段带时间：对账时人工一眼能看出"这笔单是什么时候下的"，
// 而纯随机串必须去查库才能定位。后段随机保证同一秒内多笔订单不冲突。
func GenerateTradeNo() (string, error) {
	raw := make([]byte, 3)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("model: 生成订单号失败: %w", err)
	}
	return "pay" + time.Now().Format("20060102150405") + hex.EncodeToString(raw), nil
}
