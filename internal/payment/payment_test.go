// 支付通道适配器的单元测试。
//
// 测试重点（这些都是"错了会直接资损"的地方）：
//   - 易支付签名对参数顺序不敏感（排序后拼接），但对参数值、密钥敏感；
//   - 金额解析不走浮点，10.01 必须精确得到 1001 分；
//   - 回调验签必须拒绝被篡改的请求；
//   - Stripe 签名校验必须校验时间窗口（防重放）且用常量时间比较。
package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// testSettings 返回一份启用全部通道的支付设置。
func testSettings() model.PaymentSettings {
	return model.PaymentSettings{
		Enabled:         true,
		Methods:         []string{model.PaymentMethodEPay, model.PaymentMethodStripe, model.PaymentMethodManual},
		ExchangeRate:    100,
		Currency:        "CNY",
		MinCents:        100,
		OrderTTLMinutes: 30,
		EPayGateway:     "https://pay.example.com",
		EPayPID:         "1001",
		EPayTypes:       []string{"alipay"},
	}
}

// newTestRegistry 构造带测试设置的注册表。
func newTestRegistry(secrets config.PaymentConfig) *Registry {
	return NewRegistry(Options{
		Secrets: secrets,
		Settings: func(context.Context) (model.PaymentSettings, error) {
			return testSettings(), nil
		},
	})
}

func TestEPaySign_与参数顺序无关且随值变化(t *testing.T) {
	key := "test-secret-key"

	left := map[string]string{"b": "2", "a": "1", "sign": "忽略我", "sign_type": "MD5"}
	right := map[string]string{"a": "1", "b": "2"}

	if epaySign(left, key) != epaySign(right, key) {
		t.Fatal("签名应只依赖有效参数，与传入顺序及 sign/sign_type 无关")
	}

	// 空值参数不参与签名（易支付协议的约定）
	withEmpty := map[string]string{"a": "1", "b": "2", "c": ""}
	if epaySign(withEmpty, key) != epaySign(right, key) {
		t.Fatal("空值参数不应参与签名")
	}

	// 改动任何一个值都必须改变签名，否则说明签名没有覆盖到该参数
	tampered := map[string]string{"a": "1", "b": "3"}
	if epaySign(tampered, key) == epaySign(right, key) {
		t.Fatal("参数值变化后签名必须随之变化")
	}

	// 密钥不同则签名不同
	if epaySign(right, "另一个密钥") == epaySign(right, key) {
		t.Fatal("密钥变化后签名必须随之变化")
	}
}

func TestYuanToCents_整数解析无浮点误差(t *testing.T) {
	cases := map[string]int64{
		"":      0,
		"1":     100,
		"10.01": 1001,
		"0.1":   10,
		"0.01":  1,
		"19.99": 1999,
		"100":   10000,
		// 超过两位小数：截断而非四舍五入，避免"多给"
		"1.005": 100,
		"-2.5":  -250,
	}
	for raw, want := range cases {
		if got := yuanToCents(raw); got != want {
			t.Errorf("yuanToCents(%q) = %d，期望 %d", raw, got, want)
		}
	}
}

