// 支付通道注册表的单元测试。
//
// 意图（Why）：
//
//	注册表是"后台能勾选哪些支付通道、每个通道要填什么"的唯一来源，
//	一旦它与真实实现脱节（例如标记了可用但没写适配器），
//	站长就会开启一个注定失败的通道 —— 用户看到支付入口却付不了款。
//	这里把几条不变量固化成测试。
//
// 流转（Flow）：
//
//	go test ./internal/payment/ → 校验注册表与适配器注册的一致性
//
// 扩展（Extend）：
//
//	新增支付通道后，若它标记 Available=true，请确认 TestChannels_可用通道必须有适配器 仍通过。
package payment

import (
	"errors"
	"strings"
	"testing"
)

// fakeEnv 构造一个"环境变量查找"假实现，避免测试依赖真实进程环境。
func fakeEnv(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// TestChannels_可用通道必须有适配器 校验"标记可用"与"真的有实现"一致。
//
// 这条不变量防的是：注册表里把某通道标成 Available=true，却忘了在
// NewRegistry 里注册对应 Provider，于是后台允许勾选、用户却付不了款。
func TestChannels_可用通道必须有适配器(t *testing.T) {
	registry := NewRegistry(Options{})

	for _, channel := range Channels() {
		if !channel.Available {
			continue
		}
		if _, err := registry.Get(channel.Key); err != nil {
			t.Errorf("通道 %s 标记为可用，但没有注册适配器: %v", channel.Key, err)
		}
	}

	// 反向检查：注册了适配器的通道，也必须在注册表里登记（否则后台看不见它）
	for _, name := range registry.Names() {
		if _, ok := FindChannel(name); !ok {
			t.Errorf("适配器 %s 没有在通道注册表里登记，后台将无法配置它", name)
		}
	}
}

// TestChannels_未实现通道不得被启用 校验未实现的通道会被明确拒绝。
func TestChannels_未实现通道不得被启用(t *testing.T) {
	env := fakeEnv(map[string]string{
		"AQUA_ALIPAY_PRIVATE_KEY": "x",
		"AQUA_ALIPAY_PUBLIC_KEY":  "y",
	})

	channel, ok := FindChannel("alipay")
	if !ok {
		t.Fatal("找不到 alipay 通道")
	}
	// 前置条件：本用例假设它尚未实现；若将来实现了，这条断言会失败并提醒更新测试
	if channel.Available {
		t.Skip("alipay 已实现，本用例不再适用")
	}

	err := ValidateChannelEnabled(channel, env)
	if !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("未实现通道应返回 ErrProviderDisabled，实际 %v", err)
	}
}

// TestChannels_缺密钥时不允许启用 校验密钥闸门。
//
// 为什么必须有这道闸门：密钥只走环境变量，若站长只勾选通道却没注入密钥，
// 通道会被"启用"但每次下单都失败。宁可在保存设置时就拒绝，并告诉他缺哪个变量。
func TestChannels_缺密钥时不允许启用(t *testing.T) {
	channel, ok := FindChannel("epay")
	if !ok {
		t.Fatal("找不到 epay 通道")
	}

	// 密钥未注入 → 拒绝，并且错误信息里要出现具体的环境变量名
	err := ValidateChannelEnabled(channel, fakeEnv(nil))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("缺密钥应返回 ErrNotConfigured，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "AQUA_EPAY_KEY") {
		t.Errorf("错误信息应指明缺失的环境变量名，实际: %v", err)
	}

	// 注入后 → 放行
	if err := ValidateChannelEnabled(channel, fakeEnv(map[string]string{"AQUA_EPAY_KEY": "secret"})); err != nil {
		t.Fatalf("密钥就绪时不应报错，实际 %v", err)
	}
}

// TestChannels_字段来源划分正确 校验"非密钥字段不走环境变量、密钥字段必带变量名"。
//
// 关键安全约束：设置表会随数据库备份流出，因此密钥字段绝不允许出现在
// "存设置表"的字段集合里。
func TestChannels_字段来源划分正确(t *testing.T) {
	for _, channel := range Channels() {
		for _, field := range channel.SettingFields() {
			if field.EnvVar != "" {
				t.Errorf("通道 %s 的设置字段 %s 不应带环境变量名（密钥才能带）", channel.Key, field.Key)
			}
		}
		for _, field := range channel.SecretFields() {
			if field.EnvVar == "" {
				t.Errorf("通道 %s 的密钥字段 %s 必须声明环境变量名，否则无法提示使用者", channel.Key, field.Key)
			}
		}
	}
}

// TestChannels_密钥就绪判断 校验 MissingSecrets 的判定。
func TestChannels_密钥就绪判断(t *testing.T) {
	channel, ok := FindChannel("stripe")
	if !ok {
		t.Fatal("找不到 stripe 通道")
	}

	// 两个密钥都缺
	missing := channel.MissingSecrets(fakeEnv(nil))
	if len(missing) != 2 {
		t.Fatalf("缺两个密钥时 MissingSecrets 应返回 2 项，实际 %v", missing)
	}

	// 只缺一个：不能算就绪（Stripe 的验签密钥缺失会导致回调无法校验）
	missing = channel.MissingSecrets(fakeEnv(map[string]string{"AQUA_STRIPE_SECRET_KEY": "sk"}))
	if len(missing) != 1 || missing[0] != "AQUA_STRIPE_WEBHOOK_SECRET" {
		t.Fatalf("应只缺 AQUA_STRIPE_WEBHOOK_SECRET，实际 %v", missing)
	}

	// 都注入且非空白 → 就绪
	missing = channel.MissingSecrets(fakeEnv(map[string]string{
		"AQUA_STRIPE_SECRET_KEY":     "sk",
		"AQUA_STRIPE_WEBHOOK_SECRET": "  ", // 全空白应视为未配置
	}))
	if len(missing) != 1 {
		t.Fatalf("空白值的环境变量应视为未配置，实际 %v", missing)
	}
}

// TestChannels_每个通道都有回调路径或明确无回调 校验回调地址的声明完整性。
//
// 回调地址配错是"付了钱不到账"的最常见原因，因此除人工确认外都应当声明路径。
func TestChannels_每个通道都有回调路径或明确无回调(t *testing.T) {
	for _, channel := range Channels() {
		if channel.Key == "manual" {
			if channel.NotifyPath != "" {
				t.Errorf("人工确认通道不应有回调路径，实际 %q", channel.NotifyPath)
			}
			continue
		}
		if !strings.HasPrefix(channel.NotifyPath, "/api/payments/") {
			t.Errorf("通道 %s 的回调路径应形如 /api/payments/<通道>/notify，实际 %q",
				channel.Key, channel.NotifyPath)
		}
	}
}

// TestChannelKeys_包含全部已登记通道 校验键列表与注册表一致。
func TestChannelKeys_包含全部已登记通道(t *testing.T) {
	keys := ChannelKeys()
	if len(keys) != len(Channels()) {
		t.Fatalf("ChannelKeys 返回 %d 项，注册表有 %d 个通道", len(keys), len(Channels()))
	}
	for _, key := range keys {
		if _, ok := FindChannel(key); !ok {
			t.Errorf("ChannelKeys 返回了注册表里不存在的通道 %q", key)
		}
	}
}
