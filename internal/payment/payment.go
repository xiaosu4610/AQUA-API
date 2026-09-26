// Package payment 实现支付通道适配与回调验签。
//
// 意图（Why）：
//
//	网关的充值能力必须能对接"任意一种"支付服务：有人用易支付（国内聚合支付），
//	有人用 Stripe（海外），也有人根本不需要在线支付（自用部署，管理员手动入账）。
//	把差异收敛在 Provider 接口里，server 层就只需处理"下单 → 拿支付地址"与
//	"收到回调 → 验签 → 幂等入账"两件事，永远不用为新增通道改业务代码。
//
// 两条不可动摇的约束（都要在本包内落实，不能推给调用方）：
//
//  1. 【必须验签】。回调接口是公网可访问的，若不验签，任何人构造一个
//     POST 就能给自己充值——这是最直接、最严重的资损漏洞。
//  2. 【必须校验金额】。验签通过只说明"请求来自支付平台"，
//     但不排除"用户下 1 元单、支付 0.01 元"这类篡改。
//     因此回调里的金额要与本地订单金额比对，不一致则拒绝入账。
//
// 流转（Flow）：
//
//	下单：server → Registry.Get(method) → Provider.Create → 返回支付地址
//	回调：server → Registry.Get(method) → Provider.ParseNotify → 验签 + 取金额
//	     → 比对本地订单 → 幂等加额度
//
// 扩展（Extend）：
//
//	新增通道：实现 Provider（Name / Create / ParseNotify），在 NewRegistry 注册。
//	新增通道时请一并说明"如何获取商户密钥"，使用者才配置得起来。
package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 本包对外暴露的哨兵错误，供上层用 errors.Is 精确判断并映射 HTTP 状态码。
var (
	// ErrProviderUnknown 表示请求的支付通道没有对应的适配器。
	ErrProviderUnknown = errors.New("payment: 未知的支付通道")
	// ErrProviderDisabled 表示该通道已被站长关闭或密钥未配置。
	ErrProviderDisabled = errors.New("payment: 该支付通道未启用")
	// ErrNotConfigured 表示通道缺少必要配置（网关地址 / 商户号 / 密钥）。
	ErrNotConfigured = errors.New("payment: 支付通道未配置完整")
	// ErrSignatureInvalid 表示回调验签失败——必须拒绝处理。
	ErrSignatureInvalid = errors.New("payment: 回调验签失败")
	// ErrAmountMismatch 表示回调金额与本地订单不一致——必须拒绝入账。
	ErrAmountMismatch = errors.New("payment: 回调金额与订单不一致")
	// ErrUnsupportedNotify 表示该通道不支持异步回调（如人工通道）。
	ErrUnsupportedNotify = errors.New("payment: 该支付通道不支持异步回调")
)

// httpTimeout 是调用支付服务接口的超时。
//
// 取 20 秒：支付网关偶有抖动，但下单接口不该让用户等太久——
// 超时后用户可以重试，比"页面一直转圈最后超时"体验更好。
const httpTimeout = 20 * time.Second

// Request 是一次下单请求（由 server 组装）。
type Request struct {
	Order *model.PaymentOrder
	// Subject 是支付页显示的商品名（如"充值 10.00 元"）。
	Subject string
	// NotifyURL 是异步通知地址（支付平台回调本网关）。
	NotifyURL string
	// ReturnURL 是支付成功后的浏览器跳转地址。
	ReturnURL string
	// ClientIP 是下单用户 IP。部分网关要求回传以便风控。
	ClientIP string
}

// CreateResult 是发起支付的结果。
type CreateResult struct {
	// PayURL 是用户需要跳转的收银台地址；人工通道为空。
	PayURL string
	// ProviderTradeNo 是第三方订单号（若下单时即返回）。
	ProviderTradeNo string
}