func TestEPayCreate_缺少配置应明确报错(t *testing.T) {
	// 未提供商户密钥：必须报"未配置完整"，而不是生成一个必然失败的跳转地址
	registry := newTestRegistry(config.PaymentConfig{})
	provider, err := registry.Get(model.PaymentMethodEPay)
	if err != nil {
		t.Fatalf("取通道失败: %v", err)
	}

	order := &model.PaymentOrder{TradeNo: "pay1", UserID: 1, Amount: 1000, Method: model.PaymentMethodEPay}
	_, err = provider.Create(context.Background(), &Request{Order: order, Subject: "充值"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("应返回 ErrNotConfigured，实际 %v", err)
	}
}

func TestEPayCreate_生成带签名的跳转地址(t *testing.T) {
	registry := newTestRegistry(config.PaymentConfig{EPayKey: "test-secret-key"})
	provider, _ := registry.Get(model.PaymentMethodEPay)

	order := &model.PaymentOrder{
		TradeNo: "pay20260101000000abcdef",
		UserID:  1,
		Amount:  1050, // 10.50 元
		Method:  model.PaymentMethodEPay,
	}
	result, err := provider.Create(context.Background(), &Request{
		Order:     order,
		Subject:   "充值 10.50 元",
		NotifyURL: "https://api.example.com/api/payments/epay/notify",
		ReturnURL: "https://api.example.com/console/recharge",
	})
	if err != nil {
		t.Fatalf("创建支付失败: %v", err)
	}

	parsed, err := url.Parse(result.PayURL)
	if err != nil {
		t.Fatalf("返回的地址不是合法 URL: %v", err)
	}
	query := parsed.Query()

	if got := query.Get("money"); got != "10.50" {
		t.Fatalf("金额应格式化为两位小数的元，实际 %q", got)
	}
	if got := query.Get("pid"); got != "1001" {
		t.Fatalf("商户号不正确: %q", got)
	}
	if got := query.Get("out_trade_no"); got != order.TradeNo {
		t.Fatalf("订单号不正确: %q", got)
	}
	if query.Get("sign") == "" {
		t.Fatal("跳转地址必须带签名")
	}
}

func TestEPayParseNotify_验签通过(t *testing.T) {
	key := "test-secret-key"
	registry := newTestRegistry(config.PaymentConfig{EPayKey: key})
	provider, _ := registry.Get(model.PaymentMethodEPay)

	params := map[string]string{
		"pid":          "1001",
		"out_trade_no": "pay20260101000000abcdef",
		"trade_no":     "third-999",
		"money":        "10.50",
		"trade_status": "TRADE_SUCCESS",
		"type":         "alipay",
	}
	params["sign"] = epaySign(params, key)
	params["sign_type"] = "MD5"

	form := url.Values{}
	for name, value := range params {
		form.Set(name, value)
	}

	result, err := provider.ParseNotify(context.Background(), &Notify{Form: form, Query: url.Values{}})
	if err != nil {
		t.Fatalf("验签应通过: %v", err)
	}
	if !result.Paid {
		t.Fatal("TRADE_SUCCESS 应被判定为已支付")
	}
	if result.TradeNo != "pay20260101000000abcdef" || result.ProviderTradeNo != "third-999" {
		t.Fatalf("解析结果不正确: %+v", result)
	}
	if result.AmountCents != 1050 {
		t.Fatalf("金额应为 1050 分，实际 %d", result.AmountCents)
	}
	if strings.TrimSpace(result.AckBody) != "success" {
		t.Fatalf("易支付要求应答 success，实际 %q", result.AckBody)
	}
}

func TestEPayParseNotify_篡改金额必须验签失败(t *testing.T) {
	key := "test-secret-key"
	registry := newTestRegistry(config.PaymentConfig{EPayKey: key})
	provider, _ := registry.Get(model.PaymentMethodEPay)

	params := map[string]string{
		"pid":          "1001",
		"out_trade_no": "pay20260101000000abcdef",
		"money":        "0.01",
		"trade_status": "TRADE_SUCCESS",
	}
	params["sign"] = epaySign(params, key)
	// 攻击者把金额改成 9999，但签名仍是按 0.01 算的
	params["money"] = "9999.00"

	form := url.Values{}
	for name, value := range params {
		form.Set(name, value)
	}

	_, err := provider.ParseNotify(context.Background(), &Notify{Form: form})
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("篡改金额后必须验签失败，实际 %v", err)
	}
}

func TestEPayParseNotify_未配置密钥必须拒绝(t *testing.T) {
	registry := newTestRegistry(config.PaymentConfig{})
	provider, _ := registry.Get(model.PaymentMethodEPay)

	form := url.Values{}
	form.Set("out_trade_no", "pay1")
	form.Set("trade_status", "TRADE_SUCCESS")

	_, err := provider.ParseNotify(context.Background(), &Notify{Form: form})
	if err == nil {
		t.Fatal("未配置密钥时无法验签，必须拒绝而不是放行")
	}
}

