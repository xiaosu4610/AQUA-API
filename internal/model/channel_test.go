// 领域模型的单元测试。
//
// 意图（Why）：
//
//	领域层是所有写入路径的最终守门人，因此在模型内部做全量校验测试，
//	比在每个调用方重复测试更经济、也更不容易遗漏。
//
// 流转（Flow）：
//
//	go test ./internal/model/
//
// 扩展（Extend）：
//
//	新增校验规则后，在 TestChannel_Validate 的用例表中补一行。
package model

import (
	"strings"
	"testing"
)

// validChannel 返回一个各字段均合法的渠道，供用例基于它制造单个错误。
func validChannel() *Channel {
	return &Channel{
		Name:     "OpenAI 官方",
		Type:     1,
		BaseURL:  "https://api.openai.com",
		APIKey:   "sk-abcdefghijklmnopqrstuvwxyz",
		Models:   []string{"gpt-4o"},
		Group:    "default",
		Priority: 10,
		Weight:   1,
		Status:   ChannelStatusEnabled,
	}
}

// TestChannel_Validate 表驱动覆盖全部校验规则。
func TestChannel_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Channel)
		wantErr bool
	}{
		{name: "合法渠道", mutate: func(*Channel) {}, wantErr: false},

		{name: "名称为空", mutate: func(c *Channel) { c.Name = "" }, wantErr: true},
		{name: "名称仅空白", mutate: func(c *Channel) { c.Name = "   " }, wantErr: true},

		{name: "类型为 0", mutate: func(c *Channel) { c.Type = 0 }, wantErr: true},
		{name: "类型为负", mutate: func(c *Channel) { c.Type = -1 }, wantErr: true},

		{name: "base_url 为空", mutate: func(c *Channel) { c.BaseURL = "" }, wantErr: true},
		{name: "base_url 缺协议", mutate: func(c *Channel) { c.BaseURL = "api.openai.com" }, wantErr: true},
		{name: "base_url 非 http 协议", mutate: func(c *Channel) { c.BaseURL = "ftp://x.com" }, wantErr: true},
		{name: "base_url 允许 http 内网", mutate: func(c *Channel) { c.BaseURL = "http://10.0.0.1:8000" }, wantErr: false},

		{name: "权重为 0", mutate: func(c *Channel) { c.Weight = 0 }, wantErr: true},
		{name: "权重为负", mutate: func(c *Channel) { c.Weight = -3 }, wantErr: true},

		{name: "优先级为负", mutate: func(c *Channel) { c.Priority = -1 }, wantErr: true},
		{name: "优先级为 0 合法", mutate: func(c *Channel) { c.Priority = 0 }, wantErr: false},

		{name: "状态非法", mutate: func(c *Channel) { c.Status = 0 }, wantErr: true},

		{name: "分组为空", mutate: func(c *Channel) { c.Group = "" }, wantErr: true},
		{name: "分组仅空白", mutate: func(c *Channel) { c.Group = " " }, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := validChannel()
			tc.mutate(ch)

			err := ch.Validate()
			if tc.wantErr && err == nil {
				t.Error("期望校验失败，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("期望校验通过，实际失败: %v", err)
			}
		})
	}
}

// TestChannel_HasModel 验证模型支持判断为精确匹配。
func TestChannel_HasModel(t *testing.T) {
	ch := validChannel()
	ch.Models = []string{"gpt-4o", "gpt-4o-mini"}

	if !ch.HasModel("gpt-4o") {
		t.Error("应支持 gpt-4o")
	}
	if !ch.HasModel("gpt-4o-mini") {
		t.Error("应支持 gpt-4o-mini")
	}
	// 大小写敏感：不应误命中
	if ch.HasModel("GPT-4O") {
		t.Error("不应支持 GPT-4O（匹配应大小写敏感）")
	}
	// 未声明的模型
	if ch.HasModel("gpt-3.5-turbo") {
		t.Error("不应支持未声明的模型")
	}
	// 空模型列表
	empty := validChannel()
	empty.Models = nil
	if empty.HasModel("anything") {
		t.Error("空模型列表不应支持任何模型")
	}
}

// TestChannel_MaskedAPIKey 验证密钥脱敏规则。
//
// 安全目标：既能让人辨认"这是哪把钥匙"，又不足以被直接盗用。
func TestChannel_MaskedAPIKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{name: "空密钥返回空", key: "", want: ""},
		{name: "短密钥完全遮蔽", key: "short", want: "*****"},
		{name: "恰好等于保留长度时完全遮蔽", key: "0123456789", want: "**********"},
		{name: "正常密钥保留前后缀", key: "sk-abcdefghijklmnop", want: "sk-abc****mnop"},
		{name: "NVIDIA 风格密钥", key: "nvapi-0123456789abcdefghij", want: "nvapi-****ghij"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := &Channel{APIKey: tc.key}
			got := ch.MaskedAPIKey()
			if got != tc.want {
				t.Errorf("MaskedAPIKey(%q) = %q，期望 %q", tc.key, got, tc.want)
			}
			// 脱敏结果不得包含完整明文（长密钥场景）
			if len(tc.key) > 10 && strings.Contains(got, tc.key) {
				t.Error("脱敏结果仍包含完整明文密钥")
			}
		})
	}
}

// TestChannelStatus_IsValid 验证状态取值校验。
func TestChannelStatus_IsValid(t *testing.T) {
	valid := []ChannelStatus{ChannelStatusEnabled, ChannelStatusDisabled, ChannelStatusAutoDisabled}
	for _, s := range valid {
		if !s.IsValid() {
			t.Errorf("状态 %d 应为合法", int(s))
		}
	}

	invalid := []ChannelStatus{0, 4, -1, 100}
	for _, s := range invalid {
		if s.IsValid() {
			t.Errorf("状态 %d 应为非法", int(s))
		}
	}
}

// TestChannelStatus_String 验证状态中文名（日志与前端展示依赖）。
func TestChannelStatus_String(t *testing.T) {
	cases := map[ChannelStatus]string{
		ChannelStatusEnabled:      "启用",
		ChannelStatusDisabled:     "手动禁用",
		ChannelStatusAutoDisabled: "自动禁用",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("状态 %d 的名称 = %q，期望 %q", int(status), got, want)
		}
	}
	// 未知状态应可读地展示数值，而不是返回空串
	if got := ChannelStatus(42).String(); got == "" {
		t.Error("未知状态的名称不应为空")
	}
}
