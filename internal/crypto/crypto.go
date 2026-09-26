// Package crypto 提供上游密钥等敏感数据的对称加解密能力。
//
// 意图（Why）：
//
//	上游渠道密钥（如 OpenAI Key、NVIDIA Key）一旦以明文落库，数据库泄露即等于
//	密钥泄露。本包提供 AES-256-GCM 认证加密，保证：
//	  1) 机密性：无密钥者无法还原明文；
//	  2) 完整性：密文被篡改会在解密时被检测到并报错（GCM 自带认证标签）；
//	密钥材料来自环境变量 AQUA_APP_KEY，与环境隔离，不写入配置文件与仓库。
//
// 流转（Flow）：
//
//	cmd/aqua/main.go
//	  └─ crypto.New(cfg.Security.AppKey)   创建一个 Cipher 实例
//	       └─ 注入到渠道仓储（internal/store），
//	            写入渠道时 Encrypt，读取渠道时 Decrypt
//
// 扩展（Extend）：
//
//	轮换密钥：需支持多密钥版本（密文前缀标注密钥版本号）——当前为单密钥设计，
//	         后续里程碑若需要轮换，在密文中加入版本前缀（如 "v1:"）并保留旧密钥解旧密文。
//	更换算法：保持 Encrypt/Decrypt 签名不变，仅替换内部实现即可平滑升级。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// errEmptyOrInvalid 表示密文格式非法（非 Base64、长度不足等）。
var errEmptyOrInvalid = errors.New("crypto: 密文格式非法或已损坏")

// Cipher 是对称加密器，并发安全（cipher.AEAD 本身无状态，可被多 goroutine 共享）。
type Cipher struct {
	aead cipher.AEAD
}

// New 根据密钥材料创建一个 Cipher。
//
// 参数 keyMaterial 为任意长度的密钥字符串（来自 AQUA_APP_KEY 环境变量）。
// 实现上使用 SHA-256 将其派生成 32 字节，从而固定得到 AES-256 所需的密钥长度。
//
// 安全提示：SHA-256 只是「长度规整」，不具备抗暴力破解能力，
// 因此 AQUA_APP_KEY 必须是高熵随机串（建议 32 字节随机数的十六进制，即 64 个字符），
// 不可使用 "123456" 之类的弱口令。
func New(keyMaterial string) (*Cipher, error) {
	if len(keyMaterial) == 0 {
		return nil, errors.New("crypto: 密钥材料为空，请设置环境变量 AQUA_APP_KEY")
	}

	// 派生固定 32 字节密钥（AES-256）
	key := sha256.Sum256([]byte(keyMaterial))

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("crypto: 初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: 初始化 GCM 失败: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt 加密明文，返回 Base64 编码的密文。
//
// 输出格式：base64( nonce || ciphertext || tag )
//   - nonce：每次加密随机生成，保证相同明文产生不同密文（语义安全）；
//   - tag：GCM 认证标签，由 Seal 自动追加，解密时用于校验完整性。
//
// 约定：空字符串直接返回空字符串（不做加密）。
// 这样设计是为了让「未填写密钥」的渠道记录保持为空，而不是存一段无意义的密文。
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	// 生成随机 nonce。GCM 的 nonce 长度固定为 12 字节，绝不能重复使用。
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: 生成随机数失败: %w", err)
	}

	// Seal 会把密文追加到第一个参数后面，这里传入 nonce 作为前缀
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密由 Encrypt 产出的 Base64 密文。
//
// 会校验完整性：若密文被篡改、或使用了不同的密钥，均会返回错误，绝不返回错误明文。
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: Base64 解码失败", errEmptyOrInvalid)
	}

	nonceSize := c.aead.NonceSize()
	// 长度必须至少能容纳 nonce（GCM 还要求有 tag，Open 会进一步校验）
	if len(raw) < nonceSize {
		return "", errEmptyOrInvalid
	}

	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// 注意：不要把底层错误直接暴露给调用方，避免侧信道信息泄露
		return "", errors.New("crypto: 解密失败（密钥不匹配或密文已被篡改）")
	}
	return string(plaintext), nil
}

// GenerateKeyMaterial 生成一个高熵的密钥材料（64 位十六进制字符串）。
//
// 用途：首次部署时生成 AQUA_APP_KEY，供运维写入环境变量。
// 之所以提供该函数，是为了避免使用者随手填一个弱口令。
func GenerateKeyMaterial() (string, error) {
	buf := make([]byte, 32) // 32 字节 = 256 位熵
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("crypto: 生成密钥材料失败: %w", err)
	}
	return fmt.Sprintf("%x", buf), nil
}
