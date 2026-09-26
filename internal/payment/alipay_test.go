// 支付宝官方通道的单元测试。
//
// 意图（Why）：
//
//	验签与金额换算"错了就直接资损"，因此这里用现场生成的 RSA 密钥对
//	（不依赖真实密钥与网络）把几条不变量固化成测试：
//	  - 下单签名生成后能被同一把公钥自验通过（签名串口径正确）；
//	  - 篡改金额 / 订单号后必须验签失败；
//	  - 只有 TRADE_SUCCESS / TRADE_FINISHED 才算已支付；
//	  - total_amount（元）→ 分 的换算无浮点误差。
//
// 流转（Flow）：
//
//	go test ./internal/payment/ → 生成密钥对 → 构造请求/回调 → 断言验签与解析
//
// 扩展（Extend）：
//
//	新增支付宝接口形态（如手机网站支付）时，复用本文件的自签自验思路即可。
package payment

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// generateRSAKeyPair 现场生成一把 2048 位 RSA 密钥对，
// 返回私钥对象、私钥 PEM、公钥 PEM。测试绝不依赖真实密钥或网络。
func generateRSAKeyPair(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}

	privatePEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("序列化公钥失败: %v", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})

	return key, string(privatePEM), string(publicPEM)
}

// newOfficialPaymentTestRegistry 构造一份"已启用官方通道并填好通道参数"的注册表。
//
// 与通用 newTestRegistry 的区别：它额外启用 alipay / wechatpay，
// 并注入这两个通道从设置表读取的非密钥参数（App ID、商户号、序列号、网关）。
func newOfficialPaymentTestRegistry(secrets config.PaymentConfig) *Registry {
	return NewRegistry(Options{
		Secrets: secrets,
		Settings: func(context.Context) (model.PaymentSettings, error) {
			settings := testSettings()
			settings.Methods = append(settings.Methods, model.PaymentMethodAlipay, model.PaymentMethodWeChatPay)
			settings.Params = map[string]string{
				"alipay.app_id":       "2021000000000000",
				"alipay.gateway":      alipayDefaultGateway,
				"wechatpay.mch_id":    "1900000109",
				"wechatpay.app_id":    "wx8888888888888888",
				"wechatpay.serial_no": "5157F09EFDC096DE15EBE81A47057A72",
			}
			return settings, nil
		},
	})
}

// TestParseRSAKeys_支持PEM与裸Base64 校验密钥解析的两种形态。
func TestParseRSAKeys_支持PEM与裸Base64(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)

	// PEM 形态
	if _, err := parseRSAPrivateKey(privatePEM, "私钥"); err != nil {
		t.Fatalf("PEM 私钥应能解析: %v", err)
	}
	if _, err := parseRSAPublicKey(publicPEM, "公钥"); err != nil {
		t.Fatalf("PEM 公钥应能解析: %v", err)
	}

	// 裸 base64 形态（控制台常导出成这种：去掉 BEGIN/END 头尾与换行）
	rawPriv := encodeBase64PKCS8(t, key)
	if _, err := parseRSAPrivateKey(rawPriv, "私钥"); err != nil {
		t.Fatalf("裸 base64 私钥应能解析: %v", err)
	}

	// 非法内容必须报"格式问题"，而不是静默失败
	if _, err := parseRSAPrivateKey("这不是密钥", "私钥"); err == nil {
		t.Fatal("非法私钥应报错")
	}
}

