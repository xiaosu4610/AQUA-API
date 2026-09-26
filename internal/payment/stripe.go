// 本文件实现 Stripe Checkout 通道。
//
// 意图（Why）：
//
//	面向海外用户时 Stripe 是事实标准。它的接入方式也很"协议化"：
//	下单是一次表单编码的 REST 调用，回调是 JSON + 请求头签名，
//	因此无需引入官方 SDK 就能完整实现（符合本项目"零额外依赖"的偏好）。
//
// 协议要点（来自 Stripe 公开文档，见代码内注释）：
//
//	下单：POST https://api.stripe.com/v1/checkout/sessions
//	  表单参数 mode=payment、success_url、cancel_url、
//	  line_items[0][price_data][currency|product_data][name]|[unit_amount]、
//	  client_reference_id（我们放本地订单号）、metadata[trade_no]
//	  请求头 Authorization: Bearer <secret key>
//	  响应 JSON：{"id":"cs_...","url":"https://checkout.stripe.com/..."}
//
//	回调：POST {webhook} 请求头 Stripe-Signature: t=<时间戳>,v1=<签名>
//	  签名算法：HMAC-SHA256(webhook_secret, "<t>.<原始请求体>")
//	  事件体 {"type":"checkout.session.completed","data":{"object":{...}}}
//	  成功判据：object.payment_status == "paid"
//
// 安全说明：
//
//	Stripe 的签名以【原始请求体】为输入，因此中间件与处理器都不能改写 body，
//	否则验签必然失败。这也是本包 Notify 结构体保留 Body 字段的原因。
//
// 流转（Flow）：
//
//	Create → 表单编码 REST 调用 → 返回收银台地址
//	ParseNotify → 校验 Stripe-Signature → 解析事件 → 返回订单号与金额
//
// 扩展（Extend）：
//
//	需要订阅制（mode=subscription）时，把 mode 与 line_items 形状参数化即可；
//	本文件已把 API 地址与事件名抽成常量，改动集中。
package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// Stripe 协议常量。
const (
	// stripeAPIBase 是 Stripe 的 API 根地址。
	stripeAPIBase = "https://api.stripe.com"
	// stripeCheckoutSessionsPath 是创建结账会话的端点。
	stripeCheckoutSessionsPath = "/v1/checkout/sessions"
	// stripeEventPaid 是"结账完成"事件名。
	stripeEventPaid = "checkout.session.completed"
	// stripeSignatureHeader 是签名所在的请求头。
	stripeSignatureHeader = "Stripe-Signature"
	// stripeSignatureTolerance 是签名时间戳的容忍窗口。
	//
	// 取 5 分钟：Stripe 官方建议值。作用是防止重放——
	// 超过窗口的签名即使正确也拒绝，攻击者无法用截获的旧请求反复入账。
	stripeSignatureTolerance = 5 * time.Minute
	// stripeMaxBodyBytes 是回调体的读取上限。
	stripeMaxBodyBytes = 1 << 20
)

// stripeProvider 实现 Stripe Checkout 通道。
type stripeProvider struct {
	opts   Options
	client *http.Client
}

// newStripeProvider 构造 Stripe 通道。
func newStripeProvider(opts Options) Provider {
	return &stripeProvider{opts: opts, client: newHTTPClient()}
}

// Name 返回通道名。
func (p *stripeProvider) Name() string { return model.PaymentMethodStripe }

// settings 读取当前运营参数。
func (p *stripeProvider) settings(ctx context.Context) (model.PaymentSettings, error) {
	if p.opts.Settings == nil {
		return model.PaymentSettings{}, nil
	}
	return p.opts.Settings(ctx)
}

// Create 创建 Checkout Session 并返回收银台地址。
func (p *stripeProvider) Create(ctx context.Context, req *Request) (*CreateResult, error) {
	settings, err := p.settings(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.MethodEnabled(model.PaymentMethodStripe) {
		return nil, ErrProviderDisabled
	}
	secret := strings.TrimSpace(p.opts.Secrets.StripeSecretKey)
	if secret == "" {
		return nil, fmt.Errorf("%w：未配置 Stripe Secret Key（环境变量 AQUA_STRIPE_SECRET_KEY）", ErrNotConfigured)
	}

	// 商品名从通道参数读取（键 "stripe.note"），Param 内部保留了历史字段回退
	note := settings.Param(model.PaymentMethodStripe, "note")
	if note == "" {
		note = req.Subject
	}
	if note == "" {
		note = "账户充值"
	}

	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", req.ReturnURL)
	form.Set("cancel_url", req.ReturnURL)
	// client_reference_id 是 Stripe 提供的"回传自定义标识"字段，
	// 我们放本地订单号，回调时据此定位订单。
	form.Set("client_reference_id", req.Order.TradeNo)
	form.Set("metadata[trade_no]", req.Order.TradeNo)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(settings.CurrencyOrDefault()))
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(req.Order.Amount, 10))
	form.Set("line_items[0][price_data][product_data][name]", note)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		stripeAPIBase+stripeCheckoutSessionsPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("构造 Stripe 请求失败: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+secret)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求 Stripe 失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, stripeMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 Stripe 响应失败: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		// 不回传上游原文（可能含内部标识），只给状态码与简要信息
		return nil, fmt.Errorf("Stripe 返回 HTTP %d：%s", resp.StatusCode, stripeErrorMessage(raw))
	}

	var parsed struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, errors.New("Stripe 响应不是合法 JSON")
	}
	if strings.TrimSpace(parsed.URL) == "" {
		return nil, errors.New("Stripe 未返回收银台地址")
	}

	return &CreateResult{PayURL: parsed.URL, ProviderTradeNo: parsed.ID}, nil
}

