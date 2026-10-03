// Package main 是 LTZY-API 的程序入口。
//
// 意图（Why）：
//
//	本文件只做「装配」——按依赖顺序把配置、日志、数据库、仓储、转发引擎与 HTTP 服务
//	组装起来，不包含任何业务逻辑。这样做的价值：
//	  1) 启动流程一目了然，便于排查"服务起不来"类问题；
//	  2) 各层保持独立可测（业务逻辑都在 internal 下，可脱离 main 单测）。
//
// 流转（Flow）：
//
//	main()
//	  ├─ 解析命令行参数（-config / -version / -gen-key 等）
//	  ├─ config.Load(path)                 加载并按「默认值→文件→环境变量」合并配置
//	  ├─ setupLogger(cfg)                  初始化结构化日志（slog）
//	  ├─ store.Open + store.Migrate        建立数据库连接并执行结构迁移
//	  ├─ crypto.New(cfg.Security.AppKey)   构造加解密器（保护渠道密钥）
//	  ├─ store.NewChannelRepository        渠道仓储（读写时自动加解密）
//	  ├─ relay.New                         转发引擎
//	  ├─ server.New                        装配 HTTP 路由与中间件
//	  └─ server.Run(ctx)                   启动监听，收到退出信号后优雅关闭
//
//	子命令（执行后立即退出，用于运维）：
//	  -version / -gen-key / -create-token / -init-admin / -reset-password
//
// 扩展（Extend）：
//
//	新增依赖（如 Redis 缓存、计费引擎）时：
//	  1) 在此按「被依赖者先创建」的顺序插入初始化代码；
//	  2) 注入到 server.Deps 或 relay.Options；
//	  3) 需要新配置项时，先在 internal/config 补齐（四处同步），再在此使用。
//	再次强调：请勿在本文件写业务逻辑，保持它只做装配。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ltzy "github.com/LTZY-ACU/ltzy-api"
	"github.com/LTZY-ACU/ltzy-api/internal/broadcast"
	"github.com/LTZY-ACU/ltzy-api/internal/config"
	"github.com/LTZY-ACU/ltzy-api/internal/corpus"
	"github.com/LTZY-ACU/ltzy-api/internal/crypto"
	"github.com/LTZY-ACU/ltzy-api/internal/mailer"
	"github.com/LTZY-ACU/ltzy-api/internal/metrics"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/notify"
	"github.com/LTZY-ACU/ltzy-api/internal/payment"
	"github.com/LTZY-ACU/ltzy-api/internal/relay"
	"github.com/LTZY-ACU/ltzy-api/internal/server"
	"github.com/LTZY-ACU/ltzy-api/internal/store"
	"github.com/LTZY-ACU/ltzy-api/internal/version"
)

// 默认配置文件路径。
//
// 约定：文件不存在时不算错误（回退默认值 + 环境变量），
// 因此默认值可以放心地指向一个"通常存在但允许不存在"的路径。
const defaultConfigPath = "aqua.json"

// quotaCleanupInterval 是后台周期回收"在途超时预留"的间隔。
//
// 取值 5 分钟、且不做成配置项，原因：
//  1. 预留的在途有效期本身是 15 分钟（store 的 defaultReservationTTL），
//     已相对上游首字节超时（300 秒）留足缓冲，被回收的都是"确实超时"的残留；
//  2. 崩溃残留的额度晚几分钟退回并不影响正确性，用户不会因此得到或失去额度，
//     只是"占用"多持续一会儿，因此无需秒级轮询——周期过短只会让数据库空转。
const quotaCleanupInterval = 5 * time.Minute

// quotaCleanupInitialDelay 是首次后台回收前的延迟。
//
// 目的：启动阶段已经同步回收过一次（解决"上次崩溃"的残留），
// 这里再延迟一小段，避开迁移、启动自检与撤销旧订单等数据库写入高峰，
// 让服务先进入稳定状态再开始周期性维护。
const quotaCleanupInitialDelay = time.Minute

// trialReclaimInterval 是后台周期回收"到期试用额"的间隔。
//
// 取值 2 分钟（比在途预留回收更密），原因：
//  1. 在途回收是"兜底"，晚几分钟不影响正确性；试用额到期是**精确时刻**，
//     回收前这笔钱仍躺在用户余额里可以被花掉，窗口越长越不划算；
//  2. 但也不必秒级——一轮就是一条走索引的 SELECT，命中通常为 0 行，
//     2 分钟足以把"过期后还能用"的窗口压到可忽略。
const trialReclaimInterval = 2 * time.Minute

// channelHealthInterval 是后台周期检查「渠道成功率」并自动停用不健康渠道的间隔。
//
// 取 5 分钟的理由：健康判定基于 15 分钟窗口的调用统计（config.ChannelHealth.WindowMinutes），
// 检查间隔明显小于窗口才不会被同一批数据反复判定；而过密只会空转查询。
const channelHealthInterval = 5 * time.Minute

