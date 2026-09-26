// 本文件提供支付通道共用的 RSA 密钥解析与签名 / 验签工具。
//
// 意图（Why）：
//
//	支付宝（RSA2）与微信支付（SHA256-RSA2048）底层都是 SHA256withRSA 签名，
//	区别只在"待签串怎么拼"。把密钥解析与签名原语收敛到一处，两个适配器就不必
//	各写一份、也不会各漏一种密钥形态。
//
//	密钥形态的坑：控制台导出的既可能是带 -----BEGIN----- 头尾的标准 PEM，
//	也可能是把头尾与换行全部去掉后的"裸 base64"。只认一种就会出现
//	"密钥明明粘贴对了却报错"的困惑，因此这里两种都解析，并在失败信息里点明是格式问题。
//
// 流转（Flow）：
//
//	alipay.go / wechatpay.go → parseRSAPrivateKey / parseRSAPublicKey（解析密钥）
//	                        → rsa2Sign（签名）/ rsa2Verify（验签）
//
// 扩展（Extend）：
//
//	若将来接入 ECC 或国密，在此新增对应的 parse / sign 函数即可；
//	适配器只依赖本文件暴露的函数名，不感知底层算法细节。
package payment

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// parseRSAPrivateKey 解析应用 / 商户私钥，label 用于错误信息中指明是哪个密钥。
//
// 兼容四种组合：PEM 或裸 base64 × PKCS#1（BEGIN RSA PRIVATE KEY）或 PKCS#8（BEGIN PRIVATE KEY）。
// 解析失败时明确说"是格式问题"，避免使用者误以为密钥本身不对而去反复重新申请。
func parseRSAPrivateKey(raw, label string) (*rsa.PrivateKey, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%s为空", label)
	}

	der, err := decodePEMOrBase64(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s格式无法识别（既不是 PEM，也不是合法的 base64，请检查是否复制完整）: %w", label, err)
	}

	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%s不是合法的 RSA 私钥（需 PKCS#1 或 PKCS#8 格式）: %w", label, err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s不是 RSA 私钥", label)
	}
	return rsaKey, nil
}

// parseRSAPublicKey 解析公钥，label 用于错误信息中指明是哪个密钥。
//
// 兼容四种组合：PEM 或裸 base64 × PKIX（BEGIN PUBLIC KEY）或 PKCS#1（BEGIN RSA PUBLIC KEY）。
func parseRSAPublicKey(raw, label string) (*rsa.PublicKey, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%s为空", label)
	}

	der, err := decodePEMOrBase64(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s格式无法识别（既不是 PEM，也不是合法的 base64，请检查是否复制完整）: %w", label, err)
	}

	if parsed, err := x509.ParsePKIXPublicKey(der); err == nil {
		rsaKey, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%s不是 RSA 公钥", label)
		}
		return rsaKey, nil
	}
	if key, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("%s不是合法的 RSA 公钥（需 PKIX 或 PKCS#1 格式）", label)
}

// decodePEMOrBase64 把 PEM 或裸 base64 文本统一还原为 DER 字节。
//
// 判据是文本里是否出现 "-----BEGIN"：有则按 PEM 走；没有则先剔除所有空白
// （base64 常被按固定宽度折行，折行的换行符必须去掉，否则解码失败）再按标准 base64 解码。
func decodePEMOrBase64(raw string) ([]byte, error) {
	if strings.Contains(raw, "-----BEGIN") {
		block, _ := pem.Decode([]byte(raw))
		if block == nil {
			return nil, errors.New("PEM 块解析失败，缺少完整的 -----BEGIN/-----END 包裹")
		}
		return block.Bytes, nil
	}

	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, raw)
	return base64.StdEncoding.DecodeString(compact)
}

// rsa2Sign 用 SHA256withRSA 对 content 签名，返回 base64 字符串。
//
// 支付宝称之为 RSA2，微信支付称之为 SHA256-RSA2048，是同一套算法。
func rsa2Sign(privateKey *rsa.PrivateKey, content string) (string, error) {
	digest := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("RSA 签名失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// rsa2Verify 校验 SHA256withRSA 签名（signatureBase64 为 base64 字符串）。
//
// 验签失败统一返回 ErrSignatureInvalid，调用方据此拒绝回调。
func rsa2Verify(publicKey *rsa.PublicKey, content, signatureBase64 string) error {
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureBase64))
	if err != nil {
		return fmt.Errorf("%w：签名不是合法的 base64", ErrSignatureInvalid)
	}
	digest := sha256.Sum256([]byte(content))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return fmt.Errorf("%w：RSA 验签不通过", ErrSignatureInvalid)
	}
	return nil
}
