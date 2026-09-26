// 站点设置（含支付运营参数）的单元测试。
//
// 测试重点：
//   - 充值金额 → 额度的换算必须用整数、向下取整（绝不因为浮点或四舍五入多给额度）；
//   - 支付设置的默认值必须"默认关闭充值"，避免刚部署就有人下单却付不了款；
//   - KV 中被写坏的项应回退默认值，而不是让整个充值页无法渲染。
package model

import (
	"context"
	"testing"
)

// fakeSettingRepo 是内存版设置仓储，用于测试读写映射。
type fakeSettingRepo struct {
	values map[string]string
}

func (f *fakeSettingRepo) Get(_ context.Context, key string) (string, error) {
	return f.values[key], nil
}

func (f *fakeSettingRepo) GetAll(context.Context) (map[string]string, error) {
	result := make(map[string]string, len(f.values))
	for key, value := range f.values {
		result[key] = value
	}
	return result, nil
}

func (f *fakeSettingRepo) Set(_ context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakeSettingRepo) SetMany(_ context.Context, values map[string]string) error {
	for key, value := range values {
		f.values[key] = value
	}
	return nil
}

func TestDefaultSiteSettings_默认不开放充值(t *testing.T) {
	settings := DefaultSiteSettings()

	if settings.Payment.Enabled {
		t.Fatal("默认不应开放充值（开放收款必须由站长显式配置并开启）")
	}
	if settings.Payment.ExchangeRate <= 0 {
		t.Fatalf("默认兑换比例必须为正数，实际 %d", settings.Payment.ExchangeRate)
	}
	if settings.Payment.OrderTTLMinutes <= 0 {
		t.Fatalf("默认订单有效期必须为正数，实际 %d", settings.Payment.OrderTTLMinutes)
	}
	if !settings.Payment.MethodEnabled(PaymentMethodManual) {
		t.Fatal("默认应启用人工确认通道（自用部署不依赖任何第三方）")
	}
}

func TestPaymentSettings_AmountToQuota_整数换算向下取整(t *testing.T) {
	pay := PaymentSettings{ExchangeRate: 100}

	cases := map[int64]int64{
		100:  100,  // 1 元 = 100 额度
		1050: 1050, // 10.5 元
		1:    1,    // 1 分 = 1 额度
		0:    0,    // 非法金额不产生额度
		-100: 0,    // 负数不产生额度
	}
	for cents, want := range cases {
		if got := pay.AmountToQuota(cents); got != want {
			t.Errorf("AmountToQuota(%d) = %d，期望 %d", cents, got, want)
		}
	}

	// 比例非整数时向下取整：150 分 × 3 / 100 = 4（而不是 5）
	odd := PaymentSettings{ExchangeRate: 3}
	if got := odd.AmountToQuota(150); got != 4 {
		t.Fatalf("应向下取整得到 4，实际 %d", got)
	}

	// 比例非法（0）时返回 0，避免"付钱但到账 0 额度"的订单被创建
	if got := (PaymentSettings{ExchangeRate: 0}).AmountToQuota(1000); got != 0 {
		t.Fatalf("比例为 0 时应返回 0，实际 %d", got)
	}
}

