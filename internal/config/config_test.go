// Package config 的单元测试。
//
// 意图（Why）：
//
//	配置错误的代价很高（启动失败或带着错误配置运行），因此对「三级覆盖顺序」
//	与「校验规则」都做显式测试，防止后续改动破坏既有语义。
//	此外专门锁定一条安全不变量：加密主密钥【不能】从配置文件读取。
//
// 流转（Flow）：
//
//	go test ./internal/config/ → 覆盖 Default / loadFile / applyEnv / Validate / SafeDSN
//
// 扩展（Extend）：
//
//	新增配置项时，请在 TestValidate_InvalidCases 补一行非法用例，
//	并在 TestLoad_* 中确认覆盖顺序未被破坏。
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// testAppKey 是测试用的加密主密钥（非真实密钥）。
const testAppKey = "config-test-app-key-0123456789abcdef0123456789"

// neutralizeEnv 清空所有本包关心的环境变量，保证测试不受外部环境影响。
//
// 说明：t.Setenv 设为空串后，applyEnv 会因「空值视为未设置」而跳过，
// 等价于该变量不存在——这正是我们要的隔离效果。
// 注意：会一并清空 AQUA_APP_KEY，因此需要执行 Load/Validate 的用例
// 必须再调用 setTestAppKey 提供主密钥（否则会因缺少主密钥而校验失败）。
func neutralizeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AQUA_SERVER_LISTEN", "AQUA_SERVER_MODE",
		"AQUA_DATABASE_DRIVER", "AQUA_DATABASE_DSN",
		"AQUA_LOG_LEVEL", "AQUA_LOG_FORMAT",
		"AQUA_RELAY_GROUP",
		"AQUA_APP_KEY",
	} {
		t.Setenv(k, "")
	}
}

// setTestAppKey 注入测试用加密主密钥（配置校验要求其非空）。
func setTestAppKey(t *testing.T) {
	t.Helper()
	t.Setenv("AQUA_APP_KEY", testAppKey)
}

// writeConfigFile 在临时目录写入一份配置文件并返回其路径。
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aqua.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

// TestDefault_AllFieldsHaveValues 验证默认配置提供了全部非敏感字段的默认值。
//
// 注意：加密主密钥刻意【没有】默认值（见 TestDefault_RequiresAppKey），
// 因此 Default() 单独调用时不应通过校验。
func TestDefault_AllFieldsHaveValues(t *testing.T) {
	neutralizeEnv(t)
	cfg := Default()

	if cfg.Server.Listen != DefaultListen {
		t.Errorf("默认监听地址 = %q，期望 %q", cfg.Server.Listen, DefaultListen)
	}
	if cfg.Server.Mode != DefaultMode {
		t.Errorf("默认模式 = %q，期望 %q", cfg.Server.Mode, DefaultMode)
	}
	if cfg.Database.Driver != DefaultDBDriver {
		t.Errorf("默认驱动 = %q，期望 %q", cfg.Database.Driver, DefaultDBDriver)
	}
	if cfg.Database.DSN != DefaultDBDSN {
		t.Errorf("默认 DSN = %q，期望 %q", cfg.Database.DSN, DefaultDBDSN)
	}
	if cfg.Log.Level != DefaultLogLevel || cfg.Log.Format != DefaultLogFormat {
		t.Errorf("默认日志配置 = %q/%q，期望 %q/%q",
			cfg.Log.Level, cfg.Log.Format, DefaultLogLevel, DefaultLogFormat)
	}
	// 默认分组必须与迁移初始化的分组一致，否则"零配置启动"会让所有令牌选不到渠道
	if cfg.RelayGroup != DefaultRelayGroup {
		t.Errorf("默认路由分组 = %q，期望 %q", cfg.RelayGroup, DefaultRelayGroup)
	}
	if cfg.Security.AppKey != "" {
		t.Errorf("加密主密钥不应有默认值，实际为 %q", cfg.Security.AppKey)
	}
}

// TestDefault_RequiresAppKey 验证缺少主密钥时校验必须失败。
//
// 这是一条刻意的安全设计：若主密钥可有默认值，等于所有部署共用同一把钥匙，
// 比不加密更危险。宁可启动失败，也不允许"带病运行"。
func TestDefault_RequiresAppKey(t *testing.T) {
	neutralizeEnv(t)
	cfg := Default()

	if err := cfg.Validate(); err == nil {
		t.Fatal("缺少加密主密钥时校验应失败，实际通过")
	}

	// 补上主密钥后应通过
	cfg.Security.AppKey = testAppKey
	if err := cfg.Validate(); err != nil {
		t.Errorf("补齐主密钥后应通过校验，实际: %v", err)
	}
}

