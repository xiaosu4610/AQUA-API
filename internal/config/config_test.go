// Package config 的单元测试。
//
// 意图（Why）：
//
//	配置错误的代价很高（启动失败或带着错误配置运行），因此对「三级覆盖顺序」
//	与「校验规则」都做显式测试，防止后续改动破坏既有语义。
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

// neutralizeEnv 清空所有本包关心的环境变量，保证测试不受外部环境影响。
//
// 说明：t.Setenv 设为空串后，applyEnv 会因「空值视为未设置」而跳过，
// 等价于该变量不存在——这正是我们要的隔离效果。
func neutralizeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AQUA_SERVER_LISTEN", "AQUA_SERVER_MODE",
		"AQUA_DATABASE_DRIVER", "AQUA_DATABASE_DSN",
		"AQUA_LOG_LEVEL", "AQUA_LOG_FORMAT",
	} {
		t.Setenv(k, "")
	}
}

// TestDefault_AllFieldsHaveValues 验证默认配置不含空字段，保证「零配置可启动」。
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
	// 默认配置本身必须通过校验
	if err := cfg.Validate(); err != nil {
		t.Errorf("默认配置未通过校验: %v", err)
	}
}

// TestLoad_NoConfigFile_UsesDefaults 验证不传配置文件时使用默认值。
func TestLoad_NoConfigFile_UsesDefaults(t *testing.T) {
	neutralizeEnv(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Errorf("监听地址 = %q，期望默认值 %q", cfg.Server.Listen, DefaultListen)
	}
}

// TestLoad_MissingFile_NoError 验证配置文件不存在时不报错（回退默认值）。
//
// 这是刻意的设计：最小部署场景下用户可能完全不提供配置文件。
func TestLoad_MissingFile_NoError(t *testing.T) {
	neutralizeEnv(t)

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

	path := filepath.Join(t.TempDir(), "aqua.json")
	content := `{"server":{"listen":"0.0.0.0:9000"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

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
	t.Setenv("AQUA_SERVER_LISTEN", "127.0.0.1:9999")
	t.Setenv("AQUA_LOG_LEVEL", "debug")

	path := filepath.Join(t.TempDir(), "aqua.json")
	content := `{"server":{"listen":"0.0.0.0:9000"},"log":{"level":"error"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

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

// TestLoad_InvalidJSON_ReturnsError 验证非法 JSON 会明确报错而非静默使用默认值。
func TestLoad_InvalidJSON_ReturnsError(t *testing.T) {
	neutralizeEnv(t)

	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{ not valid json "), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("非法 JSON 应返回错误，实际返回 nil")
	}
}

// TestLoad_EmptyFile_UsesDefaults 验证空配置文件等价于不提供配置。
func TestLoad_EmptyFile_UsesDefaults(t *testing.T) {
	neutralizeEnv(t)

	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

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
			name:    "合法配置（对照）",
			mutate:  func(c *Config) {},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
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
