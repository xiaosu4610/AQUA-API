// SEO 能力（sitemap.xml / robots.txt / index.html 的 meta 注入）的单元测试。
//
// 意图（Why）：
//
//	这些输出直接面向搜索引擎，一旦格式非法（XML 转义遗漏）或路由被 SPA 回退吞掉，
//	收录就会静默失效——问题很难在浏览器里被肉眼发现。因此用测试锁定：
//	XML 可解析、lastmod 为北京当天、开关关闭时 404、meta 注入的标记替换与转义。
//
// 流转（Flow）：
//
//	go test ./internal/server/ → httptest 直接调用 Handler，无需真实监听端口
//
// 扩展（Extend）：
//
//	新增 SEO 输出项时，在对应用例里补断言；端到端用例用内存文件系统模拟前端产物。
package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// seoTestSettingsRepo 是内存版设置仓储，供 SEO 测试注入自定义配置。
type seoTestSettingsRepo struct{ values map[string]string }

func (r *seoTestSettingsRepo) Get(_ context.Context, key string) (string, error) {
	return r.values[key], nil
}

func (r *seoTestSettingsRepo) GetAll(context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

func (r *seoTestSettingsRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *seoTestSettingsRepo) SetMany(ctx context.Context, values map[string]string) error {
	for k, v := range values {
		if err := r.Set(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

// newSEOServer 构造一个只注入必要依赖的测试服务。
//
// 说明：SEO 路由只依赖设置仓储与（可选的）前端文件系统，无需数据库，
// 因此这里用最小的 Deps 装配，避免为纯函数式输出拉入整套存储。
// fsys 为 nil 时不托管前端（仅用于测试 sitemap / robots 路由）。
func newSEOServer(t *testing.T, values map[string]string, fsys fs.FS) (*Server, *seoTestSettingsRepo) {
	t.Helper()

	repo := &seoTestSettingsRepo{values: values}
	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	srv := New(Deps{Config: cfg, Settings: repo, WebFS: fsys})
	return srv, repo
}

// getSEOResponse 发送一次 GET 请求并返回响应记录器。
func getSEOResponse(srv *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "fallback.example.com"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// sitemapDoc 是 sitemap.xml 的解析结构（仅取测试需要的字段）。
type sitemapDoc struct {
	XMLName xml.Name `xml:"urlset"`
	URLs    []struct {
		Loc        string `xml:"loc"`
		Lastmod    string `xml:"lastmod"`
		Changefreq string `xml:"changefreq"`
		Priority   string `xml:"priority"`
	} `xml:"url"`
}

// TestSitemap_可解析且lastmod为北京当天 验证 sitemap 的格式与内容。
func TestSitemap_可解析且lastmod为北京当天(t *testing.T) {
	srv, _ := newSEOServer(t, map[string]string{
		model.SettingKeySEOSiteURL:      "https://aqua.example.com/",
		model.SettingKeySEOSitemapPaths: "/pricing,/faq",
	}, nil)

	rec := getSEOResponse(srv, "/sitemap.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Fatalf("Content-Type = %q，期望 application/xml", ct)
	}
	// 与 robots.txt 同理：显式 no-cache，避免 CDN 用旧域名缓存住站点地图。
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q，期望 no-cache", cc)
	}

	var doc sitemapDoc
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("sitemap 无法被解析（XML 非法？）: %v\n%s", err, rec.Body.String())
	}

	// 4 个固定公开页 + 2 个额外路径
	if len(doc.URLs) != 6 {
		t.Fatalf("条目数 = %d，期望 6（4 固定 + 2 额外）", len(doc.URLs))
	}

	// lastmod 必须等于"北京时间当天"：测试用同样的固定 +8 时区算期望值，
	// 避免依赖运行机器所在时区。
	wantDate := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
	for _, u := range doc.URLs {
		if u.Lastmod != wantDate {
			t.Errorf("lastmod = %q，期望北京时间当天 %q", u.Lastmod, wantDate)
		}
	}

	locs := make(map[string]bool, len(doc.URLs))
	for _, u := range doc.URLs {
		locs[u.Loc] = true
		// 去掉协议前缀后不应再出现 "//"（避免 base 带尾斜杠拼出脏链接）
		path := strings.TrimPrefix(strings.TrimPrefix(u.Loc, "https://"), "http://")
		if strings.Contains(path, "//") {
			t.Errorf("loc 在协议之后出现多余斜杠: %q", u.Loc)
		}
		if u.Loc != "https://aqua.example.com/" && strings.HasSuffix(u.Loc, "/") {
			t.Errorf("非首页 loc 不应以 / 结尾: %q", u.Loc)
		}
	}

	// 配置的 SiteURL 必须作为绝对地址出现在 loc 中
	for _, want := range []string{
		"https://aqua.example.com/",
		"https://aqua.example.com/models",
		"https://aqua.example.com/pricing",
		"https://aqua.example.com/faq",
	} {
		if !locs[want] {
			t.Errorf("sitemap 缺少条目 %q", want)
		}
	}
}

// TestSitemap_同日命中缓存且可被清空 验证缓存策略与失效。
func TestSitemap_同日命中缓存且可被清空(t *testing.T) {
	srv, _ := newSEOServer(t, map[string]string{
		model.SettingKeySEOSiteURL: "https://aqua.example.com",
	}, nil)

	first := getSEOResponse(srv, "/sitemap.xml")
	second := getSEOResponse(srv, "/sitemap.xml")

	if first.Body.String() != second.Body.String() {
		t.Fatal("同一天两次请求的 sitemap 应完全一致（命中缓存）")
	}
	// 第二次请求应直接命中缓存：缓存中已存在本次内容。
	cached, ok := srv.cachedSitemap("https://aqua.example.com")
	if !ok {
		t.Fatal("首次请求后应存在可按同一基址命中的缓存")
	}
	if string(cached) != first.Body.String() {
		t.Fatal("缓存内容与响应内容不一致")
	}

	// 改配置后必须能立即失效（否则要等第二天才生效）。
	srv.invalidateSitemapCache()
	if _, ok := srv.cachedSitemap("https://aqua.example.com"); ok {
		t.Fatal("清空缓存后不应再命中")
	}
}

// TestSitemap与Robots_开关关闭时返回404 验证 sitemap_enabled 的行为。
func TestSitemap与Robots_开关关闭时返回404(t *testing.T) {
	srv, _ := newSEOServer(t, map[string]string{
		model.SettingKeySEOSitemapEnabled: "false",
		model.SettingKeySEOSiteURL:        "https://aqua.example.com",
	}, nil)

	for _, path := range []string{"/sitemap.xml", "/robots.txt"} {
		if rec := getSEOResponse(srv, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s 在关闭时状态码 = %d，期望 404", path, rec.Code)
		}
	}
}

// TestRobots_内容含Sitemap与Disallow 验证爬虫规则内容。
func TestRobots_内容含Sitemap与Disallow(t *testing.T) {
	srv, _ := newSEOServer(t, map[string]string{
		model.SettingKeySEOSiteURL: "https://aqua.example.com/",
	}, nil)

	rec := getSEOResponse(srv, "/robots.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q，期望 text/plain", ct)
	}
	// 必须显式声明 no-cache：否则 CDN（如 Cloudflare）会对 .txt 施加默认缓存，
	// 站长改完域名后 robots.txt 仍返回旧域名的 Sitemap 地址。
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q，期望 no-cache", cc)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"User-agent: *",
		"Allow: /",
		"Disallow: /api/",
		"Disallow: /console/",
		"Disallow: /admin/",
		"Sitemap: https://aqua.example.com/sitemap.xml",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("robots.txt 缺少 %q\n%s", want, body)
		}
	}
}

