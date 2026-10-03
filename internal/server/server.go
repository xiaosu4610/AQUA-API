// Package server 提供 HTTP 服务层：路由注册、中间件装配与请求处理。
//
// 意图（Why）：
//
//	作为进程对外的唯一入口，负责把 HTTP 请求翻译成对内部能力的调用。
//	本包刻意保持"薄"：只做协议转换与参数校验，业务逻辑放在 model / relay 中，
//	避免 HTTP 细节渗透到核心域。
//
// 流转（Flow）：
//
//	cmd/ltzy/main.go
//	  └─ server.New(Deps{Config, Store, Channels})   装配 gin 引擎与路由
//	       └─ server.Run(ctx)                         启动监听，ctx 取消后优雅关闭
//	            └─ 处理器（health.go 等）调用 model / store 完成实际工作
//
// 扩展（Extend）：
//
//	新增接口：
//	  1) 在 router.go 的 registerRoutes 中注册路径；
//	  2) 新建处理器文件（如 channel.go），方法挂在 *Server 上；
//	  3) 若需要新依赖（缓存、relay 等），在 Deps 中添加字段并在 main 中注入。
//	新增中间件：在 New 中通过 engine.Use(...) 装配，注意中间件顺序（先恢复，后日志）。
package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/broadcast"
	"github.com/LTZY-ACU/ltzy-api/internal/config"
	"github.com/LTZY-ACU/ltzy-api/internal/corpus"
	"github.com/LTZY-ACU/ltzy-api/internal/mailer"
	"github.com/LTZY-ACU/ltzy-api/internal/metrics"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/notify"
	"github.com/LTZY-ACU/ltzy-api/internal/payment"
	"github.com/LTZY-ACU/ltzy-api/internal/relay"
	"github.com/LTZY-ACU/ltzy-api/internal/server/middleware"
	"github.com/LTZY-ACU/ltzy-api/internal/store"
)