// Notify 是一次异步回调的原始输入（框架无关，便于单测直接构造）。
type Notify struct {
	// Header 用于 Stripe 等基于请求头的签名校验。
	Header http.Header
	// Query 是 URL 查询参数。
	Query url.Values
	// Form 是已解析的表单参数（application/x-www-form-urlencoded）。
	Form url.Values
	// Body 是原始请求体（Stripe 验签必须用原文，不能重新序列化）。
	Body []byte
}

// NotifyResult 是验签通过后的结论。
type NotifyResult struct {
	// TradeNo 是本地订单号（从回调参数中取回）。
	TradeNo string
	// ProviderTradeNo 是第三方订单号，用于对账。
	ProviderTradeNo string
	// AmountCents 是第三方声明的实付金额（分），用于与本地订单比对。
	AmountCents int64
	// Paid 表示这笔回调的语义是"支付成功"。
	Paid bool
	// AckBody 是回给支付平台的响应体（易支付要求返回 success 才停止重试）。
	AckBody string
	// AckContentType 是响应体的 Content-Type。
	AckContentType string
}

// Provider 是支付通道适配器。
type Provider interface {
	// Name 返回通道名（与 model.PaymentMethod* 常量对应）。
	Name() string
	// Create 发起支付。
	Create(ctx context.Context, req *Request) (*CreateResult, error)
	// ParseNotify 解析并【验签】异步回调。
	//
	// 约定：验签失败必须返回 ErrSignatureInvalid；金额不一致返回 ErrAmountMismatch。
	// 上层据此拒绝入账，本方法不得"自动容忍"。
	ParseNotify(ctx context.Context, notify *Notify) (*NotifyResult, error)
}

// Options 是注册表构造参数。
type Options struct {
	// Secrets 是各通道的密钥（只来自环境变量）。
	Secrets config.PaymentConfig
	// Settings 返回当前支付运营参数（网关地址、商户号、启用列表）。
	//
	// 为什么用回调而不是直接传值：这些参数由管理员在后台随时修改，
	// 若在下单/回调时用的是启动时的快照，就会出现"后台改了网关地址但不生效"。
	Settings func(ctx context.Context) (model.PaymentSettings, error)
}

// Registry 是按名称索引的通道注册表。
//
// 并发安全：构造后只读。
type Registry struct {
	providers map[string]Provider
	order     []string // 保持注册顺序，便于界面稳定展示
}

// NewRegistry 构造注册表并注册全部内置通道。
//
// 未配置密钥的通道也会被注册（只是调用时返回 ErrNotConfigured）：
// 这样后台能把"通道存在但还没配置好"如实告诉使用者，
// 而不是让它从列表里神秘消失、让人以为是功能缺失。
func NewRegistry(opts Options) *Registry {
	providers := []Provider{
		newEPayProvider(opts),
		newStripeProvider(opts),
		newManualProvider(opts),
	}

	registry := &Registry{
		providers: make(map[string]Provider, len(providers)),
		order:     make([]string, 0, len(providers)),
	}
	for _, provider := range providers {
		registry.providers[provider.Name()] = provider
		registry.order = append(registry.order, provider.Name())
	}
	return registry
}

// Get 按名称取通道。
func (r *Registry) Get(name string) (Provider, error) {
	if r == nil {
		return nil, ErrProviderUnknown
	}
	provider, ok := r.providers[strings.TrimSpace(name)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderUnknown, name)
	}
	return provider, nil
}

// Names 返回全部已注册的通道名（按注册顺序）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	result := make([]string, len(r.order))
	copy(result, r.order)
	return result
}

// newHTTPClient 构造支付接口专用的 HTTP 客户端。
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// yuanFromCents 把"分"格式化为支付网关要求的"元"字符串（两位小数）。
//
// 支付网关普遍要求 "10.00" 这种形式，用整数拼字符串可避免浮点误差。
func yuanFromCents(cents int64) string {
	return model.FormatCents(cents)
}
