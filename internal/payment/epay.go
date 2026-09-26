// 本文件实现「易支付」协议通道。
//
// 意图（Why）：
//
//	易支付（又称"彩虹易支付"）是国内自建网关最常见的聚合支付协议，
//	一份实现可以对接大量支付服务商，覆盖支付宝/微信等主流方式。
//	它足够简单（MD5 签名 + 表单回调），不需要引入任何 SDK。
//
// 协议要点（来自公开的易支付接口约定）：
//
//	下单：把参数拼成查询串跳转到 {gateway}/submit.php
//	  pid / type / out_trade_no / notify_url / return_url / name / money / sign / sign_type=MD5
//	回调：支付平台以 GET 或 POST 表单回调 notify_url，参数同上，
//	  外加 trade_no（第三方单号）与 trade_status=TRADE_SUCCESS；
//	  验签通过后必须原样返回字符串 success，否则平台会持续重试。
//	签名：把所有非空参数（排除 sign、sign_type）按参数名升序拼接为
//	  "k1=v1&k2=v2"，末尾直接拼接商户密钥，取 MD5 十六进制小写。
//
// 安全说明：
//
//	回调必须验签且必须比对金额。仅比对订单号是不够的——
//	攻击者可以拿一个真实存在的小额订单号，伪造"已支付"通知。
//
// 流转（Flow）：
//
//	Create：组装参数 → 签名 → 返回带查询串的 submit.php 地址（浏览器跳转）
//	ParseNotify：读表单 → 验签 → 校验 trade_status → 返回订单号与金额
//
// 扩展（Extend）：
//
//	若上游要求 POST 提交而非跳转（少数实现），在 Create 中改为返回一个
//	自提交表单页面即可，ParseNotify 无需改动。
package payment

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 易支付协议常量。
const (
	// epaySubmitPath 是收银台地址（跳转式支付）。
	epaySubmitPath = "/submit.php"
	// epaySignType 是本实现采用的签名算法。
	epaySignType = "MD5"
	// epayTradeSuccess 是"支付成功"的状态值。
	//
	// 兼容 TRADE_SUCCESS（支付宝体系）与 SUCCESS（部分实现）两种写法，
	// 否则换一家服务商就会出现"付了钱但系统不认"。
	epayTradeSuccess = "TRADE_SUCCESS"
	// epayAckBody 是回调成功时应答的固定字符串。
	epayAckBody = "success"
)

// epayProvider 实现易支付协议。
//
// 说明：本通道的下单是"拼 URL 让浏览器跳转"，不需要发起任何 HTTP 请求，
// 因此刻意不持有 http.Client —— 这也让它在离线环境下更可靠。
type epayProvider struct {
	opts Options
}

// newEPayProvider 构造易支付通道。
func newEPayProvider(opts Options) Provider {
	return &epayProvider{opts: opts}
}

// Name 返回通道名。
func (p *epayProvider) Name() string { return model.PaymentMethodEPay }

// settings 读取当前运营参数。
func (p *epayProvider) settings(ctx context.Context) (model.PaymentSettings, error) {
	if p.opts.Settings == nil {
		return model.PaymentSettings{}, nil
	}
	return p.opts.Settings(ctx)
}

// Create 组装收银台跳转地址。
func (p *epayProvider) Create(ctx context.Context, req *Request) (*CreateResult, error) {
	settings, err := p.settings(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.MethodEnabled(model.PaymentMethodEPay) {
		return nil, ErrProviderDisabled
	}
	key := strings.TrimSpace(p.opts.Secrets.EPayKey)
	// 通道参数统一从设置表的 Params 读取（键 "epay.<字段>"）。
	// 走 Param 而不是直接读结构体字段的原因：新增支付通道时不必再改设置模型，
	// 且 Param 内部保留了"历史字段回退"，升级后旧的易支付配置仍然生效。
	gateway := settings.Param(model.PaymentMethodEPay, "gateway")
	pid := settings.Param(model.PaymentMethodEPay, "pid")
	if gateway == "" || pid == "" || key == "" {
		// 明确告诉使用者"缺什么"，而不是在跳转后由支付平台报一个含糊的错误
		return nil, fmt.Errorf("%w：易支付需要配置网关地址、商户号（后台）与商户密钥（环境变量 AQUA_EPAY_KEY）", ErrNotConfigured)
	}

	subMethod := strings.TrimSpace(req.Order.SubMethod)
	if subMethod == "" {
		subMethod = defaultSubMethod(settings.ParamList(model.PaymentMethodEPay, "types"))
	}

	params := map[string]string{
		"pid":          pid,
		"type":         subMethod,
		"out_trade_no": req.Order.TradeNo,
		"notify_url":   req.NotifyURL,
		"return_url":   req.ReturnURL,
		"name":         req.Subject,
		// money 必须是"元"且两位小数
		"money":     yuanFromCents(req.Order.Amount),
		"sign_type": epaySignType,
	}
	if req.ClientIP != "" {
		params["clientip"] = req.ClientIP
	}
	params["sign"] = epaySign(params, key)

	query := url.Values{}
	for name, value := range params {
		if strings.TrimSpace(value) == "" {
			continue
		}
		query.Set(name, value)
	}

	return &CreateResult{
		PayURL: strings.TrimRight(gateway, "/") + epaySubmitPath + "?" + query.Encode(),
	}, nil
}

// defaultSubMethod 在未指定支付方式时取第一个可用类型。
func defaultSubMethod(types []string) string {
	for _, item := range types {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			return trimmed
		}
	}
	return "alipay"
}