// Deps 汇总服务运行所需的外部依赖，由 main 装配后注入。
//
// 设计说明：用结构体聚合依赖，而非把依赖作为多个参数传递——
// 这样后续新增依赖（如缓存、计费）时，调用方无需修改函数签名。
// 所有字段均为必需项，缺失会在启动或首次请求时立即暴露（而非静默降级）。
type Deps struct {
	Config   *config.Config          // 运行配置（监听地址、模式等）
	Store    *store.Store            // 数据库（健康检查需要探测其连通性）
	Channels model.ChannelRepository // 渠道仓储（上游凭证，供路由与管理使用）
	// ChannelKeys 是渠道密钥池仓储：一个渠道可挂多把上游密钥并轮询使用。
	ChannelKeys model.ChannelKeyRepository
	Tokens      model.TokenRepository    // 访问令牌仓储（下游凭证，供模型接口鉴权）
	Users       model.UserRepository     // 用户仓储
	Sessions    model.SessionRepository  // 登录会话仓储
	UsageLogs   model.UsageLogRepository // 调用日志仓储（用量统计）
	Settings    model.SettingRepository  // 系统设置仓储
	Relay       *relay.Relay             // 转发引擎（模型 API 的核心处理器）

	// ModelPrices 是模型计价规则仓储（后台维护价格、计算用量费用）。
	ModelPrices model.ModelPriceRepository
	// Groups 是模型分组仓储（分组倍率参与计费，也是模型广场的分组来源）。
	Groups model.ModelGroupRepository
	// Models 是模型实体仓储（后台维护模型清单，并支撑删除前的引用统计）。
	Models model.ModelRepository
	// ChannelModelMappings 是渠道级模型映射仓储（对外名 ↔ 上游名的双向解析来源）。
	ChannelModelMappings model.ChannelModelMappingRepository
	// RedeemCodes 是兑换码仓储（后台批量生成/管理，用户在门户兑换领取额度）。
	RedeemCodes model.RedeemCodeRepository
	// Billing 用于在改价后清空价格缓存，保证"改完立即生效"。
	Billing *relay.Billing
	// OAuthProviders 是 OAuth 提供方配置仓储（订阅账号池刷新令牌时使用）。
	OAuthProviders model.OAuthProviderRepository

	// Tasks 是异步任务仓储（列表查询、按任务号查询、取消）。
	Tasks model.TaskRepository
	// TaskService 是异步任务编排服务（提交上游、轮询推进、失败退还）。
	//
	// 与 Tasks 分开的原因：Tasks 只做存取，TaskService 才承载
	// "选渠道 → 扣费 → 提交上游 → 落库"的业务流程；
	// 前者用于纯查询场景（后台列表），后者用于需要副作用的操作。
	TaskService *relay.TaskService

	// Orders 是充值订单仓储（下单、回调入账、后台管理）。
	Orders model.PaymentOrderRepository
	// Payment 是支付通道注册表（按通道名取适配器做下单与验签）。
	Payment *payment.Registry

	// Audit 是后台操作审计日志仓储（查询用；写入由 middleware.AdminAudit 完成）。
	//
	// 之所以把"写"放在中间件而不是各处理器：后台写接口会持续增加，
	// 逐个补调用必然漏；挂在分组上才能保证"任何写操作都被记录"这一硬要求。
	Audit model.AuditLogRepository
	// Announcements 是站点公告仓储（后台发布维护，前台横幅读取生效公告）。
	Announcements model.AnnouncementRepository
	// Referrals 是邀请返利与签到仓储（邀请码、邀请关系、奖励台账、签到记录）。
	Referrals model.ReferralRepository

	// SensitiveWords 是敏感词表仓储（内容合规过滤的词条来源；中间件自行编译并缓存）。
	//
	// 为 nil 时过滤中间件退化为直接放行（见 middleware.SensitiveFilter）。
	SensitiveWords model.SensitiveWordRepository

	// ChannelModelCosts 是上游进价仓储（渠道 × 模型）。
	//
	// 用途：后台按密钥聚合用量后据此估算消耗，与人工录入的余额相减得出剩余。
	// 它是"能否看到密钥还剩多少"的前提——没有进价，消耗无从估算。
	ChannelModelCosts model.ChannelModelCostRepository

	// ModelSpeeds 是模型测速结果仓储（渠道 × 模型的最新延迟快照）。
	//
	// 为 nil 时测速接口仍可用但不落库（结果只在本次响应中可见），
	// 模型广场相应不展示延迟——测速是运营增强能力，缺失不应拖垮核心链路。
	ModelSpeeds model.ModelSpeedRepository

	// EmailCodes 是注册邮箱验证码仓储（由 main 注入；验证码相关接口依赖它）。
	EmailCodes model.EmailCodeRepository
	// Mailer 是出站邮件发送器；未配置时验证码接口会返回明确的"邮件服务未配置"提示，
	// 而不是让用户误以为"验证码已发出但没收到"。
	Mailer *mailer.Sender
	// SMTP 是 SMTP 配置仓储（后台可视化配置邮件通道；落库口令为密文）。
	SMTP model.SMTPRepository
	// SMTPBase 是"环境变量 / 内置默认值"提供的兜底 SMTP 参数。
	//
	// 用途有两个：
	//  1) 后台未启用配置时，它是实际生效的参数（首次部署即可用环境变量跑起来）；
	//  2) 后台保存后要重新合成"最终生效配置"，需要它作为回退项。
	// 注意它【不含】任何硬编码的账号或口令：默认值只有服务商地址与端口（见 config.DefaultSMTPHost）。
	SMTPBase config.SMTPConfig

	// Broadcasts 是邮件群发仓储（批次与逐人收件明细，是断点续发的依据）。
	Broadcasts model.EmailBroadcastRepository
	// Broadcast 是邮件群发执行器（名单入队、节流发送、重启续发、停止）。
	//
	// 与 Mailer 分开的原因：Mailer 只负责"发一封"，节流、去重、断点续发
	// 属于批量语义，混进 Mailer 会让它从"一次尝试快速失败"变成难以推理的长调用。
	Broadcast *broadcast.Sender

	// WebFS 是前端构建产物的嵌入文件系统；为 nil 时不托管前端页面（接口仍可用）。
	//
	// 用 fs.FS 而非 *embed.FS：便于测试注入内存文件系统，
	// 也让 server 包不必依赖承载嵌入声明的根包。
	WebFS fs.FS

	// TrialGrants 是限时试用额发放台账（后台发放、到期回收、门户展示）。
	//
	// 为 nil 时相关接口降级为"没有试用额"（门户横幅不展示）或 503（后台发放），
	// 而不是 panic——它是可选运营能力，缺失不应拖垮整个服务。
	TrialGrants model.TrialGrantRepository

	// Corpus 是语料共建计划的判定组件（内存快照），供后台展示与接口层使用；
	// CorpusSamples 是语料样本仓储（后台列表 / 导出 / 统计）。
	//
	// 两者都为 nil 时，后台的语料接口统一返回 503；转发链路的采集能力
	// 由 relay.Options 单独注入，与本字段无关。
	Corpus        *corpus.Guard
	CorpusSamples model.CorpusRepository

	// Metrics 是进程内指标注册表。
	//
	// 为什么允许由外部传入而不是服务内部创建：后续的告警派发器也要往同一张表里
	// 写（成功 / 失败 / 被抑制），若各自 new 一份，/metrics 只会显示其中一半，
	// 表现为"指标时有时无"这种极难定位的问题。传 nil 时内部会兜底新建一份。
	Metrics *metrics.Registry

	// AlertChannels 是告警通道配置仓储（后台 CRUD 与发送时读取已启用通道）。
	//
	// 为 nil 时告警相关接口统一返回 404（功能未启用），而不是 panic——
	// 告警是可选运营能力，缺失不应拖垮整个服务。
	AlertChannels model.AlertChannelRepository
	// Notifier 是告警派发器（发送测试告警、按事件订阅投递）。
	//
	// 为 nil 时"发送测试"返回 503；总开关默认关闭，判定来源见 Server.AlertEnabled。
	Notifier *notify.Dispatcher
}

