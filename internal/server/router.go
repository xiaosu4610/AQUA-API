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
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/config"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/server/middleware"
)

// loginBodyTooLargeKey 是"请求体超限/无法解析"时使用的固定限流键。
//
// 为什么不沿用空串：RateLimiter 对空串一律放行（避免误伤无法识别来源的正常请求），
// 那样"拿超大 body 打登录接口"就完全不吃账号维度配额，等于给爆破留了后门。
// 固定桶只对这类畸形请求生效——正常登录的用户名走自己的桶，不会被它挤占。
const loginBodyTooLargeKey = "__oversized_or_invalid_login_body__"

// maxLoginKeyBodyBytes 是 keyFunc 读取请求体的上限。
//
// 它只需要 username 一个字段，合法请求不到 1KB；取 64KiB 是给
// "账号名带全角字符/超长备注"留足余量。全局 bodyLimit 已封到 4MiB，
// 这里再收一层：避免 keyFunc 在业务处理器之前就把允许范围内的大 body
// 整份读进内存（它每个登录请求都会跑一次）。
const maxLoginKeyBodyBytes = 64 << 10

// loginUsernameKey 是登录接口的账号维度限流键（安全审计 P2-4）。
//
// 为什么不用 IP：分布式多 IP 对同一账号爆破时，每个 IP 都在自己的配额内，
// IP 维度限流完全无效。按用户名计数才能把攻击成本压在攻击者
// 无法无限扩展的维度上（账号名）。
//
// 【读请求体必须把 body 放回】（线上事故，勿删此说明）：
//
//	gin 的 ShouldBindJSON 直接读 c.Request.Body，读完即空，且**不会**自动复原。
//	这个 keyFunc 跑在业务处理器之前，一旦在此吃掉 body，
//	紧随其后的 handleLogin 就会拿到 EOF、一律回 "请求体格式错误" —— 表现为
//	**所有账号都无法登录**。因此这里用 io.ReadAll 读一次、立刻把同一份字节
//	塞回 c.Request.Body，再对内存里的字节做解析（不要再 ShouldBindJSON）。
//	超限时用 MultiReader 把已读部分与剩余流拼回去：截断或丢弃都会让
//	处理器拿到与客户端发出的不同的 body。
func loginUsernameKey(c *gin.Context) string {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxLoginKeyBodyBytes+1))
	if err != nil {
		return ""
	}
	if len(raw) > maxLoginKeyBodyBytes {
		// 超限：把"已读到的部分 + 剩余未读流"原样拼回去交给处理器，
		// 同时用固定桶计数，避免超大 body 完全不吃账号维度配额。
		c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), c.Request.Body))
		return loginBodyTooLargeKey
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	var req struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return ""
	}
	// 统一小写：避免攻击者用 Admin / ADMIN / admin 绕过同一账号的计数
	return strings.ToLower(strings.TrimSpace(req.Username))
}

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
	//
	// 叠加「可选登录」：已登录的代理看自己拿货分组的模型与代理价（原价划线对照），
	// 未登录或不带会话则完全按公开结果返回，与从前逐字一致。
	api.GET("/models", middleware.SessionAuthOptional(s.deps.Sessions, s.deps.Users), s.handleModelPlaza)

	// 公开定价试算（无需登录）：给定模型/分组/用量，返回折算后的预估费用。
	// 只读对外的售价规则，不含任何上游成本信息；金额按站点兑换比例换算为元（微元）。
	api.GET("/models/quote", s.handleModelQuote)

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
	//
	// 登录额外叠加【账号维度】节流（loginAccountLimiter）：
	// IP 维度挡不住分布式多 IP 对同一账号的口令爆破（见 server.go 的说明）。
	// keyFunc 取请求体里的 username——限流发生在 body 解析之前也能拿到，
	// 因为这个 keyFunc 自己 ShouldBindJSON 一次（Body 由 gin 缓存，处理器仍可再读）。
	//
	// 注意中间件顺序：IP 限流 → 账号限流 → 业务处理器。
	authLimit := s.loginLimiter.Middleware(middleware.ClientIP)
	api.POST("/auth/register", authLimit, s.handleRegister)
	api.POST("/auth/login", authLimit,
		s.loginAccountLimiter.Middleware(loginUsernameKey), s.handleLogin)
	// 超管入口：只输密码（不输用户名）。必须与普通登录共用限流器——
	// 它会枚举管理员逐个比对 bcrypt 哈希，不限流即等于开放一个 CPU 放大器。
	api.POST("/auth/admin-login", authLimit, s.handleAdminLogin)
	// 安装向导：未安装时可用，安装完成后接口自锁（返回 409）。
	// 安装请求同样含 bcrypt 运算，因此也套限流。
	api.GET("/install/status", s.handleInstallStatus)
	api.POST("/install", authLimit, s.handleInstall)
	// 发送邮箱验证码（注册 / 登录 / 重置密码三种用途共用一个入口，
	// 由请求体里的 purpose 区分）。同样叠加限流：该接口会触发真实发信（有成本），
	// 且是"把本站当邮件轰炸机"的直接入口。
	api.POST("/auth/email-code", authLimit, s.handleSendEmailCode)
	// 邮箱验证码登录：忘记用户名或忘记密码时的一站式自助入口。
	api.POST("/auth/email-login", authLimit, s.handleEmailLogin)
	// 邮箱验证码重置密码：重置成功后会吊销该账号全部会话。
	api.POST("/auth/password-reset", authLimit, s.handleResetPassword)

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
	// 取明文密钥（创建弹层没复制/复制失败时的正式找回入口）；归属校验在处理器内。
	portal.GET("/tokens/:id/key", s.handleMyTokenKey)
	portal.PATCH("/tokens/:id", s.handleMyUpdateToken)
	portal.DELETE("/tokens/:id", s.handleMyDeleteToken)
	// 可选分组（带"当前用户是否已解锁"标记）：令牌页的"所属分组"下拉据此置灰未解锁项。
	// 未登录的下拉用公开的模型广场（/api/models），两者分工：广场给"有什么"，
	// 本接口给"你能用什么"。
	portal.GET("/groups", s.handleMyGroups)
	portal.GET("/usage", s.handleMyUsage)
	portal.GET("/logs", s.handleMyLogs)
	// 用量排行榜（付费榜 / 免费榜）：登录可见；管理员带 ?all=1 可看完整榜
	portal.GET("/leaderboard", s.handleLeaderboard)
	// 模型实时指标（tokens/s / 平均耗时 / TTFB）：模型详情页使用。
	//
	// 模型名走查询参数而不是路径段：本项目对外模型名普遍带斜杠
	// （如 LTZY-CALL/deepseek-v4.1-flash），放进路径会被 gin 拆成多段而 404
	// （与 /admin/corpus/models/delete 等接口同一处理，见上方 corpus 段注释）。
	portal.GET("/models/stats", s.handleModelStats)
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
	portal.GET("/referral/rewards", s.handleMyReferralRewards)
	portal.GET("/checkin", s.handleGetCheckin)
	portal.POST("/checkin", s.handleCheckin)

	// 财务记录：把钱相关的四个数字（余额/累计充值/累计返利/累计消费）一次给全，
	// 明细列表复用上面的订单 / 返利明细 / 调用日志接口。
	portal.GET("/finance", s.handleFinanceSummary)

	// 限时试用额：当前用户"还剩多少、几时过期"，供概览页横幅展示。
	portal.GET("/trial", s.handleMyTrialGrant)

	// ── 管理后台（需管理员）──────────────────────────────────────
	admin := authed.Group("/admin")

	// 管理面网络边界白名单（可选）：必须在 RequireAdmin 之前。
	//
	// 顺序理由（为什么放在身份鉴权之前）：
	//  1) 白名单判断只依赖来源 IP，与"你是谁"无关；先在网络层拒绝非白名单来源，
	//     可省掉一次鉴权开销（查会话 + 载入用户 + 角色判断，含数据库往返）；
	//  2) 对公网暴露的后台，未授权来源本就不该触碰到任何鉴权代码路径。
	// 语义：未配置（空列表）时不挂任何中间件，行为与从前逐字一致。
	if prefixes, err := config.ParseAdminAllowCIDRs(s.deps.Config.AdminAllowCIDRs); err != nil {
		// fail-closed 兜底：正常情况下 config.Load 的 Validate 已拦截非法 CIDR 并让进程退出，
		// 走不到这里。若真走到，宁可 403 拒绝全部后台请求，也绝不放行到鉴权与业务层。
		slog.Error("管理面 CIDR 白名单解析失败，已拒绝全部后台请求", "error", err)
		admin.Use(denyAllAdminRequests)
	} else if len(prefixes) > 0 {
		admin.Use(middleware.RequireAdminCIDR(prefixes))
	}

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
	// 模型测速：逐模型测首字延迟（TTFB），结果落库并供广场展示。
	// 与 /test（测活）的分工：测活回答"渠道通不通"，测速回答"每个模型各有多快"。
	admin.POST("/channels/:id/speedtest", s.handleSpeedTestChannel)
	// 密钥池明细与单把密钥的状态/调度参数管理
	admin.GET("/channels/:id/keys", s.handleListChannelKeys)
	admin.PUT("/keys/:keyId", s.handleUpdateChannelKeyStatus)
	// 订阅账号额度探测：把上游额度窗口取回来并落库，
	// 让"哪个账号快满了"与"何时恢复"在后台可见，而不是等被限流才发现。
	admin.POST("/channels/:id/keys/:keyId/quota", s.handleProbeChannelKeyQuota)

	// 上游计费（渠道 × 模型进价）：站长核算成本与毛利的唯一录入入口。
	//
	// 放在渠道之下而不是与 /prices 并列，原因是它的归属维度是"渠道"：
	// 同一模型在不同渠道的成本可能相差数倍，两者是不同维度、各管一个方向。
	admin.GET("/channels/:id/costs", s.handleListChannelCosts)
	admin.PUT("/channels/:id/costs", s.handleReplaceChannelCosts)
	// 密钥余额核算：按密钥聚合用量 × 进价 = 已消耗，与录入余额相减得剩余。
	admin.GET("/channels/:id/key-usage", s.handleChannelKeyUsage)

	// 凭据调度策略目录：后台渠道表单据此渲染「调度策略」下拉与帮助文案，
	// 因此新增策略不需要改前端代码。
	admin.GET("/key-strategies", s.handleListKeyStrategies)

	// 密钥失败处置策略目录：渠道表单据此渲染「失败后怎么办」下拉与帮助文案。
	//
	// 存在的意义：站长最关心的是"密钥失败了会不会就没了"。把两种策略
	// （只冷却不摘除 / 失败自动摘除）连说明一起下发，页面无需硬编码文案。
	admin.GET("/key-failure-policies", s.handleListKeyFailurePolicies)

	// 从上游拉取模型列表。
	//
	// 刻意不挂在 /channels/ 之下：gin 的路由树中 `/channels/fetch-models`
	// 会与已有的 `/channels/:id` 在同一层级产生静态段/参数段冲突，
	// 放在顶层既避免冲突，也更贴合它的语义——它可能被用于"尚未保存的渠道"。
	admin.POST("/fetch-models", s.handleFetchModels)

	admin.GET("/tokens", s.handleAdminListTokens)
	admin.POST("/tokens", s.handleAdminCreateToken)
	// 取明文密钥（后台代客户复制/找回）；RequireAdmin 已挂在 /admin 分组上。
	admin.GET("/tokens/:id/key", s.handleAdminTokenKey)
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

	// 全站通知邮件（群发）：预览 → 确认发送 → 看进度 / 看失败明细 → 可停止。
	//
	// 模板目录刻意放在顶层 /broadcast-templates 而不是 /broadcasts/templates：
	// 后者会与 /broadcasts/:id 在同一层级形成静态段/参数段冲突
	// （与 /fetch-models 同样的处理，见上方注释）。
	admin.GET("/broadcast-templates", s.handleListBroadcastTemplates)
	admin.POST("/broadcasts/preview", s.handlePreviewBroadcast)
	admin.GET("/broadcasts", s.handleListBroadcasts)
	admin.POST("/broadcasts", s.handleCreateBroadcast)
	admin.GET("/broadcasts/:id/recipients", s.handleListBroadcastRecipients)
	admin.POST("/broadcasts/:id/cancel", s.handleCancelBroadcast)

	// 限时试用额：给全站用户发一笔会过期的额度（需显式 confirm + 唯一批次）。
	// 到期回收由后台协程负责，不占用接口（见 cmd/ltzy 的 runTrialGrantReclaimer）。
	admin.POST("/trial-grants", s.handleGrantTrial)

	// 语料共建计划：模型清单 / 特别福利账户 / 样本查看与导出。
	//
	// 注意两处刻意的路径设计：
	//   - 删除类操作用 POST + 请求体而不是 DELETE /:name —— 模型名里带斜杠
	//     （LTZY-CALL/deepseek-v4.1-flash），放进路径会被路由拆成多段；
	//   - 样本列表只回预览，看全文走 /samples/:id，两条访问路径都会写审计。
	admin.GET("/corpus/models", s.handleListCorpusModels)
	admin.POST("/corpus/models", s.handleUpsertCorpusModel)
	admin.POST("/corpus/models/delete", s.handleDeleteCorpusModel)
	admin.GET("/corpus/grants", s.handleListCorpusGrants)
	admin.POST("/corpus/grants", s.handleUpsertCorpusGrant)
	admin.POST("/corpus/grants/delete", s.handleDeleteCorpusGrant)
	admin.GET("/corpus/samples", s.handleListCorpusSamples)
	admin.GET("/corpus/samples/:id", s.handleGetCorpusSample)
	admin.GET("/corpus/export", s.handleExportCorpusSamples)
	admin.GET("/corpus/stats", s.handleCorpusStats)

	// 模型计价规则（用量 → 费用的换算依据）
	admin.GET("/prices", s.handleListPrices)
	admin.POST("/prices", s.handleCreatePrice)
	admin.PUT("/prices/:id", s.handleUpdatePrice)
	admin.DELETE("/prices/:id", s.handleDeletePrice)
	// 费用试算：给定模型与 token 数，返回应扣额度
	admin.GET("/prices/quote", s.handleQuotePreview)

	// 敏感词过滤：词表维护（总开关在系统设置里）
	admin.GET("/sensitive-words", s.handleListSensitiveWords)
	admin.POST("/sensitive-words", s.handleCreateSensitiveWord)
	// 批量导入放在单条新增之后注册：路径更长更具体，语义上属于"新增"的变体
	admin.POST("/sensitive-words/import", s.handleImportSensitiveWords)
	admin.PUT("/sensitive-words/:id", s.handleUpdateSensitiveWord)
	admin.DELETE("/sensitive-words/:id", s.handleDeleteSensitiveWord)

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

	// 全站模型 ID 映射总览（跨渠道聚合）：
	// 用一页回答「我的平台有哪些模型 ID、分别映射到哪个上游 ID、走哪个渠道/分组」。
	// 刻意独立于渠道路由：它是"总览"视角，不是某个渠道的属性。
	admin.GET("/model-mappings", s.handleListAllModelMappings)

	// OAuth 提供方配置（订阅账号池刷新令牌时使用）
	admin.GET("/oauth-providers", s.handleListOAuthProviders)
	admin.POST("/oauth-providers", s.handleCreateOAuthProvider)
	admin.PUT("/oauth-providers/:id", s.handleUpdateOAuthProvider)
	admin.DELETE("/oauth-providers/:id", s.handleDeleteOAuthProvider)

	admin.GET("/settings", s.handleGetSettings)
	admin.PUT("/settings", s.handleUpdateSettings)

	// 运行上限（超管可调）：请求体大小、批量导入条数、统计窗口、试用时长、公告条数。
	//
	// 独立于 /settings：它是数值型"保护性上限"，每项都带默认值与合法区间，
	// 由后端下发、前端据此渲染表单；默认值与改动前的硬编码常量逐一相等。
	admin.GET("/limits", s.handleGetLimits)
	admin.PUT("/limits", s.handleUpdateLimits)

	// 邮件通道（SMTP）配置：站长在后台填写自己的发信账号与授权码。
	//
	// 为什么单独成组而不是塞进 /settings：
	// 口令的读写规则与普通设置项完全不同（只进不出、留空即沿用），
	// 混在通用设置接口里极易被"整体覆盖保存"的语义误清空。
	admin.GET("/smtp", s.handleGetSMTP)
	admin.PUT("/smtp", s.handleUpdateSMTP)
	// 发送测试邮件：配完当场验证"能不能发出去"，是排查发信问题最快的手段。
	admin.POST("/smtp/test", s.handleTestSMTP)

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

	// 充值订单管理（人工确认入账 / 关单）
	//
	// 刻意没有"退款"接口：退款属于站方与用户之间另行约定的事项，
	// 不以公开条款或自助功能的形式对外提供，故不暴露任何退款入口
	// （历史上曾存在 POST /orders/:tradeNo/refund，已下线）。
	admin.GET("/orders", s.handleAdminListOrders)
	admin.POST("/orders/:tradeNo/mark-paid", s.handleAdminMarkOrderPaid)
	admin.POST("/orders/:tradeNo/close", s.handleAdminCloseOrder)

	// 真实成本对账（收入 − 上游成本 = 毛利）：按 分组 / 渠道 / 模型 聚合。
	//
	// 收入取自 usage_logs（用户实扣额度），成本按 (渠道, 上游模型名) 匹配进价估算，
	// 按次计费渠道同样计入（否则面板会显示"全是利润"，与真实账目背离）。
	admin.GET("/finance/reconciliation", s.handleAdminFinanceReconciliation)

	// ── 模型 API（访问令牌鉴权）──────────────────────────────────
	//
	// gin.WrapF 把标准库风格的 http.HandlerFunc 适配为 gin 处理器。
	// 这样 relay 包只依赖 net/http，不必依赖 gin —— 核心域与 Web 框架保持解耦，
	// 既便于单元测试（可直接用 httptest），也便于将来替换框架。
	v1 := r.Group("/v1")
	// 第三个参数（计费组件）用于"请求前额度预扣"：额度不足直接 429，
	// 避免并发请求全部通过检查后再各自扣费导致超支。
	v1.Use(middleware.TokenAuth(s.deps.Tokens, s.deps.Users, s.deps.Billing))
	// 分组 RPM 限流：按"本次请求所属分组"做固定 1 分钟窗口计数（见 middleware.GroupRPMLimiter）。
	//
	// 挂在 TokenAuth 之后：分组来自 TokenAuth 写入 request context 的值。
	// 挂在敏感词过滤之前：超限的请求在读到请求体之前就被拒绝，省掉一次正文扫描。
	// 默认分组 rpm_limit=0（不限）时"零成本"通过，行为与引入前逐字一致。
	//
	// 同一个中间件实例同时挂到 /v1 与 /v1beta：两条协议共用一张"分组 × 分钟"计数表，
	// 否则用户可以改走另一协议绕过限流，速率上限形同虚设。
	groupRPM := s.buildGroupRPMMiddleware()
	if groupRPM != nil {
		v1.Use(groupRPM)
	}
	// 内容合规过滤：在【鉴权之后、转发之前】扫描请求正文，命中敏感词即拒绝。
	//
	// 放在鉴权之后的原因：过滤本身要读完整请求体，未鉴权的请求没必要为其付出这个成本；
	// 放在转发之前则是为了"命中的请求根本不产生上游成本"。
	v1.Use(s.sensitiveFilter.Middleware())
	// OpenAI 兼容的模型清单：客户端（SDK / IDE 插件 / Web UI）启动时普遍会先调它，
	// 缺了会显示"未获取到模型列表"，使用者容易误判为网关故障。
	v1.GET("/models", s.handleListModels)
	v1.POST("/chat/completions", gin.WrapF(s.deps.Relay.ServeChatCompletions))
	// Responses 端点：Codex 系订阅账号的上游只认这个协议，官方 Codex CLI
	// 与新版 SDK 也以它为主入口。没有它，站长就没法把现成客户端直接指向本站。
	//
	// 与 chat/completions 的差别只在协议语言：客户端说 Responses 时，
	// 若上游也是 Responses（订阅账号）则原样直通，不做任何转写。
	v1.POST("/responses", gin.WrapF(s.deps.Relay.ServeResponses))
	// 向量嵌入：不少免费上游（如 NVIDIA 的 embedding / rerank / clip 模型）
	// 只提供这个端点，对 /v1/chat/completions 一律 404。支持它才能把这些
	// 模型真正用起来，否则它们在清单里等于"上架了但调不通"。
	v1.POST("/embeddings", gin.WrapF(s.deps.Relay.ServeEmbeddings))
	// 图像 / 音频类入站端点：与对话接口共用同一套令牌鉴权、分组路由、密钥池、
	// 失败重试与计费（见 relay/media.go）。它们是"非文本"的转发变体——
	// 图像请求/响应为 JSON，TTS 响应为二进制音频流，ASR 入参为 multipart/form-data，
	// 网关一律按原始字节透传（含 Content-Type）。
	v1.POST("/images/generations", gin.WrapF(s.deps.Relay.ServeImageGenerations))
	v1.POST("/audio/speech", gin.WrapF(s.deps.Relay.ServeAudioSpeech))
	v1.POST("/audio/transcriptions", gin.WrapF(s.deps.Relay.ServeAudioTranscriptions))
	v1.POST("/audio/translations", gin.WrapF(s.deps.Relay.ServeAudioTranslations))
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
	// Gemini 原生协议复用同一个分组 RPM 限流实例（与 /v1 共享计数，避免绕过）。
	if groupRPM != nil {
		gemini.Use(groupRPM)
	}
	// Gemini 原生协议同样受内容合规过滤约束（同一个组件，同一份词表）。
	gemini.Use(s.sensitiveFilter.Middleware())
	gemini.POST("/models/*action", gin.WrapF(s.deps.Relay.ServeGeminiGenerate))
}

