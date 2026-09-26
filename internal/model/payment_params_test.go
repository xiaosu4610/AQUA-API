// 支付通道参数（PaymentSettings.Params）的单元测试。
//
// 意图（Why）：
//
//	通道级参数是"支持任意多种支付通道"的承载，且这次改动把易支付的四个参数
//	从专用字段迁到了 Params。迁移类改动最怕两件事：
//	  1) 往返丢值（保存后再读回来变了或没了）；
//	  2) 老库里的历史配置"看起来丢了"（升级事故）。
//	这里把这两点固化成测试。
package model

import "testing"

// TestPaymentParams_往返读写 校验参数写入设置表后能原样读回。
func TestPaymentParams_往返读写(t *testing.T) {
	settings := DefaultSiteSettings()
	settings.Payment.Params = map[string]string{
		"epay.gateway": "https://pay.example.com",
		"epay.pid":     "1001",
		"epay.types":   "alipay,wxpay",
		"stripe.note":  "账户充值",
	}

	loaded := DefaultSiteSettings()
	loadPaymentSettings(&loaded.Payment, settings.ToMap())

	cases := []struct{ channel, field, want string }{
		{"epay", "gateway", "https://pay.example.com"},
		{"epay", "pid", "1001"},
		{"stripe", "note", "账户充值"},
	}
	for _, tc := range cases {
		if got := loaded.Payment.Param(tc.channel, tc.field); got != tc.want {
			t.Errorf("%s.%s = %q，期望 %q", tc.channel, tc.field, got, tc.want)
		}
	}

	// 列表型字段：逗号分隔应被正确拆开
	types := loaded.Payment.ParamList("epay", "types")
	if len(types) != 2 || types[0] != "alipay" || types[1] != "wxpay" {
		t.Errorf("epay.types = %v，期望 [alipay wxpay]", types)
	}
}

// TestPaymentParams_历史字段回退 校验老库里的专用字段仍能被读到并迁移进 Params。
//
// 场景：升级前站长已经把易支付配好了（值只在旧键里）。升级后若不兼容，
// 后台会显示"未配置"，他要么以为配置丢了，要么重新填一遍。
func TestPaymentParams_历史字段回退(t *testing.T) {
	// 只给旧键，不给 payment_params
	values := map[string]string{
		SettingKeyPaymentEPayGateway:     "https://old.example.com",
		SettingKeyPaymentEPayPID:         "2002",
		SettingKeyPaymentEPayTypes:       "alipay",
		SettingKeyPaymentStripePriceNote: "老商品名",
	}

	loaded := DefaultSiteSettings()
	loadPaymentSettings(&loaded.Payment, values)

	if got := loaded.Payment.Param("epay", "gateway"); got != "https://old.example.com" {
		t.Errorf("历史字段回退失败：epay.gateway = %q", got)
	}
	if got := loaded.Payment.Param("epay", "pid"); got != "2002" {
		t.Errorf("历史字段回退失败：epay.pid = %q", got)
	}
	if got := loaded.Payment.Param("stripe", "note"); got != "老商品名" {
		t.Errorf("历史字段回退失败：stripe.note = %q", got)
	}

	// 同时应当被迁移进 Params，这样下次保存就会落到新键上
	for _, key := range []string{"epay.gateway", "epay.pid"} {
		if loaded.Payment.Params[key] == "" {
			t.Errorf("历史值未迁移进 Params：%s 为空", key)
		}
	}
}

// TestPaymentParams_新值优先于历史字段 校验迁移不会把站长的新配置覆盖掉。
func TestPaymentParams_新值优先于历史字段(t *testing.T) {
	loaded := DefaultSiteSettings()
	loadPaymentSettings(&loaded.Payment, map[string]string{
		// 新键与旧键同时存在，且值不同
		SettingKeyPaymentParams:      `{"epay.pid":"new-pid"}`,
		SettingKeyPaymentEPayPID:     "old-pid",
		SettingKeyPaymentEPayGateway: "https://old.example.com",
	})

	if got := loaded.Payment.Param("epay", "pid"); got != "new-pid" {
		t.Errorf("新键应优先于历史字段，实际 %q", got)
	}
	// Params 里没有的键，仍应从历史字段补齐
	if got := loaded.Payment.Param("epay", "gateway"); got != "https://old.example.com" {
		t.Errorf("Params 缺该键时应回退历史字段，实际 %q", got)
	}
}

// TestPaymentParams_坏JSON不致命 校验被手工写坏的 JSON 不会让设置加载失败。
//
// 为什么重要：设置加载失败会导致整个后台配置页打不开，
// 而"一个键被写坏"完全可能由人工改库造成，代价不该如此之高。
func TestPaymentParams_坏JSON不致命(t *testing.T) {
	loaded := DefaultSiteSettings()
	loadPaymentSettings(&loaded.Payment, map[string]string{
		SettingKeyPaymentParams:  `{"epay.pid":`, // 非法 JSON
		SettingKeyPaymentEPayPID: "fallback-pid",
	})

	if got := loaded.Payment.Param("epay", "pid"); got != "fallback-pid" {
		t.Errorf("坏 JSON 时应回退历史字段，实际 %q", got)
	}
}

// TestPaymentParams_空对象与未配置可区分 校验序列化约定。
func TestPaymentParams_空对象与未配置可区分(t *testing.T) {
	settings := DefaultSiteSettings()
	settings.Payment.Params = nil
	if got := settings.ToMap()[SettingKeyPaymentParams]; got != "{}" {
		t.Errorf("空 Params 应序列化为 \"{}\"，实际 %q", got)
	}
}
