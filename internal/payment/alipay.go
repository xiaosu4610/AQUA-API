// 本文件实现「支付宝官方」电脑网站支付（alipay.trade.page.pay）通道。
//
// 意图（Why）：
//
//	易支付是经由第三方聚合网关转发支付宝，费率与到账链路都隔了一层；
//	有自己支付宝商户的站长更愿意直连官方：资金直达、费率透明、对账清晰。
//	官方通道的接入方式也很"协议化"——下单是把参数签名后拼成跳转 URL，
//	回调是表单参数 + RSA2 验签，因此无需引入官方 SDK 即可完整实现
//	（符合本项目"零额外依赖"的偏好）。
//
// 协议要点（来自支付宝开放平台公开规范）：
//
//	下单（网关跳转）：
//	  把公共参数 app_id / method / charset / sign_type=RSA2 / timestamp / version=1.0
//	  / notify_url / return_url / biz_content 与签名 sign 拼成
//	  {gateway}?k1=v1&k2=v2... 让浏览器跳转；
//	  biz_content 是业务参数 JSON，含 out_trade_no / total_amount（元）/ subject
//	  / product_code=FAST_INSTANT_TRADE_PAY。
//	  签名：剔除 sign 与 sign_type、剔除空值后按参数名 ASCII 升序，
//	  拼成 "k=v&k=v"（值不做 URL 编码，biz_content 用原文），用应用私钥 RSA2 签名。
//
//	回调（异步通知）：
//	  支付宝以 POST 表单回调 notify_url，字段同上外加 trade_no / trade_status / total_amount；
//	  把参数剔除 sign 与 sign_type 后按名升序拼成 "k=v&k=v"，用【支付宝公钥】验签；
//	  验签通过后再看 trade_status，只有 TRADE_SUCCESS / TRADE_FINISHED 视为已支付。
//	  处理成功必须回纯文本 success，否则支付宝会持续重试。
//
// 安全说明：
//
//	回调必须验签【且】必须比对金额。仅比对订单号不够——攻击者可拿一个
//	真实存在的小额订单号伪造"已支付"通知。本文件只负责验签与取回金额，
//	金额与本地订单的比对由上层用 NotifyResult.AmountCents 完成。
//
// 流转（Flow）：
//
//	Create：读 App ID / 网关 → 组装公共参数与 biz_content → 签名 → 返回跳转 URL
//	ParseNotify：读表单 → 用支付宝公钥验签 → 判 trade_status → 返回订单号与金额
//
// 扩展（Extend）：
//
//	若要支持手机网站支付（alipay.trade.wap.pay）或当面付（alipay.trade.precreate），
//	只需替换 method 常量与 biz_content 形状，验签逻辑完全复用，改动集中在本文件顶部常量。
package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 支付宝协议常量。
const (
	// alipayDefaultGateway 是支付宝生产网关；沙箱环境才需要改。
	alipayDefaultGateway = "https://openapi.alipay.com/gateway.do"
	// alipayPagePayMethod 是"电脑网站支付"接口名。
	alipayPagePayMethod = "alipay.trade.page.pay"
	// alipaySignType 是本实现采用的签名算法（SHA256withRSA）。
	alipaySignType = "RSA2"
	// alipayCharset 是字符集。
	alipayCharset = "utf-8"
	// alipayVersion 是接口版本号。
	alipayVersion = "1.0"
	// alipayProductCode 是电脑网站支付的销售产品码。
	alipayProductCode = "FAST_INSTANT_TRADE_PAY"
	// alipayTradeSuccess 是"交易支付成功"状态。
	alipayTradeSuccess = "TRADE_SUCCESS"
	// alipayTradeFinished 是"交易结束，不可退款"状态（同样视为已支付）。
	alipayTradeFinished = "TRADE_FINISHED"
	// alipayAckBody 是回调处理成功时应答的固定字符串。
	alipayAckBody = "success"
	// alipayAckContentType 是应答的 Content-Type（支付宝要求纯文本）。
	alipayAckContentType = "text/plain; charset=utf-8"
	// alipayTimeFormat 是 timestamp 参数的格式。
	alipayTimeFormat = "2006-01-02 15:04:05"
)

// alipayProvider 实现支付宝官方电脑网站支付。
//
// 说明：下单是"拼 URL 让浏览器跳转"，不需要发起任何 HTTP 请求，
// 因此刻意不持有 http.Client —— 也让它更容易在离线环境下测试。
type alipayProvider struct {
	opts Options
}

// newAlipayProvider 构造支付宝通道。
func newAlipayProvider(opts Options) Provider {
	return &alipayProvider{opts: opts}
}

// Name 返回通道名。
func (p *alipayProvider) Name() string { return model.PaymentMethodAlipay }

// settings 读取当前运营参数。
func (p *alipayProvider) settings(ctx context.Context) (model.PaymentSettings, error) {
	if p.opts.Settings == nil {
		return model.PaymentSettings{}, nil
	}
	return p.opts.Settings(ctx)
}

