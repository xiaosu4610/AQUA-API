// Package config 负责 AQUA-API 的配置加载、覆盖与校验。
//
// 意图（Why）：
//
//	把程序所有运行参数集中到一处管理，并实现「默认值 → 配置文件 → 环境变量」三级覆盖。
//	这样做的价值：
//	  1) 单二进制部署时零配置即可启动（默认值兜底）；
//	  2) 复杂部署用 JSON 文件描述；
//	  3) 容器/CI 场景用环境变量注入，无需改文件。
//	环境变量优先级最高，便于临时覆盖与密钥注入（密钥不应写进文件与仓库）。
//
// 流转（Flow）：
//
//	cmd/aqua/main.go
//	  └─ config.Load(path)
//	       ├─ Default()          生成默认值
//	       ├─ loadFile()         读取 JSON 并「局部覆盖」（文件不存在不算错误）
//	       ├─ applyEnv()         环境变量覆盖（AQUA_ 前缀）
//	       └─ Validate()         校验取值合法性
//	     └─ 返回 *Config，注入 store / server / relay 等模块
//
// 扩展（Extend）：
//
//	新增一个配置项时，必须同步修改以下四处，缺一不可：
//	  1) 对应的结构体加字段（并写 json tag）；
//	  2) Default() 里补默认值；
//	  3) applyEnv() 里补环境变量映射（如需要）；
//	  4) Validate() 里补合法性校验（如该字段有取值范围）。
//	安全类字段（如密钥）额外要求：json tag 必须写 "-"（禁止从配置文件读取），
//	只在 applyEnv 中由环境变量注入，绝不允许出现在文件与仓库里。
//	注意：本包只允许依赖标准库，不得引入第三方库，也不得反向依赖 internal 下其他包。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// 默认值常量。
//
// 设计说明：
//   - 默认监听 127.0.0.1 而非 0.0.0.0：确保默认不对外暴露，
//     公网访问必须由使用者显式改成 0.0.0.0 或置于反向代理之后。
//   - 默认使用 SQLite 落在 ./data 目录：零依赖即可跑起来，方便本地与小型部署。
const (
	DefaultListen    = "127.0.0.1:8787" // 默认监听地址
	DefaultMode      = "release"        // 默认运行模式：gin 的 debug/release
	DefaultDBDriver  = "sqlite"         // 默认数据库驱动
	DefaultDBDSN     = "./data/aqua.db" // 默认数据源（SQLite 文件路径）
	DefaultLogLevel  = "info"           // 默认日志级别
	DefaultLogFormat = "text"           // 默认日志格式
	EnvPrefix        = "AQUA_"          // 环境变量统一前缀
	driverSQLite     = "sqlite"         // M1 阶段仅实现该驱动
	driverPostgres   = "postgres"       // 预留（M5+ 生产推荐）
	driverMySQL      = "mysql"          // 预留

	// 邮件发送（注册验证码）相关默认值。
	//
	// 默认指向阿里云邮件推送的 SSL 端口：显式使用 465 而非 25，
	// 因为云厂商普遍封禁 25 端口的出站连接，用 25 会出现"配置看起来正确但永远发不出信"。
	DefaultSMTPHost     = "smtpdm.aliyun.com" // 默认 SMTP 服务器
	DefaultSMTPPort     = 465                 // 默认端口（SSL）
	DefaultSMTPFromName = "AQUA-API"          // 默认发件人显示名
)

// Config 是程序运行所需的全部配置。
//
// 说明：结构体字段与 JSON 一一对应，便于配置文件书写与阅读。
// 该结构在加载完成后被视为只读，运行期不做热更新（热更新见后续里程碑）。
type Config struct {
	Server   ServerConfig   `json:"server"`   // HTTP 服务相关
	Database DatabaseConfig `json:"database"` // 数据库相关
	Log      LogConfig      `json:"log"`      // 日志相关
	SMTP     SMTPConfig     `json:"smtp"`     // 邮件发送相关（口令仅来自环境变量）
	Security SecurityConfig `json:"security"` // 安全相关（密钥仅来自环境变量）
}

// ServerConfig 描述 HTTP 服务的监听与运行模式。
type ServerConfig struct {
	// Listen 是 HTTP 监听地址，形如 "127.0.0.1:8787" 或 "0.0.0.0:8787"。
	Listen string `json:"listen"`
	// Mode 取值 debug / release / test，对应 gin 的运行模式。
	// debug 会输出详细路由与调试信息，生产环境应使用 release。
	Mode string `json:"mode"`
}