// TestInjectSEOMeta_标签齐全 验证各 meta 标签与 canonical 的生成。
func TestInjectSEOMeta_标签齐全(t *testing.T) {
	settings := model.DefaultSiteSettings()
	settings.SEO.GoogleVerification = "google-verify"
	settings.SEO.BaiduVerification = "baidu-verify"
	settings.SEO.GeoRegion = "CN-44"
	settings.SEO.GeoPlacename = "Shenzhen"
	settings.SEO.GeoPosition = "22.5431;114.0579"

	page := []byte("<html><head>\n<!--aqua:seo:start-->\n<!--aqua:seo:end-->\n</head></html>")
	out := string(injectSEOMeta(page, settings, "https://aqua.example.com/", "/pricing"))

	for _, want := range []string{
		// 默认内置的必应收录码
		`<meta name="msvalidate.01" content="1B0EEE739DC3DB2ACD026924B711EC01" />`,
		`<meta name="keywords" content="LLM API 网关,大模型中转,OpenAI 兼容,Anthropic,自托管" />`,
		`<meta name="google-site-verification" content="google-verify" />`,
		`<meta name="baidu-site-verification" content="baidu-verify" />`,
		`<meta name="geo.region" content="CN-44" />`,
		`<meta name="geo.placename" content="Shenzhen" />`,
		`<meta name="geo.position" content="22.5431;114.0579" />`,
		// ICBM 用逗号分隔
		`<meta name="ICBM" content="22.5431, 114.0579" />`,
		`<link rel="canonical" href="https://aqua.example.com/pricing" />`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("注入结果缺少 %q\n%s", want, out)
		}
	}

	// 标记本身必须保留，便于下次再替换
	if !strings.Contains(out, "<!--aqua:seo:start-->") || !strings.Contains(out, "<!--aqua:seo:end-->") {
		t.Errorf("标记应被保留\n%s", out)
	}
}