func main() {
	// 用 run() 承载全部逻辑并统一处理退出码：
	// 既便于集中做 defer 收尾，也便于将来对 run 做集成测试。
	if err := run(); err != nil {
		// 启动阶段的错误直接打到 stderr（此时日志器可能尚未就绪）
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

// run 执行完整的启动流程，返回错误表示启动失败。
func run() error {
	var (
		configPath  = flag.String("config", defaultConfigPath, "配置文件路径（不存在则使用默认值与环境变量）")
		showVersion = flag.Bool("version", false, "打印版本信息后退出")
		genKey      = flag.Bool("gen-key", false, "生成一个加密主密钥（AQUA_APP_KEY）后退出")
		createToken = flag.String("create-token", "", "创建一个访问令牌（取值为令牌名称）后退出")
		initAdmin   = flag.String("init-admin", "", "创建一个管理员账号（取值为用户名），随机密码仅打印一次后退出")
		resetPwUser = flag.String("reset-password", "", "重置指定用户（取值为用户名）的密码后退出")
		resetPwNew  = flag.String("new-password", "", "配合 -reset-password：指定新密码；留空则随机生成并打印一次")
	)
	flag.Parse()

	// ── 子命令：打印版本 ────────────────────────────────────────
	if *showVersion {
		info := version.Get()
		fmt.Printf("LTZY-API %s (commit=%s, built=%s)\n", info.Version, info.GitCommit, info.BuildTime)
		return nil
	}

	// ── 子命令：生成加密主密钥 ──────────────────────────────────
	// 提供该命令是为了避免使用者随手填一个弱口令作为主密钥。
	if *genKey {
		key, err := crypto.GenerateKeyMaterial()
		if err != nil {
			return fmt.Errorf("生成主密钥失败: %w", err)
		}
		fmt.Printf(`已生成加密主密钥。请通过环境变量注入（切勿写入配置文件或提交进仓库）：

  Windows PowerShell:  $env:AQUA_APP_KEY="%s"
  Linux / systemd:     Environment=AQUA_APP_KEY=%s

⚠ 重要：请妥善备份该密钥。它一旦变更，已加密的渠道密钥将【无法解密】。
`, key, key)
		return nil
	}

	// ── 加载配置 ────────────────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// ── 初始化日志 ──────────────────────────────────────────────
	logger := setupLogger(cfg)
	slog.SetDefault(logger)

	info := version.Get()
	logger.Info("LTZY-API 启动中",
		"version", info.Version,
		"commit", info.GitCommit,
		"listen", cfg.Server.Listen,
		"database_driver", cfg.Database.Driver,
		// 使用 SafeDSN 而非原始 DSN：后者可能包含数据库密码
		"database_dsn", cfg.SafeDSN(),
		"log_level", cfg.Log.Level,
	)

	// ── 退出信号处理 ────────────────────────────────────────────
	// 收到 Ctrl+C(SIGINT) 或 SIGTERM(容器停止) 时取消 ctx，
	// 由 server.Run 触发优雅关闭（给在途的流式请求留出完成时间）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── 数据库：连接 + 迁移 ─────────────────────────────────────
	st, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Warn("关闭数据库连接失败", "error", cerr)
		}
	}()

	if err := st.Migrate(ctx); err != nil {
		return err
	}
	migrationVersion, _ := st.LatestMigrationVersion(ctx)
	logger.Info("数据库迁移完成", "schema_version", migrationVersion)

	// ── 加解密器 ────────────────────────────────────────────────
	// 渠道密钥以密文落库，加解密能力由仓储使用；此处构造后注入即可。
	cipher, err := crypto.New(cfg.Security.AppKey)
	if err != nil {
		return err
	}

	// ── 仓储层装配 ──────────────────────────────────────────────
	channels := store.NewChannelRepository(st.DB(), cipher)
	channelKeys := store.NewChannelKeyRepository(st.DB(), cipher)
	tokens := store.NewTokenRepository(st.DB(), cipher)
	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())
	// 这两个仓储内含与数据库方言相关的语句（按天分桶、UPSERT），因此额外注入方言；
	// 其余仓储只写通用 SQL，不需要方言。
	usageLogs := store.NewUsageLogRepository(st.DB(), st.Dialect())
	settings := store.NewSettingRepository(st.DB(), st.Dialect())
	emailCodes := store.NewEmailCodeRepository(st.DB())
	// 邮件群发：批次 + 逐人收件明细（断点续发与"绝不重复发"的依据）。
	emailBroadcasts := store.NewEmailBroadcastRepository(st.DB())
	modelPrices := store.NewModelPriceRepository(st.DB())
	oauthProviders := store.NewOAuthProviderRepository(st.DB(), cipher)
	tasks := store.NewTaskRepository(st.DB())
	orders := store.NewPaymentOrderRepository(st.DB())
	modelGroups := store.NewModelGroupRepository(st.DB())
	// 模型实体与渠道级模型映射：把"模型"从渠道上的字符串升级为可维护实体，
	// 并支持对外名 ↔ 上游名的双向解析（映射解析本身是 model 层的纯函数）。
	modelMetas := store.NewModelMetaRepository(st.DB())
	channelModelMappings := store.NewChannelModelMappingRepository(st.DB())
	redeemCodes := store.NewRedeemCodeRepository(st.DB())
	// 操作审计日志：后台写操作的追溯记录（写入由审计中间件完成，这里提供查询/清理）。
	auditLogs := store.NewAuditLogRepository(st.DB())
	// 站点公告：后台发布维护，前台横幅读取生效公告。
	announcements := store.NewAnnouncementRepository(st.DB())
	// 邀请返利 / 签到：注册与充值发的奖励、每人每天的签到记录。
	referrals := store.NewReferralRepository(st.DB())
	// SMTP 配置仓储：口令以密文落库（加密器与渠道密钥同一个）。
	smtpSettings := store.NewSMTPRepository(st.DB(), cipher)
	// 额度预留台账：鉴权时预扣、响应后结算/退还，堵住并发超支漏洞。
	quotaReservations := store.NewQuotaRepository(st.DB())
	// 敏感词表：内容合规过滤的词条来源（匹配器由 server 中间件按需编译并缓存）。
	sensitiveWords := store.NewSensitiveWordRepository(st.DB())
	// 上游进价（渠道 × 模型）：密钥余额核算与毛利的基础数据。
	channelModelCosts := store.NewChannelModelCostRepository(st.DB())
	// 限时试用额度：发放台账（后台发放、到期回收、门户展示）。
	trialGrants := store.NewTrialGrantRepository(st.DB())
	// 语料共建计划：模型清单 / 语料样本 / 特殊福利账户。
	corpusRepo := store.NewCorpusRepository(st.DB())
	// 语料判定组件：把"采哪些模型""谁免计费"做成内存快照，
	// 让转发热点路径上零数据库查询（与本项目敏感词过滤器同一套做法）。
	corpusGuard := corpus.NewGuard(corpusRepo)
	// 告警通道：把渠道熔断/自动停用/登录锁定等关键事件发到站长配置的外部通道。
	alertChannels := store.NewAlertChannelRepository(st.DB())

	// 启动时清理过期会话：会话表随登录次数持续增长，不清理会无限膨胀。
	// 清理失败不阻断启动（这只是维护动作，不影响核心功能）。
	if cleaned, err := sessions.DeleteExpired(ctx, time.Now()); err != nil {
		logger.Warn("清理过期会话失败", "error", err)
	} else if cleaned > 0 {
		logger.Info("已清理过期会话", "count", cleaned)
	}

	// 启动时清理过期验证码：验证码是短生命周期数据，过期即无价值。
	if cleaned, err := emailCodes.DeleteExpired(ctx, time.Now()); err != nil {
		logger.Warn("清理过期邮箱验证码失败", "error", err)
	} else if cleaned > 0 {
		logger.Info("已清理过期邮箱验证码", "count", cleaned)
	}

	// 启动时回收"在途但已超时"的额度预留并退还额度。
	//
	// 为什么必须做：进程被强杀/崩溃会在预留台账里留下永久"在途"记录，
	// 不回收则这部分额度永远退不回来，用户会以为额度凭空消失。
	if cleaned, err := quotaReservations.CleanupExpired(ctx, time.Now()); err != nil {
		logger.Warn("回收在途超时预留失败", "error", err)
	} else if cleaned > 0 {
		logger.Info("已回收在途超时预留并退还额度", "count", cleaned)
	}

	// 后台周期回收：上面那一次只解决"上次进程退出时"的残留；
	// 运行期仍可能因请求处理中途崩溃而留下新的在途记录（可用额度 = 额度 − 已用 − 在途，
	// 在途不释放则额度被永久占用），因此必须持续兜底。随 ctx 退出，失败不阻断。
	go runQuotaReservationCleaner(ctx, quotaReservations, logger)

	// 启动时回收"已到期但未收回"的试用额。
	//
	// 为什么必须做：试用额是先加到用户余额、到期再收回的，进程停机期间没人回收，
	// 不补一次的话这些额度会一直留在用户余额里，等于把限时活动变成了永久赠送。
	if cleaned, err := trialGrants.ReclaimExpired(ctx, time.Now()); err != nil {
		logger.Warn("回收到期试用额失败", "error", err)
	} else if cleaned > 0 {
		logger.Info("已回收到期试用额", "count", cleaned)
	}

	// 后台周期回收到期试用额：试用额的到期时刻是精确的（如"24 小时后"），
	// 间隔取 2 分钟是为了让"过期后仍能用"的窗口足够小，同时不至于频繁抢写锁。
	go runTrialGrantReclaimer(ctx, trialGrants, logger)

	// ── 支付 / 充值 ─────────────────────────────────────────────
	// 支付通道注册表：各通道的运营参数（网关地址、商户号、启用列表）从设置表实时读取，
	// 密钥只从环境变量读取。这样"改配置"与"改密钥"两条路径彻底分离：
	// 前者管理员在后台随时改且立即生效，后者只能由运维改环境变量（避免密钥落库）。
	paymentRegistry := payment.NewRegistry(payment.Options{
		Secrets: cfg.Payment,
		Settings: func(ctx context.Context) (model.PaymentSettings, error) {
			// 每次调用都重新读库：管理员在后台改完网关地址应立刻生效，
			// 而不是等到重启。
			loaded, err := model.LoadSiteSettings(ctx, settings)
			if err != nil {
				return model.PaymentSettings{}, err
			}
			return loaded.Payment, nil
		},
	})

	// 启动补偿：处理"已支付但未入账"的订单。
	// 这类订单只会出现在"标记支付成功"与"入账"之间的极端中断（进程被杀、断电），
	// 数量应为 0；一旦出现必须补上，否则就是用户付了钱没到账。
	if pending, err := orders.ListPaidUncredited(ctx, 50); err != nil {
		logger.Warn("查询未入账订单失败", "error", err)
	} else {
		for _, order := range pending {
			if credited, err := orders.CreditOrder(ctx, order.TradeNo, time.Now()); err != nil {
				logger.Error("补入账失败，需人工处理", "trade_no", order.TradeNo, "error", err)
			} else if credited {
				logger.Warn("已补入账未到账订单", "trade_no", order.TradeNo, "quota", order.Quota)
			}
		}
	}

	// 启动时关闭超时未支付订单：避免"待支付"订单无限堆积，让对账失去意义。
	if closed, err := orders.CloseExpired(ctx, time.Now()); err != nil {
		logger.Warn("关闭超时订单失败", "error", err)
	} else if closed > 0 {
		logger.Info("已关闭超时未支付订单", "count", closed)
	}

	// ── 邮件发送器 ──────────────────────────────────────────────
	//
	// 配置来源有两条，优先级见 0023 迁移与本段说明：
	//   1) 后台「系统设置 → 邮件通道」里显式启用的配置（口令密文落库）；
	//   2) 环境变量 / 内置默认值（AQUA_SMTP_*），作为兜底与首次部署的预设。
	// 后台启用即为准：站长在界面点"启用并保存"是一个明确意图，
	// 若仍被环境变量覆盖，就会出现"我明明配好了却不生效"这类最难排查的问题。
	//
	// 默认值里【只有服务商地址与端口】，不含任何账号或口令——
	// 每个站长的发信账号都不一样，账号与口令必须由站长自己提供。
	smtpBase := cfg.SMTP
	if stored, err := smtpSettings.Get(ctx); err != nil {
		logger.Warn("读取后台邮件通道配置失败，本次回退环境变量", "error", err)
	} else if stored != nil && stored.Configured() {
		cfg.SMTP = config.SMTPConfig{
			Host:     stored.Host,
			Port:     stored.Port,
			Username: stored.Username,
			From:     stored.From,
			FromName: stored.FromName,
			Password: stored.Password,
		}
	}

	mailerSender := mailer.New(cfg.SMTP)
	// 只记录"是否就绪"与发件地址，绝不打印口令。
	if mailerSender.Configured() {
		source := "环境变量"
		if cfg.SMTP.Username != strings.TrimSpace(smtpBase.Username) || cfg.SMTP.Host != smtpBase.Host {
			source = "后台配置"
		}
		logger.Info("SMTP 邮件通道已就绪", "source", source,
			"host", cfg.SMTP.Host, "port", cfg.SMTP.Port, "from", cfg.SMTP.From)
	} else {
		logger.Warn("SMTP 邮件通道未配置，注册邮箱验证码将不可用；" +
			"可在后台「系统设置 → 邮件通道」填写，或设置环境变量 " +
			"AQUA_SMTP_USERNAME / AQUA_SMTP_FROM / AQUA_SMTP_PASSWORD")
	}

	// 邮件群发执行器：名单入队、节流逐封发送、停止与重启续发。
	//
	// 与 mailerSender 分开的原因：mailer 刻意保持"一次尝试、快速失败"，
	// 而群发要的节流、去重、断点续发都属于批量语义，混进 mailer 会让它
	// 从"发一封"变成难以推理的长调用（见 internal/mailer 文件头说明）。
	// Options{} 表示用默认节奏（2 秒/封、每 50 封停 30 秒），见 broadcast 包的常量说明。
	broadcastSender := broadcast.New(emailBroadcasts, users, mailerSender, broadcast.Options{})

	// 指标注册表：本进程【唯一】一份，同时交给 HTTP 采集层与告警派发器，
	// 这样两类指标（aqua_http_* 与 aqua_alerts_*）出现在同一份 /metrics 输出里。
	//
	// 为什么必须共享同一个实例：若各自 new 一份，/metrics 只会渲染其中一半，
	// 表现为"指标时有时无"这种极难定位的问题（这也是 Deps.Metrics 存在的原因）。
	// 两处注册的指标名互不重叠，不会触发"重复注册"的 panic。
	metricsReg := metrics.New()

	// 告警派发器：邮件通道复用 mailerSender，Webhook 类通道用带 netguard 护栏的
	// HTTP 投递器（投递目标由站长填写，属外部输入，必须挡内网与云元数据地址）。
	// 外发总开关默认关闭；判定函数在 server 构造完成后接线（见下方 SetEnabledFunc）。
	alertNotifier := notify.NewDispatcher(alertChannels, mailerSender,
		map[string]notify.Sender{
			model.AlertChannelWebhook:  notify.NewHTTPSender(model.AlertChannelWebhook),
			model.AlertChannelDingTalk: notify.NewHTTPSender(model.AlertChannelDingTalk),
			model.AlertChannelWeCom:    notify.NewHTTPSender(model.AlertChannelWeCom),
		}, metricsReg)

	// 子命令：创建访问令牌（M2 遗留入口，保留以兼容既有脚本）
	if *createToken != "" {
		return createAndPrintToken(ctx, tokens, *createToken, cfg.Server.Listen)
	}

	// 子命令：创建管理员账号。
	// 为什么必须有它：系统初始没有任何账号，若没有管理员则该站点无人可管理
	// （渠道配不了、令牌发不出去）。密码随机生成并只打印一次，避免使用弱口令。
	if *initAdmin != "" {
		return createAdminAndPrint(ctx, users, *initAdmin)
	}

	// 子命令：重置用户密码。
	//
	// 为什么需要独立入口：管理员忘记密码时若没有它，就只能手工改数据库，
	// 而本项目口令哈希带 SHA-256 预处理（见 crypto.HashPassword），
	// 手工构造几乎必然算错，结果是"改了密码却登录不上"。
	// 注意：密码只作为命令行参数/随机值传入，绝不出现在代码与配置文件里。
	if *resetPwUser != "" {
		return resetPasswordAndReport(ctx, users, sessions, *resetPwUser, *resetPwNew)
	}

	// 检查是否存在管理员：没有则给出明确指引（不自动创建，避免生成"弱口令管理员"）
	adminCount, err := users.CountAdmins(ctx)
	if err != nil {
		logger.Warn("统计管理员数量失败", "error", err)
	} else if adminCount == 0 {
		logger.Warn("系统中尚不存在管理员账号，无法登录管理后台。请执行：" +
			"aqua -init-admin <用户名>（会生成随机密码并打印一次）")
	}

	// ── 计费组件 ────────────────────────────────────────────────
	// 负责按模型单价与用量换算额度，并扣减令牌与用户额度。
	// 价格规则缓存在内存中（后台改价后会主动失效），避免每次转发都查库。
	// 注入额度预留台账后，鉴权阶段会"请求前预扣"、响应后"多退少补"，
	// 从而堵住并发请求全部通过检查、各自后扣费导致超支的漏洞。
	//
	// 最后一个参数【必须】与下面 relayEngine 的默认分组取同一个值（cfg.RelayGroup）：
	// 路由用它选渠道、计费用它查价格，两者一旦不同，不带分组的令牌就会
	// "按 A 组选渠道、按 B 组查价格"，而查不到价格的模型一律被视为【不计费】——
	// 结果是收费模型被免费调用（余额 0 也能调），账单上还看不出异常。
	// 历史上这里曾写死空串（回退常量 "default"），而 relay_group 被配成 free，正是这个错配。
	billing := relay.NewBilling(modelPrices, modelGroups, tokens, users, cfg.RelayGroup).
		WithQuotaRepository(quotaReservations).
		// 语料共建的特殊福利账户：在指定模型上免计费（判定走内存快照）。
		WithFreeChecker(corpusGuard)

	// 订阅账号令牌刷新器：让 OAuth 凭据在 access_token 过期前自动续期
	oauthRefresher := relay.NewOAuthRefresher(oauthProviders, channelKeys)

	// 模型测速结果仓储：测速落库、广场延迟展示、auto 路由共用同一实例，
	// 保证"广场显示的延迟"与"auto 选出的延迟"来自同一份数据。
	modelSpeeds := store.NewModelSpeedRepository(st.DB())

	relayEngine := relay.New(channels, relay.Options{
		// 默认路由分组：令牌未指定分组时落到这里。
		// 由配置注入（AQUA_RELAY_GROUP，默认 default）——站点把渠道迁到自有分组后，
		// 不改这里就会导致所有不带分组的令牌找不到渠道（表现为 503）。
		Group:     cfg.RelayGroup,
		UsageLogs: usageLogs,
		Tokens:    tokens,
		// 渠道密钥池：让一个渠道可以挂多把上游密钥并轮询使用
		Keys: channelKeys,
		// 订阅账号池：OAuth 凭据自动刷新
		OAuth: oauthRefresher,
		// 计费：把 usage 换算成额度并扣减
		Billing: billing,
		// 渠道级模型映射：让"对外名 ↔ 上游名"的改写真正在转发链路生效
		// （后台改完映射会主动清缓存，见 handler_model_meta.go）
		ChannelModelMappings: channelModelMappings,
		// 语料共建：判定组件 + 样本仓储（同时非 nil 才启用原文采集）
		Corpus:        corpusGuard,
		CorpusSamples: corpusRepo,
		// auto 路由：模型名填 auto 时按延迟选可对话模型（需要测速数据）
		ModelSpeeds: modelSpeeds,
	})

	// 语料共建快照的后台刷新：启动加载一次，之后每 30 秒刷新。
	// 刷新失败保留旧快照（不清空），因此"撤销福利资格"最多延迟一个周期失效。
	go corpusGuard.Start(ctx, 0)

	// 异步任务编排：把"选渠道 → 扣费 → 提交上游 → 落库 → 轮询推进"串起来。
	// 轮询器以 goroutine 启动，随进程退出信号一起结束（ctx 取消即返回）。
	taskService := relay.NewTaskService(tasks, relayEngine)
	go taskService.RunPoller(ctx, 0)

	srv := server.New(server.Deps{
		Config:      cfg,
		Store:       st,
		Channels:    channels,
		ChannelKeys: channelKeys,
		Tokens:      tokens,
		Users:       users,
		Sessions:    sessions,
		UsageLogs:   usageLogs,
		Settings:    settings,
		Relay:       relayEngine,
		// 计费：价格规则仓储 + 计费组件（后台改价后用它清缓存）
		ModelPrices: modelPrices,
		// 模型分组：分组倍率参与计费，也是模型广场的分组来源
		Groups:  modelGroups,
		Billing: billing,
		// 模型实体与渠道级映射：后台维护模型清单 / 映射，删除前做引用统计
		Models:               modelMetas,
		ChannelModelMappings: channelModelMappings,
		// 兑换码：后台批量生成，用户在门户兑换领取额度
		RedeemCodes: redeemCodes,
		// 后台操作审计、站点公告、邀请返利与签到
		Audit:         auditLogs,
		Announcements: announcements,
		Referrals:     referrals,
		// 敏感词表：/v1 入口的内容合规过滤
		SensitiveWords: sensitiveWords,
		// 上游进价：按密钥核算消耗、计算余额剩余与毛利
		ChannelModelCosts: channelModelCosts,
		// 模型测速：渠道 × 模型的最新延迟快照（管理端测速落库、广场展示）
		ModelSpeeds: modelSpeeds,
		// 订阅账号：OAuth 提供方配置（后台维护）
		OAuthProviders: oauthProviders,
		// 异步任务：仓储（查询）+ 编排服务（提交/轮询/取消）
		Tasks:       tasks,
		TaskService: taskService,
		// 充值：订单仓储 + 支付通道注册表
		Orders:  orders,
		Payment: paymentRegistry,
		// 注册邮箱验证码：仓储 + 发信通道
		EmailCodes: emailCodes,
		Mailer:     mailerSender,
		// 邮件群发：批次仓储 + 执行器（后台触发"通知全站用户"时使用）
		Broadcasts: emailBroadcasts,
		Broadcast:  broadcastSender,
		// 邮件通道（SMTP）：后台可视化配置；SMTPBase 是环境变量/默认值兜底项
		SMTP:     smtpSettings,
		SMTPBase: smtpBase,
		// 限时试用额：后台发放、到期回收、门户展示
		TrialGrants: trialGrants,
		// 语料共建：后台清单/福利/样本/导出接口所需的仓储与判定组件
		Corpus:        corpusGuard,
		CorpusSamples: corpusRepo,
		// 告警外发：通道仓储（后台 CRUD）+ 派发器（测试发送、按事件投递）。
		// 外发默认关闭，总开关在 server 就绪后接线（见下方 SetEnabledFunc）。
		AlertChannels: alertChannels,
		Notifier:      alertNotifier,
		// 指标注册表：与告警派发器共用同一实例（见上方 metricsReg 的说明）。
		Metrics: metricsReg,
		// 前端构建产物（web/dist）已通过根包的 go:embed 嵌入二进制
		WebFS: ltzy.WebDist,
	})

	// 把"告警外发总开关"判定接到派发器上。
	// 顺序原因：派发器要先于 server 构造才能注入 Deps，而判定需要 server 持有的
	// 设置仓储与 TTL 缓存，二者构成先后依赖，故在装配完成后接线。
	alertNotifier.SetEnabledFunc(srv.AlertEnabled)

	logger.Info("HTTP 服务已就绪，等待请求", "addr", cfg.Server.Listen)

	// 后台周期检查渠道健康度：按成功率自动停用不健康渠道。
	//
	// 默认关闭（AQUA_CHANNEL_AUTO_DISABLE_MIN_REQUESTS=0）——刻意不设正数默认值，
	// 避免升级后低峰期的正常抖动把渠道误停。站长开启后本协程才真正生效。
	go runChannelHealthWatcher(ctx, srv, logger)

	// 续发上次进程退出时未完成的邮件群发（串行，不并发开多条 SMTP 连接）。
	// 已发过的收件人在明细表里是 sent，不会被再取到——重启导致的重复投递由数据保证不会发生。
	broadcastSender.ResumeAll(ctx)

	// ── 启动并阻塞 ──────────────────────────────────────────────
	// Run 在收到退出信号后会优雅关闭并返回 nil；
	// 仅在监听失败等异常情况下返回错误。
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	}

	logger.Info("LTZY-API 已退出")
	return nil
}

