// 本文件实现站点的 SEO 能力：sitemap.xml、robots.txt 与 SPA 首页的 meta 注入。
//
// 意图（Why）：
//
//	中转站类产品的内容页是登录后才可见的，搜索引擎能收录的只有少数公开页
//	（首页、模型广场、注册/登录）。与其让搜索引擎"瞎猜"，不如由后端按后台配置
//	动态生成站点地图与爬虫规则，并把关键词、站长验证码、Geo 信息注入首页——
//	这些都是纯运营参数，站长改完应立即生效，不应依赖重新构建前端。
//
// 流转（Flow）：
//
//	GET /sitemap.xml → handleSitemap → 读设置 → 命中"北京日期"缓存则直接返回，
//	                                        否则重新生成并写入缓存
//	GET /robots.txt  → handleRobots  → 读设置 → 组装固定规则 + Sitemap 行
//	GET 任意页面路径 → static.go 的 SPA 回退 → injectSEOMeta 替换标记块
//	后台改设置       → handler_admin.handleUpdateSettings → invalidateSitemapCache
//
// 扩展（Extend）：
//
//	新增站点地图页面：在 buildSitemap 的固定清单里追加，或让站长填 seo.sitemap_paths；
//	新增 meta 标签：在 buildSEOMetaBlock 里追加一行，空白值会自动跳过。
package server

import (
	"bytes"
	"encoding/xml"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// seoMetaStartMarker / seoMetaEndMarker 是 index.html 中 SEO 块的包裹标记。
//
// 前端在 <head> 内预置这一对注释；后端只替换"标记之间的内容"，标记本身保留，
// 这样下次发版或热更新仍能再次替换。找不到标记时原样返回（见 injectSEOMeta），
// 保证老版本前端产物也能正常服务。
const (
	seoMetaStartMarker = "<!--aqua:seo:start-->"
	seoMetaEndMarker   = "<!--aqua:seo:end-->"
)

// beijingZone 是北京时间的固定时区（UTC+8）。
//
// 刻意使用 FixedZone 而非 time.LoadLocation("Asia/Shanghai")：
// 精简容器常缺 tzdata，LoadLocation 会直接返回错误，导致 sitemap 生成失败。
// 本场景只需固定 +8 偏移，不涉及夏令时，FixedZone 完全够用。
var beijingZone = time.FixedZone("CST", 8*3600)

// beijingDate 返回给定时刻的北京日期（YYYY-MM-DD）。
//
// sitemap 的 lastmod 与缓存键都用它，从而保证"每日北京时间零点自动刷新"。
func beijingDate(t time.Time) string {
	return t.In(beijingZone).Format("2006-01-02")
}

// sitemapCache 缓存"某一天 + 某个基址"下生成的 sitemap.xml 字节。
//
// 设计说明（为什么不用定时器做"每日更新"）：
//
//	lastmod 取的是"北京日期"，缓存键也是北京日期。于是只要跨过北京时间零点，
//	缓存键自然失效、下次请求会重新生成，lastmod 也就变成新的一天。
//	"每日刷新"由日期键天然保证，无需定时器、无需后台协程，也就没有
//	"定时器没起来/没触发"这类隐患。
type sitemapCache struct {
	date  string // 生成时的北京日期（YYYY-MM-DD）
	base  string // 生成时使用的公开基址（便于按 Host 兜底时区分）
	bytes []byte // 已生成的 XML 字节
}

// sitemapEntry 是站点地图中的一个条目。
type sitemapEntry struct {
	Path       string
	Changefreq string
	Priority   string
}

// publicBaseURL 返回站点的公开绝对地址（不含结尾斜杠）。
//
// 优先用后台配置的 seo.site_url；未配置时按请求推导（尊重 X-Forwarded-Proto /
// X-Forwarded-Host），推导结果仅用于本次响应，不落库。
//
// 为什么优先用配置值：反向代理后网关看到的是内网地址，推导出的链接会被搜索引擎
// 拒绝收录，所以显式配置是主路径、推导只是兜底。
func (s *Server) publicBaseURL(c *gin.Context) string {
	settings, err := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings)
	if err != nil {
		// 读取设置失败不应让页面/地图整体不可用，退回按请求推导。
		return deriveBaseURL(c)
	}
	return resolveBaseURL(settings, c)
}

// resolveBaseURL 在已有设置的情况下解析公开基址，避免重复读库。
func resolveBaseURL(settings model.SiteSettings, c *gin.Context) string {
	if configured := model.NormalizeSiteURL(settings.SEO.SiteURL); configured != "" {
		return configured
	}
	return deriveBaseURL(c)
}

