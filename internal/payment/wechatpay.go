// 本文件实现「微信支付官方 APIv3」Native（Native 扫码）支付通道。
//
// 意图（Why）：
//
//	要直连微信支付官方收款，就必须按 APIv3 规范实现：请求用商户私钥做
//	SHA256-RSA2048 签名，回调既要验签（平台证书公钥）又要用 APIv3 密钥做
//	AES-256-GCM 解密。规范虽长，但全是公开算法，用标准库即可完整实现，
//	无需引入官方 SDK（符合本项目"零额外依赖"的偏好）。
//
// 协议要点（来自微信支付 APIv3 公开文档）：
//
//	下单（Native）：
//	  POST {api}/v3/pay/transactions/native
//	  请求头 Authorization: WECHATPAY2-SHA256-RSA2048
//	    mchid="..",nonce_str="..",signature="..",timestamp="..",serial_no=".."
//	  待签串：HTTP方法\nURL路径\n时间戳\n随机串\n请求体\n（每行以 \n 结束，含末尾换行）
//	  请求体 JSON：mchid / out_trade_no / description / notify_url / amount.total（分）
//	  响应取 code_url（形如 weixin://wxpay/bizpayurl?pr=..），前端据此生成二维码。
//
//	回调：
//	  请求头 Wechatpay-Timestamp / Wechatpay-Nonce / Wechatpay-Signature / Wechatpay-Serial
//	  待签串：时间戳\n随机串\n请求体\n；用【平台证书公钥】验签，
//	  并校验时间戳在容忍窗口内（防重放）；
//	  再用【APIv3 密钥】对 resource.ciphertext 做 AES-256-GCM 解密，
//	  明文 JSON 中取 out_trade_no 与 amount.total。
//	  处理成功应答 {"code":"SUCCESS"}。
//
// 安全说明：
//
//	回调必须"验签 + 解密"双达标才可信：验签证明请求来自微信平台，
//	解密才拿得到业务数据。缺任一步都可能被伪造通知骗过，因此两步都不可省。
//
// 流转（Flow）：
//
//	Create：读商户号 / 序列号 → 组请求体 → 商户私钥签名 → POST → 返回 code_url
//	ParseNotify：校验签名头与时间戳 → AES-GCM 解密 resource → 返回订单号与金额
//
// 扩展（Extend）：
//
//	若要支持 JSAPI / H5，只需替换下单路径（/v3/pay/transactions/jsapi 等）
//	与请求体形状；签名、回调验签与解密逻辑完全复用，改动集中在本文件顶部常量。
package payment

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 微信支付 APIv3 协议常量。
const (
	// wechatAPIBase 是微信支付 APIv3 的根地址。
	wechatAPIBase = "https://api.mch.weixin.qq.com"
	// wechatNativePath 是 Native 下单端点（同时用于拼接待签串中的 URL 路径）。
	wechatNativePath = "/v3/pay/transactions/native"
	// wechatAuthScheme 是 Authorization 头的认证类型前缀。
	wechatAuthScheme = "WECHATPAY2-SHA256-RSA2048"
	// wechatCurrency 是账币种，官方规定人民币为 CNY。
	wechatCurrency = "CNY"
	// wechatTradeStateSuccess 是"支付成功"的交易状态。
	wechatTradeStateSuccess = "SUCCESS"
	// wechatAckBody 是回调处理成功时应答的 JSON。
	wechatAckBody = `{"code":"SUCCESS"}`
	// wechatAckContentType 是应答的 Content-Type。
	wechatAckContentType = "application/json; charset=utf-8"

	// 回调验签相关请求头。
	wechatSignatureHeader = "Wechatpay-Signature"
	wechatTimestampHeader = "Wechatpay-Timestamp"
	wechatNonceHeader     = "Wechatpay-Nonce"
	wechatSerialHeader    = "Wechatpay-Serial"

	// wechatSignatureTolerance 是回调时间戳的容忍窗口。
	//
	// 取 5 分钟（与官方建议一致）：超过窗口的签名即使正确也拒绝，
	// 攻击者无法用截获的旧请求反复入账。
	wechatSignatureTolerance = 5 * time.Minute
	// wechatAPIV3KeyLength 是 APIv3 密钥的字节长度（AES-256 要求 32 字节）。
	wechatAPIV3KeyLength = 32
	// wechatMaxBodyBytes 是响应 / 回调体的读取上限。
	wechatMaxBodyBytes = 1 << 20
)

// wechatPayProvider 实现微信支付 APIv3 Native 通道。
type wechatPayProvider struct {
	opts   Options
	client *http.Client
}

// newWeChatPayProvider 构造微信支付通道。
func newWeChatPayProvider(opts Options) Provider {
	return &wechatPayProvider{opts: opts, client: newHTTPClient()}
}

// Name 返回通道名。
func (p *wechatPayProvider) Name() string { return model.PaymentMethodWeChatPay }

// settings 读取当前运营参数。
func (p *wechatPayProvider) settings(ctx context.Context) (model.PaymentSettings, error) {
	if p.opts.Settings == nil {
		return model.PaymentSettings{}, nil
	}
	return p.opts.Settings(ctx)
}

