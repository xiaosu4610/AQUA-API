// 微信支付 APIv3 通道的单元测试。
//
// 意图（Why）：
//
//	微信支付的回调安全依赖"验签 + 时间窗 + 解密"三道关，任何一道写错
//	都可能让攻击者伪造到账通知。这里用现场生成的密钥对与 APIv3 密钥，
//	不依赖真实密钥与网络，把这几道关固化成测试：
//	  - code_url 解析；
//	  - 回调签名通过与被拒；
//	  - 时间戳超窗必须拒绝（防重放）；
//	  - AES-256-GCM 解密结果解析正确、金额取整正确。
//
// 流转（Flow）：
//
//	go test ./internal/payment/ → 加密构造回调 → 断言验签、解密与解析
//
// 扩展（Extend）：
//
//	新增 JSAPI / H5 等下单形态时，复用本文件的回调构造与断言思路即可。
package payment

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// wechatTestAPIv3Key 是测试用的 32 字节 APIv3 密钥。
func wechatTestAPIv3Key() string { return "0123456789abcdef0123456789abcdef" }

// TestWeChatCreate_未配置密钥应报错 校验缺密钥时明确失败。
func TestWeChatCreate_未配置密钥应报错(t *testing.T) {
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{})
	provider, _ := registry.Get(model.PaymentMethodWeChatPay)

	_, err := provider.Create(context.Background(), &Request{
		Order: &model.PaymentOrder{TradeNo: "pay1", UserID: 1, Amount: 100, Method: model.PaymentMethodWeChatPay},
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("应返回 ErrNotConfigured，实际 %v", err)
	}
}

// TestParseWeChatNativeResponse_解析CodeURL 校验下单响应解析。
func TestParseWeChatNativeResponse_解析CodeURL(t *testing.T) {
	codeURL, err := parseWeChatNativeResponse([]byte(`{"code_url":"weixin://wxpay/bizpayurl?pr=abc123"}`))
	if err != nil {
		t.Fatalf("解析 code_url 应成功: %v", err)
	}
	if codeURL != "weixin://wxpay/bizpayurl?pr=abc123" {
		t.Fatalf("code_url 不正确: %q", codeURL)
	}

	if _, err := parseWeChatNativeResponse([]byte(`{}`)); err == nil {
		t.Fatal("响应缺少 code_url 时应报错")
	}
	if _, err := parseWeChatNativeResponse([]byte(`不是 JSON`)); err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
}

// TestWeChatParseNotify_验签解密与金额 校验正常回调的完整链路。
func TestWeChatParseNotify_验签解密与金额(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)
	apiV3Key := wechatTestAPIv3Key()
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		WeChatPayAPIv3Key:          apiV3Key,
		WeChatPayPrivateKey:        privatePEM,
		WeChatPayPlatformPublicKey: publicPEM,
	})
	provider, _ := registry.Get(model.PaymentMethodWeChatPay)

	transaction := `{"out_trade_no":"pay20260101000000abcdef","transaction_id":"4200001234202601011234567890",` +
		`"trade_state":"SUCCESS","amount":{"total":1050,"currency":"CNY"}}`
	notify := buildWeChatNotify(t, key, apiV3Key,
		strconv.FormatInt(time.Now().Unix(), 10), "nonce-123456", transaction)

	result, err := provider.ParseNotify(context.Background(), notify)
	if err != nil {
		t.Fatalf("正常回调应验签解密成功: %v", err)
	}
	if !result.Paid {
		t.Fatal("trade_state=SUCCESS 应判定为已支付")
	}
	if result.TradeNo != "pay20260101000000abcdef" {
		t.Fatalf("订单号不正确: %q", result.TradeNo)
	}
	if result.ProviderTradeNo != "4200001234202601011234567890" {
		t.Fatalf("第三方单号不正确: %q", result.ProviderTradeNo)
	}
	if result.AmountCents != 1050 {
		t.Fatalf("金额应为 1050 分，实际 %d", result.AmountCents)
	}
	if result.AckBody != wechatAckBody {
		t.Fatalf("回调应答应为 %q，实际 %q", wechatAckBody, result.AckBody)
	}
	if result.AckContentType != wechatAckContentType {
		t.Fatalf("应答 Content-Type 不正确: %q", result.AckContentType)
	}
}