// deriveBaseURL 按请求头推导公开基址（不含结尾斜杠）。
func deriveBaseURL(c *gin.Context) string {
	scheme := "http"
	if proto := firstHeaderValue(c.GetHeader("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if c.Request.TLS != nil {
		scheme = "https"
	}

	host := firstHeaderValue(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + strings.TrimRight(strings.TrimSpace(host), "/")
}

// firstHeaderValue 取逗号分隔代理头中的第一个值（代理链会追加多段）。
func firstHeaderValue(raw string) string {
	if raw == "" {
		return ""
	}
	first, _, _ := strings.Cut(raw, ",")
	return strings.TrimSpace(first)
}

// handleSitemap 动态生成并返回 sitemap.xml。
//
// 缓存策略见 sitemapCache 的说明：以"北京日期"为键，跨日自动重生成。
func (s *Server) handleSitemap(c *gin.Context) {
	settings, err := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取系统设置失败")
		return
	}
	// 这是非 API 路径，用朴素 404 而不是 API 错误体：对爬虫更友好。
	if !settings.SEO.SitemapEnabled {
		c.Status(http.StatusNotFound)
		return
	}

	base := resolveBaseURL(settings, c)
	setSEOCacheControl(c)
	if cached, ok := s.cachedSitemap(base); ok {
		c.Data(http.StatusOK, "application/xml; charset=utf-8", cached)
		return
	}

	body := buildSitemap(settings, base)
	s.storeSitemapCache(base, body)
	c.Data(http.StatusOK, "application/xml; charset=utf-8", body)
}

// handleRobots 动态生成并返回 robots.txt。
func (s *Server) handleRobots(c *gin.Context) {
	settings, err := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取系统设置失败")
		return
	}
	if !settings.SEO.SitemapEnabled {
		c.Status(http.StatusNotFound)
		return
	}

	base := resolveBaseURL(settings, c)
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	b.WriteString("Allow: /\n")
	// 接口、登录后页面（/console/ 与 /admin/）对爬虫无价值：
	// 让它们抓只会产生"需要登录"的无效索引，反而稀释站点质量信号。
	b.WriteString("Disallow: /api/\n")
	b.WriteString("Disallow: /console/\n")
	b.WriteString("Disallow: /admin/\n")
	b.WriteString("Sitemap: " + base + "/sitemap.xml\n")

	setSEOCacheControl(c)
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(b.String()))
}

// setSEOCacheControl 给 sitemap.xml / robots.txt 显式声明不缓存。
//
// 为什么必须显式设置（实测教训）：
//
//	Cloudflare 这类 CDN 对部分扩展名（尤其 .txt）有"默认缓存 + 注入
//	Cache-Control: max-age=14400"的行为。站长在后台把域名改掉后，
//	sitemap.xml 会立刻更新，但 robots.txt 仍由边缘节点返回 4 小时前的旧域名，
//	且爬虫拿到的 Sitemap 指向旧地址——排查起来极具迷惑性（源站逻辑其实是对的）。
//	在源站显式声明 no-cache 可覆盖 CDN 的默认缓存策略，保证"后台改完立即生效"。
//	两个端点都由进程内缓存承担重复生成开销，no-cache 不会带来实际性能问题。
func setSEOCacheControl(c *gin.Context) {
	c.Header("Cache-Control", "no-cache")
}