// buildGroupRPMMiddleware 构造「分组 RPM 限流」中间件；未配置分组仓储时返回 nil。
//
// registerRoutes 只在启动时执行一次，因此这里构造的实例在整个进程生命周期内唯一，
// 供 /v1 与 /v1beta 共用（见调用处的说明）。
func (s *Server) buildGroupRPMMiddleware() gin.HandlerFunc {
	if s.deps.Groups == nil {
		return nil
	}
	return middleware.NewGroupRPMLimiter(s.deps.Groups, s.defaultGroupForRPM()).Middleware()
}

// defaultGroupForRPM 返回限流使用的默认分组名（与计费默认分组保持一致）。
//
// 必须与计费/路由的默认分组同源，否则不带分组的请求会"按 A 组计费、按 B 组限流"。
func (s *Server) defaultGroupForRPM() string {
	if s.deps.Billing != nil {
		if group := s.deps.Billing.DefaultGroup(); group != "" {
			return group
		}
	}
	return model.DefaultGroupName
}

// denyAllAdminRequests 是白名单配置解析失败时的 fail-closed 兜底中间件：拒绝一切后台请求。
//
// 只在 config.ParseAdminAllowCIDRs 意外失败时挂载（正常应由启动校验拦截并让进程退出）。
// 之所以"全拒"而不是"全放"：白名单配置损坏时放行，等于把后台直接暴露到公网，
// 后果远比"后台暂时不可用"严重。
func denyAllAdminRequests(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": gin.H{
			"message": "管理后台访问白名单配置无效，已拒绝全部请求（请检查 " + config.EnvPrefix + "ADMIN_ALLOW_CIDRS）",
			"type":    "permission_error",
			"code":    "ip_not_allowed",
		},
	})
}