// Server 是 HTTP 服务的运行时载体。
type Server struct {
	deps       Deps
	engine     *gin.Engine
	httpServer *http.Server
	startedAt  time.Time // 记录启动时刻，用于健康检查上报运行时长

	// loginLimiter 限制登录与注册接口的频率。
	//
	// 为什么必须限流：口令校验（bcrypt）是刻意昂贵的操作，
	// 不限流时攻击者可用少量并发请求打满 CPU（生产实例仅 2 核且与转发共享）。
	loginLimiter *middleware.RateLimiter

	// loginAccountLimiter 是登录接口的账号维度节流（见 New 处的说明）。
	loginAccountLimiter *middleware.RateLimiter

	// sensitiveFilter 是 /v1 入口的内容合规过滤器（敏感词）。
	//
	// 与登录限流器一样属于"进程内状态"：它缓存编译好的词表匹配器；
	// 后台改词表后调用 Invalidate 让新词立即生效（见 handler_sensitive.go）。
	sensitiveFilter *middleware.SensitiveFilter

	// sitemapMu 保护 sitemapCache（见 seo.go）。
	// sitemap.xml 是"读多写少"的端点，用互斥锁而非原子指针，保持实现直观。
	sitemapMu sync.Mutex
	sitemap   sitemapCache

	// nameCache 缓存「用户 ID → 用户名」「渠道 ID → 渠道名」等名称映射，
	// 供日志列表等需要批量解析名称的接口复用，避免每次请求全表扫描。
	// 名称变化频率远低于查询频率，短 TTL（30s）足够；管理员改名的改动
	// 至多滞后 30 秒出现在列表上，展示性数据可接受。
	nameCache *ttlCache

	// limitCache 缓存「运行上限」设置（limit_* 键）。
	//
	// 这些值变化频率极低（只有超管在后台主动修改），却被请求体限额等热路径
	// 高频读取；加一层短 TTL 缓存，避免每请求查一次设置表。后台保存后主动失效，
	// 保证"改完立即生效"，而不是等 TTL 到期。
	limitCache *ttlCache

	// alertCache 缓存「告警外发总开关」（alert_enabled 键）。
	//
	// 投递发生在后台协程里，每次投递都去查一次设置表没有意义：
	// 该值只有超管在后台主动切换，加一层短 TTL 缓存即可；切换后主动失效，
	// 保证"改完立即生效"。为 nil 或未命中时按关闭处理（安全默认）。
	alertCache *ttlCache

	// metrics 是进程内指标注册表（采集中间件与 /metrics 端点共用同一实例）。
	metrics *metrics.Registry
}