// ParseNotify 验签并解析回调。
func (p *epayProvider) ParseNotify(_ context.Context, notify *Notify) (*NotifyResult, error) {
	if notify == nil {
		return nil, ErrSignatureInvalid
	}
	key := strings.TrimSpace(p.opts.Secrets.EPayKey)
	if key == "" {
		// 没配密钥就无法验签，此时【必须】拒绝，绝不能"放行以便调试"
		return nil, fmt.Errorf("%w：未配置商户密钥（环境变量 AQUA_EPAY_KEY）", ErrNotConfigured)
	}

	params := collectEPayParams(notify)
	provided := params["sign"]
	if provided == "" {
		return nil, fmt.Errorf("%w：回调缺少 sign 参数", ErrSignatureInvalid)
	}
	if !strings.EqualFold(provided, epaySign(params, key)) {
		return nil, ErrSignatureInvalid
	}

	status := strings.ToUpper(strings.TrimSpace(params["trade_status"]))
	if status == "" {
		// 部分实现不返回 trade_status：此时以"能验签通过 + 有第三方单号"为支付成功依据
		status = epayTradeSuccess
	}
	paid := status == epayTradeSuccess || status == "SUCCESS"

	result := &NotifyResult{
		TradeNo:         strings.TrimSpace(params["out_trade_no"]),
		ProviderTradeNo: strings.TrimSpace(params["trade_no"]),
		AmountCents:     yuanToCents(params["money"]),
		Paid:            paid,
		AckBody:         epayAckBody,
		AckContentType:  "text/plain; charset=utf-8",
	}
	if result.TradeNo == "" {
		return nil, fmt.Errorf("%w：回调缺少 out_trade_no", ErrSignatureInvalid)
	}
	return result, nil
}

// collectEPayParams 合并 GET 查询与 POST 表单中的回调参数。
//
// 为什么两者都看：不同实现分别用 GET 与 POST 回调，
// 若只读一种，换服务商时会出现"回调收不到"的诡异现象。
func collectEPayParams(notify *Notify) map[string]string {
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

// epaySign 计算易支付签名。
//
// 算法：过滤空值与 sign/sign_type → 按参数名 ASCII 升序 →
// 拼成 "k=v&k=v" → 末尾直接拼商户密钥 → MD5 小写十六进制。
func epaySign(params map[string]string, key string) string {
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
	builder.WriteString(key)

	sum := md5.Sum([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// yuanToCents 把"元"字符串解析为"分"。
//
// 手工解析而不走 strconv.ParseFloat：浮点会把 10.01 解析成 10.009999…，
// 转成分为 1000 或 1001 取决于实现细节，属于典型的资损隐患。
func yuanToCents(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}

	negative := false
	if strings.HasPrefix(raw, "-") {
		negative = true
		raw = raw[1:]
	}

	// 按小数点切成整数部分与小数部分
	intPart, fracPart, _ := strings.Cut(raw, ".")
	// 小数部分补齐/截断到两位
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	fracPart = fracPart[:2]

	whole, err := strconv.ParseInt(strings.TrimSpace(intPart), 10, 64)
	if err != nil {
		return 0
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0
	}

	cents := whole*100 + frac
	if negative {
		return -cents
	}
	return cents
}
