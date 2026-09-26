// 口令哈希与校验的单元测试。
//
// 意图（Why）：
//
//	口令是账号体系的最后一道防线。这里不仅验证"能正确校验"，
//	更重点验证一条容易被忽视的严重缺陷：bcrypt 只取前 72 字节，
//	若不预处理，"前 72 字节相同但后缀不同"的两个口令会互相登录。
//
// 流转（Flow）：
//
//	go test ./internal/crypto/
//
// 扩展（Extend）：
//
//	更换哈希算法时，请保留 TestHashPassword_NoTruncationForLongPassword 该用例。
package crypto

import (
	"strings"
	"testing"
)

// TestHashPassword_SaltedDifferentOutputs 验证相同口令每次哈希结果不同（含随机盐）。
func TestHashPassword_SaltedDifferentOutputs(t *testing.T) {
	const password = "correct-horse-battery"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("首次哈希失败: %v", err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("二次哈希失败: %v", err)
	}

	if first == second {
		t.Error("相同口令产生了相同哈希，说明未使用随机盐")
	}
	// 但两者都应能校验通过
	if !VerifyPassword(password, first) || !VerifyPassword(password, second) {
		t.Error("两个哈希都应能校验通过")
	}
}

// TestVerifyPassword_CorrectAndIncorrect 验证正确口令通过、错误口令拒绝。
func TestVerifyPassword_CorrectAndIncorrect(t *testing.T) {
	const password = "s3cret-Passw0rd"

	hashed, err := HashPassword(password)
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}

	if !VerifyPassword(password, hashed) {
		t.Error("正确口令应校验通过")
	}
	// 大小写敏感
	if VerifyPassword(strings.ToUpper(password), hashed) {
		t.Error("大小写不同的口令不应通过")
	}
	if VerifyPassword(password+"x", hashed) {
		t.Error("多一个字符的口令不应通过")
	}
	if VerifyPassword("", hashed) {
		t.Error("空口令不应通过")
	}
}

// TestHashPassword_NoTruncationForLongPassword 验证超长口令不会被截断。
//
// 这是本文件最重要的用例：
//
//	bcrypt 原始实现只使用输入的前 72 字节。若不做预处理，
//	"72 个 a" 与 "72 个 a + 任意后缀" 会被视为同一口令，
//	攻击者只要拿到前缀就能登录，属于严重安全缺陷。
//	本实现先用 SHA-256 摘要再哈希，从而规避该限制。
func TestHashPassword_NoTruncationForLongPassword(t *testing.T) {
	prefix := strings.Repeat("a", 72)
	short := prefix
	longer := prefix + "DIFFERENT-SUFFIX"

	hashed, err := HashPassword(short)
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}

	if VerifyPassword(short, hashed) != true {
		t.Error("原口令应校验通过")
	}
	if VerifyPassword(longer, hashed) {
		t.Fatal("超长口令被截断：前 72 字节相同但后缀不同的口令竟能互相登录（严重安全缺陷）")
	}
}

// TestHashPassword_SupportsUnicode 验证中文等多字节口令可用。
//
// 意义：UTF-8 下中文一字 3 字节，24 个汉字即达 bcrypt 的 72 字节上限，
// 若不预处理，长中文口令会大面积互相冲突。
func TestHashPassword_SupportsUnicode(t *testing.T) {
	// 30 个汉字 = 90 字节，超过 bcrypt 的 72 字节限制
	pw1 := strings.Repeat("密", 30)
	pw2 := pw1 + "码"

	hashed, err := HashPassword(pw1)
	if err != nil {
		t.Fatalf("中文口令哈希失败: %v", err)
	}
	if !VerifyPassword(pw1, hashed) {
		t.Error("中文口令应校验通过")
	}
	if VerifyPassword(pw2, hashed) {
		t.Error("超过 72 字节的中文口令被截断，不同口令可互相登录")
	}
}

// TestHashPassword_LengthValidation 验证口令长度规则。
//
// 现行规则：只要非空且不超过技术上限（1024 字节）即通过——
// 不限制最小长度，也不限制字符类型。
func TestHashPassword_LengthValidation(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "空口令", input: "", wantErr: true},
		{name: "单字符（数字）", input: "1", wantErr: false},
		{name: "单字符（中文）", input: "水", wantErr: false},
		{name: "极短（3 位）", input: "abc", wantErr: false},
		{name: "恰好 8 字节", input: "12345678", wantErr: false},
		{name: "常规长度", input: "a-reasonable-password", wantErr: false},
		{name: "恰好上限（1024 字节）", input: strings.Repeat("a", 1024), wantErr: false},
		{name: "超过上限（1025 字节）", input: strings.Repeat("a", 1025), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := HashPassword(tc.input)
			if tc.wantErr && err == nil {
				t.Error("期望报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
		})
	}
}

// TestPassword_任意字符类型_均可哈希并校验通过 验证"支持任何类型密码"。
//
// 这是产品明确要求的能力：中文、空格、各类特殊符号、emoji 都必须能被接受，
// 且哈希后能被正确校验（即使用者真的能用这个口令登录）。
func TestPassword_任意字符类型_均可哈希并校验通过(t *testing.T) {
	passwords := []string{
		"1", // 极短
		"水", // 单个中文
		"我的密码就是这一句话别猜了",                // 纯中文短语
		"  p@ss w#rd!￥%…&*（）<>?/\\|  ", // 含空格与各类特殊符号
		"🔐💧🌊",                          // emoji
		"pass\tword\nwith\nnewlines",   // 含控制字符
		"ｆｕｌｌｗｉｄｔｈ　全角",                 // 全角字符
	}

	for _, pw := range passwords {
		hashed, err := HashPassword(pw)
		if err != nil {
			t.Errorf("口令 %q 应被接受，实际报错: %v", pw, err)
			continue
		}
		if !VerifyPassword(pw, hashed) {
			t.Errorf("口令 %q 哈希后无法通过校验", pw)
		}
		// 反向验证：改一个字符即应失败
		if VerifyPassword(pw+"x", hashed) {
			t.Errorf("口令 %q 的哈希对错误口令也放行了", pw)
		}
	}
}

// TestValidatePasswordStrength_MatchesHashRules 验证对外校验函数与内部规则一致。
//
// 意义：注册接口会先调用该函数做预检，若它与实际哈希规则不一致，
// 会出现"接口校验通过但入库失败"的割裂体验。
func TestValidatePasswordStrength_MatchesHashRules(t *testing.T) {
	inputs := []string{"", "short", "12345678", strings.Repeat("x", 200), strings.Repeat("x", 300)}

	for _, input := range inputs {
		precheckErr := ValidatePasswordStrength(input)

		_, hashErr := HashPassword(input)

		if (precheckErr == nil) != (hashErr == nil) {
			t.Errorf("输入长度 %d 时预检与哈希结果不一致：预检=%v 哈希=%v",
				len(input), precheckErr, hashErr)
		}
	}
}

// TestVerifyPassword_InvalidHashFormat 验证非法哈希串被安全拒绝（不 panic）。
func TestVerifyPassword_InvalidHashFormat(t *testing.T) {
	cases := []string{
		"",
		"not-a-bcrypt-hash",
		"$2a$12$tooshort",
		"$2a$99$invalidcostaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}

	for _, hashed := range cases {
		if VerifyPassword("any-password", hashed) {
			t.Errorf("非法哈希串 %q 不应校验通过", hashed)
		}
	}
}
