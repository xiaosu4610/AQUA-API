// 令牌领域模型的单元测试。
//
// 意图（Why）：
//
//	令牌是网关的对外凭证，其状态判定（过期、额度、白名单）直接决定"谁能调用什么"。
//	这些规则一旦出错就是安全漏洞，因此逐条用测试固化。
//
// 流转（Flow）：
//
//	go test ./internal/model/
//
// 扩展（Extend）：
//
//	新增令牌规则后，在对应表驱动用例中补一行。
package model

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// validToken 返回一个各字段均合法的令牌，供用例基于它制造单个错误。
func validToken() *Token {
	return &Token{
		Name:           "测试令牌",
		Key:            "sk-0123456789abcdef0123456789abcdef0123456789abcdef",
		Status:         TokenStatusEnabled,
		RemainQuota:    1000,
		UnlimitedQuota: false,
		UsedQuota:      0,
		Models:         []string{"gpt-4o"},
	}
}

// TestGenerateTokenKey 验证令牌 KEY 的格式与随机性。
func TestGenerateTokenKey(t *testing.T) {
	first, err := GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}

	if !strings.HasPrefix(first, TokenKeyPrefix) {
		t.Errorf("令牌应以 %q 开头，实际 %q", TokenKeyPrefix, first)
	}
	// 24 字节十六进制 = 48 字符，加上前缀共 51
	if len(first) != len(TokenKeyPrefix)+48 {
		t.Errorf("令牌长度 = %d，期望 %d", len(first), len(TokenKeyPrefix)+48)
	}

	// 随机部分必须是合法十六进制（保证 GenerateTokenKey 与校验逻辑一致）
	if _, err := hex.DecodeString(first[len(TokenKeyPrefix):]); err != nil {
		t.Errorf("令牌随机部分不是合法十六进制: %v", err)
	}

	// 两次生成不能相同
	second, err := GenerateTokenKey()
	if err != nil {
		t.Fatalf("第二次生成令牌失败: %v", err)
	}
	if first == second {
		t.Error("两次生成的令牌相同，随机性不足")
	}

	// 生成的令牌必须能通过校验
	tk := validToken()
	tk.Key = first
	if err := tk.Validate(); err != nil {
		t.Errorf("生成的令牌未通过校验: %v", err)
	}
}

// TestToken_Validate 表驱动覆盖全部校验规则。
func TestToken_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Token)
		wantErr bool
	}{
		{name: "合法令牌", mutate: func(*Token) {}, wantErr: false},

		{name: "名称为空", mutate: func(tk *Token) { tk.Name = "" }, wantErr: true},
		{name: "名称仅空白", mutate: func(tk *Token) { tk.Name = "  " }, wantErr: true},

		{name: "KEY 无前缀", mutate: func(tk *Token) { tk.Key = "0123456789" }, wantErr: true},
		{name: "KEY 无随机部分", mutate: func(tk *Token) { tk.Key = "sk-" }, wantErr: true},

		{name: "状态非法", mutate: func(tk *Token) { tk.Status = 0 }, wantErr: true},

		{name: "剩余额度为负", mutate: func(tk *Token) { tk.RemainQuota = -1 }, wantErr: true},
		{name: "不限额度时允许负数剩余", mutate: func(tk *Token) {
			tk.UnlimitedQuota = true
			tk.RemainQuota = -1
		}, wantErr: false},
		{name: "已用额度为负", mutate: func(tk *Token) { tk.UsedQuota = -1 }, wantErr: true},
		{name: "剩余额度为 0 合法（由额度状态判定接管）", mutate: func(tk *Token) {
			tk.RemainQuota = 0
		}, wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk := validToken()
			tc.mutate(tk)

			err := tk.Validate()
			if tc.wantErr && err == nil {
				t.Error("期望校验失败，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("期望校验通过，实际失败: %v", err)
			}
		})
	}
}

// TestToken_IsExpired 验证过期判定。
//
// 关键点：零值过期时间表示"永不过期"，绝不能因为零值时间早于当前时间就被判定过期。
func TestToken_IsExpired(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	t.Run("永不过期", func(t *testing.T) {
		tk := &Token{ExpiresAt: time.Time{}}
		if tk.IsExpired(now) {
			t.Error("零值过期时间应表示永不过期")
		}
	})

	t.Run("已过期", func(t *testing.T) {
		tk := &Token{ExpiresAt: now.Add(-time.Minute)}
		if !tk.IsExpired(now) {
			t.Error("过期时间早于当前时间应判定为已过期")
		}
	})

	t.Run("未过期", func(t *testing.T) {
		tk := &Token{ExpiresAt: now.Add(time.Minute)}
		if tk.IsExpired(now) {
			t.Error("过期时间晚于当前时间不应判定为已过期")
		}
	})
}