// TestAlipayCreate_生成可自验签的跳转地址 校验下单参数与签名口径。
func TestAlipayCreate_生成可自验签的跳转地址(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		AlipayPrivateKey: privatePEM,
		AlipayPublicKey:  publicPEM,
	})
	provider, _ := registry.Get(model.PaymentMethodAlipay)

	order := &model.PaymentOrder{
		TradeNo: "pay20260101000000abcdef",
		UserID:  1,
		Amount:  1050, // 10.50 元
		Method:  model.PaymentMethodAlipay,
	}
	result, err := provider.Create(context.Background(), &Request{
		Order:     order,
		Subject:   "充值 10.50 元",
		NotifyURL: "https://api.example.com/api/payments/alipay/notify",
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

	if got := query.Get("method"); got != alipayPagePayMethod {
		t.Fatalf("method 应为 %q，实际 %q", alipayPagePayMethod, got)
	}
	if got := query.Get("sign_type"); got != alipaySignType {
		t.Fatalf("sign_type 应为 %q，实际 %q", alipaySignType, got)
	}
	if got := query.Get("app_id"); got != "2021000000000000" {
		t.Fatalf("app_id 不正确: %q", got)
	}

	var biz struct {
		OutTradeNo  string `json:"out_trade_no"`
		TotalAmount string `json:"total_amount"`
		ProductCode string `json:"product_code"`
	}
	if err := json.Unmarshal([]byte(query.Get("biz_content")), &biz); err != nil {
		t.Fatalf("biz_content 不是合法 JSON: %v", err)
	}
	if biz.OutTradeNo != order.TradeNo {
		t.Fatalf("biz_content.out_trade_no 不正确: %q", biz.OutTradeNo)
	}
	if biz.TotalAmount != "10.50" {
		t.Fatalf("biz_content.total_amount 应为两位小数的元，实际 %q", biz.TotalAmount)
	}
	if biz.ProductCode != alipayProductCode {
		t.Fatalf("product_code 不正确: %q", biz.ProductCode)
	}

	// 自验签：用同一口径重算待签串，再用公钥验证 sign
	params := map[string]string{}
	for name, values := range query {
		if len(values) > 0 {
			params[name] = values[0]
		}
	}
	if err := rsa2Verify(&key.PublicKey, alipaySignContent(params), params["sign"]); err != nil {
		t.Fatalf("下单签名应能被公钥验证通过: %v", err)
	}
}

// TestAlipayParseNotify_状态判定与验签 校验只有成功状态才算已支付。
func TestAlipayParseNotify_状态判定与验签(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		AlipayPrivateKey: privatePEM,
		AlipayPublicKey:  publicPEM,
	})
	provider, _ := registry.Get(model.PaymentMethodAlipay)

	cases := []struct {
		status   string
		wantPaid bool
	}{
		{alipayTradeSuccess, true},
		{alipayTradeFinished, true},
		{"WAIT_BUYER_PAY", false},
		{"TRADE_CLOSED", false},
	}
	for _, testCase := range cases {
		form := signedAlipayForm(t, key, map[string]string{
			"app_id":       "2021000000000000",
			"out_trade_no": "pay20260101000000abcdef",
			"trade_no":     "2026010122001400000000000001",
			"total_amount": "10.00",
			"trade_status": testCase.status,
			"sign_type":    alipaySignType,
		})

		result, err := provider.ParseNotify(context.Background(), &Notify{Form: form})
		if err != nil {
			t.Fatalf("状态 %s 验签应通过: %v", testCase.status, err)
		}
		if result.Paid != testCase.wantPaid {
			t.Fatalf("状态 %s 的 Paid 应为 %v，实际 %v", testCase.status, testCase.wantPaid, result.Paid)
		}
		if result.TradeNo != "pay20260101000000abcdef" {
			t.Fatalf("订单号不正确: %q", result.TradeNo)
		}
		if result.AmountCents != 1000 {
			t.Fatalf("10.00 元应为 1000 分，实际 %d", result.AmountCents)
		}
		if result.AckBody != alipayAckBody {
			t.Fatalf("回调应答应为 %q，实际 %q", alipayAckBody, result.AckBody)
		}
		if result.AckContentType != alipayAckContentType {
			t.Fatalf("应答 Content-Type 不正确: %q", result.AckContentType)
		}
	}
}