// DatabaseConfig 描述数据库连接。
type DatabaseConfig struct {
	// Driver 取值 sqlite / postgres / mysql。
	// 注意：M1 里程碑仅实现 sqlite（纯 Go 驱动，无需 CGO）。
	Driver string `json:"driver"`
	// DSN 是数据源名称：
	//   - sqlite：文件路径，如 ./data/aqua.db
	//   - postgres/mysql：连接串（预留，可能包含密码，日志输出前须脱敏）
	DSN string `json:"dsn"`
}

// LogConfig 描述日志输出。
type LogConfig struct {
	// Level 取值 debug / info / warn / error。
	Level string `json:"level"`
	// Format 取值 text / json。json 便于日志采集系统解析。
	Format string `json:"format"`
}

// SMTPConfig 描述出站邮件（注册验证码等）的发送参数。
//
// 安全约束（重要）：Password 的 json tag 为 "-"，即【不允许】从配置文件读取，
// 只能由环境变量 AQUA_SMTP_PASSWORD 注入。理由与 AppKey 相同——
// 配置文件模板极易被复制、备份甚至误提交，口令一旦落入文件就等同于泄露。
type SMTPConfig struct {
	// Host 是 SMTP 服务器地址，如 smtpdm.aliyun.com。
	Host string `json:"host"`
	// Port 是 SMTP 端口。465 为 SSL 直连（推荐），587 为 STARTTLS。
	// 不建议用 25：云厂商普遍封禁其出站连接。
	Port int `json:"port"`
	// Username 是 SMTP 登录账号（阿里云邮件推送为发信地址本身）。
	Username string `json:"username"`
	// From 是发件人地址，必须与 Username 同域且已在服务商处验证。
	From string `json:"from"`
	// FromName 是收件人看到的发件人显示名。
	FromName string `json:"from_name"`
	// Password 是 SMTP 登录口令，仅由环境变量注入。
	Password string `json:"-"`
}

// Configured 判断邮件发送能力是否可用。
//
// 用途：未配置时注册验证码流程应给出明确指引，而不是在发信时报一个底层网络错误。
// 判定为「可用」需同时具备账号、口令与发件地址三项。
func (s SMTPConfig) Configured() bool {
	return strings.TrimSpace(s.Host) != "" &&
		strings.TrimSpace(s.Username) != "" &&
		strings.TrimSpace(s.Password) != "" &&
		strings.TrimSpace(s.From) != ""
}

// SecurityConfig 描述安全相关配置。
//
// 安全约束（重要）：本结构体的字段【不允许】从配置文件读取，只能由环境变量注入，
// 因此所有字段的 json tag 均为 "-"（encoding/json 会忽略它们）。
// 这样即便有人把配置模板误提交进仓库，也不会泄露密钥。
type SecurityConfig struct {
	// AppKey 是加密渠道密钥的主密钥，来自环境变量 AQUA_APP_KEY。
	//
	// 为什么必须存在：渠道密钥以密文落库，若没有稳定的主密钥就无法解密，
	// 也无法安全地新增渠道。它不应写入配置文件与仓库。
	//
	// 重要提醒：主密钥一旦变更，已加密的渠道密钥将【无法解密】。
	// 因此必须妥善备份并保持稳定；必要时请按"新增密钥版本 + 保留旧密钥"的方式轮换。
	AppKey string `json:"-"`
}

// Default 返回一份带完整默认值的配置。
//
// 设计意图：所有字段都有合理默认，保证「零配置可启动」。
// 返回指针而非值，是为了让调用方（Load）能在其上做局部覆盖。
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen: DefaultListen,
			Mode:   DefaultMode,
		},
		Database: DatabaseConfig{
			Driver: DefaultDBDriver,
			DSN:    DefaultDBDSN,
		},
		Log: LogConfig{
			Level:  DefaultLogLevel,
			Format: DefaultLogFormat,
		},
		SMTP: SMTPConfig{
			Host:     DefaultSMTPHost,
			Port:     DefaultSMTPPort,
			FromName: DefaultSMTPFromName,
			// Username / From / Password 不提供默认值：
			// 它们与具体账号绑定，填错地址比留空更难排查。
		},
		// Security 刻意不提供默认值：加密主密钥必须由使用者显式提供，
		// 若给出固定默认值等于"所有人都用同一把钥匙"，比没有加密更危险。
		Security: SecurityConfig{},
	}
}