func TestSiteSettings_ToMap与Load往返一致(t *testing.T) {
	original := DefaultSiteSettings()
	original.SiteName = "测试站点"
	original.Payment = PaymentSettings{
		Enabled:         true,
		Methods:         []string{PaymentMethodEPay, PaymentMethodManual},
		ExchangeRate:    250,
		Currency:        "CNY",
		MinCents:        500,
		MaxCents:        100000,
		OrderTTLMinutes: 15,
		NotifyBase:      "https://api.example.com",
		EPayGateway:     "https://pay.example.com",
		EPayPID:         "2001",
		EPayTypes:       []string{"alipay", "wxpay"},
		StripeNote:      "账户充值",
	}

	repo := &fakeSettingRepo{values: original.ToMap()}
	loaded, err := LoadSiteSettings(context.Background(), repo)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	if !loaded.Payment.Enabled || loaded.Payment.ExchangeRate != 250 {
		t.Fatalf("支付开关或比例未往返一致: %+v", loaded.Payment)
	}
	if len(loaded.Payment.Methods) != 2 || loaded.Payment.Methods[0] != PaymentMethodEPay {
		t.Fatalf("支付通道未往返一致: %+v", loaded.Payment.Methods)
	}
	if loaded.Payment.MinCents != 500 || loaded.Payment.MaxCents != 100000 {
		t.Fatalf("金额限额未往返一致: %+v", loaded.Payment)
	}
	if loaded.Payment.NotifyBase != "https://api.example.com" {
		t.Fatalf("回调基址未往返一致: %q", loaded.Payment.NotifyBase)
	}
	if len(loaded.Payment.EPayTypes) != 2 || loaded.Payment.EPayTypes[1] != "wxpay" {
		t.Fatalf("易支付类型未往返一致: %+v", loaded.Payment.EPayTypes)
	}
}

func TestLoadSiteSettings_脏值回退默认值(t *testing.T) {
	repo := &fakeSettingRepo{values: map[string]string{
		SettingKeyPaymentExchangeRate:    "不是数字",
		SettingKeyPaymentMinCents:        "-5",
		SettingKeyPaymentOrderTTLMinutes: "0",
		// 中文逗号也应被正确解析
		SettingKeyPaymentMethods: "epay，manual",
	}}

	loaded, err := LoadSiteSettings(context.Background(), repo)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	defaults := DefaultSiteSettings()
	if loaded.Payment.ExchangeRate != defaults.Payment.ExchangeRate {
		t.Fatalf("非法比例应回退默认值 %d，实际 %d",
			defaults.Payment.ExchangeRate, loaded.Payment.ExchangeRate)
	}
	if loaded.Payment.MinCents != defaults.Payment.MinCents {
		t.Fatalf("负数限额应回退默认值 %d，实际 %d", defaults.Payment.MinCents, loaded.Payment.MinCents)
	}
	if loaded.Payment.OrderTTLMinutes != defaults.Payment.OrderTTLMinutes {
		t.Fatalf("0 分钟有效期应回退默认值 %d，实际 %d",
			defaults.Payment.OrderTTLMinutes, loaded.Payment.OrderTTLMinutes)
	}
	if len(loaded.Payment.Methods) != 2 {
		t.Fatalf("中文逗号分隔的通道应被解析为 2 项，实际 %+v", loaded.Payment.Methods)
	}
}

func TestPaymentOrder_AmountYuan与校验(t *testing.T) {
	order := &PaymentOrder{TradeNo: "pay1", UserID: 1, Amount: 1050, Method: PaymentMethodManual}
	if err := order.Validate(); err != nil {
		t.Fatalf("合法订单不应报错: %v", err)
	}
	if order.AmountYuan() != "10.50" {
		t.Fatalf("金额展示应为 10.50，实际 %q", order.AmountYuan())
	}
	if order.Status != PaymentStatusPending {
		t.Fatalf("未指定状态时应默认待支付，实际 %v", order.Status)
	}

	zero := &PaymentOrder{TradeNo: "pay2", UserID: 1, Amount: 0, Method: PaymentMethodManual}
	if err := zero.Validate(); err == nil {
		t.Fatal("金额为 0 的订单应被拒绝")
	}

	noMethod := &PaymentOrder{TradeNo: "pay3", UserID: 1, Amount: 100}
	if err := noMethod.Validate(); err == nil {
		t.Fatal("缺少支付通道的订单应被拒绝")
	}
}

func TestGenerateTradeNo_带时间前缀且唯一(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		tradeNo, err := GenerateTradeNo()
		if err != nil {
			t.Fatalf("生成订单号失败: %v", err)
		}
		if len(tradeNo) != 3+14+6 {
			t.Fatalf("订单号长度不符（应为 pay + 14 位时间 + 6 位随机）: %q", tradeNo)
		}
		if _, dup := seen[tradeNo]; dup {
			t.Fatalf("订单号重复: %s", tradeNo)
		}
		seen[tradeNo] = struct{}{}
	}
}
