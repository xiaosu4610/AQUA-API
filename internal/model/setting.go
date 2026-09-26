// 本文件定义「系统设置」领域模型。
//
// 意图（Why）：
//
//	站点名称、是否开放注册、新用户默认额度等运营参数需要能在后台修改，
//	而不必改代码或重启。这些参数变化频率低、读写频繁，用 KV 表最合适。
//
// 安全约束（重要）：
//
//	本表【禁止】存放任何密钥类内容（AQUA_APP_KEY、上游密钥、支付密钥等）。
//	原因：设置会随数据库一起备份/导出，一旦写入密钥就等于把密钥散播到了备份链条上。
//	密钥只允许通过环境变量注入。
//
// 流转（Flow）：
//
//	后台读取：SettingRepository.GetAll → 与 DefaultSiteSettings 合并 → 返回给前端
//	后台保存：前端提交完整对象 → 校验 → SetMany
//
// 扩展（Extend）：
//
//	新增设置项时：
//	  1) 在 SiteSettings 加字段；
//	  2) 在 DefaultSiteSettings 补默认值；
//	  3) 在 LoadSiteSettings 与 ToMap 中补读写映射（三处必须同步）。
package model

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// 设置键名常量。
//
// 集中定义避免各处拼写字符串——拼错时不会编译报错，只会静默读不到值并回退默认，
// 属于典型的"看起来正常但配置不生效"的问题。
const (
	// SettingKeySiteName 站点名称。
	SettingKeySiteName = "site_name"
	// SettingKeySiteDescription 站点描述（落地页与页脚展示）。
	SettingKeySiteDescription = "site_description"
	// SettingKeyRegistrationEnabled 是否开放注册（"true" / "false"）。
	SettingKeyRegistrationEnabled = "registration_enabled"
	// SettingKeyRegistrationRequireEmailCode 注册是否必须通过邮箱验证码校验。
	//
	// 与"是否开放注册"是两个独立开关：
	//   registration_enabled=false 表示完全关闭注册入口；
	//   本项=false 表示允许注册但不校验邮箱（适合内网/自用场景，不依赖邮件通道）。
	SettingKeyRegistrationRequireEmailCode = "registration_require_email_code"
	// SettingKeyDefaultUserQuota 新用户默认额度。
	SettingKeyDefaultUserQuota = "default_user_quota"
	// SettingKeyDefaultGroup 新用户默认分组。
	SettingKeyDefaultGroup = "default_group"

	// ── 支付 / 充值相关（M5）────────────────────────────────────
	//
	// 这些是"非密钥"的运营参数：网关地址、商户号、兑换比例、启用哪些通道。
	// 它们需要管理员随时调整且希望立即生效，因此放在设置表（DB）而非环境变量。
	// 真正的密钥（易支付商户密钥、Stripe Secret Key）只走环境变量，
	// 见 internal/config 的 PaymentConfig——本表【禁止】存放密钥。

	// SettingKeyPaymentEnabled 充值功能总开关（"true" / "false"）。
	SettingKeyPaymentEnabled = "payment_enabled"
	// SettingKeyPaymentMethods 启用的支付通道，逗号分隔（如 "epay,manual"）。
	SettingKeyPaymentMethods = "payment_methods"
	// SettingKeyPaymentExchangeRate 兑换比例：1 个货币单位（元）可兑换的额度。
	//
	// 用整数表示，避免浮点误差导致"充 10 元到账 9.99 元额度"。
	SettingKeyPaymentExchangeRate = "payment_exchange_rate"
	// SettingKeyPaymentCurrency 货币代码（CNY / USD）。
	SettingKeyPaymentCurrency = "payment_currency"
	// SettingKeyPaymentMinCents 单笔最小充值金额（分）。
	SettingKeyPaymentMinCents = "payment_min_cents"
	// SettingKeyPaymentMaxCents 单笔最大充值金额（分）；0 表示不限。
	SettingKeyPaymentMaxCents = "payment_max_cents"
	// SettingKeyPaymentOrderTTLMinutes 订单支付有效期（分钟）。
	SettingKeyPaymentOrderTTLMinutes = "payment_order_ttl_minutes"
	// SettingKeyPaymentNotifyBase 回调基址（公网地址，如 https://api.example.com）。
	//
	// 为什么要显式配置：支付平台必须能回调到本网关，而网关自己看到的
	// Host 可能是内网地址或反向代理后的地址。留空时按请求推导。
	SettingKeyPaymentNotifyBase = "payment_notify_base"
	// SettingKeyPaymentEPayGateway 易支付网关地址（如 https://pay.example.com）。
	SettingKeyPaymentEPayGateway = "payment_epay_gateway"
	// SettingKeyPaymentEPayPID 易支付商户号（PID）。
	SettingKeyPaymentEPayPID = "payment_epay_pid"
	// SettingKeyPaymentEPayTypes 易支付可用的支付类型，逗号分隔（如 "alipay,wxpay"）。
	SettingKeyPaymentEPayTypes = "payment_epay_types"
	// SettingKeyPaymentStripePriceNote Stripe 结账页显示的商品名。
	SettingKeyPaymentStripePriceNote = "payment_stripe_note"
)