// Load 按「默认值 → 配置文件 → 环境变量」的顺序装配配置并校验。
//
// 参数 path 为配置文件路径；为空字符串，或文件不存在时，跳过文件加载（不算错误），
// 直接使用默认值 + 环境变量。这样设计是为了兼容「无配置文件」的极简部署。
//
// 返回的 *Config 已完成校验，调用方可直接使用。
func Load(path string) (*Config, error) {
	cfg := Default()

	// 第一级覆盖：配置文件（局部覆盖默认值）
	if path != "" {
		if err := loadFile(cfg, path); err != nil {
			return nil, err
		}
	}

	// 第二级覆盖：环境变量（优先级最高）
	applyEnv(cfg)

	// 校验：任何非法取值都在启动阶段暴露，避免运行期才出错
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// loadFile 读取 JSON 配置文件并覆盖 cfg 中的对应字段。
//
// 实现要点：json.Unmarshal 只会覆盖文件中「出现过的」字段，
// 未出现的字段保留 Default() 填好的默认值——这正是我们想要的局部覆盖语义。
// 文件不存在时不报错，交由上层使用默认值。
func loadFile(cfg *Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		// 文件不存在属于正常情况（使用默认值即可），不作为错误向上抛
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	// 空文件同样视为「无覆盖」，避免因空文件导致解析错误
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, cfg); err != nil {
		return fmt.Errorf("解析配置文件 %s 失败（请检查 JSON 语法）: %w", path, err)
	}
	return nil
}

// applyEnv 用环境变量覆盖配置。
//
// 命名规则：EnvPrefix + 大写的「结构体名_字段名」，例如：
//
//	AQUA_SERVER_LISTEN、AQUA_SERVER_MODE
//	AQUA_DATABASE_DRIVER、AQUA_DATABASE_DSN
//	AQUA_LOG_LEVEL、AQUA_LOG_FORMAT
//
// 设计说明：只覆盖「非空」的环境变量，空字符串视为「未设置」，
// 避免容器编排里注入空值意外抹掉默认配置。
func applyEnv(cfg *Config) {
	setIfNotEmpty(&cfg.Server.Listen, EnvPrefix+"SERVER_LISTEN")
	setIfNotEmpty(&cfg.Server.Mode, EnvPrefix+"SERVER_MODE")
	setIfNotEmpty(&cfg.Database.Driver, EnvPrefix+"DATABASE_DRIVER")
	setIfNotEmpty(&cfg.Database.DSN, EnvPrefix+"DATABASE_DSN")
	setIfNotEmpty(&cfg.Log.Level, EnvPrefix+"LOG_LEVEL")
	setIfNotEmpty(&cfg.Log.Format, EnvPrefix+"LOG_FORMAT")
	setIfNotEmpty(&cfg.SMTP.Host, EnvPrefix+"SMTP_HOST")
	setIfNotEmptyInt(&cfg.SMTP.Port, EnvPrefix+"SMTP_PORT")
	setIfNotEmpty(&cfg.SMTP.Username, EnvPrefix+"SMTP_USERNAME")
	setIfNotEmpty(&cfg.SMTP.From, EnvPrefix+"SMTP_FROM")
	setIfNotEmpty(&cfg.SMTP.FromName, EnvPrefix+"SMTP_FROM_NAME")
	// 邮件口令与安全类字段一样，只允许来自环境变量（json tag 为 "-"）
	setIfNotEmpty(&cfg.SMTP.Password, EnvPrefix+"SMTP_PASSWORD")
	// 安全类字段只允许来自环境变量（其 json tag 为 "-"，无法从文件读取）
	setIfNotEmpty(&cfg.Security.AppKey, EnvPrefix+"APP_KEY")
}

// setIfNotEmptyInt 是 setIfNotEmpty 的整数版本：解析失败时保留原值。
//
// 刻意不报错：配置项由运维手工注入，写错一个端口号就导致进程无法启动，
// 会让"改错配置 → 服务起不来 → 无从下手"变成常见故障；保留默认值并继续启动更友好。
func setIfNotEmptyInt(dst *int, key string) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return
	}
	*dst = parsed
}

