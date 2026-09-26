// 本文件集中注册所有 HTTP 路由。
//
// 意图（Why）：
//
//	把「有哪些接口、分别挂在什么路径」集中在一处，便于通读与审计。
//	处理器实现分散在各业务文件，但路由表只有一个入口。
//
// 流转（Flow）：
//
//	server.New() → registerRoutes() → engine 持有全部路由
//
// 扩展（Extend）：
//
//	新增接口时在这里加一行，并保持「按用途分组 + 组内按路径排序」的组织方式，
//	便于快速判断某接口是否已存在。管理类接口应挂到独立分组下（如 /api）。
package server

import (
	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
)

// registerRoutes 注册全部路由。
//
// 命名约定（后续里程碑沿用）：
//   - /healthz、/readyz   ：运维探活，无需鉴权
//   - /v1/...             ：面向客户端的模型 API，需令牌鉴权
//   - /api/...            ：管理后台接口，需管理员鉴权
func (s *Server) registerRoutes() {
	r := s.engine

	// ── 运维与探活（无需鉴权）────────────────────────────────────
	r.GET("/", s.handleRoot)           // 服务信息（名称/版本），便于人工确认服务是否正常
	r.GET("/healthz", s.handleHealthz) // 健康检查：含数据库连通性探测

	// ── 模型 API（需令牌鉴权）────────────────────────────────────
	//
	// 中间件顺序说明（顺序会影响行为，勿随意调整）：
	//   TokenAuth 必须最先执行——后续处理器（转发）依赖上下文中已认证的令牌，
	//   而且鉴权应当发生在任何实际工作之前，避免未授权请求消耗上游额度。
	v1 := r.Group("/v1")
	v1.Use(middleware.TokenAuth(s.deps.Tokens))

	// gin.WrapF 把标准库风格的 http.HandlerFunc 适配为 gin 处理器。
	// 这样 relay 包只依赖 net/http，不必依赖 gin —— 核心域与 Web 框架保持解耦，
	// 既便于单元测试（可直接用 httptest），也便于将来替换框架。
	v1.POST("/chat/completions", gin.WrapF(s.deps.Relay.ServeChatCompletions))

	// ── 管理接口（需管理员鉴权，后续里程碑启用）──────────────────
	// api := r.Group("/api")
	// api.Use(middleware.AdminAuth())
	// api.GET("/channels", s.handleListChannels)
}
