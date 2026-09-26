// Package main 是 AQUA-API 的程序入口。
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
//	  ├─ 解析命令行参数（-config / -version / -gen-key）
//	  ├─ config.Load(path)                 加载并按「默认值→文件→环境变量」合并配置
//	  ├─ setupLogger(cfg)                  初始化结构化日志（slog）
//	  ├─ store.Open + store.Migrate        建立数据库连接并执行结构迁移
//	  ├─ crypto.New(cfg.Security.AppKey)   构造加解密器（保护渠道密钥）
//	  ├─ store.NewChannelRepository        渠道仓储（读写时自动加解密）
//	  ├─ relay.New                         转发引擎
//	  ├─ server.New                        装配 HTTP 路由与中间件
//	  └─ server.Run(ctx)                   启动监听，收到退出信号后优雅关闭
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

	aqua "gitee.com/xiaosu4610/aqua-api"
	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/relay"
	"gitee.com/xiaosu4610/aqua-api/internal/server"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
	"gitee.com/xiaosu4610/aqua-api/internal/version"
)

// 默认配置文件路径。
//
// 约定：文件不存在时不算错误（回退默认值 + 环境变量），
// 因此默认值可以放心地指向一个"通常存在但允许不存在"的路径。
const defaultConfigPath = "aqua.json"

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
	)
	flag.Parse()

	// ── 子命令：打印版本 ────────────────────────────────────────
	if *showVersion {
		info := version.Get()
		fmt.Printf("AQUA-API %s (commit=%s, built=%s)\n", info.Version, info.GitCommit, info.BuildTime)
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
	logger.Info("AQUA-API 启动中",
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
	tokens := store.NewTokenRepository(st.DB(), cipher)
	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())
	usageLogs := store.NewUsageLogRepository(st.DB())
	settings := store.NewSettingRepository(st.DB())

	// 启动时清理过期会话：会话表随登录次数持续增长，不清理会无限膨胀。
	// 清理失败不阻断启动（这只是维护动作，不影响核心功能）。
	if cleaned, err := sessions.DeleteExpired(ctx, time.Now()); err != nil {
		logger.Warn("清理过期会话失败", "error", err)
	} else if cleaned > 0 {
		logger.Info("已清理过期会话", "count", cleaned)
	}

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

	// 检查是否存在管理员：没有则给出明确指引（不自动创建，避免生成"弱口令管理员"）
	adminCount, err := users.CountAdmins(ctx)
	if err != nil {
		logger.Warn("统计管理员数量失败", "error", err)
	} else if adminCount == 0 {
		logger.Warn("系统中尚不存在管理员账号，无法登录管理后台。请执行：" +
			"aqua -init-admin <用户名>（会生成随机密码并打印一次）")
	}

	relayEngine := relay.New(channels, relay.Options{
		UsageLogs: usageLogs,
		Tokens:    tokens,
	})

	srv := server.New(server.Deps{
		Config:    cfg,
		Store:     st,
		Channels:  channels,
		Tokens:    tokens,
		Users:     users,
		Sessions:  sessions,
		UsageLogs: usageLogs,
		Settings:  settings,
		Relay:     relayEngine,
		// 前端构建产物（web/dist）已通过根包的 go:embed 嵌入二进制
		WebFS: aqua.WebDist,
	})

	logger.Info("HTTP 服务已就绪，等待请求", "addr", cfg.Server.Listen)

	// ── 启动并阻塞 ──────────────────────────────────────────────
	// Run 在收到退出信号后会优雅关闭并返回 nil；
	// 仅在监听失败等异常情况下返回错误。
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	}

	logger.Info("AQUA-API 已退出")
	return nil
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
