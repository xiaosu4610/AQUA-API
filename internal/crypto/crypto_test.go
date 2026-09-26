// Package crypto 的单元测试。
//
// 意图（Why）：
//
//	加密是安全底线，必须验证三件事：能正确往返、相同明文产生不同密文（语义安全）、
//	密文被篡改时能检测出来（完整性）。任何一项失效都意味着密钥可能泄露。
//
// 流转（Flow）：
//
//	go test ./internal/crypto/
//
// 扩展（Extend）：
//
//	若未来支持密钥轮换，请补充「旧密钥解旧密文」的兼容性用例。
package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 测试用密钥材料（仅用于单元测试，非真实密钥）。
const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// newTestCipher 构造一个测试用加密器。
func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := New(testKey)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return c
}

// TestNew_RejectsEmptyKey 验证空密钥会被拒绝（防止无密钥情况下"假加密"）。
func TestNew_RejectsEmptyKey(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("空密钥应返回错误，实际返回 nil")
	}
}

// TestEncryptDecrypt_RoundTrip 验证各种输入的加解密往返一致。
func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	c := newTestCipher(t)

	cases := []struct {
		name      string
		plaintext string
	}{
		{name: "空字符串（约定不加密）", plaintext: ""},
		{name: "普通 ASCII", plaintext: "sk-abcdef1234567890"},
		{name: "NVIDIA 风格密钥", plaintext: "nvapi-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		{name: "含中文", plaintext: "中文密钥测试：你好世界"},
		{name: "含特殊字符", plaintext: `p@ss"word'\with\slashes`},
		{name: "长文本", plaintext: strings.Repeat("a", 4096)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encrypted, err := c.Encrypt(tc.plaintext)
			if err != nil {
				t.Fatalf("Encrypt 失败: %v", err)
			}
			// 空输入约定：密文也为空
			if tc.plaintext == "" {
				if encrypted != "" {
					t.Errorf("空明文应返回空密文，实际 %q", encrypted)
				}
				return
			}
			// 密文不得等于明文（最基本的安全底线）
			if encrypted == tc.plaintext {
				t.Error("密文与明文相同，加密未生效")
			}

			decrypted, err := c.Decrypt(encrypted)
			if err != nil {
				t.Fatalf("Decrypt 失败: %v", err)
			}
			if decrypted != tc.plaintext {
				t.Errorf("往返结果不一致：得到 %q，期望 %q", decrypted, tc.plaintext)
			}
		})
	}
}

// TestEncrypt_SameInputDifferentOutput 验证相同明文每次产生不同密文。
//
// 原理：每次加密使用随机 nonce。若两次结果相同，说明 nonce 被复用，
// 会破坏 GCM 的安全性（可能被推导出密钥流），属于严重缺陷。
func TestEncrypt_SameInputDifferentOutput(t *testing.T) {
	c := newTestCipher(t)
	const plaintext = "same-input-should-produce-different-ciphertext"

	first, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("第一次加密失败: %v", err)
	}
	second, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("第二次加密失败: %v", err)
	}

	if first == second {
		t.Error("两次加密结果相同，说明 nonce 被复用（严重安全问题）")
	}

	// 但两者都必须能解回原文
	for i, enc := range []string{first, second} {
		got, err := c.Decrypt(enc)
		if err != nil {
			t.Fatalf("第 %d 个密文解密失败: %v", i+1, err)
		}
		if got != plaintext {
			t.Errorf("第 %d 个密文解出的明文不正确", i+1)
		}
	}
}

// TestDecrypt_TamperedCiphertextFails 验证密文被篡改时解密失败。
//
// 这是 GCM 的核心价值：任何一位被改动都会被认证标签检出，
// 而不是解出一段错误但看似正常的明文。
func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	c := newTestCipher(t)

	encrypted, err := c.Encrypt("需要被保护的密钥")
	if err != nil {
		t.Fatalf("Encrypt 失败: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		t.Fatalf("Base64 解码失败: %v", err)
	}
	// 翻转最后一个字节（属于认证标签区域）
	raw[len(raw)-1] ^= 0xFF
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("篡改后的密文应解密失败，实际成功")
	}
}

// TestDecrypt_WrongKeyFails 验证用另一个密钥无法解密。
func TestDecrypt_WrongKeyFails(t *testing.T) {
	encrypter := newTestCipher(t)

	other, err := New("another-completely-different-key-material-for-testing-only")
	if err != nil {
		t.Fatalf("构造第二个加密器失败: %v", err)
	}

	encrypted, err := encrypter.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt 失败: %v", err)
	}

	if _, err := other.Decrypt(encrypted); err == nil {
		t.Fatal("使用错误密钥解密应失败，实际成功")
	}
}

// TestDecrypt_InvalidInput 验证非法密文输入会被安全拒绝。
func TestDecrypt_InvalidInput(t *testing.T) {
	c := newTestCipher(t)

	cases := []struct {
		name    string
		encoded string
	}{
		{name: "非 Base64 字符", encoded: "!!!not-base64!!!"},
		{name: "长度不足（比 nonce 还短）", encoded: base64.StdEncoding.EncodeToString([]byte("short"))},
		{name: "Base64 但内容随机", encoded: base64.StdEncoding.EncodeToString(make([]byte, 32))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Decrypt(tc.encoded); err == nil {
				t.Error("非法密文应返回错误，实际返回 nil")
			}
		})
	}
}

// TestDecrypt_EmptyReturnsEmpty 验证空密文返回空明文（与 Encrypt 的约定对称）。
func TestDecrypt_EmptyReturnsEmpty(t *testing.T) {
	c := newTestCipher(t)

	got, err := c.Decrypt("")
	if err != nil {
		t.Fatalf("空密文不应报错，实际: %v", err)
	}
	if got != "" {
		t.Errorf("空密文应返回空明文，实际 %q", got)
	}
}

// TestGenerateKeyMaterial 验证生成的密钥材料格式与随机性。
func TestGenerateKeyMaterial(t *testing.T) {
	first, err := GenerateKeyMaterial()
	if err != nil {
		t.Fatalf("生成密钥材料失败: %v", err)
	}

	// 32 字节十六进制 = 64 个字符
	if len(first) != 64 {
		t.Errorf("密钥材料长度 = %d，期望 64", len(first))
	}

	second, err := GenerateKeyMaterial()
	if err != nil {
		t.Fatalf("第二次生成失败: %v", err)
	}
	if first == second {
		t.Error("两次生成的密钥材料相同，随机性不足")
	}

	// 生成的材料必须能正常用于加解密
	c, err := New(first)
	if err != nil {
		t.Fatalf("用生成的密钥材料构造 Cipher 失败: %v", err)
	}
	if _, err := c.Encrypt("test"); err != nil {
		t.Fatalf("用生成的密钥材料加密失败: %v", err)
	}
}
