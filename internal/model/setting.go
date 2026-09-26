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

	return settings, nil
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
