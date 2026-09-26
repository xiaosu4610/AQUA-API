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
//	cmd/aqua/main.go
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
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/mailer"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/relay"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
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

	// EmailCodes 是注册邮箱验证码仓储（由 main 注入；验证码相关接口依赖它）。
	EmailCodes model.EmailCodeRepository
	// Mailer 是出站邮件发送器；未配置时验证码接口会返回明确的"邮件服务未配置"提示，
	// 而不是让用户误以为"验证码已发出但没收到"。
	Mailer *mailer.Sender

	// WebFS 是前端构建产物的嵌入文件系统；为 nil 时不托管前端页面（接口仍可用）。
	//
	// 用 fs.FS 而非 *embed.FS：便于测试注入内存文件系统，
	// 也让 server 包不必依赖承载嵌入声明的根包。
	WebFS fs.FS
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
}

// New 创建并装配 HTTP 服务（不启动监听，便于测试直接取用 Handler）。
func New(deps Deps) *Server {
	// 按配置切换 gin 运行模式：release 下不输出路由调试信息，降低日志噪音与信息暴露
	gin.SetMode(toGinMode(deps.Config.Server.Mode))

	// 使用 gin.New() 而非 gin.Default()：
	// Default 会自带 Logger + Recovery，但我们希望显式控制中间件及其顺序。
	engine := gin.New()
	// Recovery 必须最先装配：保证后续任何 panic 都不会导致进程退出
	engine.Use(gin.Recovery())
	// 访问日志（M1 先用 gin 默认实现；结构化日志与请求 ID 将在后续里程碑替换）
	engine.Use(gin.Logger())

	s := &Server{
		deps:      deps,
		engine:    engine,
		startedAt: time.Now(),
		// 每个来源 IP 每 5 分钟最多 20 次登录/注册尝试：
		// 正常使用者远达不到该频率，而爆破攻击会被有效拖慢。
		loginLimiter: middleware.NewRateLimiter(20, 5*time.Minute),
	}
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
		return s.httpServer.Shutdown(shutdownCtx)
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
