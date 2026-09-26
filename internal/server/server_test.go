// HTTP 服务层的单元测试。
//
// 意图（Why）：
//
//	健康检查是自动化运维的判据（负载均衡据此摘流量），其语义必须被测试锁定：
//	依赖正常返回 200、依赖故障返回 503。否则一旦行为悄然改变，线上会出现
//	"流量打向数据库不可用的实例"这类严重问题。
//
// 流转（Flow）：
//
//	go test ./internal/server/ → 用 httptest 直接调用 Handler，无需真实监听端口
//
// 扩展（Extend）：
//
//	新增接口后，按同样方式补充"正常路径 + 异常路径"两类用例。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/relay"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// testEncryptionKey 是测试用密钥材料（非真实密钥）。
const testEncryptionKey = "server-test-key-material-0123456789abcdef0123456789"

// newTestServer 构造一个装配完整、但不监听端口的 HTTP 服务。
//
// 说明：直接使用 httptest 调用 Handler，避免占用真实端口，
// 同时仍覆盖路由、中间件与处理器的完整链路。
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	// 测试期间关闭 gin 的访问日志输出，保持测试结果整洁
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "server_test.db")
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	cipher, err := crypto.New(testEncryptionKey)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}

	cfg := config.Default()
	cfg.Server.Mode = "test"          // 使用 gin 测试模式，抑制调试输出
	cfg.Server.Listen = "127.0.0.1:0" // 端口 0 表示由系统分配，测试中不会被真正使用

	channels := store.NewChannelRepository(st.DB(), cipher)

	srv := New(Deps{
		Config:    cfg,
		Store:     st,
		Channels:  channels,
		Tokens:    store.NewTokenRepository(st.DB(), cipher),
		Users:     store.NewUserRepository(st.DB()),
		Sessions:  store.NewSessionRepository(st.DB()),
		UsageLogs: store.NewUsageLogRepository(st.DB(), st.Dialect()),
		Settings:  store.NewSettingRepository(st.DB(), st.Dialect()),
		Relay:     relay.New(channels, relay.Options{}),
		// 测试不注入前端产物：静态托管由 e2e 冒烟验证覆盖
	})
	return srv, st
}

// doRequest 发送一次请求并返回响应记录器与解析后的 JSON 体。
func doRequest(t *testing.T, srv *Server, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := map[string]any{}
	if rec.Body.Len() > 0 {
		// 解析失败不直接失败测试，交由具体用例判断（例如 404 可能返回空体）
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec, body
}

// TestHealthz_ReturnsOK 验证依赖正常时健康检查返回 200 且字段完整。
func TestHealthz_ReturnsOK(t *testing.T) {
	srv, _ := newTestServer(t)

	rec, body := doRequest(t, srv, http.MethodGet, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if body["status"] != statusOK {
		t.Errorf("status = %v，期望 %q", body["status"], statusOK)
	}
	if body["database"] != dependencyOK {
		t.Errorf("database = %v，期望 %q", body["database"], dependencyOK)
	}
	// 迁移版本应与二进制内置的最高版本一致（不写死数字，避免新增迁移后测试失效）
	if v, ok := body["migration_version"].(float64); !ok || int(v) != store.SupportedSchemaVersion() {
		t.Errorf("migration_version = %v，期望 %d", body["migration_version"], store.SupportedSchemaVersion())
	}
	// 版本字段必须存在（可为 dev）
	if _, ok := body["version"]; !ok {
		t.Error("响应缺少 version 字段")
	}
	// 运行时长必须存在且非负
	if v, ok := body["uptime_seconds"].(float64); !ok || v < 0 {
		t.Errorf("uptime_seconds = %v，期望非负数", body["uptime_seconds"])
	}
}

// TestHealthz_DegradedWhenDatabaseUnavailable 验证数据库不可用时返回 503。
//
// 这是健康检查最关键的行为：宁可让上游摘除流量，也不能把请求转发到
// 无法读写数据的实例上。
func TestHealthz_DegradedWhenDatabaseUnavailable(t *testing.T) {
	srv, st := newTestServer(t)

	// 主动关闭数据库，模拟依赖故障
	if err := st.Close(); err != nil {
		t.Fatalf("关闭测试数据库失败: %v", err)
	}

	rec, body := doRequest(t, srv, http.MethodGet, "/healthz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("数据库不可用时状态码 = %d，期望 503", rec.Code)
	}
	if body["status"] != statusDegraded {
		t.Errorf("status = %v，期望 %q", body["status"], statusDegraded)
	}
	if body["database"] != dependencyFailed {
		t.Errorf("database = %v，期望 %q", body["database"], dependencyFailed)
	}
}

// TestHealthz_DoesNotLeakInternalDetails 验证错误响应不泄露内部信息。
//
// 安全要求：健康检查端点通常无需鉴权，绝不能在其中暴露文件路径、SQL 或驱动错误。
func TestHealthz_DoesNotLeakInternalDetails(t *testing.T) {
	srv, st := newTestServer(t)
	if err := st.Close(); err != nil {
		t.Fatalf("关闭测试数据库失败: %v", err)
	}

	rec, _ := doRequest(t, srv, http.MethodGet, "/healthz")

	raw := rec.Body.String()
	for _, forbidden := range []string{"sql:", "database is closed", ".db", "C:", "D:"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("健康检查响应泄露了内部细节 %q，响应体: %s", forbidden, raw)
		}
	}
}

// TestUnknownPath_Returns404 验证未注册路径返回 404（而不是 500 或空响应）。
//
// 说明：未注入前端产物时，NoRoute 会走"接口不存在"分支并返回 404。
func TestUnknownPath_Returns404(t *testing.T) {
	srv, _ := newTestServer(t)

	rec, _ := doRequest(t, srv, http.MethodGet, "/no-such-endpoint")

	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", rec.Code)
	}
}

// TestToGinMode 验证运行模式映射（未知取值必须回落到 release，避免泄露调试信息）。
func TestToGinMode(t *testing.T) {
	cases := map[string]string{
		"debug":      gin.DebugMode,
		"test":       gin.TestMode,
		"release":    gin.ReleaseMode,
		"":           gin.ReleaseMode, // 空值不应变成 debug
		"unexpected": gin.ReleaseMode, // 未知值一律按 release 处理
	}
	for input, want := range cases {
		if got := toGinMode(input); got != want {
			t.Errorf("toGinMode(%q) = %q，期望 %q", input, got, want)
		}
	}
}