// TestLoad_NoConfigFile_UsesDefaults 验证不传配置文件时使用默认值。
func TestLoad_NoConfigFile_UsesDefaults(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Errorf("监听地址 = %q，期望默认值 %q", cfg.Server.Listen, DefaultListen)
	}
	if cfg.Security.AppKey != testAppKey {
		t.Error("加密主密钥未从环境变量注入")
	}
}

// TestLoad_MissingFile_NoError 验证配置文件不存在时不报错（回退默认值）。
//
// 这是刻意的设计：最小部署场景下用户可能完全不提供配置文件。
func TestLoad_MissingFile_NoError(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)

	missing := filepath.Join(t.TempDir(), "not-exist.json")
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("配置文件不存在时不应报错，实际: %v", err)
	}
	if cfg.Database.DSN != DefaultDBDSN {
		t.Errorf("DSN = %q，期望回退默认值 %q", cfg.Database.DSN, DefaultDBDSN)
	}
}

// TestLoad_FileOverridesDefaults 验证配置文件能局部覆盖默认值。
//
// 关键点：文件中只写了 server.listen，其余字段应保留默认值。
func TestLoad_FileOverridesDefaults(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)

	path := writeConfigFile(t, `{"server":{"listen":"0.0.0.0:9000"}}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.Server.Listen != "0.0.0.0:9000" {
		t.Errorf("监听地址 = %q，期望被文件覆盖为 0.0.0.0:9000", cfg.Server.Listen)
	}
	// 未被文件提及的字段应保持默认
	if cfg.Log.Level != DefaultLogLevel {
		t.Errorf("日志级别 = %q，期望保持默认 %q", cfg.Log.Level, DefaultLogLevel)
	}
	if cfg.Database.Driver != DefaultDBDriver {
		t.Errorf("数据库驱动 = %q，期望保持默认 %q", cfg.Database.Driver, DefaultDBDriver)
	}
}

// TestLoad_EnvOverridesFile 验证环境变量优先级高于配置文件。
func TestLoad_EnvOverridesFile(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)
	t.Setenv("AQUA_SERVER_LISTEN", "127.0.0.1:9999")
	t.Setenv("AQUA_LOG_LEVEL", "debug")

	path := writeConfigFile(t, `{"server":{"listen":"0.0.0.0:9000"},"log":{"level":"error"}}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:9999" {
		t.Errorf("监听地址 = %q，期望环境变量覆盖为 127.0.0.1:9999", cfg.Server.Listen)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("日志级别 = %q，期望环境变量覆盖为 debug", cfg.Log.Level)
	}
}

// TestLoad_RelayGroup 验证默认路由分组的三级覆盖语义。
//
// 重点锁定第一条：未配置时取默认值 "default"，保证"不配任何东西"的行为与改动前一致
// （否则所有不带分组的令牌会因找不到渠道而报 503）。
func TestLoad_RelayGroup(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)

	// 1) 不提供配置 → 默认分组
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.RelayGroup != DefaultRelayGroup {
		t.Errorf("未配置时 relay_group = %q，期望默认 %q", cfg.RelayGroup, DefaultRelayGroup)
	}

	// 2) 配置文件覆盖
	path := writeConfigFile(t, `{"relay_group":"free"}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.RelayGroup != "free" {
		t.Errorf("配置文件 relay_group = %q，期望被覆盖为 free", cfg.RelayGroup)
	}

	// 3) 环境变量优先级最高
	t.Setenv("AQUA_RELAY_GROUP", "aqua")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.RelayGroup != "aqua" {
		t.Errorf("环境变量 relay_group = %q，期望覆盖为 aqua", cfg.RelayGroup)
	}
}

// TestLoad_AppKeyCannotComeFromFile 验证加密主密钥无法从配置文件注入。
//
// 这是本包最重要的安全断言：即使攻击者/误操作把密钥写进配置文件并提交，
// 程序也不会读取它，从而避免密钥随仓库泄露。
func TestLoad_AppKeyCannotComeFromFile(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)

	// 配置文件中刻意写入 security.appKey（字段名符合直觉，但 json tag 为 "-"）
	path := writeConfigFile(t, `{"security":{"appKey":"key-from-file-should-be-ignored"}}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.Security.AppKey == "key-from-file-should-be-ignored" {
		t.Fatal("加密主密钥被从配置文件读取了（严重安全问题）")
	}
	if cfg.Security.AppKey != testAppKey {
		t.Errorf("加密主密钥应仅来自环境变量，实际 = %q", cfg.Security.AppKey)
	}
}

