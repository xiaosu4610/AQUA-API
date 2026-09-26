// 本文件是邮箱验证码领域逻辑的单元测试。
//
// 测试重点（为什么测这些）：
//   - 验证码随机性：若退化为可预测序列，等于没有验证码；
//   - 邮箱规范化：不做大小写归一，同一邮箱可用变体绕过限流；
//   - 邮箱格式校验：边界用例（无 @、多 @、无点域名）必须被判非法；
//   - 哈希加盐：同一验证码在不同邮箱下必须得到不同摘要，否则一张预计算表即可反查；
//   - 校验比对：错误验证码绝不能被判为通过。
package model

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateEmailCode_长度与字符集_满足既定规格(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := GenerateEmailCode()
		if err != nil {
			t.Fatalf("生成验证码失败: %v", err)
		}
		if len(code) != EmailCodeLength {
			t.Fatalf("验证码长度应为 %d，实际 %d（值 %q）", EmailCodeLength, len(code), code)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("验证码应只含数字，实际出现 %q（值 %q）", r, code)
			}
		}
	}
}

func TestGenerateEmailCode_多次生成_不应完全相同(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		code, err := GenerateEmailCode()
		if err != nil {
			t.Fatalf("生成验证码失败: %v", err)
		}
		seen[code] = struct{}{}
	}
	// 100 次生成若只出现 1 个值，说明随机源失效（正常情况几乎必然出现数十个不同值）
	if len(seen) < 10 {
		t.Fatalf("验证码随机性异常：100 次仅生成 %d 个不同值", len(seen))
	}
}

func TestNormalizeEmail_存在空白与大小写_应统一为标准形式(t *testing.T) {
	got := NormalizeEmail("  Aqua.Admin@LTZY.Top  ")
	want := "aqua.admin@ltzy.top"
	if got != want {
		t.Fatalf("规范化结果应为 %q，实际 %q", want, got)
	}
}

func TestValidateEmailFormat_用例表(t *testing.T) {
	valid := []string{
		"a@b.cn",
		"aqua@ltzy.top",
		"first.last@sub.example.com",
		"user+tag@example.co.uk",
	}
	for _, email := range valid {
		if err := ValidateEmailFormat(email); err != nil {
			t.Errorf("邮箱 %q 应判为合法，实际报错: %v", email, err)
		}
	}

	invalid := []string{
		"",                                  // 空
		"   ",                               // 全空白
		"no-at-sign",                        // 无 @
		"@example.com",                      // 缺本地部分
		"user@",                             // 缺域名
		"user@localhost",                    // 域名无点（无法收信）
		"user@.com",                         // 域名以点开头
		"user@example.",                     // 域名以点结尾
		"a@b@c.com",                         // 多个 @
		"user name@example.com",             // 含空格
		"user@exam ple.com",                 // 域名含空格
		strings.Repeat("a", 250) + "@x.com", // 超长
	}
	for _, email := range invalid {
		if err := ValidateEmailFormat(email); err == nil {
			t.Errorf("邮箱 %q 应判为非法，实际通过校验", email)
		}
	}
}

func TestHashEmailCode_相同输入_结果稳定(t *testing.T) {
	a := HashEmailCode("aqua@ltzy.top", EmailCodePurposeRegister, "123456")
	b := HashEmailCode("aqua@ltzy.top", EmailCodePurposeRegister, "123456")
	if a != b {
		t.Fatalf("相同输入应得到相同摘要，实际 %q 与 %q", a, b)
	}
}

func TestHashEmailCode_不同邮箱同验证码_摘要必须不同(t *testing.T) {
	a := HashEmailCode("user1@ltzy.top", EmailCodePurposeRegister, "123456")
	b := HashEmailCode("user2@ltzy.top", EmailCodePurposeRegister, "123456")
	if a == b {
		t.Fatal("不同邮箱的同一验证码不应产生相同摘要（盐未生效，可被预计算表反查）")
	}
}

func TestHashEmailCode_大小写不同的邮箱_应视为同一邮箱(t *testing.T) {
	a := HashEmailCode("Aqua@LTZY.Top", EmailCodePurposeRegister, "123456")
	b := HashEmailCode("aqua@ltzy.top", EmailCodePurposeRegister, "123456")
	if a != b {
		t.Fatal("邮箱未规范化，大小写变体会被当成不同邮箱而绕过限流")
	}
}

func TestVerifyEmailCode_正确与错误验证码(t *testing.T) {
	record := &EmailCode{
		Email:    "aqua@ltzy.top",
		Purpose:  EmailCodePurposeRegister,
		CodeHash: HashEmailCode("aqua@ltzy.top", EmailCodePurposeRegister, "654321"),
	}

	if !VerifyEmailCode(record, "654321") {
		t.Error("正确的验证码应通过校验")
	}
	if VerifyEmailCode(record, "654320") {
		t.Error("错误的验证码不应通过校验")
	}
	// 带空白应被容忍（用户复制粘贴时常带空格）
	if !VerifyEmailCode(record, "  654321 ") {
		t.Error("验证码两侧空白应被忽略")
	}
}

func TestEmailCode_状态判定(t *testing.T) {
	now := time.Now()

	active := &EmailCode{ExpiresAt: now.Add(time.Minute)}
	if active.IsExpired(now) {
		t.Error("未到期的验证码不应判为过期")
	}
	if active.IsConsumed() {
		t.Error("未消费的验证码不应判为已消费")
	}

	expired := &EmailCode{ExpiresAt: now.Add(-time.Second)}
	if !expired.IsExpired(now) {
		t.Error("已过期的验证码应判为过期")
	}

	// 边界：到期时刻本身即视为过期（半开区间，避免"正好卡在边界仍可用"）
	boundary := &EmailCode{ExpiresAt: now}
	if !boundary.IsExpired(now) {
		t.Error("到期时刻应判为过期")
	}

	consumed := &EmailCode{ExpiresAt: now.Add(time.Minute), ConsumedAt: now}
	if !consumed.IsConsumed() {
		t.Error("已标记消费的验证码应判为已消费")
	}
}
