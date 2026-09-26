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

	// ── SEO：站点地图与爬虫规则（无需鉴权）────────────────────────
	// 必须显式注册，否则会被 SPA 回退拦截成 index.html（爬虫将拿不到 XML/纯文本）。
	// 它们不是 API 路径，走独立处理器，不受 isAPIPath 影响。
	r.GET("/sitemap.xml", s.handleSitemap)
	r.GET("/robots.txt", s.handleRobots)

	// ── 公开接口（无需登录）──────────────────────────────────────
	api := r.Group("/api")

	// 站点信息：落地页与登录页靠它渲染站点名称、注册开关与可用模型
	api.GET("/status", s.handleSiteStatus)

	// 充值参数（无需登录）：登录页/落地页需要提前知道"是否开放充值、汇率多少"。
	// 只暴露非敏感参数，不含任何密钥或后台配置细节。
	api.GET("/payment/public", s.handlePublicPaymentInfo)

	// 模型广场（无需登录）：站点能力清单，是使用者了解"本站能做什么"的入口。
	// 只暴露模型名、分组、价格与可用状态，不暴露渠道名与上游地址。
	api.GET("/models", s.handleModelPlaza)

	// 站点公告（无需登录）：前台横幅据此展示当前生效的公告。
	api.GET("/announcements", s.handlePublicListAnnouncements)

	// 支付平台异步回调：必须公开（第三方服务器无法携带我们的会话），
	// 其安全性完全由通道适配器的验签保证（见 internal/payment）。
	api.POST("/payments/:method/notify", s.handlePaymentNotify)
	// 少数支付平台用 GET 回调
	api.GET("/payments/:method/notify", s.handlePaymentNotify)

	// 登录与注册叠加频率限制。
	//
	// 为什么单独给这两个接口限流：口令校验（bcrypt）是刻意昂贵的操作，
	// 不限流时攻击者可用少量并发请求打满 CPU（生产实例仅 2 核且与转发共享 CPU）。
	// 注意中间件顺序：限流在业务处理器之前，避免昂贵操作先被执行。
	authLimit := s.loginLimiter.Middleware(middleware.ClientIP)
	api.POST("/auth/register", authLimit, s.handleRegister)
	api.POST("/auth/login", authLimit, s.handleLogin)
	// 超管入口：只输密码（不输用户名）。必须与普通登录共用限流器——
	// 它会枚举管理员逐个比对 bcrypt 哈希，不限流即等于开放一个 CPU 放大器。
	api.POST("/auth/admin-login", authLimit, s.handleAdminLogin)
	// 安装向导：未安装时可用，安装完成后接口自锁（返回 409）。
	// 安装请求同样含 bcrypt 运算，因此也套限流。
	api.GET("/install/status", s.handleInstallStatus)
	api.POST("/install", authLimit, s.handleInstall)
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
	// 异步任务（用户只能看自己的）
	portal.GET("/tasks", s.handleMyListTasks)

	// ── 充值（用户自己的订单）────────────────────────────────────
	portal.POST("/orders", s.handleCreateOrder)
	portal.GET("/orders", s.handleMyListOrders)
	portal.GET("/orders/:tradeNo", s.handleMyGetOrder)

	// 兑换码：用户输入兑换码领取额度（归属约束由会话强制，无需传用户 id）
	portal.POST("/redeem", s.handleUserRedeem)

	// 邀请返利与每日签到（用户自己的邀请码/关系与签到记录）
	portal.GET("/referral", s.handleReferralInfo)
	portal.GET("/checkin", s.handleGetCheckin)
	portal.POST("/checkin", s.handleCheckin)

	// ── 管理后台（需管理员）──────────────────────────────────────
	admin := authed.Group("/admin")
	admin.Use(middleware.RequireAdmin())
	// 审计中间件：记录后台所有写操作（POST/PUT/PATCH/DELETE）。
	// 必须挂在 RequireAdmin 之后，才能从上下文取到已鉴权的管理员身份；
	// 只读请求不记录，避免审计表被列表页刷新淹没。
	admin.Use(middleware.AdminAudit(s.deps.Audit))

	admin.GET("/dashboard", s.handleDashboard)

	// 上游渠道类型目录：后台新建/编辑渠道时据此做「选类型 → 展开该类型必填项」
	// 的触发式渲染，因此新增上游类型不需要改前端代码。
	admin.GET("/channel-types", s.handleChannelTypes)

	admin.GET("/channels", s.handleListChannels)
	admin.POST("/channels", s.handleCreateChannel)
	admin.GET("/channels/:id", s.handleGetChannel)
	admin.PUT("/channels/:id", s.handleUpdateChannel)
	admin.DELETE("/channels/:id", s.handleDeleteChannel)
	admin.POST("/channels/:id/test", s.handleTestChannel)
	// 密钥池明细与单把密钥的状态/调度参数管理
	admin.GET("/channels/:id/keys", s.handleListChannelKeys)
	admin.PUT("/keys/:keyId", s.handleUpdateChannelKeyStatus)

	// 凭据调度策略目录：后台渠道表单据此渲染「调度策略」下拉与帮助文案，
	// 因此新增策略不需要改前端代码。
	admin.GET("/key-strategies", s.handleListKeyStrategies)

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
	// 后台操作审计日志（只读查询；写操作由中间件自动记录）
	admin.GET("/audit-logs", s.handleAdminListAuditLogs)

	// 站点公告（发布/编辑/删除；列表含停用与已过期）
	admin.GET("/announcements", s.handleAdminListAnnouncements)
	admin.POST("/announcements", s.handleAdminCreateAnnouncement)
	admin.PUT("/announcements/:id", s.handleAdminUpdateAnnouncement)
	admin.DELETE("/announcements/:id", s.handleAdminDeleteAnnouncement)

	// 模型计价规则（用量 → 费用的换算依据）
	admin.GET("/prices", s.handleListPrices)
	admin.POST("/prices", s.handleCreatePrice)
	admin.PUT("/prices/:id", s.handleUpdatePrice)
	admin.DELETE("/prices/:id", s.handleDeletePrice)
	// 费用试算：给定模型与 token 数，返回应扣额度
	admin.GET("/prices/quote", s.handleQuotePreview)

	// 模型分组（分组本身是实体，可配置计费倍率）
	admin.GET("/groups", s.handleListGroups)
	admin.POST("/groups", s.handleCreateGroup)
	admin.PUT("/groups/:id", s.handleUpdateGroup)
	admin.DELETE("/groups/:id", s.handleDeleteGroup)

	// 模型实体（把"模型"从渠道上的字符串升级为可维护实体）
	//
	// 注意路由形态：`/models/:name/references` 只用 GET，与 GET 列表 /models 不冲突；
	// PUT/DELETE 用的是 `/models/:id`，参数名与 references 的 :name 处于不同
	// HTTP 方法树上，gin 不会产生参数名冲突。
	admin.GET("/models", s.handleListModelMetas)
	admin.POST("/models", s.handleCreateModelMeta)
	admin.PUT("/models/:id", s.handleUpdateModelMeta)
	admin.DELETE("/models/:id", s.handleDeleteModelMeta)
	// 删除前引用统计（只读）：返回有多少令牌/分组/渠道/映射在引用该模型
	admin.GET("/models/:name/references", s.handleModelMetaReferences)

	// 渠道级模型映射（对外名 ↔ 上游名），整组替换便于界面一次提交
	admin.GET("/channels/:id/mappings", s.handleListChannelMappings)
	admin.PUT("/channels/:id/mappings", s.handleReplaceChannelMappings)

	// OAuth 提供方配置（订阅账号池刷新令牌时使用）
	admin.GET("/oauth-providers", s.handleListOAuthProviders)
	admin.POST("/oauth-providers", s.handleCreateOAuthProvider)
	admin.PUT("/oauth-providers/:id", s.handleUpdateOAuthProvider)
	admin.DELETE("/oauth-providers/:id", s.handleDeleteOAuthProvider)

	admin.GET("/settings", s.handleGetSettings)
	admin.PUT("/settings", s.handleUpdateSettings)

	// 运维监控与数据库备份（概览 / 一致性快照下载 / 备份只读校验）
	admin.GET("/maintenance/overview", s.handleMaintenanceOverview)
	admin.GET("/maintenance/backup", s.handleMaintenanceBackup)
	admin.POST("/maintenance/backup/inspect", s.handleMaintenanceInspectBackup)

	// 异步任务管理（管理员可见全部任务并取消）
	admin.GET("/tasks", s.handleAdminListTasks)
	admin.POST("/tasks/:ref/cancel", s.handleAdminCancelTask)
	// 已注册的上游任务适配器（供任务表单提示可选 provider）
	admin.GET("/task-providers", s.handleTaskProviders)

	// 兑换码（批量生成活动码、按状态/批次筛选、作废与清理）
	admin.GET("/redeem-codes", s.handleAdminListRedeemCodes)
	admin.POST("/redeem-codes", s.handleAdminCreateRedeemCodes)
	// 静态段 /invalid 与参数段 /:id 在同一层级可共存（gin 优先匹配静态段），
	// 且语义独立，故放在 :id 之前便于阅读。
	admin.DELETE("/redeem-codes/invalid", s.handleAdminDeleteInvalidRedeemCodes)
	admin.PUT("/redeem-codes/:id", s.handleAdminUpdateRedeemCode)
	admin.DELETE("/redeem-codes/:id", s.handleAdminDeleteRedeemCode)

	// 充值订单管理（人工确认入账 / 关单 / 退款）
	admin.GET("/orders", s.handleAdminListOrders)
	admin.POST("/orders/:tradeNo/mark-paid", s.handleAdminMarkOrderPaid)
	admin.POST("/orders/:tradeNo/close", s.handleAdminCloseOrder)
	admin.POST("/orders/:tradeNo/refund", s.handleAdminRefundOrder)

	// ── 模型 API（访问令牌鉴权）──────────────────────────────────
	//
	// gin.WrapF 把标准库风格的 http.HandlerFunc 适配为 gin 处理器。
	// 这样 relay 包只依赖 net/http，不必依赖 gin —— 核心域与 Web 框架保持解耦，
	// 既便于单元测试（可直接用 httptest），也便于将来替换框架。
	v1 := r.Group("/v1")
	// 第三个参数（计费组件）用于"请求前额度预扣"：额度不足直接 429，
	// 避免并发请求全部通过检查后再各自扣费导致超支。
	v1.Use(middleware.TokenAuth(s.deps.Tokens, s.deps.Users, s.deps.Billing))
	// OpenAI 兼容的模型清单：客户端（SDK / IDE 插件 / Web UI）启动时普遍会先调它，
	// 缺了会显示"未获取到模型列表"，使用者容易误判为网关故障。
	v1.GET("/models", s.handleListModels)
	v1.POST("/chat/completions", gin.WrapF(s.deps.Relay.ServeChatCompletions))
	// 向量嵌入：不少免费上游（如 NVIDIA 的 embedding / rerank / clip 模型）
	// 只提供这个端点，对 /v1/chat/completions 一律 404。支持它才能把这些
	// 模型真正用起来，否则它们在清单里等于"上架了但调不通"。
	v1.POST("/embeddings", gin.WrapF(s.deps.Relay.ServeEmbeddings))
	// Anthropic Messages 协议：Claude 官方 SDK、Claude Code 等客户端默认走这里。
	// 网关内部会把请求转换为 OpenAI 格式再转发，响应再转换回 Anthropic 格式。
	v1.POST("/messages", gin.WrapF(s.deps.Relay.ServeAnthropicMessages))

	// 异步任务：提交后返回任务号，客户端凭任务号轮询取结果。
	//
	// 与对话接口共用同一套令牌鉴权与额度体系（任务在提交时即扣费，
	// 失败/取消会自动退还），因此使用者无需学习第二套凭据机制。
	v1.POST("/tasks", s.handleSubmitTask)
	v1.GET("/tasks", s.handleListMyTasks)
	v1.GET("/tasks/:ref", s.handleGetTask)

	// Gemini 原生协议：模型名与动作都在路径里
	// （/v1beta/models/{model}:generateContent 与 :streamGenerateContent），
	// 因此用通配段承接，由适配器自行解析路径。
	gemini := r.Group("/v1beta")
	gemini.Use(middleware.TokenAuth(s.deps.Tokens, s.deps.Users, s.deps.Billing))
	gemini.POST("/models/*action", gin.WrapF(s.deps.Relay.ServeGeminiGenerate))
}