// Create 组装电脑网站支付的跳转地址。
func (p *alipayProvider) Create(ctx context.Context, req *Request) (*CreateResult, error) {
	settings, err := p.settings(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.MethodEnabled(model.PaymentMethodAlipay) {
		return nil, ErrProviderDisabled
	}

	// 非密钥参数走通道参数（键 "alipay.<字段>"），密钥只走环境变量。
	appID := settings.Param(model.PaymentMethodAlipay, "app_id")
	gateway := settings.Param(model.PaymentMethodAlipay, "gateway")
	if strings.TrimSpace(gateway) == "" {
		gateway = alipayDefaultGateway
	}
	privateKeyRaw := strings.TrimSpace(p.opts.Secrets.AlipayPrivateKey)
	if appID == "" || privateKeyRaw == "" {
		return nil, fmt.Errorf("%w：支付宝需要配置 App ID（后台）与应用私钥（环境变量 AQUA_ALIPAY_PRIVATE_KEY）", ErrNotConfigured)
	}
	privateKey, err := parseRSAPrivateKey(privateKeyRaw, "支付宝应用私钥（AQUA_ALIPAY_PRIVATE_KEY）")
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrNotConfigured, err)
	}

	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = "账户充值"
	}

	// biz_content 必须是紧凑 JSON：签名用的是它的原文，任何空格差异都会导致验签失败。
	bizContent, err := json.Marshal(struct {
		OutTradeNo  string `json:"out_trade_no"`
		TotalAmount string `json:"total_amount"`
		Subject     string `json:"subject"`
		ProductCode string `json:"product_code"`
	}{
		OutTradeNo: req.Order.TradeNo,
		// total_amount 必须是"元"且两位小数
		TotalAmount: yuanFromCents(req.Order.Amount),
		Subject:     subject,
		ProductCode: alipayProductCode,
	})
	if err != nil {
		return nil, fmt.Errorf("构造支付宝 biz_content 失败: %w", err)
	}

	params := map[string]string{
		"app_id":      appID,
		"method":      alipayPagePayMethod,
		"charset":     alipayCharset,
		"sign_type":   alipaySignType,
		"timestamp":   alipayTimestamp(time.Now()),
		"version":     alipayVersion,
		"notify_url":  req.NotifyURL,
		"return_url":  req.ReturnURL,
		"biz_content": string(bizContent),
	}

	signature, err := rsa2Sign(privateKey, alipaySignContent(params))
	if err != nil {
		return nil, err
	}
	params["sign"] = signature

	query := url.Values{}
	for name, value := range params {
		if strings.TrimSpace(value) == "" {
			continue
		}
		query.Set(name, value)
	}

	return &CreateResult{
		PayURL: strings.TrimRight(gateway, "/") + "?" + query.Encode(),
	}, nil
}

// ParseNotify 用支付宝公钥验签并解析回调。
func (p *alipayProvider) ParseNotify(_ context.Context, notify *Notify) (*NotifyResult, error) {
	if notify == nil {
		return nil, ErrSignatureInvalid
	}
	publicKeyRaw := strings.TrimSpace(p.opts.Secrets.AlipayPublicKey)
	if publicKeyRaw == "" {
		// 没配公钥就无法验签，此时【必须】拒绝，绝不能"放行以便调试"
		return nil, fmt.Errorf("%w：未配置支付宝公钥（环境变量 AQUA_ALIPAY_PUBLIC_KEY）", ErrNotConfigured)
	}
	publicKey, err := parseRSAPublicKey(publicKeyRaw, "支付宝公钥（AQUA_ALIPAY_PUBLIC_KEY）")
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrNotConfigured, err)
	}

	params := collectAlipayParams(notify)
	signature := params["sign"]
	if strings.TrimSpace(signature) == "" {
		return nil, fmt.Errorf("%w：回调缺少 sign 参数", ErrSignatureInvalid)
	}
	if err := rsa2Verify(publicKey, alipaySignContent(params), signature); err != nil {
		return nil, err
	}

	status := strings.ToUpper(strings.TrimSpace(params["trade_status"]))
	paid := status == alipayTradeSuccess || status == alipayTradeFinished

	result := &NotifyResult{
		TradeNo:         strings.TrimSpace(params["out_trade_no"]),
		ProviderTradeNo: strings.TrimSpace(params["trade_no"]),
		// total_amount 是"元"，用整数解析换算为"分"避免浮点误差
		AmountCents:    yuanToCents(params["total_amount"]),
		Paid:           paid,
		AckBody:        alipayAckBody,
		AckContentType: alipayAckContentType,
	}
	if result.TradeNo == "" {
		return nil, fmt.Errorf("%w：回调缺少 out_trade_no", ErrSignatureInvalid)
	}
	return result, nil
}

// alipayTimestamp 返回支付宝要求的北京时区时间戳。
//
// 为什么固定 +8：支付宝服务器按北京时间解析 timestamp，若网关以 UTC 运行，
// 直接 time.Now() 会得到相差 8 小时的字符串，导致"签名对但请求被拒"。
func alipayTimestamp(now time.Time) string {
	return now.In(time.FixedZone("CST", 8*60*60)).Format(alipayTimeFormat)
}

// collectAlipayParams 合并回调中的表单与查询参数。
//
// 支付宝异步通知用 POST 表单；同步跳转（return_url）用 GET 查询串。
// 两者都看，便于同一套逻辑兼顾。
func collectAlipayParams(notify *Notify) map[string]string {
	params := make(map[string]string, 16)
	for name, values := range notify.Query {
		if len(values) > 0 {
			params[name] = values[0]
		}
	}
	for name, values := range notify.Form {
		if len(values) > 0 {
			params[name] = values[0]
		}
	}
	return params
}

// alipaySignContent 按支付宝规则拼接待签串。
//
// 算法：剔除 sign 与 sign_type、剔除空值 → 按参数名 ASCII 升序 →
// 拼成 "k=v&k=v"（值不做 URL 编码，biz_content 保持 JSON 原文）。
// 下单签名与回调验签必须使用【同一个】函数，否则两边的口径一旦不一致就会
// 出现"自己能签、支付宝验不过"这类极难排查的问题。
func alipaySignContent(params map[string]string) string {
	names := make([]string, 0, len(params))
	for name := range params {
		if name == "sign" || name == "sign_type" {
			continue
		}
		if strings.TrimSpace(params[name]) == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var builder strings.Builder
	for index, name := range names {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(params[name])
	}
	return builder.String()
}