// wechatNativeRequest 是 Native 下单请求体。
//
// 字段顺序即 JSON 序列化顺序，必须与实际发送的字节一致——
// 待签串里含请求体原文，任何差异都会让微信侧验签失败。
type wechatNativeRequest struct {
	AppID       string       `json:"appid,omitempty"`
	MchID       string       `json:"mchid"`
	Description string       `json:"description"`
	OutTradeNo  string       `json:"out_trade_no"`
	NotifyURL   string       `json:"notify_url"`
	Amount      wechatAmount `json:"amount"`
}

// wechatAmount 是金额对象，total 单位为【分】。
type wechatAmount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency"`
}

// Create 发起 Native 下单并返回 code_url。
func (p *wechatPayProvider) Create(ctx context.Context, req *Request) (*CreateResult, error) {
	settings, err := p.settings(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.MethodEnabled(model.PaymentMethodWeChatPay) {
		return nil, ErrProviderDisabled
	}

	// 非密钥参数走通道参数（键 "wechatpay.<字段>"），密钥只走环境变量。
	mchID := settings.Param(model.PaymentMethodWeChatPay, "mch_id")
	serialNo := settings.Param(model.PaymentMethodWeChatPay, "serial_no")
	appID := settings.Param(model.PaymentMethodWeChatPay, "app_id")
	privateKeyRaw := strings.TrimSpace(p.opts.Secrets.WeChatPayPrivateKey)
	if mchID == "" || serialNo == "" || privateKeyRaw == "" {
		return nil, fmt.Errorf("%w：微信支付需要配置商户号与证书序列号（后台）及商户私钥（环境变量 AQUA_WECHATPAY_PRIVATE_KEY）", ErrNotConfigured)
	}
	privateKey, err := parseRSAPrivateKey(privateKeyRaw, "微信支付商户私钥（AQUA_WECHATPAY_PRIVATE_KEY）")
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrNotConfigured, err)
	}

	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = "账户充值"
	}

	payload, err := json.Marshal(wechatNativeRequest{
		AppID:       appID,
		MchID:       mchID,
		Description: subject,
		OutTradeNo:  req.Order.TradeNo,
		NotifyURL:   req.NotifyURL,
		Amount: wechatAmount{
			Total:    req.Order.Amount,
			Currency: wechatCurrency,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("构造微信支付请求体失败: %w", err)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := randomNonce()
	signature, err := wechatRequestSignature(privateKey, http.MethodPost, wechatNativePath, timestamp, nonce, string(payload))
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		wechatAPIBase+wechatNativePath, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造微信支付请求失败: %w", err)
	}
	httpReq.Header.Set("Authorization", wechatAuthorizationHeader(mchID, nonce, signature, timestamp, serialNo))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求微信支付失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, wechatMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取微信支付响应失败: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		// 不回传上游原文（可能含内部标识），只给状态码与简要信息
		return nil, fmt.Errorf("微信支付返回 HTTP %d：%s", resp.StatusCode, wechatErrorMessage(raw))
	}

	codeURL, err := parseWeChatNativeResponse(raw)
	if err != nil {
		return nil, err
	}
	return &CreateResult{PayURL: codeURL}, nil
}

