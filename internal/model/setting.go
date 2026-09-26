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
	"encoding/json"
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
	//
	// Deprecated: 新代码请用 PaymentSettings.Params（键 "stripe.note"）。
	// 保留它只为读旧库时能把历史配置迁移进 Params，不再写入。
	SettingKeyPaymentStripePriceNote = "payment_stripe_note"
	// SettingKeyPaymentParams 各支付通道的通道级参数（JSON 对象）。
	//
	// 键名形如 "<通道>.<字段>"，例如：
	//
	//	{"epay.gateway":"https://pay.example.com","epay.pid":"1001","stripe.note":"充值"}
	//
	// 为什么用一个 JSON 键而不是每项一个设置键：
	//	支付通道会不断增加（易支付/Stripe/支付宝/微信/PayPal…），
	//	每个通道又有若干字段；若逐项建键，则每加一个通道都要动设置表键名、
	//	加载映射与校验三处代码。用一个 JSON 键承载，新增通道只需在通道注册表里
	//	声明字段（见 internal/payment 的通道注册表），存取逻辑完全复用。
	SettingKeyPaymentParams = "payment_params"

	// ── SEO / 搜索引擎优化（M6）──────────────────────────────────
	//
	// 这些是"非密钥"的运营参数：站点公开地址、关键词、各站长平台验证码、Geo 信息。
	// 后端据此动态生成 sitemap.xml / robots.txt 并向 SPA 首页注入 meta 标签，
	// 让站点能被搜索引擎正确收录并展示地域信息。

	// SettingKeySEOSiteURL 站点公开访问地址（如 https://aqua.ltzy.top）。
	//
	// 用于生成 sitemap 的绝对链接与 canonical。留空时按请求推导，
	// 但反向代理后推导出的可能是内网地址，因此生产环境建议显式配置。
	SettingKeySEOSiteURL = "seo_site_url"
	// SettingKeySEOKeywords 搜索引擎关键词，逗号分隔。
	SettingKeySEOKeywords = "seo_keywords"
	// SettingKeySEOBingVerification 必应站长验证码（msvalidate.01 的 content）。
	SettingKeySEOBingVerification = "seo_bing_verification"
	// SettingKeySEOGoogleVerification Google Search Console 验证码。
	SettingKeySEOGoogleVerification = "seo_google_verification"
	// SettingKeySEOBaiduVerification 百度站长验证码。
	SettingKeySEOBaiduVerification = "seo_baidu_verification"
	// SettingKeySEOGeoRegion 地域代码，如 CN-44（省份）或 US-CA。
	SettingKeySEOGeoRegion = "seo_geo_region"
	// SettingKeySEOGeoPlacename 地名，如 Shenzhen。
	SettingKeySEOGeoPlacename = "seo_geo_placename"
	// SettingKeySEOGeoPosition 经纬度，形如 "22.5431;114.0579"。
	SettingKeySEOGeoPosition = "seo_geo_position"
	// SettingKeySEOSitemapEnabled 是否输出 sitemap.xml 与 robots.txt（"true" / "false"）。
	SettingKeySEOSitemapEnabled = "seo_sitemap_enabled"
	// SettingKeySEOSitemapPaths 额外的公开路径，逗号分隔（如 "/pricing,/faq"）。
	SettingKeySEOSitemapPaths = "seo_sitemap_paths"
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

	// SEO 是搜索引擎优化相关的运营参数（域名、关键词、站长验证码、Geo 信息）。
	SEO SEOSettings
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
	// Params 是各支付通道的通道级参数，键形如 "<通道>.<字段>"。
	//
	// 这是"支持任意多种支付通道"的关键：通道与其字段由通道注册表声明
	// （见 internal/payment），存取值统一走 Params，因此新增一个支付通道
	// 不需要改动设置模型、设置表结构或存取代码。
	//
	// 约定：这里只放【非密钥】参数（网关地址、商户号、商品名等）。
	// 密钥（商户密钥、Secret Key、API v3 密钥…）一律走环境变量，
	// 因为设置表会随数据库备份/导出（见本文件头部的安全约束）。
	Params map[string]string

	// EPayGateway / EPayPID / EPayTypes 是易支付通道的历史字段。
	//
	// Deprecated: 已被 Params（"epay.gateway" / "epay.pid" / "epay.types"）取代。
	// 保留它们只为一件事：读旧库时把历史配置迁移进 Params，避免升级后配置"看起来丢了"。
	// 写入路径不再更新这三个字段。
	EPayGateway string
	EPayPID     string
	EPayTypes   []string
	// StripeNote 是 Stripe 结账页显示的商品名。
	//
	// Deprecated: 已被 Params 的 "stripe.note" 取代，保留原因同上。
	StripeNote string
}

// Param 读取某个支付通道的某个字段值。
//
// 取值顺序：Params → 历史固定字段 → 空串。
// 为什么要带历史回退：升级到 Params 之前配置过易支付/Stripe 的站点，
// 其值只存在于旧字段里；回退能保证"升级后配置仍然生效"，
// 而不是让站长以为得重新填一遍。
func (p PaymentSettings) Param(channel, field string) string {
	if v, ok := p.Params[channel+"."+field]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	switch channel + "." + field {
	case "epay.gateway":
		return strings.TrimSpace(p.EPayGateway)
	case "epay.pid":
		return strings.TrimSpace(p.EPayPID)
	case "epay.types":
		return strings.Join(p.EPayTypes, ",")
	case "stripe.note":
		return strings.TrimSpace(p.StripeNote)
	}
	return ""
}

// ParamList 读取一个逗号分隔的列表型字段（自动去掉空白项）。
func (p PaymentSettings) ParamList(channel, field string) []string {
	return splitList(p.Param(channel, field))
}