// New 创建并装配 HTTP 服务（不启动监听，便于测试直接取用 Handler）。
func New(deps Deps) *Server {
	// 按配置切换 gin 运行模式：release 下不输出路由调试信息，降低日志噪音与信息暴露
	gin.SetMode(toGinMode(deps.Config.Server.Mode))

	// 使用 gin.New() 而非 gin.Default()：
	// Default 会自带 Logger + Recovery，但我们希望显式控制中间件及其顺序。
	engine := gin.New()

	// 指标注册表必须【先于引擎装配】创建：采集中间件与 /metrics 端点要共用同一个
	// 实例，否则端点渲染的是一个空表（最典型的"接了监控但看不到数据"）。
	// 允许由 Deps 注入（后续告警派发器共用），未注入时内部兜底新建。
	reg := deps.Metrics
	if reg == nil {
		reg = metrics.New()
	}
	// Recovery 必须最先装配：保证后续任何 panic 都不会导致进程退出
	engine.Use(gin.Recovery())
	// 追踪 ID：紧接 Recovery 之后、其余中间件之前，保证「每个请求都有 ID」，
	// 这样后续鉴权 / 限流 / 敏感词报错时都能带上同一个 ID（便于一线工单精确定位）。
	engine.Use(middleware.Trace())
	// 结构化访问日志（slog）：输出 trace_id/method/path/status/latency/client_ip，
	// 替换原 gin.Logger() 的非结构化文本输出（后者无法按字段检索、且无请求标识）。
	engine.Use(middleware.AccessLog())
	// 指标采集：放在追踪与访问日志之后、其余会拒绝请求的中间件（如请求体限额）
	// 之前——它必须包住这些"提前拒绝"的路径，否则错误率会被系统性低估。
	engine.Use(middleware.Metrics(reg))
	// 解析 Accept-Language 并把语言偏好写入请求 context。
	// 放在业务处理器之前：让所有错误响应都能按用户语言返回（未携带时回退中文）。
	engine.Use(middleware.Locale())

	s := &Server{
		deps:      deps,
		engine:    engine,
		startedAt: time.Now(),
		// 每个来源 IP 每 5 分钟最多 20 次登录/注册尝试：
		// 正常使用者远达不到该频率，而爆破攻击会被有效拖慢。
		loginLimiter: middleware.NewRateLimiter(20, 5*time.Minute),
		// loginAccountLimiter 是登录接口的【账号维度】节流（安全审计 P2-4）。
		//
		// IP 维度限流挡不住"分布式多 IP 对同一账号爆破"：每个 IP 20 次/5 分钟
		// 看起来无关紧要，一百个 IP 就是对一个账号每小时 2400 次口令猜测。
		// 按用户名再叠一层（对同一用户名 10 次/5 分钟），把"锁定单个账号"
		// 的成本从攻击者的 IP 池转移到他无法无限扩展的账号名上。
		// 正常用户几乎感知不到：连续输错 10 次的人本来就该歇 5 分钟。
		loginAccountLimiter: middleware.NewRateLimiter(10, 5*time.Minute),
		// 内容合规过滤器：词表编译结果在组件内缓存，改词后由后台主动失效。
		sensitiveFilter: middleware.NewSensitiveFilter(deps.SensitiveWords, deps.Settings),
		// 名称映射缓存：用户/渠道名等低频变化数据，30 秒 TTL。
		nameCache: newTTLCache(30 * time.Second),
		// 运行上限缓存：请求体限额等热路径高频读取，30 秒 TTL + 保存后主动失效。
		limitCache: newTTLCache(30 * time.Second),
		// 告警总开关缓存：投递判定高频读取，30 秒 TTL + 切换后主动失效。
		alertCache: newTTLCache(30 * time.Second),
		// 指标注册表：与采集中间件共用同一实例（见上方 reg 的说明）。
		metrics: reg,
	}

	// 进程级指标：只能在拿到 s 之后注册（运行时长要以 startedAt 为基准）。
	s.registerProcessMetrics()

	// 请求体大小上限：必须在任何会读 body 的中间件（如登录限流的 keyFunc）
	// 与业务处理器之前装配，否则它们会把无上限的整个请求体读进内存。
	// 限额值改为运行期可调（见 handler_limits.go），故需 s 构造完成后装配。
	engine.Use(s.bodyLimit())

	s.registerRoutes()

	// 注册前端静态资源与 SPA 回退。
	// 必须在 registerRoutes 之后：SPA 回退依赖 gin 的 NoRoute 钩子，
	// 早于路由注册设置也不会出错，但放在之后更符合阅读顺序（先接口、后兜底）。
	if deps.WebFS != nil {
		s.registerStaticRoutes(deps.WebFS)
	}

	s.httpServer = &http.Server{
		Addr:    deps.Config.Server.Listen,
		Handler: engine,

		// ReadHeaderTimeout 用于防御 Slowloris 类攻击（攻击者缓慢发送请求头占满连接）。
		ReadHeaderTimeout: 10 * time.Second,

		// 刻意【不设置】ReadTimeout / WriteTimeout：
		// 大模型接口以 SSE 流式返回，单次响应可能持续数分钟（长回答、思考模型）。
		// 若设置 WriteTimeout，连接会在生成中途被强制掐断，客户端表现为"回答写到一半断掉"。
		// 因此超时控制交由：上游请求超时 + 业务层总时长上限 + 反向代理超时 共同负责。
	}

	return s
}