// TestWeChatParseNotify_非成功状态不入账 校验只有 SUCCESS 才算已支付。
func TestWeChatParseNotify_非成功状态不入账(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)
	apiV3Key := wechatTestAPIv3Key()
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		WeChatPayAPIv3Key:          apiV3Key,
		WeChatPayPrivateKey:        privatePEM,
		WeChatPayPlatformPublicKey: publicPEM,
	})
	provider, _ := registry.Get(model.PaymentMethodWeChatPay)

	transaction := `{"out_trade_no":"pay1","trade_state":"NOTPAY","amount":{"total":100,"currency":"CNY"}}`
	notify := buildWeChatNotify(t, key, apiV3Key,
		strconv.FormatInt(time.Now().Unix(), 10), "nonce-123456", transaction)

	result, err := provider.ParseNotify(context.Background(), notify)
	if err != nil {
		t.Fatalf("回调解析不应报错: %v", err)
	}
	if result.Paid {
		t.Fatal("trade_state 非 SUCCESS 时不应判定为已支付")
	}
}

// TestWeChatParseNotify_签名或时间戳异常必须拒绝 校验防伪造与防重放。
func TestWeChatParseNotify_签名或时间戳异常必须拒绝(t *testing.T) {
	key, privatePEM, publicPEM := generateRSAKeyPair(t)
	apiV3Key := wechatTestAPIv3Key()
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		WeChatPayAPIv3Key:          apiV3Key,
		WeChatPayPrivateKey:        privatePEM,
		WeChatPayPlatformPublicKey: publicPEM,
	})
	provider, _ := registry.Get(model.PaymentMethodWeChatPay)

	now := time.Now()
	transaction := `{"out_trade_no":"pay1","trade_state":"SUCCESS","amount":{"total":100,"currency":"CNY"}}`

	// 1) 用另一把私钥签名 → 平台公钥验签失败
	otherKey, _, _ := generateRSAKeyPair(t)
	badSigner := buildWeChatNotify(t, otherKey, apiV3Key,
		strconv.FormatInt(now.Unix(), 10), "nonce-1", transaction)
	if _, err := provider.ParseNotify(context.Background(), badSigner); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("错误签名应被拒绝，实际 %v", err)
	}

	// 2) 时间戳超出容忍窗口（防重放）
	oldTimestamp := now.Add(-10 * time.Minute)
	expired := buildWeChatNotify(t, key, apiV3Key,
		strconv.FormatInt(oldTimestamp.Unix(), 10), "nonce-2", transaction)
	if _, err := provider.ParseNotify(context.Background(), expired); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("超窗时间戳应被拒绝，实际 %v", err)
	}

	// 3) 请求体被篡改（签名不再匹配）
	tampered := buildWeChatNotify(t, key, apiV3Key,
		strconv.FormatInt(now.Unix(), 10), "nonce-3", transaction)
	tampered.Body = append([]byte{}, tampered.Body...)
	tampered.Body[0] = 'X'
	if _, err := provider.ParseNotify(context.Background(), tampered); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("请求体被篡改应被拒绝，实际 %v", err)
	}
}

// TestWeChatParseNotify_缺少签名头或密钥必须拒绝 校验缺件时的兜底。
func TestWeChatParseNotify_缺少签名头或密钥必须拒绝(t *testing.T) {
	_, privatePEM, publicPEM := generateRSAKeyPair(t)
	apiV3Key := wechatTestAPIv3Key()

	// 缺密钥（配置层）
	registryWithoutSecrets := newOfficialPaymentTestRegistry(config.PaymentConfig{})
	provider, _ := registryWithoutSecrets.Get(model.PaymentMethodWeChatPay)
	if _, err := provider.ParseNotify(context.Background(), &Notify{Header: http.Header{}}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("缺密钥应返回 ErrNotConfigured，实际 %v", err)
	}

	// 有密钥但缺签名头
	registry := newOfficialPaymentTestRegistry(config.PaymentConfig{
		WeChatPayAPIv3Key:          apiV3Key,
		WeChatPayPrivateKey:        privatePEM,
		WeChatPayPlatformPublicKey: publicPEM,
	})
	provider, _ = registry.Get(model.PaymentMethodWeChatPay)
	if _, err := provider.ParseNotify(context.Background(), &Notify{Header: http.Header{}}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("缺签名头应返回 ErrSignatureInvalid，实际 %v", err)
	}
}