// TestInjectSEOMeta_缺失标记原样返回 验证老版本前端产物不会报错或空白。
func TestInjectSEOMeta_缺失标记原样返回(t *testing.T) {
	original := []byte("<html><head></head><body>no markers</body></html>")

	out := injectSEOMeta(original, model.DefaultSiteSettings(), "https://aqua.example.com", "/")

	if string(out) != string(original) {
		t.Fatalf("缺失标记时应原样返回，实际: %s", out)
	}
}

// TestInjectSEOMeta_关键词引号被转义 验证注入不会破坏 HTML 结构（防 XSS）。
func TestInjectSEOMeta_关键词引号被转义(t *testing.T) {
	settings := model.DefaultSiteSettings()
	settings.SEO.Keywords = []string{`a"b`}

	page := []byte(`<head><!--aqua:seo:start--><!--aqua:seo:end--></head>`)
	out := string(injectSEOMeta(page, settings, "https://aqua.example.com", "/"))

	if !strings.Contains(out, "&#34;") {
		t.Errorf("引号应被 HTML 转义\n%s", out)
	}
	// 不能出现未转义的裸引号破坏属性
	if strings.Contains(out, `content="a"b`) {
		t.Errorf("关键词中的引号未被转义，会破坏 HTML 结构\n%s", out)
	}
}

// TestMergeSEOSettings_校验 覆盖后台写入的校验分支。
func TestMergeSEOSettings_校验(t *testing.T) {
	base := model.DefaultSiteSettings()

	// 非法经纬度（缺分隔符）
	badPosition := "123"
	if err := mergeSEOSettings(&base.SEO, &seoSettingsDTO{GeoPosition: &badPosition}); err == nil {
		t.Error("缺少分隔符的经纬度应被拒绝")
	}
	// 经纬度含非数字
	nonNumeric := "abc;114.0"
	if err := mergeSEOSettings(&base.SEO, &seoSettingsDTO{GeoPosition: &nonNumeric}); err == nil {
		t.Error("非数字经纬度应被拒绝")
	}

	// 额外路径必须 / 开头
	badPaths := []string{"pricing"}
	if err := mergeSEOSettings(&base.SEO, &seoSettingsDTO{SitemapPaths: &badPaths}); err == nil {
		t.Error("未以 / 开头的额外路径应被拒绝")
	}

	// 数量超限
	many := make([]string, maxSEOListItems+1)
	for i := range many {
		many[i] = fmt.Sprintf("/page-%d", i)
	}
	if err := mergeSEOSettings(&base.SEO, &seoSettingsDTO{SitemapPaths: &many}); err == nil {
		t.Error("超过数量上限的额外路径应被拒绝")
	}

	// 合法：site_url 被规范化；关键词去重去空
	siteURL := "aqua.ltzy.top/"
	keywords := []string{"网关", "网关", "  ", "自托管"}
	if err := mergeSEOSettings(&base.SEO, &seoSettingsDTO{SiteURL: &siteURL, Keywords: &keywords}); err != nil {
		t.Fatalf("合法输入不应报错: %v", err)
	}
	if base.SEO.SiteURL != "https://aqua.ltzy.top" {
		t.Errorf("site_url 应被规范化，实际 %q", base.SEO.SiteURL)
	}
	if len(base.SEO.Keywords) != 2 {
		t.Errorf("关键词应去重去空后剩 2 项，实际 %+v", base.SEO.Keywords)
	}
}

// TestSPA回退注入SEO元信息 是端到端用例：请求一个未命中静态文件的页面路径，
// 断言返回的 HTML 里已注入 meta。
//
// 说明：仓库内 web/dist 目前只有 PLACEHOLDER.txt（前端产物不入库），
// 无法直接用真实嵌入产物做端到端测试。因此这里用内存文件系统
// （testing/fstest）提供一份带标记的 index.html —— Deps.WebFS 刻意声明为 fs.FS
// 正是为了便于测试注入内存文件系统（见 server.go 的注释）。
func TestSPA回退注入SEO元信息(t *testing.T) {
	web := fstest.MapFS{
		"web/dist/index.html": &fstest.MapFile{Data: []byte(
			"<!DOCTYPE html><html><head>\n<!--aqua:seo:start-->\n<!--aqua:seo:end-->\n</head><body></body></html>")},
	}
	srv, _ := newSEOServer(t, map[string]string{
		model.SettingKeySEOSiteURL: "https://aqua.example.com",
	}, web)

	rec := getSEOResponse(srv, "/pricing")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（走 SPA 回退）", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `<meta name="msvalidate.01"`) {
		t.Errorf("SPA 页面未注入必应验证码\n%s", body)
	}
	// canonical 必须是"当前请求路径"，而不是写死的首页
	if !strings.Contains(body, `href="https://aqua.example.com/pricing"`) {
		t.Errorf("canonical 应指向当前路径 /pricing\n%s", body)
	}
}
