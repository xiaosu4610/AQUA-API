// 本文件定义站点 SEO（搜索引擎优化）相关模型与工具函数。
//
// 意图（Why）：
//
//	站点需要在搜索引擎里被正确收录：需要域名、关键词、站长验证码与 Geo 信息。
//	这些参数由站长在后台配置，后端据此动态生成 sitemap.xml / robots.txt
//	并向 SPA 首页注入 meta 标签。把它们收敛在 model 层，
//	是为了让"默认值、类型转换、地址规范化"有唯一实现处，server 层只负责组装响应。
//
// 流转（Flow）：
//
//	后台保存：server.handleUpdateSettings → mergeSEOSettings → ToMap → SettingRepository
//	读取合并：setting.LoadSiteSettings → loadSEOSettings → 覆盖默认值
//	生成页面：server.handleSitemap / handleRobots / injectSEOMeta 读取本结构
//
// 扩展（Extend）：
//
//	新增 SEO 字段时，遵循 setting.go 的"三处同步"约定：
//	  1) 在 SEOSettings 加字段；
//	  2) 在 DefaultSiteSettings 补默认值；
//	  3) 在 ToMap 与 loadSEOSettings 补读写映射。
package model

import (
	"strconv"
	"strings"
)

// SEOSettings 是搜索引擎优化相关的站点参数。
type SEOSettings struct {
	// SiteURL 是站点公开访问地址（如 https://aqua.ltzy.top），用于生成绝对链接。
	SiteURL string
	// Keywords 是 SEO 关键词。
	Keywords []string
	// BingVerification 是必应站长验证码（msvalidate.01 的 content）。
	BingVerification string
	// GoogleVerification 是 Google Search Console 验证码。
	GoogleVerification string
	// BaiduVerification 是百度站长验证码。
	BaiduVerification string
	// GeoRegion 是地域代码，如 CN-44（省份）或 US-CA。
	GeoRegion string
	// GeoPlacename 是地名，如 Shenzhen。
	GeoPlacename string
	// GeoPosition 是经纬度，形如 "22.5431;114.0579"。
	GeoPosition string
	// SitemapEnabled 表示是否输出 sitemap.xml 与 robots.txt。
	SitemapEnabled bool
	// SitemapPaths 是额外的公开路径，如 ["/pricing", "/faq"]。
	SitemapPaths []string
}

// NormalizeSiteURL 规范化站点公开地址：去空白、去结尾斜杠、补全协议。
//
// 规则：
//   - 去首尾空白；空串返回空串；
//   - 以 http:// 或 https:// 开头则保留，仅去掉结尾的 "/"；
//   - 否则视为裸域名，补 https:// 前缀并去掉结尾的 "/"。
//
// 为什么要补 https：现代部署基本都在 TLS 之后，裸域名按 http 生成会被搜索引擎
// 视为不安全链接；同时统一"有无结尾斜杠"能避免拼出 "//sitemap.xml" 这类脏链接。
func NormalizeSiteURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return strings.TrimRight(trimmed, "/")
	}
	return "https://" + strings.TrimRight(trimmed, "/")
}

// loadSEOSettings 把 KV 中的 SEO 参数合并进强类型结构。
//
// 逐项"存在且解析成功才覆盖"：数据库里被手工写坏的项应回退默认值，
// 而不是让整个后台设置页或 sitemap 生成失败。
func loadSEOSettings(target *SEOSettings, values map[string]string) {
	if v, ok := values[SettingKeySEOSiteURL]; ok && strings.TrimSpace(v) != "" {
		target.SiteURL = NormalizeSiteURL(v)
	}
	if v, ok := values[SettingKeySEOKeywords]; ok {
		// 允许显式清空（空字符串 → 空列表）
		target.Keywords = splitList(v)
	}
	if v, ok := values[SettingKeySEOBingVerification]; ok {
		target.BingVerification = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeySEOGoogleVerification]; ok {
		target.GoogleVerification = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeySEOBaiduVerification]; ok {
		target.BaiduVerification = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeySEOGeoRegion]; ok {
		target.GeoRegion = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeySEOGeoPlacename]; ok {
		target.GeoPlacename = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeySEOGeoPosition]; ok {
		target.GeoPosition = strings.TrimSpace(v)
	}
	if v, ok := values[SettingKeySEOSitemapEnabled]; ok {
		if parsed, err := strconv.ParseBool(v); err == nil {
			target.SitemapEnabled = parsed
		}
	}
	if v, ok := values[SettingKeySEOSitemapPaths]; ok {
		target.SitemapPaths = splitList(v)
	}
}