// TestDecryptWeChatResource_密钥不匹配应失败 校验 AES-GCM 解密对密钥敏感。
func TestDecryptWeChatResource_密钥不匹配应失败(t *testing.T) {
	apiV3Key := wechatTestAPIv3Key()
	aad := "transaction"
	nonce := "0123456789ab"
	plaintext := `{"out_trade_no":"pay1"}`

	ciphertext := encryptWeChatResource(t, apiV3Key, plaintext, nonce, aad)
	decrypted, err := decryptWeChatResource(apiV3Key, ciphertext, nonce, aad)
	if err != nil {
		t.Fatalf("正确密钥应能解密: %v", err)
	}
	if string(decrypted) != plaintext {
		t.Fatalf("解密结果不一致: %q", string(decrypted))
	}

	// 换一把（长度相同但内容不同）密钥 → 解密失败
	wrongKey := "abcdef0123456789abcdef0123456789"
	if _, err := decryptWeChatResource(wrongKey, ciphertext, nonce, aad); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("错误密钥应解密失败，实际 %v", err)
	}

	// 密钥长度非法
	if _, err := decryptWeChatResource("太短", ciphertext, nonce, aad); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("密钥长度非法应返回 ErrNotConfigured，实际 %v", err)
	}
}

// buildWeChatNotify 构造一条"平台侧发来"的回调：加密 resource 并用私钥签名请求头。
//
// signer 决定签名有效性：传入与被测平台公钥对应的私钥则验签通过；
// 传入另一把私钥即模拟"伪造者签名"。
func buildWeChatNotify(t *testing.T, signer *rsa.PrivateKey, apiV3Key, timestamp, nonce, transactionJSON string) *Notify {
	t.Helper()

	associatedData := "transaction"
	resourceNonce := "0123456789ab" // GCM nonce 大小（12 字节）
	ciphertext := encryptWeChatResource(t, apiV3Key, transactionJSON, resourceNonce, associatedData)

	body := fmt.Sprintf(
		`{"id":"evt-1","event_type":"TRANSACTION.SUCCESS","resource_type":"encrypt-resource",`+
			`"resource":{"algorithm":"AEAD_AES_256_GCM","original_type":"transaction",`+
			`"ciphertext":%q,"nonce":%q,"associated_data":%q}}`,
		ciphertext, resourceNonce, associatedData)

	signature, err := rsa2Sign(signer, timestamp+"\n"+nonce+"\n"+body+"\n")
	if err != nil {
		t.Fatalf("生成回调签名失败: %v", err)
	}

	header := http.Header{}
	header.Set(wechatTimestampHeader, timestamp)
	header.Set(wechatNonceHeader, nonce)
	header.Set(wechatSignatureHeader, signature)
	header.Set(wechatSerialHeader, "5157F09EFDC096DE15EBE81A47057A72")

	return &Notify{Header: header, Body: []byte(body)}
}

// encryptWeChatResource 用 APIv3 密钥对明文做 AES-256-GCM 加密，返回 base64 密文。
// 用于在测试中模拟微信平台的加密回调。
func encryptWeChatResource(t *testing.T, apiV3Key, plaintext, nonce, associatedData string) string {
	t.Helper()

	block, err := aes.NewCipher([]byte(apiV3Key))
	if err != nil {
		t.Fatalf("初始化 AES 失败: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("初始化 GCM 失败: %v", err)
	}
	ciphertext := gcm.Seal(nil, []byte(nonce), []byte(plaintext), []byte(associatedData))
	return base64.StdEncoding.EncodeToString(ciphertext)
}