// stripeErrorMessage 从 Stripe 错误体中提取可读信息。
func stripeErrorMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return "请求被拒绝"
}

// ParseNotify 校验 Stripe 签名并解析事件。
func (p *stripeProvider) ParseNotify(_ context.Context, notify *Notify) (*NotifyResult, error) {
	if notify == nil {
		return nil, ErrSignatureInvalid
	}
	secret := strings.TrimSpace(p.opts.Secrets.StripeWebhookSecret)
	if secret == "" {
		return nil, fmt.Errorf("%w：未配置 Webhook 签名密钥（环境变量 AQUA_STRIPE_WEBHOOK_SECRET）", ErrNotConfigured)
	}

	if err := verifyStripeSignature(notify.Header.Get(stripeSignatureHeader), notify.Body, secret, time.Now()); err != nil {
		return nil, err
	}

	var event struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID                string `json:"id"`
				ClientReferenceID string `json:"client_reference_id"`
				PaymentStatus     string `json:"payment_status"`
				AmountTotal       int64  `json:"amount_total"`
				Metadata          struct {
					TradeNo string `json:"trade_no"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(notify.Body, &event); err != nil {
		return nil, errors.New("Stripe 回调不是合法 JSON")
	}

	object := event.Data.Object
	tradeNo := strings.TrimSpace(object.Metadata.TradeNo)
	if tradeNo == "" {
		tradeNo = strings.TrimSpace(object.ClientReferenceID)
	}

	// Stripe 会推送多种事件，只有结账完成且确实已付款才算入账依据。
	paid := event.Type == stripeEventPaid && object.PaymentStatus == "paid"

	return &NotifyResult{
		TradeNo:         tradeNo,
		ProviderTradeNo: strings.TrimSpace(object.ID),
		AmountCents:     object.AmountTotal,
		Paid:            paid,
		// Stripe 只要求 2xx，响应体内容不限
		AckBody:        `{"received":true}`,
		AckContentType: "application/json; charset=utf-8",
	}, nil
}

// verifyStripeSignature 校验 Stripe-Signature 请求头。
//
// 头格式：t=<unix 秒>,v1=<十六进制签名>[,v1=<另一个签名>]
// 签名内容：HMAC-SHA256(secret, "<t>.<原始请求体>")
//
// 为什么不校验 v0：v0 是已废弃的旧方案，接受它等于放宽攻击面。
func verifyStripeSignature(header string, body []byte, secret string, now time.Time) error {
	if strings.TrimSpace(header) == "" {
		return fmt.Errorf("%w：缺少 %s 请求头", ErrSignatureInvalid, stripeSignatureHeader)
	}

	var (
		timestamp  int64
		signatures []string
	)
	for _, part := range strings.Split(header, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch name {
		case "t":
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return fmt.Errorf("%w：时间戳非法", ErrSignatureInvalid)
			}
			timestamp = parsed
		case "v1":
			signatures = append(signatures, strings.TrimSpace(value))
		}
	}

	if timestamp == 0 || len(signatures) == 0 {
		return fmt.Errorf("%w：请求头缺少时间戳或签名", ErrSignatureInvalid)
	}
	// 时间窗口校验：防止重放旧请求
	if now.Sub(time.Unix(timestamp, 0)).Abs() > stripeSignatureTolerance {
		return fmt.Errorf("%w：签名时间戳超出容忍窗口", ErrSignatureInvalid)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	// 用常量时间比较，避免通过响应时间差逐字节猜测签名
	for _, candidate := range signatures {
		if hmac.Equal([]byte(candidate), []byte(expected)) {
			return nil
		}
	}
	return ErrSignatureInvalid
}