// SiteSettings 是站点设置的强类型视图。
//
// 为什么用结构体而非直接透传 map：结构体能在编译期约束字段名，
// 也让"默认值"与"类型转换"有唯一实现处，避免前端拿到字符串型数字。
type SiteSettings struct {
	SiteName            string // 站点名称
	SiteDescription     string // 站点描述
	RegistrationEnabled bool   // 是否开放注册
	// RegistrationRequireEmailCode 注册是否必须通过邮箱验证码校验。
	// 默认 true：注册即代表进入一个有额度的账号体系，验证邮箱能有效遏制脚本批量注册。
	RegistrationRequireEmailCode bool   // 注册是否需要邮箱验证码
	DefaultUserQuota             int64  // 新用户默认额度（-1 表示不限）
	DefaultGroup                 string // 新用户默认分组

	// Payment 是充值相关的运营参数（不含任何密钥）。
	Payment PaymentSettings
}

// PaymentSettings 是充值 / 支付的运营参数。
//
// 设计说明：这里【只放非密钥参数】。密钥（易支付商户密钥、Stripe Secret Key）
// 一律走环境变量，因为设置表会随数据库备份/导出，写入密钥等于把密钥
// 散播到备份链条上（见本文件头部的安全约束）。
type PaymentSettings struct {
	// Enabled 是充值功能总开关。
	Enabled bool
	// Methods 是启用的支付通道（epay / stripe / manual）。
	Methods []string
	// ExchangeRate 是兑换比例：1 个货币单位可兑换的额度。
	ExchangeRate int64
	// Currency 是货币代码（CNY / USD）。
	Currency string
	// MinCents / MaxCents 是单笔充值金额上下限（分）；MaxCents 为 0 表示不限。
	MinCents int64
	MaxCents int64
	// OrderTTLMinutes 是订单支付有效期（分钟）。
	OrderTTLMinutes int
	// NotifyBase 是回调基址（公网地址）；留空时按请求推导。
	NotifyBase string
	// EPayGateway / EPayPID / EPayTypes 是易支付通道的网关地址、商户号与可用类型。
	EPayGateway string
	EPayPID     string
	EPayTypes   []string
	// StripeNote 是 Stripe 结账页显示的商品名。
	StripeNote string
}

// MethodEnabled 判断某个支付通道是否启用。
func (p PaymentSettings) MethodEnabled(name string) bool {
	for _, method := range p.Methods {
		if method == name {
			return true
		}
	}
	return false
}

// AmountToQuota 把"分"换算为入账额度。
//
// 公式：quota = cents × ExchangeRate / 100
//
// 全程整数运算并向下取整。取整意味着不足 1 个额度的零头被舍去，
// 这比"四舍五入多给用户额度"更安全（宁可少给一点点，也不能凭空产生额度）。
func (p PaymentSettings) AmountToQuota(cents int64) int64 {
	if cents <= 0 || p.ExchangeRate <= 0 {
		return 0
	}
	return cents * p.ExchangeRate / 100
}

// CurrencyOrDefault 返回货币代码，未配置时回退 CNY。
func (p PaymentSettings) CurrencyOrDefault() string {
	if strings.TrimSpace(p.Currency) == "" {
		return "CNY"
	}
	return p.Currency
}