// TestToken_HasQuota 验证额度判定。
func TestToken_HasQuota(t *testing.T) {
	cases := []struct {
		name         string
		unlimited    bool
		remain       int64
		wantHasQuota bool
	}{
		{name: "不限额度", unlimited: true, remain: 0, wantHasQuota: true},
		{name: "不限额度且剩余为负", unlimited: true, remain: -1, wantHasQuota: true},
		{name: "有剩余额度", unlimited: false, remain: 1, wantHasQuota: true},
		{name: "额度为零", unlimited: false, remain: 0, wantHasQuota: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk := &Token{UnlimitedQuota: tc.unlimited, RemainQuota: tc.remain}
			if got := tk.HasQuota(); got != tc.wantHasQuota {
				t.Errorf("HasQuota() = %v，期望 %v", got, tc.wantHasQuota)
			}
		})
	}
}

// TestToken_AllowsModel 验证模型白名单判定。
func TestToken_AllowsModel(t *testing.T) {
	t.Run("白名单为空表示不限制", func(t *testing.T) {
		tk := &Token{}
		if !tk.AllowsModel("任意模型") {
			t.Error("空白名单应允许任意模型")
		}
	})

	t.Run("精确匹配", func(t *testing.T) {
		tk := &Token{Models: []string{"gpt-4o", "claude-3"}}
		if !tk.AllowsModel("gpt-4o") {
			t.Error("应允许白名单内的模型")
		}
		if tk.AllowsModel("gpt-3.5") {
			t.Error("不应允许白名单外的模型")
		}
	})

	t.Run("大小写敏感", func(t *testing.T) {
		tk := &Token{Models: []string{"gpt-4o"}}
		if tk.AllowsModel("GPT-4O") {
			t.Error("匹配应大小写敏感")
		}
	})
}

// TestToken_EffectiveStatus 验证"结合时间与额度"的实际状态判定。
//
// 优先级约定：手动禁用 > 已过期 > 额度耗尽 > 启用。
func TestToken_EffectiveStatus(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		token *Token
		want  TokenStatus
	}{
		{
			name:  "正常启用",
			token: &Token{Status: TokenStatusEnabled, RemainQuota: 100},
			want:  TokenStatusEnabled,
		},
		{
			name:  "手动禁用优先于额度充足",
			token: &Token{Status: TokenStatusDisabled, RemainQuota: 100},
			want:  TokenStatusDisabled,
		},
		{
			name:  "手动禁用优先于已过期",
			token: &Token{Status: TokenStatusDisabled, ExpiresAt: now.Add(-time.Hour)},
			want:  TokenStatusDisabled,
		},
		{
			name:  "已过期优先于额度充足",
			token: &Token{Status: TokenStatusEnabled, ExpiresAt: now.Add(-time.Minute), RemainQuota: 100},
			want:  TokenStatusExpired,
		},
		{
			name:  "额度耗尽",
			token: &Token{Status: TokenStatusEnabled, RemainQuota: 0},
			want:  TokenStatusExhausted,
		},
		{
			name:  "不限额度且未过期",
			token: &Token{Status: TokenStatusEnabled, UnlimitedQuota: true},
			want:  TokenStatusEnabled,
		},
		{
			name:  "不限额度但已过期",
			token: &Token{Status: TokenStatusEnabled, UnlimitedQuota: true, ExpiresAt: now.Add(-time.Minute)},
			want:  TokenStatusExpired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.token.EffectiveStatus(now); got != tc.want {
				t.Errorf("EffectiveStatus() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestToken_MaskedKey 验证令牌脱敏规则。
func TestToken_MaskedKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{name: "空 KEY", key: "", want: ""},
		{name: "过短完全遮蔽", key: "sk-ab", want: "*****"},
		{name: "标准 KEY 保留前后段", key: "sk-0123456789abcdefghij", want: "sk-012****ghij"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk := &Token{Key: tc.key}
			got := tk.MaskedKey()

			if got != tc.want {
				t.Errorf("MaskedKey() = %q，期望 %q", got, tc.want)
			}
			// 脱敏结果不得等于原文（标准长度场景）
			if len(tc.key) > 10 && got == tc.key {
				t.Error("脱敏结果等于原文，脱敏未生效")
			}
		})
	}
}

// TestTokenStatus_IsValidAndString 验证状态枚举的取值校验与可读名称。
func TestTokenStatus_IsValidAndString(t *testing.T) {
	valid := map[TokenStatus]string{
		TokenStatusEnabled:   "启用",
		TokenStatusDisabled:  "手动禁用",
		TokenStatusExpired:   "已过期",
		TokenStatusExhausted: "额度耗尽",
	}

	for status, wantName := range valid {
		if !status.IsValid() {
			t.Errorf("状态 %d 应为合法", int(status))
		}
		if got := status.String(); got != wantName {
			t.Errorf("状态 %d 的名称 = %q，期望 %q", int(status), got, wantName)
		}
	}

	for _, status := range []TokenStatus{0, 5, -1} {
		if status.IsValid() {
			t.Errorf("状态 %d 应为非法", int(status))
		}
		if status.String() == "" {
			t.Errorf("未知状态 %d 的名称不应为空", int(status))
		}
	}
}