// ParamKeys 返回当前已填写过值的参数键（形如 "epay.pid"），供后台展示"哪些通道已配置"。
func (p PaymentSettings) ParamKeys() []string {
	keys := make([]string, 0, len(p.Params))
	for key, value := range p.Params {
		if strings.TrimSpace(value) != "" {
			keys = append(keys, key)
		}
	}
	return keys
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
		// 新用户默认额度：默认【不限】。
		//
		// 为什么不是 0（曾经就是 0，并因此引发过一次线上事故）：
		//   本站的典型用法是"免费开放给朋友用 / 自用"，上游成本由站长自己承担。
		//   默认给 0 等于"注册了也不能用"，而故障现象是【全站 429】——
		//   且这些 429 由网关在鉴权阶段以微秒级返回（根本没走到上游），
		//   站长极易误判成"上游在限流"，极难自查。
		//   需要限额的站点在后台把这一项改成具体数值即可（改完立即生效）。
		DefaultUserQuota: QuotaUnlimited,
		DefaultGroup:     "default",
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
		// SEO 默认值：开箱即用——默认开启站点地图，并内置项目官方站点的必应收录码。
		SEO: SEOSettings{
			// SiteURL 默认留空：留空时按请求推导（见 server.publicBaseURL）。
			// 反向代理后建议在后台显式配置为公开域名，否则推导出的可能是内网地址。
			SiteURL: "",
			// Keywords 默认给一组贴合本站定位的中文关键词。
			Keywords: []string{"LLM API 网关", "大模型中转", "OpenAI 兼容", "Anthropic", "自托管"},
			// BingVerification 默认填项目官方站点的必应收录码。
			//
			// 说明：这是项目官方站点的默认收录码，自建用户可在后台改成自己的。
			// 只有改成自己域名对应的收录码，必应才会认可该站点的验证。
			BingVerification: "1B0EEE739DC3DB2ACD026924B711EC01",
			// SitemapEnabled 默认开启：让搜索引擎自动发现站点页面，无需站长手动配置。
			SitemapEnabled: true,
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
		SettingKeyPaymentParams:          marshalPaymentParams(s.Payment.Params),
		SettingKeyPaymentEPayGateway:     s.Payment.EPayGateway,
		SettingKeyPaymentEPayPID:         s.Payment.EPayPID,
		SettingKeyPaymentEPayTypes:       strings.Join(s.Payment.EPayTypes, ","),
		SettingKeyPaymentStripePriceNote: s.Payment.StripeNote,

		SettingKeySEOSiteURL:            s.SEO.SiteURL,
		SettingKeySEOKeywords:           strings.Join(s.SEO.Keywords, ","),
		SettingKeySEOBingVerification:   s.SEO.BingVerification,
		SettingKeySEOGoogleVerification: s.SEO.GoogleVerification,
		SettingKeySEOBaiduVerification:  s.SEO.BaiduVerification,
		SettingKeySEOGeoRegion:          s.SEO.GeoRegion,
		SettingKeySEOGeoPlacename:       s.SEO.GeoPlacename,
		SettingKeySEOGeoPosition:        s.SEO.GeoPosition,
		SettingKeySEOSitemapEnabled:     strconv.FormatBool(s.SEO.SitemapEnabled),
		SettingKeySEOSitemapPaths:       strings.Join(s.SEO.SitemapPaths, ","),
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
	loadSEOSettings(&settings.SEO, values)

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
	// 通道级参数（新）：解析 JSON 后再把历史字段迁移进来。
	if v, ok := values[SettingKeyPaymentParams]; ok && strings.TrimSpace(v) != "" {
		parsed := map[string]string{}
		// 解析失败不报错：宁可用历史字段或默认值，也不要让整页设置加载失败
		//（一个被手工写坏的 JSON 不该导致站点配置页打不开）。
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			target.Params = parsed
		}
	}
	seedLegacyPaymentParams(target)
}

// marshalPaymentParams 把通道级参数序列化为 JSON。
//
// 空 map 也返回 "{}" 而不是 ""：让"已被显式清空"与"从未配置"在库里可区分，
// 便于排查"配置是不是被谁清掉了"。
func marshalPaymentParams(params map[string]string) string {
	if len(params) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(params)
	if err != nil {
		// map[string]string 理论上不会序列化失败；真失败时回退空对象，
		// 避免把不可用的值写进库。
		return "{}"
	}
	return string(raw)
}

// legacyPaymentParamKeys 是"历史固定字段 → 新 Params 键"的迁移映射。
var legacyPaymentParamKeys = []string{
	"epay.gateway", "epay.pid", "epay.types", "stripe.note",
}

// seedLegacyPaymentParams 把历史固定字段的值补进 Params（仅在 Params 缺该键时）。
//
// 为什么只在缺键时补：Params 是新代码的唯一写入路径，若它已有值，
// 说明站长已在新界面配置过，此时再用历史值覆盖会"回滚"他的修改。
func seedLegacyPaymentParams(target *PaymentSettings) {
	if target.Params == nil {
		target.Params = make(map[string]string)
	}
	for _, key := range legacyPaymentParamKeys {
		if strings.TrimSpace(target.Params[key]) != "" {
			continue
		}
		channel, field, found := strings.Cut(key, ".")
		if !found {
			continue
		}
		// Param 已包含"Params → 历史字段"的回退逻辑；此处 Params 缺该键，
		// 因此取到的必然是历史字段值（没有则为空串，会被下面的判断跳过）。
		if value := target.Param(channel, field); value != "" {
			target.Params[key] = value
		}
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
