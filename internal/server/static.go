// 本文件实现前端静态资源的托管与 SPA 路由回退。
//
// 意图（Why）：
//
//	本项目把前端构建产物嵌入二进制并由 Go 直接提供，好处是部署只有一个文件、
//	不存在"静态资源与后端版本不匹配"的问题。
//	由于前端使用 history 路由（如 /admin/channels），必须实现 SPA 回退：
//	未命中静态文件的路径要返回 index.html，否则用户直接访问或刷新这些地址会得到 404。
//
// 流转（Flow）：
//
//	GET /assets/*      → 直接返回嵌入的静态文件（带长缓存头）
//	GET /favicon.ico   → 返回嵌入的图标
//	其他未命中路径      → 返回 index.html（SPA 回退）
//	/api/*、/v1/*      → 不回退，仍返回 404/405（避免把接口 404 变成 HTML）
//
// 扩展（Extend）：
//
//	新增静态资源目录（如 /static/*）时：在 registerStaticRoutes 中追加一条路由，
//	并确认其前缀不在"不回退"列表中。
package server

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// 静态资源缓存时长。
//
// 为什么可以设一年：前端产物文件名带内容哈希（如 index-DzqYykNe.js），
// 内容变化必然导致文件名变化，因此可以放心地让浏览器长期缓存。
// index.html 本身不缓存（见下），保证发版后用户能立即拿到新的资源引用。
const assetCacheControl = "public, max-age=31536000, immutable"

// distRoot 是嵌入文件系统内前端产物的根目录名。
const distRoot = "web/dist"

// indexFileName 是 SPA 入口文件名。
const indexFileName = "index.html"

// registerStaticRoutes 注册前端静态资源与 SPA 回退。
//
// 参数 fsys 为整个嵌入文件系统（通常是根包的 WebDist）；
// 若为 nil 或其中不含前端产物，则只在访问页面时给出明确提示，不影响接口可用。
func (s *Server) registerStaticRoutes(fsys fs.FS) {
	dist, err := fs.Sub(fsys, distRoot)
	if err != nil {
		// 嵌入路径异常属于构建配置问题，记录后不注册静态路由：
		// 接口仍可用，便于在浏览器之外排查。
		// TODO(server): 接入结构化日志后记录 err
		return
	}

	fileServer := http.FileServer(http.FS(dist))

	// 静态资源：交给标准库文件服务器，并在响应前补上长缓存头
	s.engine.GET("/assets/*filepath", func(c *gin.Context) {
		c.Header("Cache-Control", assetCacheControl)
		fileServer.ServeHTTP(c.Writer, c.Request)
	})
	s.engine.GET("/favicon.ico", gin.WrapH(fileServer))

	// SPA 回退：所有未匹配的 GET 请求返回 index.html
	s.engine.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path

		// 接口路径不做回退：否则前端请求打错地址时会收到 HTML，
		// 报错信息会变成"Unexpected token < in JSON"这类令人困惑的解析错误。
		if isAPIPath(path) {
			if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodPost {
				oai.WriteError(c.Writer, http.StatusNotFound,
					"接口不存在", oai.TypeInvalidRequest, "endpoint_not_found")
				return
			}
			c.Status(http.StatusMethodNotAllowed)
			return
		}

		// 非 GET 的页面请求无意义（前端页面只通过 GET 访问）
		if c.Request.Method != http.MethodGet {
			c.Status(http.StatusMethodNotAllowed)
			return
		}

		index, err := fs.ReadFile(dist, indexFileName)
		if err != nil {
			// 前端未构建：给出可操作的提示，而不是空白页或 500
			c.String(http.StatusServiceUnavailable,
				"前端尚未构建。请先执行：cd web && npm install && npm run build，然后重新编译后端。")
			return
		}

		// 注入 SEO 元信息（关键词、站长验证码、canonical 等）。
		// 静态资源（/assets/*）走上面的专用路由，这里是"未命中静态文件的页面路径"，
		// 首页 "/" 也走这条路径，因此注入能覆盖全部页面。
		// 读设置失败时跳过注入、照常返回页面：SEO 是增强项，不应让页面打不开。
		if settings, loadErr := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings); loadErr == nil {
			base := resolveBaseURL(settings, c)
			index = injectSEOMeta(index, settings, base, path)
		}

		// index.html 自身不做缓存：它引用的是带哈希的资源文件名，
		// 若缓存了旧版 index.html，发版后用户会继续请求已不存在的旧资源。
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	})
}

// isAPIPath 判断路径是否属于后端接口（这些路径不应回退到前端页面）。
func isAPIPath(path string) bool {
	prefixes := []string{"/api/", "/v1/", "/v1beta/", "/healthz", "/readyz"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	// 精确匹配接口根路径（如 /api 本身）
	return path == "/api" || path == "/v1"
}