// DefaultSiteSettings 返回全部设置项的默认值。
//
// 设计意图：任何一项未配置时都应回退到合理默认，保证"零配置可用"。
func DefaultSiteSettings() SiteSettings {
	return SiteSettings{
		SiteName:            "AQUA-API",
		SiteDescription:     "新一代 AI 资产网关",
		RegistrationEnabled: true,
		// 默认要求邮箱验证码：这是"防批量注册"的第一道闸门，
		// 安全默认值应当偏严（需要放宽时由管理员在后台关闭）。
		RegistrationRequireEmailCode: true,
		DefaultUserQuota:             0, // 新用户默认无额度，由管理员分配（避免被白嫖）
		DefaultGroup:                 "default",
		// 支付默认值：默认【关闭】充值。
		//
		// 为什么默认关闭：开放充值意味着真实收款，必须由站长显式配置网关、
		// 商户号与密钥后才能启用。默认开启会让"刚部署好就有人下单却付不了款"，
		// 是非常糟糕的第一印象。
		Payment: PaymentSettings{
			Enabled: false,
			// 只默认启用人工通道：它不依赖任何第三方，管理员可在后台手动入账，
			// 适合自用部署；需要在线支付时站长再自行开易支付/Stripe。
			Methods:         []string{PaymentMethodManual},
			ExchangeRate:    100, // 1 元 = 100 额度（即 1 额度约等于 1 分）
			Currency:        "CNY",
			MinCents:        100, // 最小 1 元，避免大量 1 分订单把订单表撑爆
			MaxCents:        0,   // 不限上限
			OrderTTLMinutes: 30,  // 30 分钟未支付自动关单
			EPayTypes:       []string{"alipay", "wxpay"},
		},
	}
}

// ToMap 把关类型视图转为 KV，供持久化使用。
func (s SiteSettings) ToMap() map[string]string {
	return map[string]string{
		SettingKeySiteName:                     s.SiteName,
		SettingKeySiteDescription:              s.SiteDescription,
		SettingKeyRegistrationEnabled:          strconv.FormatBool(s.RegistrationEnabled),
		SettingKeyRegistrationRequireEmailCode: strconv.FormatBool(s.RegistrationRequireEmailCode),
		SettingKeyDefaultUserQuota:             strconv.FormatInt(s.DefaultUserQuota, 10),
		SettingKeyDefaultGroup:                 s.DefaultGroup,

		SettingKeyPaymentEnabled:         strconv.FormatBool(s.Payment.Enabled),
		SettingKeyPaymentMethods:         strings.Join(s.Payment.Methods, ","),
		SettingKeyPaymentExchangeRate:    strconv.FormatInt(s.Payment.ExchangeRate, 10),
		SettingKeyPaymentCurrency:        s.Payment.Currency,
		SettingKeyPaymentMinCents:        strconv.FormatInt(s.Payment.MinCents, 10),
		SettingKeyPaymentMaxCents:        strconv.FormatInt(s.Payment.MaxCents, 10),
		SettingKeyPaymentOrderTTLMinutes: strconv.Itoa(s.Payment.OrderTTLMinutes),
		SettingKeyPaymentNotifyBase:      s.Payment.NotifyBase,
		SettingKeyPaymentEPayGateway:     s.Payment.EPayGateway,
		SettingKeyPaymentEPayPID:         s.Payment.EPayPID,
		SettingKeyPaymentEPayTypes:       strings.Join(s.Payment.EPayTypes, ","),
		SettingKeyPaymentStripePriceNote: s.Payment.StripeNote,
	}
}