// buildSitemap 组装 sitemap.xml 字节。
//
// lastmod 统一取"北京时间当天"：站点内容由后端动态提供，不便于逐页记录修改时间，
// 统一用当日日期既符合"每天都在更新"的语义，也让缓存能按天刷新。
func buildSitemap(settings model.SiteSettings, base string) []byte {
	lastmod := beijingDate(time.Now())

	entries := []sitemapEntry{
		// 固定公开页：按重要程度给 priority，首页最高。
		{Path: "/", Changefreq: "daily", Priority: "1.0"},
		{Path: "/models", Changefreq: "daily", Priority: "0.9"},
		{Path: "/register", Changefreq: "weekly", Priority: "0.6"},
		{Path: "/login", Changefreq: "monthly", Priority: "0.3"},
	}
	// 站长自定义的额外公开页（如价格页、常见问题页）。
	for _, path := range settings.SEO.SitemapPaths {
		clean := strings.TrimSpace(path)
		if clean == "" {
			continue
		}
		entries = append(entries, sitemapEntry{Path: clean, Changefreq: "weekly", Priority: "0.6"})
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, entry := range entries {
		b.WriteString("  <url>\n")
		b.WriteString("    <loc>" + xmlEscape(base+entry.Path) + "</loc>\n")
		b.WriteString("    <lastmod>" + lastmod + "</lastmod>\n")
		b.WriteString("    <changefreq>" + entry.Changefreq + "</changefreq>\n")
		b.WriteString("    <priority>" + entry.Priority + "</priority>\n")
		b.WriteString("  </url>\n")
	}
	b.WriteString("</urlset>\n")
	return []byte(b.String())
}

// xmlEscape 转义 XML 文本（URL 里可能含 & 等字符，不转义会让文档非法）。
func xmlEscape(raw string) string {
	var buf bytes.Buffer
	// EscapeText 只对已读入的字节写出，写入 bytes.Buffer 不会失败。
	_ = xml.EscapeText(&buf, []byte(raw))
	return buf.String()
}

// cachedSitemap 返回命中的缓存（键 = 北京日期 + 基址）；未命中返回 ok=false。
func (s *Server) cachedSitemap(base string) ([]byte, bool) {
	s.sitemapMu.Lock()
	defer s.sitemapMu.Unlock()

	if s.sitemap.bytes == nil || s.sitemap.date != beijingDate(time.Now()) || s.sitemap.base != base {
		return nil, false
	}
	return s.sitemap.bytes, true
}

// storeSitemapCache 写入缓存，键含当前北京日期与基址。
func (s *Server) storeSitemapCache(base string, body []byte) {
	s.sitemapMu.Lock()
	defer s.sitemapMu.Unlock()

	s.sitemap = sitemapCache{date: beijingDate(time.Now()), base: base, bytes: body}
}

// invalidateSitemapCache 清空缓存。
//
// 后台保存设置后必须调用：否则站长改完域名/路径要等到第二天才生效，
// 这是很容易漏掉的一步。
func (s *Server) invalidateSitemapCache() {
	s.sitemapMu.Lock()
	s.sitemap = sitemapCache{}
	s.sitemapMu.Unlock()
}

// injectSEOMeta 把 SEO 元信息注入 SPA 的 index.html。
//
// 注入方式：用一对标记（<!--aqua:seo:start--> / <!--aqua:seo:end-->）包裹默认块，
// 后端整块替换标记之间的内容，标记本身保留（便于下次再替换）。
// 找不到标记时原样返回：老版本前端产物也能正常服务，不能因此报错或返回空页。
func injectSEOMeta(page []byte, settings model.SiteSettings, baseURL, path string) []byte {
	content := string(page)

	start := strings.Index(content, seoMetaStartMarker)
	if start < 0 {
		return page
	}
	relEnd := strings.Index(content[start:], seoMetaEndMarker)
	if relEnd < 0 {
		return page
	}
	end := start + relEnd

	block := buildSEOMetaBlock(settings, baseURL, path)
	replaced := content[:start+len(seoMetaStartMarker)] + "\n" + block + content[end:]
	return []byte(replaced)
}

// buildSEOMetaBlock 组装 SEO 块（meta 标签 + canonical 链接）。
//
// 值以 html.EscapeString 转义：关键词里出现引号/尖括号时会破坏 HTML 结构甚至造成 XSS，
// 必须在"拼进属性"之前转义。
func buildSEOMetaBlock(settings model.SiteSettings, baseURL, path string) string {
	var b strings.Builder

	meta := func(name, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		b.WriteString(`<meta name="` + name + `" content="` + html.EscapeString(value) + `" />` + "\n")
	}

	if keywords := strings.Join(settings.SEO.Keywords, ","); strings.TrimSpace(keywords) != "" {
		meta("keywords", keywords)
	}
	meta("msvalidate.01", settings.SEO.BingVerification)
	meta("google-site-verification", settings.SEO.GoogleVerification)
	meta("baidu-site-verification", settings.SEO.BaiduVerification)
	meta("geo.region", settings.SEO.GeoRegion)
	meta("geo.placename", settings.SEO.GeoPlacename)

	// 经纬度：geo.position 用分号，ICBM 用逗号（Google 抓取 ICBM 格式）。
	if position := strings.TrimSpace(settings.SEO.GeoPosition); position != "" {
		meta("geo.position", position)
		meta("ICBM", strings.Replace(position, ";", ", ", 1))
	}

	canonical := strings.TrimRight(baseURL, "/") + path
	b.WriteString(`<link rel="canonical" href="` + html.EscapeString(canonical) + `" />` + "\n")

	return b.String()
}