// TestAlipayParseNotify_篡改金额或订单号必须验签失败 校验防篡改。
func TestAlipayParseNotify_篡改金额或订单号必须验签失败(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		AlipayPrivateKey: privatePEM,
		AlipayPublicKey:  publicPEM,
	})
	provider, _ := registry.Get(model.PaymentMethodAlipay)

	base := map[string]string{
		"app_id":       "2021000000000000",
		"out_trade_no": "pay20260101000000abcdef",
		"total_amount": "0.01",
		"trade_status": alipayTradeSuccess,
		"sign_type":    alipaySignType,
	}
	signature := alipaySign(t, key, base)

	// 篡改金额
	tamperedAmount := cloneParams(base)
	tamperedAmount["total_amount"] = "9999.00"
	tamperedAmount["sign"] = signature
	if _, err := provider.ParseNotify(context.Background(), &Notify{Form: toForm(tamperedAmount)}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("篡改金额后必须验签失败，实际 %v", err)
	}

	// 篡改订单号
	tamperedTradeNo := cloneParams(base)
	tamperedTradeNo["out_trade_no"] = "pay-attacker"
	tamperedTradeNo["sign"] = signature
	if _, err := provider.ParseNotify(context.Background(), &Notify{Form: toForm(tamperedTradeNo)}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("篡改订单号后必须验签失败，实际 %v", err)
	}
}

// TestAlipayParseNotify_元转分正确 校验金额换算整数化无浮点误差。
func TestAlipayParseNotify_元转分正确(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		AlipayPrivateKey: privatePEM,
		AlipayPublicKey:  publicPEM,
	})
	provider, _ := registry.Get(model.PaymentMethodAlipay)

	cases := map[string]int64{
		"10.00": 1000,
		"0.01":  1,
		"1":     100,
		"99.99": 9999,
	}
	for yuan, want := range cases {
		form := signedAlipayForm(t, key, map[string]string{
			"out_trade_no": "pay1",
			"total_amount": yuan,
			"trade_status": alipayTradeSuccess,
			"sign_type":    alipaySignType,
		})
		result, err := provider.ParseNotify(context.Background(), &Notify{Form: form})
		if err != nil {
			t.Fatalf("金额 %q 验签应通过: %v", yuan, err)
		}
		if result.AmountCents != want {
			t.Fatalf("金额 %q 应换算为 %d 分，实际 %d", yuan, want, result.AmountCents)
		}
	}
}

// TestAlipayCreate_未配置密钥应报错 校验缺密钥时明确失败。
func TestAlipayCreate_未配置密钥应报错(t *testing.T) {
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{})
	provider, _ := registry.Get(model.PaymentMethodAlipay)

	_, err := provider.Create(context.Background(), &Request{
		Order: &model.PaymentOrder{TradeNo: "pay1", UserID: 1, Amount: 100, Method: model.PaymentMethodAlipay},
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("应返回 ErrNotConfigured，实际 %v", err)
	}
}

// TestAlipayParseNotify_未配置公钥必须拒绝 校验缺公钥时无法验签、一律拒绝。
func TestAlipayParseNotify_未配置公钥必须拒绝(t *testing.T) {
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{})
	provider, _ := registry.Get(model.PaymentMethodAlipay)

	form := url.Values{}
	form.Set("out_trade_no", "pay1")
	form.Set("trade_status", alipayTradeSuccess)

	_, err := provider.ParseNotify(context.Background(), &Notify{Form: form})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("未配置公钥应返回 ErrNotConfigured，实际 %v", err)
	}
}

// signedAlipayForm 按支付宝口径给参数签名并转成回调表单。
func signedAlipayForm(t *testing.T, key *rsa.PrivateKey, params map[string]string) url.Values {
	t.Helper()
	signed := cloneParams(params)
	signed["sign"] = alipaySign(t, key, params)
	return toForm(signed)
}

// alipaySign 用测试私钥按支付宝待签串规则生成签名。
func alipaySign(t *testing.T, key *rsa.PrivateKey, params map[string]string) string {
	t.Helper()
	signature, err := rsa2Sign(key, alipaySignContent(params))
	if err != nil {
		t.Fatalf("生成签名失败: %v", err)
	}
	return signature
}

// cloneParams 复制参数 map，避免测试用例之间互相污染。
func cloneParams(params map[string]string) map[string]string {
	cloned := make(map[string]string, len(params))
	for name, value := range params {
		cloned[name] = value
	}
	return cloned
}

// toForm 把参数 map 转成 url.Values。
func toForm(params map[string]string) url.Values {
	form := url.Values{}
	for name, value := range params {
		form.Set(name, value)
	}
	return form
}

// encodeBase64PKCS8 把私钥序列化为 PKCS#8 再裸 base64 编码（模拟控制台导出的形态）。
func encodeBase64PKCS8(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("序列化 PKCS#8 私钥失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}