// LoadSiteSettings 从 KV 仓储读取设置并与默认值合并。
//
// 合并语义：仅覆盖"存在且解析成功"的项；解析失败或缺失的项保留默认值。
// 这样即使有人在数据库里手工写入了非法值，站点也不会因解析失败而无法启动。
func LoadSiteSettings(ctx context.Context, repo SettingRepository) (SiteSettings, error) {
	settings := DefaultSiteSettings()

	values, err := repo.GetAll(ctx)
	if err != nil {
		return settings, err
	}

	if v, ok := values[SettingKeySiteName]; ok && v != "" {
		settings.SiteName = v
	}
	if v, ok := values[SettingKeySiteDescription]; ok {
		settings.SiteDescription = v
	}
	if v, ok := values[SettingKeyRegistrationEnabled]; ok {
		if parsed, err := strconv.ParseBool(v); err == nil {
			settings.RegistrationEnabled = parsed
		}
	}
	if v, ok := values[SettingKeyRegistrationRequireEmailCode]; ok {
		if parsed, err := strconv.ParseBool(v); err == nil {
			settings.RegistrationRequireEmailCode = parsed
		}
	}
	if v, ok := values[SettingKeyDefaultUserQuota]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= QuotaUnlimited {
			settings.DefaultUserQuota = parsed
		}
	}
	if v, ok := values[SettingKeyDefaultGroup]; ok && v != "" {
		settings.DefaultGroup = v
	}

	loadPaymentSettings(&settings.Payment, values)

	return settings, nil
}

// loadPaymentSettings 把 KV 中的支付参数合并进强类型结构。
//
// 逐项"存在且解析成功才覆盖"：数据库里被手工写坏的项应回退默认值，
// 而不是让整个站点因一个脏值而无法渲染充值页。
func loadPaymentSettings(target *PaymentSettings, values map[string]string) {
	if v, ok := values[SettingKeyPaymentEnabled]; ok {
		if parsed, err := strconv.ParseBool(v); err == nil {
			target.Enabled = parsed
		}
	}
	if v, ok := values[SettingKeyPaymentMethods]; ok {
		// 允许显式清空（空字符串 → 空列表），这是"关闭所有在线通道"的合法表达
		target.Methods = splitList(v)
	}
	if v, ok := values[SettingKeyPaymentExchangeRate]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			target.ExchangeRate = parsed
		}
	}
	if v, ok := values[SettingKeyPaymentCurrency]; ok && strings.TrimSpace(v) != "" {
		target.Currency = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeyPaymentMinCents]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 0 {
			target.MinCents = parsed
		}
	}
	if v, ok := values[SettingKeyPaymentMaxCents]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 0 {
			target.MaxCents = parsed
		}
	}
	if v, ok := values[SettingKeyPaymentOrderTTLMinutes]; ok {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			target.OrderTTLMinutes = parsed
		}
	}
	if v, ok := values[SettingKeyPaymentNotifyBase]; ok {
		target.NotifyBase = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v, ok := values[SettingKeyPaymentEPayGateway]; ok {
		target.EPayGateway = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v, ok := values[SettingKeyPaymentEPayPID]; ok {
		target.EPayPID = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeyPaymentEPayTypes]; ok {
		target.EPayTypes = splitList(v)
	}
	if v, ok := values[SettingKeyPaymentStripePriceNote]; ok && strings.TrimSpace(v) != "" {
		target.StripeNote = v
	}
}

// splitList 解析逗号分隔的列表，去除空白项。
//
// 同时兼容中文逗号：使用者在中后台输入时很容易打出"，"，
// 若只认英文逗号会出现"填了通道却没生效"的困惑。
func splitList(raw string) []string {
	normalized := strings.ReplaceAll(raw, "，", ",")
	parts := strings.Split(normalized, ",")

	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// Setting 表示一条键值设置。
type Setting struct {
	Key       string    // 键
	Value     string    // 值（字符串；复杂类型由上层自行序列化）
	UpdatedAt time.Time // 更新时间
}

// SettingRepository 定义设置的持久化操作。
type SettingRepository interface {
	// Get 读取单个设置；不存在时返回空字符串与 nil（不视为错误）。
	Get(ctx context.Context, key string) (string, error)

	// GetAll 读取全部设置。
	GetAll(ctx context.Context) (map[string]string, error)

	// Set 写入单个设置（存在则覆盖）。
	Set(ctx context.Context, key, value string) error

	// SetMany 批量写入（在单个事务内完成），用于保存整个设置页。
	//
	// 为什么需要事务：设置页一次提交多项，若中途失败只写入一半，
	// 会出现"注册开关已改、默认额度未改"这类难以察觉的不一致状态。
	SetMany(ctx context.Context, values map[string]string) error
}