// runQuotaReservationCleaner 周期性回收"在途超时"的额度预留并退还额度。
//
// 为什么必须有它：CleanupExpired 若只在启动时执行，进程在"预留之后、结算之前"崩溃时，
// 那笔预留会一直停留在途状态；而可用额度 = 额度 − 已用 − 在途，
// 在途不释放就等于用户的额度被永久占用（看起来像额度凭空消失）。
// 周期性兜底即可把这类残留自动退回，无需人工干预。
//
// 行为约定（与其它后台协程保持一致）：
//   - 随 ctx 取消（进程退出信号）立即返回，不阻塞关闭；
//   - 每轮仅在实际回收发生时打 info，正常情况下不产生日志噪音；
//   - 单轮失败只打 warn 并继续下一轮，绝不 panic、不退出进程。
//
// 说明：本回收不做多实例互斥——回收本身以「条件更新 + 受影响行数」保证幂等，
// 多个实例并发回收同一批残留时只有一方生效，另一方读到 0 行，不会重复退还。
func runQuotaReservationCleaner(ctx context.Context, repo model.QuotaRepository, logger *slog.Logger) {
	// 首轮延迟启动，避开启动迁移/自检的数据库写入高峰。
	delay := time.NewTimer(quotaCleanupInitialDelay)
	defer delay.Stop()
	select {
	case <-ctx.Done():
		return
	case <-delay.C:
	}

	ticker := time.NewTicker(quotaCleanupInterval)
	defer ticker.Stop()

	for {
		cleaned, err := repo.CleanupExpired(ctx, time.Now())
		switch {
		case err != nil:
			logger.Warn("回收在途超时预留失败，将在下一轮重试", "error", err)
		case cleaned > 0:
			logger.Info("已回收在途超时预留并退还额度", "count", cleaned)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runTrialGrantReclaimer 周期回收到期试用额（把未用完的部分从用户余额扣回）。
//
// 行为约定（与其它后台协程一致）：
//   - 随 ctx 取消立即返回，不阻塞关闭；
//   - 每轮仅在实际回收发生时打 info，正常情况下不产生日志噪音；
//   - 单轮失败只打 warn 并继续下一轮，绝不 panic、不退出进程。
//
// 幂等性由仓储保证（以台账状态为闸门），因此这里不做多实例互斥：
// 两个实例同时跑也只会有一次真正扣款。
func runTrialGrantReclaimer(ctx context.Context, repo model.TrialGrantRepository, logger *slog.Logger) {
	// 首轮不再额外延迟：启动时已经同步回收过一次，这里直接进入周期即可。
	ticker := time.NewTicker(trialReclaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		cleaned, err := repo.ReclaimExpired(ctx, time.Now())
		switch {
		case err != nil:
			logger.Warn("回收到期试用额失败，将在下一轮重试", "error", err)
		case cleaned > 0:
			logger.Info("已回收到期试用额", "count", cleaned)
		}
	}
}

// runChannelHealthWatcher 周期检查渠道健康度，按成功率自动停用不健康渠道。
//
// 为什么放在后台周期而不是请求路径：健康判定要聚合一段窗口的调用统计，
// 属于"慢查询 + 低频"的运维工作，绝不能挂在热路径上拖慢每一次转发。
//
// 行为约定（与其它后台协程一致）：
//   - 随 ctx 取消（进程退出信号）立即返回，不阻塞关闭；
//   - 功能默认关闭（MinRequests=0），未启用时每轮空转即返回、不产生日志噪音；
//   - 单轮失败只打日志、绝不 panic、不退出进程——风控能力的故障不该拖垮主服务。
//
// 只停用不删除：停用可人工一键恢复，删除则不可逆；自动化的边界必须停在这里。
func runChannelHealthWatcher(ctx context.Context, srv *server.Server, logger *slog.Logger) {
	ticker := time.NewTicker(channelHealthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		srv.AutoDisableUnhealthyChannels(ctx)
	}
}

// setupLogger 依据配置构造结构化日志器。
//
// 设计说明：
//   - 使用标准库 log/slog，不引入第三方日志库（可审计、无额外依赖）；
//   - 输出到 stdout：容器与 systemd 环境下由运行时负责收集；
//   - JSON 格式便于日志采集系统（如 ELK/Loki）解析，text 便于人工阅读。
func setupLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Log.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// createAndPrintToken 创建一个访问令牌并把明文打印给运维，随后退出。
//
// 设计取舍：
//   - 创建的令牌为「不限额度、永不过期、不限模型」——M2 尚未实现计费与配额管理，
//     此时若默认给零额度，令牌将完全不可用；这些限制会在计费模块落地后按需求开放配置。
//   - 明文只在此刻输出一次：数据库仅保存 SHA-256 摘要（供查找）与 AES 密文（供后台展示），
//     因此运维必须立即保存，遗失后只能重建。
func createAndPrintToken(ctx context.Context, tokens model.TokenRepository, name, listenAddr string) error {
	key, err := model.GenerateTokenKey()
	if err != nil {
		return fmt.Errorf("生成令牌失败: %w", err)
	}

	token := &model.Token{
		Name:           name,
		Key:            key,
		Status:         model.TokenStatusEnabled,
		UnlimitedQuota: true, // M2 未实现计费，暂不限额度
		// ExpiresAt 为零值 = 永不过期
	}
	if err := tokens.Create(ctx, token); err != nil {
		return fmt.Errorf("保存令牌失败: %w", err)
	}

	fmt.Printf(`已创建访问令牌：

  名称：%s
  令牌：%s
  权限：不限额度（M2 未实现计费）、不限模型、永不过期

调用示例：

  curl http://%s/v1/chat/completions \
    -H "Authorization: Bearer %s" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

⚠ 令牌明文仅此一次展示（数据库只存摘要与密文），请立即妥善保存。
`, token.Name, key, listenAddr, key)

	return nil
}

// createAdminAndPrint 创建管理员账号并打印随机密码，随后退出。
//
// 为什么随机生成密码：手工设定时使用者往往图省事填弱口令，
// 而管理员账号可配置渠道与查看全部数据，一旦被爆破后果严重。
// 随机密码只打印一次，请立即保存并尽快在后台修改为自己的密码。
func createAdminAndPrint(ctx context.Context, users model.UserRepository, username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("用户名不能为空")
	}

	// 18 字节随机数 → 36 位十六进制字符，熵足够且便于复制
	rawPassword := make([]byte, 18)
	if _, err := rand.Read(rawPassword); err != nil {
		return fmt.Errorf("生成随机密码失败: %w", err)
	}
	password := hex.EncodeToString(rawPassword)

	hash, err := crypto.HashPassword(password)
	if err != nil {
		return fmt.Errorf("计算口令哈希失败: %w", err)
	}

	admin := &model.User{
		Username:     username,
		PasswordHash: hash,
		Role:         model.UserRoleAdmin,
		Status:       model.UserStatusEnabled,
		// 管理员不限额度：其调用不应因额度耗尽而被拒绝
		Quota: model.QuotaUnlimited,
	}
	if err := users.Create(ctx, admin); err != nil {
		if errors.Is(err, model.ErrUsernameTaken) {
			return fmt.Errorf("用户名 %q 已被占用", username)
		}
		return fmt.Errorf("创建管理员失败: %w", err)
	}

	fmt.Printf(`已创建管理员账号：

  用户名：%s
  密码：  %s

请立即登录并修改密码（管理后台可配置全部渠道与查看所有数据，
随机密码仅此一次展示，请勿通过聊天工具明文留存）。
`, admin.Username, password)

	return nil
}

// resetPasswordAndReport 重置指定用户的密码，并吊销其全部登录会话。
//
// 设计要点：
//   - newPassword 为空时生成 36 位十六进制随机密码并打印一次（推荐用法，
//     避免明文密码出现在进程列表 `ps` 中）；
//   - 指定明文密码时【不回显】密码本身，只确认"已重置"，避免在终端与
//     日志里留下可被翻出的明文；
//   - 无论哪种方式，都强制吊销该用户的全部会话：改密的语义就是"旧凭据失效"，
//     否则已泄露的会话仍能继续使用，重置密码就失去了意义。
func resetPasswordAndReport(
	ctx context.Context,
	users model.UserRepository,
	sessions model.SessionRepository,
	username, newPassword string,
) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("用户名不能为空")
	}

	user, err := users.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			return fmt.Errorf("用户 %q 不存在", username)
		}
		return fmt.Errorf("查询用户失败: %w", err)
	}

	password := strings.TrimSpace(newPassword)
	generated := false
	if password == "" {
		// 18 字节随机数 → 36 位十六进制，熵足够且便于复制
		raw := make([]byte, 18)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("生成随机密码失败: %w", err)
		}
		password = hex.EncodeToString(raw)
		generated = true
	}
	// 与注册/改密走同一套强度规则，避免出现"重置出来的密码反而注册不了"的不一致
	if err := crypto.ValidatePasswordStrength(password); err != nil {
		return err
	}

	hash, err := crypto.HashPassword(password)
	if err != nil {
		return fmt.Errorf("计算口令哈希失败: %w", err)
	}

	user.PasswordHash = hash
	if err := users.Update(ctx, user); err != nil {
		return fmt.Errorf("更新用户密码失败: %w", err)
	}

	// 吊销全部会话：改密后旧登录态必须失效
	if err := sessions.DeleteByUserID(ctx, user.ID); err != nil {
		return fmt.Errorf("吊销旧会话失败: %w", err)
	}

	if generated {
		// 说明：这里把随机生成的密码打印到 stdout 是刻意的——CLI 子命令没有
		// 其他交付渠道，且这是"仅此一次"的展示（与 gh auth login 同类做法）。
		// 密钥/密码绝不写日志（服务端铁律），本处是管理员手动执行的本地 CLI，
		// 不属于日志落盘路径。下面的消警注释用于告知 CodeQL 这是设计行为。
		// codeql[go/clear-text-logging]
		fmt.Printf(`已重置用户密码（随机生成）：

  用户名：%s
  密码：  %s

随机密码仅此一次展示，请立即登录并妥善保存。
该用户的全部旧登录会话已失效，需重新登录。
`, user.Username, password)
	} else {
		fmt.Printf(`已重置用户密码（使用指定值）：

  用户名：%s

出于安全考虑，此处不回显密码明文。
该用户的全部旧登录会话已失效，需重新登录。
`, user.Username)
	}

	return nil
}