// TestLoad_InvalidJSON_ReturnsError 验证非法 JSON 会明确报错而非静默使用默认值。
func TestLoad_InvalidJSON_ReturnsError(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)

	path := writeConfigFile(t, "{ not valid json ")

	if _, err := Load(path); err == nil {
		t.Fatal("非法 JSON 应返回错误，实际返回 nil")
	}
}

// TestLoad_EmptyFile_UsesDefaults 验证空配置文件等价于不提供配置。
func TestLoad_EmptyFile_UsesDefaults(t *testing.T) {
	neutralizeEnv(t)
	setTestAppKey(t)

	path := writeConfigFile(t, "   \n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("空配置文件不应报错，实际: %v", err)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Errorf("监听地址 = %q，期望默认值 %q", cfg.Server.Listen, DefaultListen)
	}
}

// TestValidate_InvalidCases 表驱动覆盖各类非法配置。
//
// 覆盖原则：每个「枚举字段」的非法取值 + 每个「必填字段」为空的情况都应有用例。
func TestValidate_InvalidCases(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config) // 在当前（合法）配置上制造错误
		wantErr bool
	}{
		{
			name:    "监听地址为空",
			mutate:  func(c *Config) { c.Server.Listen = "" },
			wantErr: true,
		},
		{
			name:    "监听地址缺少端口",
			mutate:  func(c *Config) { c.Server.Listen = "127.0.0.1" },
			wantErr: true,
		},
		{
			name:    "运行模式非法",
			mutate:  func(c *Config) { c.Server.Mode = "production" },
			wantErr: true,
		},
		{
			name:    "数据库驱动非法",
			mutate:  func(c *Config) { c.Database.Driver = "oracle" },
			wantErr: true,
		},
		{
			name:    "数据库驱动尚未实现（postgres）",
			mutate:  func(c *Config) { c.Database.Driver = "postgres" },
			wantErr: true,
		},
		{
			name:    "DSN 为空",
			mutate:  func(c *Config) { c.Database.DSN = "" },
			wantErr: true,
		},
		{
			name:    "日志级别非法",
			mutate:  func(c *Config) { c.Log.Level = "verbose" },
			wantErr: true,
		},
		{
			name:    "日志格式非法",
			mutate:  func(c *Config) { c.Log.Format = "xml" },
			wantErr: true,
		},
		{
			name:    "加密主密钥为空",
			mutate:  func(c *Config) { c.Security.AppKey = "" },
			wantErr: true,
		},
		{
			name:    "加密主密钥仅空白",
			mutate:  func(c *Config) { c.Security.AppKey = "   " },
			wantErr: true,
		},
		{
			name:    "默认分组为空",
			mutate:  func(c *Config) { c.RelayGroup = "" },
			wantErr: true,
		},
		{
			name:    "默认分组含大写",
			mutate:  func(c *Config) { c.RelayGroup = "Free" },
			wantErr: true,
		},
		{
			name:    "默认分组含斜杠",
			mutate:  func(c *Config) { c.RelayGroup = "free/vip" },
			wantErr: true,
		},
		{
			name:    "默认分组含空格",
			mutate:  func(c *Config) { c.RelayGroup = "free vip" },
			wantErr: true,
		},
		{
			name:    "合法配置（对照）",
			mutate:  func(c *Config) {},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Security.AppKey = testAppKey // 先补齐必需项，再制造目标错误
			tc.mutate(cfg)

			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("期望校验失败，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("期望校验通过，实际失败: %v", err)
			}
		})
	}
}

// TestSafeDSN_RedactsPassword 验证日志脱敏不会泄露 DSN 中的密码。
func TestSafeDSN_RedactsPassword(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "SQLite 文件路径（无密码，原样返回）",
			dsn:  "./data/aqua.db",
			want: "./data/aqua.db",
		},
		{
			name: "PostgreSQL DSN 隐藏密码",
			dsn:  "postgres://aqua:s3cret@db.local:5432/aqua",
			want: "postgres://aqua:******@db.local:5432/aqua",
		},
		{
			name: "MySQL DSN 隐藏密码",
			dsn:  "mysql://root:pass123@127.0.0.1:3306/aqua",
			want: "mysql://root:******@127.0.0.1:3306/aqua",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Database: DatabaseConfig{Driver: driverSQLite, DSN: tc.dsn}}
			if got := cfg.SafeDSN(); got != tc.want {
				t.Errorf("SafeDSN() = %q，期望 %q", got, tc.want)
			}
		})
	}
}
