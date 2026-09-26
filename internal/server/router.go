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
)

// registerRoutes 注册全部路由。
//
// 命名约定（后续里程碑沿用）：
//   - /healthz、/readyz   ：运维探活，无需鉴权
//   - /v1/...             ：面向客户端的模型 API，需令牌鉴权
//   - /api/...            ：管理后台接口，需管理员鉴权
func (s *Server) registerRoutes() {
	r := s.engine

	// ── 运维与探活 ──────────────────────────────────────────────
	r.GET("/", s.handleRoot)           // 服务信息（名称/版本），便于人工确认服务是否正常
	r.GET("/healthz", s.handleHealthz) // 健康检查：含数据库连通性探测

	// ── 模型 API ────────────────────────────────────────────────
	// gin.WrapF 把标准库风格的 http.HandlerFunc 适配为 gin 处理器。
	// 这样 relay 包只依赖 net/http，不必依赖 gin —— 核心域与 Web 框架保持解耦，
	// 既便于单元测试（可直接用 httptest），也便于将来替换框架。
	//
	// TODO(server): M2 在本组挂载令牌鉴权中间件（校验 sk- 令牌、额度与模型白名单）。
	r.POST("/v1/chat/completions", gin.WrapF(s.deps.Relay.ServeChatCompletions))

	// ── 管理接口（M5 起启用）────────────────────────────────────
	// api := r.Group("/api")
	// api.Use(middleware.AdminAuth())
	// api.GET("/channels", s.handleListChannels)
}