// setIfNotEmpty 在环境变量存在且非空时，将其值写入 dst 指向的字段。
func setIfNotEmpty(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		*dst = v
	}
}

// Validate 校验配置取值的合法性。
//
// 设计原则：宁可启动失败，也不要带着错误配置运行——
// 例如监听地址写错却启动成功，会让使用者误以为服务正常。
// 所有错误信息都明确指出「字段 + 实际值 + 合法取值范围」，便于自助排查。
func (c *Config) Validate() error {
	// 监听地址必须包含端口分隔符，否则 net.Listen 会在运行期才报错
	if c.Server.Listen == "" {
		return fmt.Errorf("配置错误：server.listen 不能为空")
	}
	if !strings.Contains(c.Server.Listen, ":") {
		return fmt.Errorf("配置错误：server.listen=%q 缺少端口，正确格式如 %s", c.Server.Listen, DefaultListen)
	}
	if !oneOf(c.Server.Mode, "debug", "release", "test") {
		return fmt.Errorf("配置错误：server.mode=%q 非法，可选 debug/release/test", c.Server.Mode)
	}

	if !oneOf(c.Database.Driver, driverSQLite, driverPostgres, driverMySQL) {
		return fmt.Errorf("配置错误：database.driver=%q 非法，可选 sqlite/postgres/mysql", c.Database.Driver)
	}
	// M1 只实现了 SQLite，提前拦截以免使用者在运行期困惑
	if c.Database.Driver != driverSQLite {
		return fmt.Errorf("配置错误：database.driver=%q 暂未实现（当前版本仅支持 sqlite）", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("配置错误：database.dsn 不能为空")
	}

	if !oneOf(c.Log.Level, "debug", "info", "warn", "error") {
		return fmt.Errorf("配置错误：log.level=%q 非法，可选 debug/info/warn/error", c.Log.Level)
	}
	if !oneOf(c.Log.Format, "text", "json") {
		return fmt.Errorf("配置错误：log.format=%q 非法，可选 text/json", c.Log.Format)
	}

	// SMTP 校验策略：只校验「填了就一定要合法」，不强制必须填。
	// 理由：不发邮件的部署（如仅用令牌调用）不应被邮件配置卡住启动；
	// 是否真正需要邮件能力由站点开关（注册验证码）在运行期决定。
	if c.SMTP.Port < 0 || c.SMTP.Port > 65535 {
		return fmt.Errorf("配置错误：smtp.port=%d 非法，合法范围 1-65535", c.SMTP.Port)
	}
	if c.SMTP.Host != "" && c.SMTP.Port == 0 {
		return fmt.Errorf("配置错误：已设置 smtp.host 但未设置 smtp.port")
	}
	if (c.SMTP.Username != "" || c.SMTP.Password != "") && !c.SMTP.Configured() {
		return fmt.Errorf("配置错误：SMTP 配置不完整，需同时提供 host/port/username/password/from" +
			"（口令只能通过环境变量 " + EnvPrefix + "SMTP_PASSWORD 注入）")
	}

	// 加密主密钥必须存在：没有它无法解密已存的渠道密钥，也无法安全新增渠道
	if strings.TrimSpace(c.Security.AppKey) == "" {
		return fmt.Errorf("配置错误：缺少加密主密钥，请设置环境变量 %sAPP_KEY（可用 aqua -gen-key 生成一个）", EnvPrefix)
	}

	return nil
}

// oneOf 判断 v 是否属于候选集合，用于枚举型字段校验。
func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// SafeDSN 返回脱敏后的 DSN，专供日志输出使用。
//
// 安全考虑：PostgreSQL/MySQL 的 DSN 通常形如
// "postgres://user:password@host/db"，若原样打进日志会泄露密码。
// 因此日志一律使用本方法，绝不直接打印 Database.DSN。
func (c *Config) SafeDSN() string {
	dsn := c.Database.DSN
	// 仅处理 "scheme://user:pass@host" 形式；SQLite 文件路径不含密码，直接返回
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	schemeEnd := strings.Index(dsn, "://")
	if schemeEnd < 0 {
		return dsn
	}
	credStart := schemeEnd + len("://")
	colon := strings.Index(dsn[credStart:at], ":")
	if colon < 0 {
		return dsn
	}
	// 保留用户名，隐藏密码
	return dsn[:credStart+colon+1] + "******" + dsn[at:]
}
