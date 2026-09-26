// 本文件集中注册所有 HTTP 路由。
//
// 意图（Why）：
//
//	把「有哪些接口、分别挂在什么路径、需要什么权限」集中在一处，便于通读与审计。
//	权限通过路由分组表达（公开 / 需登录 / 需管理员），而不是散落在各处理器内部判断——
//	后者极易漏判，产生越权漏洞。
//
// 流转（Flow）：
//
//	server.New() → registerRoutes() → engine 持有全部路由
//	  ├─ /healthz         运维探活（无需鉴权）
//	  ├─ /api 公开接口     站点信息、注册、登录、邮箱验证码
//	  ├─ /api 需登录       当前用户、退出、用户门户
//	  ├─ /api/admin 需管理员 仪表盘、渠道、令牌、用户、日志、设置
//	  └─ /v1 模型接口      访问令牌鉴权后转发到上游
//	未匹配的路径由 static.go 处理（SPA 回退）
//
// 扩展（Extend）：
//
//	新增接口时按用途挂到对应分组；若新增了权限级别，请新建分组并挂载对应中间件，
//	不要在处理器内写角色判断。
package server

import (
	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
)

// registerRoutes 注册全部路由。
func (s *Server) registerRoutes() {
	r := s.engine

	// ── 运维探活（无需鉴权）──────────────────────────────────────
	// 仅保留健康检查：站点首页由前端页面承载（见 static.go 的 SPA 回退）。
	r.GET("/healthz", s.handleHealthz)

	// ── 公开接口（无需登录）──────────────────────────────────────
	api := r.Group("/api")

	// 站点信息：落地页与登录页靠它渲染站点名称、注册开关与可用模型
	api.GET("/status", s.handleSiteStatus)

	// 登录与注册叠加频率限制。
	//
	// 为什么单独给这两个接口限流：口令校验（bcrypt）是刻意昂贵的操作，
	// 不限流时攻击者可用少量并发请求打满 CPU（生产实例仅 2 核且与转发共享 CPU）。
	// 注意中间件顺序：限流在业务处理器之前，避免昂贵操作先被执行。
	authLimit := s.loginLimiter.Middleware(middleware.ClientIP)
	api.POST("/auth/register", authLimit, s.handleRegister)
	api.POST("/auth/login", authLimit, s.handleLogin)
	// 发送注册邮箱验证码。同样叠加限流：该接口会触发真实发信（有成本），
	// 且是"把本站当邮件轰炸机"的直接入口。
	api.POST("/auth/email-code", authLimit, s.handleSendEmailCode)

	// ── 需登录（网站会话）────────────────────────────────────────
	authed := api.Group("")
	authed.Use(middleware.SessionAuth(s.deps.Sessions, s.deps.Users))

	authed.GET("/auth/me", s.handleMe)
	authed.POST("/auth/logout", s.handleLogout)

	// ── 用户门户（仅能操作自己的资源）────────────────────────────
	//
	// 归属校验在处理器内通过 ownerID 强制约束（见 handler_token.go），
	// 无论请求体传什么 user_id 都会被忽略。
	portal := authed.Group("/user")
	portal.GET("/tokens", s.handleMyListTokens)
	portal.POST("/tokens", s.handleMyCreateToken)
	portal.PATCH("/tokens/:id", s.handleMyUpdateToken)
	portal.DELETE("/tokens/:id", s.handleMyDeleteToken)
	portal.GET("/usage", s.handleMyUsage)
	portal.GET("/logs", s.handleMyLogs)

	// ── 管理后台（需管理员）──────────────────────────────────────
	admin := authed.Group("/admin")
	admin.Use(middleware.RequireAdmin())

	admin.GET("/dashboard", s.handleDashboard)

	admin.GET("/channels", s.handleListChannels)
	admin.POST("/channels", s.handleCreateChannel)
	admin.GET("/channels/:id", s.handleGetChannel)
	admin.PUT("/channels/:id", s.handleUpdateChannel)
	admin.DELETE("/channels/:id", s.handleDeleteChannel)
	admin.POST("/channels/:id/test", s.handleTestChannel)
	// 密钥池明细与单把密钥的状态管理
	admin.GET("/channels/:id/keys", s.handleListChannelKeys)
	admin.PUT("/keys/:keyId", s.handleUpdateChannelKeyStatus)

	// 从上游拉取模型列表。
	//
	// 刻意不挂在 /channels/ 之下：gin 的路由树中 `/channels/fetch-models`
	// 会与已有的 `/channels/:id` 在同一层级产生静态段/参数段冲突，
	// 放在顶层既避免冲突，也更贴合它的语义——它可能被用于"尚未保存的渠道"。
	admin.POST("/fetch-models", s.handleFetchModels)

	admin.GET("/tokens", s.handleAdminListTokens)
	admin.POST("/tokens", s.handleAdminCreateToken)
	admin.PUT("/tokens/:id", s.handleAdminUpdateToken)
	admin.DELETE("/tokens/:id", s.handleAdminDeleteToken)

	admin.GET("/users", s.handleListUsers)
	admin.POST("/users", s.handleCreateUser)
	admin.PUT("/users/:id", s.handleUpdateUser)
	admin.DELETE("/users/:id", s.handleDeleteUser)

	admin.GET("/logs", s.handleAdminListLogs)

	// 模型计价规则（用量 → 费用的换算依据）
	admin.GET("/prices", s.handleListPrices)
	admin.POST("/prices", s.handleCreatePrice)
	admin.PUT("/prices/:id", s.handleUpdatePrice)
	admin.DELETE("/prices/:id", s.handleDeletePrice)
	// 费用试算：给定模型与 token 数，返回应扣额度
	admin.GET("/prices/quote", s.handleQuotePreview)

	admin.GET("/settings", s.handleGetSettings)
	admin.PUT("/settings", s.handleUpdateSettings)

	// ── 模型 API（访问令牌鉴权）──────────────────────────────────
	//
	// gin.WrapF 把标准库风格的 http.HandlerFunc 适配为 gin 处理器。
	// 这样 relay 包只依赖 net/http，不必依赖 gin —— 核心域与 Web 框架保持解耦，
	// 既便于单元测试（可直接用 httptest），也便于将来替换框架。
	v1 := r.Group("/v1")
	v1.Use(middleware.TokenAuth(s.deps.Tokens, s.deps.Users))
	v1.POST("/chat/completions", gin.WrapF(s.deps.Relay.ServeChatCompletions))
	// Anthropic Messages 协议：Claude 官方 SDK、Claude Code 等客户端默认走这里。
	// 网关内部会把请求转换为 OpenAI 格式再转发，响应再转换回 Anthropic 格式。
	v1.POST("/messages", gin.WrapF(s.deps.Relay.ServeAnthropicMessages))

	// Gemini 原生协议：模型名与动作都在路径里
	// （/v1beta/models/{model}:generateContent 与 :streamGenerateContent），
	// 因此用通配段承接，由适配器自行解析路径。
	gemini := r.Group("/v1beta")
	gemini.Use(middleware.TokenAuth(s.deps.Tokens, s.deps.Users))
	gemini.POST("/models/*action", gin.WrapF(s.deps.Relay.ServeGeminiGenerate))
}