func TestManual_不支持回调(t *testing.T) {
	registry := newTestRegistry(config.PaymentConfig{})
	provider, _ := registry.Get(model.PaymentMethodManual)

	_, err := provider.ParseNotify(context.Background(), &Notify{})
	if !errors.Is(err, ErrUnsupportedNotify) {
		t.Fatalf("人工通道应拒绝回调，实际 %v", err)
	}

	// Create 不返回支付地址（界面据此提示"等待管理员确认"）
	result, err := provider.Create(context.Background(), &Request{
		Order: &model.PaymentOrder{TradeNo: "pay1", UserID: 1, Amount: 100, Method: model.PaymentMethodManual},
	})
	if err != nil {
		t.Fatalf("人工通道下单不应报错: %v", err)
	}
	if result.PayURL != "" {
		t.Fatalf("人工通道不应返回跳转地址，实际 %q", result.PayURL)
	}
}

func TestStripeCreate_未配置密钥应报错(t *testing.T) {
	registry := newTestRegistry(config.PaymentConfig{})
	provider, _ := registry.Get(model.PaymentMethodStripe)

	_, err := provider.Create(context.Background(), &Request{
		Order: &model.PaymentOrder{TradeNo: "pay1", UserID: 1, Amount: 100, Method: model.PaymentMethodStripe},
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("应返回 ErrNotConfigured，实际 %v", err)
	}
}

// signStripe 按 Stripe 的算法生成签名头，用于测试验签逻辑本身。
func signStripe(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyStripeSignature_通过与被拒(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"type":"checkout.session.completed"}`)
	now := time.Now()

	if err := verifyStripeSignature(signStripe(secret, now.Unix(), body), body, secret, now); err != nil {
		t.Fatalf("正确签名应通过: %v", err)
	}

	// 1) 签名不匹配
	if err := verifyStripeSignature(signStripe("另一个密钥", now.Unix(), body), body, secret, now); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("错误密钥应验签失败，实际 %v", err)
	}
	// 2) 请求体被篡改
	if err := verifyStripeSignature(signStripe(secret, now.Unix(), body), []byte(`{"type":"x"}`), secret, now); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("请求体被篡改应验签失败，实际 %v", err)
	}
	// 3) 时间戳过期（防重放）
	old := now.Add(-10 * time.Minute)
	if err := verifyStripeSignature(signStripe(secret, old.Unix(), body), body, secret, now); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("过期签名应被拒绝，实际 %v", err)
	}
	// 4) 缺少请求头
	if err := verifyStripeSignature("", body, secret, now); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("缺少签名头应被拒绝，实际 %v", err)
	}
}

func TestStripeParseNotify_事件与订单号解析(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"type":"checkout.session.completed","data":{"object":{` +
		`"id":"cs_test_1","client_reference_id":"pay123","payment_status":"paid",` +
		`"amount_total":1050,"metadata":{"trade_no":"pay123"}}}}`)

	registry := NewRegistry(Options{
		Secrets: config.PaymentConfig{StripeSecretKey: "sk_test", StripeWebhookSecret: secret},
		Settings: func(context.Context) (model.PaymentSettings, error) {
			return testSettings(), nil
		},
	})
	provider, _ := registry.Get(model.PaymentMethodStripe)

	header := http.Header{}
	header.Set(stripeSignatureHeader, signStripe(secret, time.Now().Unix(), body))

	result, err := provider.ParseNotify(context.Background(), &Notify{Header: header, Body: body})
	if err != nil {
		t.Fatalf("验签应通过: %v", err)
	}
	if !result.Paid {
		t.Fatal("checkout.session.completed + paid 应判定为已支付")
	}
	if result.TradeNo != "pay123" {
		t.Fatalf("订单号应为 pay123，实际 %q", result.TradeNo)
	}
	if result.AmountCents != 1050 {
		t.Fatalf("金额应为 1050 分，实际 %d", result.AmountCents)
	}
}

func TestRegistry_Get未知通道(t *testing.T) {
	registry := newTestRegistry(config.PaymentConfig{})

	if _, err := registry.Get("不存在的通道"); !errors.Is(err, ErrProviderUnknown) {
		t.Fatalf("应返回 ErrProviderUnknown，实际 %v", err)
	}
	names := registry.Names()
	if len(names) != 3 {
		t.Fatalf("应注册 3 个内置通道，实际 %d 个: %v", len(names), names)
	}
}