// ParseNotify 校验签名与时间戳、解密报文并解析回调。
func (p *wechatPayProvider) ParseNotify(_ context.Context, notify *Notify) (*NotifyResult, error) {
	if notify == nil {
		return nil, ErrSignatureInvalid
	}

	apiV3Key := strings.TrimSpace(p.opts.Secrets.WeChatPayAPIv3Key)
	platformKeyRaw := strings.TrimSpace(p.opts.Secrets.WeChatPayPlatformPublicKey)
	if apiV3Key == "" || platformKeyRaw == "" {
		// 缺任一项都无法完成"验签 + 解密"，此时【必须】拒绝
		return nil, fmt.Errorf("%w：微信支付需要 APIv3 密钥（AQUA_WECHATPAY_APIV3_KEY）与平台证书公钥（AQUA_WECHATPAY_PLATFORM_PUBLIC_KEY）", ErrNotConfigured)
	}
	platformKey, err := parseRSAPublicKey(platformKeyRaw, "微信支付平台证书公钥（AQUA_WECHATPAY_PLATFORM_PUBLIC_KEY）")
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrNotConfigured, err)
	}

	timestamp := strings.TrimSpace(notify.Header.Get(wechatTimestampHeader))
	nonce := strings.TrimSpace(notify.Header.Get(wechatNonceHeader))
	signature := strings.TrimSpace(notify.Header.Get(wechatSignatureHeader))
	if timestamp == "" || nonce == "" || signature == "" {
		return nil, fmt.Errorf("%w：回调缺少微信支付签名头（%s / %s / %s）",
			ErrSignatureInvalid, wechatTimestampHeader, wechatNonceHeader, wechatSignatureHeader)
	}
	if err := verifyWeChatTimestamp(timestamp, time.Now()); err != nil {
		return nil, err
	}

	// 待签串：时间戳\n随机串\n请求体\n（含末尾换行）
	message := strings.Join([]string{timestamp, nonce, string(notify.Body)}, "\n") + "\n"
	if err := rsa2Verify(platformKey, message, signature); err != nil {
		return nil, err
	}

	var callback struct {
		Resource struct {
			Ciphertext     string `json:"ciphertext"`
			Nonce          string `json:"nonce"`
			AssociatedData string `json:"associated_data"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(notify.Body, &callback); err != nil {
		return nil, errors.New("微信支付回调不是合法 JSON")
	}

	plaintext, err := decryptWeChatResource(apiV3Key,
		callback.Resource.Ciphertext, callback.Resource.Nonce, callback.Resource.AssociatedData)
	if err != nil {
		return nil, err
	}

	var transaction struct {
		OutTradeNo    string `json:"out_trade_no"`
		TransactionID string `json:"transaction_id"`
		TradeState    string `json:"trade_state"`
		Amount        struct {
			Total int64 `json:"total"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(plaintext, &transaction); err != nil {
		return nil, errors.New("微信支付回调解密后不是合法 JSON")
	}

	return &NotifyResult{
		TradeNo:         strings.TrimSpace(transaction.OutTradeNo),
		ProviderTradeNo: strings.TrimSpace(transaction.TransactionID),
		AmountCents:     transaction.Amount.Total,
		Paid:            strings.EqualFold(strings.TrimSpace(transaction.TradeState), wechatTradeStateSuccess),
		AckBody:         wechatAckBody,
		AckContentType:  wechatAckContentType,
	}, nil
}

// wechatRequestSignature 计算 APIv3 请求签名。
//
// 待签串：HTTP方法\nURL路径\n时间戳\n随机串\n请求体\n（每部分以 \n 结束，含末尾换行）。
func wechatRequestSignature(privateKey *rsa.PrivateKey, method, urlPath, timestamp, nonce, body string) (string, error) {
	message := strings.Join([]string{method, urlPath, timestamp, nonce, body}, "\n") + "\n"
	return rsa2Sign(privateKey, message)
}

// wechatAuthorizationHeader 拼装 Authorization 请求头。
func wechatAuthorizationHeader(mchID, nonce, signature, timestamp, serialNo string) string {
	return fmt.Sprintf(`%s mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`,
		wechatAuthScheme, mchID, nonce, signature, timestamp, serialNo)
}

// parseWeChatNativeResponse 从下单响应中取出 code_url。
func parseWeChatNativeResponse(raw []byte) (string, error) {
	var parsed struct {
		CodeURL string `json:"code_url"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", errors.New("微信支付响应不是合法 JSON")
	}
	codeURL := strings.TrimSpace(parsed.CodeURL)
	if codeURL == "" {
		return "", errors.New("微信支付未返回 code_url")
	}
	return codeURL, nil
}

// wechatErrorMessage 从微信支付错误体中提取可读信息。
func wechatErrorMessage(raw []byte) string {
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Message != "" {
		return envelope.Message
	}
	return "请求被拒绝"
}

// verifyWeChatTimestamp 校验回调时间戳是否在容忍窗口内（防重放）。
func verifyWeChatTimestamp(raw string, now time.Time) error {
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return fmt.Errorf("%w：时间戳非法", ErrSignatureInvalid)
	}
	if now.Sub(time.Unix(parsed, 0)).Abs() > wechatSignatureTolerance {
		return fmt.Errorf("%w：签名时间戳超出容忍窗口", ErrSignatureInvalid)
	}
	return nil
}

// decryptWeChatResource 用 APIv3 密钥做 AES-256-GCM 解密。
//
// 参数：ciphertextBase64 是 resource.ciphertext；nonce 是 resource.nonce；
// associatedData 是 resource.associated_data（无关联数据时为空串，GCM 允许 nil）。
func decryptWeChatResource(apiV3Key, ciphertextBase64, nonce, associatedData string) ([]byte, error) {
	if len(apiV3Key) != wechatAPIV3KeyLength {
		return nil, fmt.Errorf("%w：APIv3 密钥长度必须为 %d 字节（当前 %d 字节）",
			ErrNotConfigured, wechatAPIV3KeyLength, len(apiV3Key))
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ciphertextBase64))
	if err != nil {
		return nil, fmt.Errorf("%w：回调密文不是合法的 base64", ErrSignatureInvalid)
	}

	block, err := aes.NewCipher([]byte(apiV3Key))
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("%w：回调 nonce 长度非法", ErrSignatureInvalid)
	}

	plaintext, err := gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
	if err != nil {
		return nil, fmt.Errorf("%w：AES-GCM 解密失败（APIv3 密钥或密文不匹配）", ErrSignatureInvalid)
	}
	return plaintext, nil
}

// randomNonce 生成 32 位十六进制随机串，用于 APIv3 请求。
func randomNonce() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand 在受支持平台上不会失败；真失败时退回纳秒时间戳，
		// 保证仍能生成一个可用的（唯一性稍弱的）随机串，而不是让下单直接失败。
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(raw)
}