// Handler 返回 HTTP 处理器，供测试（httptest）或自定义监听方式使用。
func (s *Server) Handler() http.Handler {
	return s.engine
}

// Run 启动监听并阻塞，直到 ctx 被取消或监听出错。
//
// 优雅关闭的意义：网关可能正有流式请求在传输，直接杀进程会让客户端拿到截断的响应；
// 先停止接受新连接、等待在途请求完成（最多 10 秒）能显著改善升级/重启体验。
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	// 启动时先把库里保存的 SSE 流式上限推送给 relay（进程级状态，见 relay/stream_limits.go）。
	// 不做这一步的话，后台改过的值要等下一次"保存设置"或重启后才生效。
	s.applyStreamLimits(ctx)

	// 支付对账循环：补偿丢失的回调（主动向网关查单），随 ctx 取消退出。
	// 见 reconcile_payment.go 的头注释——这是"回调是唯一入账触发器"的单点风险治理。
	go s.startPaymentReconciler(ctx)

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// 收到取消信号：给在途请求 10 秒完成时间
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			// 超时（例如仍有流式响应在传输）不是"启动失败"：进程本来就要退出。
			// 只记一条告警并返回 nil，让退出码为 0——否则 systemd 会把一次
			// 正常重启标记为 failed，既污染日志，也会误触重启策略与告警。
			slog.Warn("优雅关闭未在期限内完成，进程即将退出", "error", err)
		}
		return nil
	}
}

// toGinMode 把配置中的运行模式翻译为 gin 的常量。
//
// 说明：配置层已校验取值合法性，因此这里只需处理 debug 与 test 两种情况，
// 其余一律按 release 处理（生产环境优先保证不外泄调试信息）。
func toGinMode(mode string) string {
	switch mode {
	case "debug":
		return gin.DebugMode
	case "test":
		return gin.TestMode
	default:
		return gin.ReleaseMode
	}
}
