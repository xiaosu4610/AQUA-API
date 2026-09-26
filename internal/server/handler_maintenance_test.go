// 运维监控与备份接口的单元测试。
//
// 意图（Why）：
//
//	备份相关的行为一旦出错就可能导致「以为有备份、真正需要时却恢复不了」，
//	因此必须锁定三件事：概览能返回核心指标、备份是【可用的 SQLite 快照】、
//	非法上传被明确拒绝。这比覆盖率更重要的是防止危险回归。
//
// 流转（Flow）：
//
//	go test ./internal/server/ → httptest 带管理员会话调用 /api/admin/maintenance/*
//
// 扩展（Extend）：
//
//	新增运维接口时，按「正常路径 + 鉴权 + 异常输入」三类补充用例。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// maintenanceFixture 汇总运维接口测试所需的服务与管理员会话。
//
// handler 是本用例【自建】的路由（叠加与生产一致的鉴权中间件）：
// 运维路由由 registerRoutes 注册（见 router.go），测试不依赖它，
// 从而既能独立验证处理器行为，也不会与日后 router.go 的注册重复冲突。
type maintenanceFixture struct {
	srv      *Server
	handler  http.Handler
	adminTok string
}

// newMaintenanceFixture 构造一个装配完整（含会话鉴权）但不监听端口的服务。
func newMaintenanceFixture(t *testing.T) *maintenanceFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "maintenance_server_test.db")
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())

	admin := &model.User{
		Username: "maintenance-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled,
		Quota: model.QuotaUnlimited,
	}
	if err := users.Create(context.Background(), admin); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}

	srv := New(Deps{
		Config:   cfg,
		Store:    st,
		Users:    users,
		Sessions: sessions,
		Settings: store.NewSettingRepository(st.DB(), st.Dialect()),
	})

	// 自建路由：叠加与生产一致的鉴权链，便于独立测试处理器。
	// Local 中间件保证错误响应语言与生产一致（未携带时回退中文）。
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(middleware.Locale())
	adminGroup := engine.Group("/api/admin")
	adminGroup.Use(middleware.SessionAuth(sessions, users))
	adminGroup.Use(middleware.RequireAdmin())
	adminGroup.Use(middleware.AdminAudit(nil)) // nil 时不写审计（见 AdminAudit 的 nil 分支）
	adminGroup.GET("/maintenance/overview", srv.handleMaintenanceOverview)
	adminGroup.GET("/maintenance/backup", srv.handleMaintenanceBackup)
	adminGroup.POST("/maintenance/backup/inspect", srv.handleMaintenanceInspectBackup)

	return &maintenanceFixture{
		srv:      srv,
		handler:  engine,
		adminTok: createMaintenanceTestSession(t, sessions, admin.ID),
	}
}

// createMaintenanceTestSession 为管理员建立一条有效会话并返回其明文令牌。
func createMaintenanceTestSession(t *testing.T, sessions model.SessionRepository, userID uint64) string {
	t.Helper()
	token := "session-" + strconv.FormatUint(userID, 10) + "-maintenance-test-token"
	if err := sessions.Create(context.Background(), &model.Session{
		UserID:    userID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	return token
}

// doMaintenanceRequest 带会话令牌发起一次请求（可为空体）。
func doMaintenanceRequest(t *testing.T, handler http.Handler, method, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec, body
}

// doMaintenanceUpload 以 multipart/form-data 上传一个文件。
func doMaintenanceUpload(t *testing.T, handler http.Handler, path, token, field, filename string, content []byte) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("构造 multipart 表单失败: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("写入上传内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart 表单失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec, body
}

// TestMaintenanceOverview_RequiresAdmin 验证未登录访问被拒绝。
func TestMaintenanceOverview_RequiresAdmin(t *testing.T) {
	fx := newMaintenanceFixture(t)

	rec, _ := doMaintenanceRequest(t, fx.handler, http.MethodGet, "/api/admin/maintenance/overview", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带会话应返回 401，实际 %d", rec.Code)
	}
}

// TestMaintenanceOverview_ReturnsStats 验证概览返回版本、数据库、表行数与调用健康度。
func TestMaintenanceOverview_ReturnsStats(t *testing.T) {
	fx := newMaintenanceFixture(t)

	rec, body := doMaintenanceRequest(t, fx.handler, http.MethodGet, "/api/admin/maintenance/overview", fx.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("概览接口返回 %d：%s", rec.Code, rec.Body.String())
	}
	if _, ok := body["version"].(string); !ok {
		t.Errorf("响应缺少 version 字段：%v", body)
	}
	if uptime, ok := body["uptime_seconds"].(float64); !ok || uptime < 0 {
		t.Errorf("uptime_seconds = %v，期望非负数", body["uptime_seconds"])
	}

	database, ok := body["database"].(map[string]any)
	if !ok || database["driver"] != "sqlite" {
		t.Fatalf("database = %v，期望 driver=sqlite", body["database"])
	}
	if available, _ := database["size_available"].(bool); !available {
		t.Errorf("SQLite 应能取到数据库体积：%v", database)
	}

	tables, ok := body["tables"].([]any)
	if !ok || len(tables) == 0 {
		t.Fatalf("tables 应为非空数组：%v", body["tables"])
	}
	found := false
	for _, raw := range tables {
		item, _ := raw.(map[string]any)
		if item["name"] == "users" {
			found = true
		}
	}
	if !found {
		t.Errorf("各表行数应包含 users：%v", tables)
	}

	if _, ok := body["usage"].(map[string]any); !ok {
		t.Errorf("响应缺少 usage 字段：%v", body)
	}
}

// TestMaintenanceBackup_DownloadsSQLiteSnapshot 验证导出的确实是一个 SQLite 库文件。
func TestMaintenanceBackup_DownloadsSQLiteSnapshot(t *testing.T) {
	fx := newMaintenanceFixture(t)

	rec, _ := doMaintenanceRequest(t, fx.handler, http.MethodGet, "/api/admin/maintenance/backup", fx.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("备份接口返回 %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q，期望 application/octet-stream", ct)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, ".db") {
		t.Errorf("Content-Disposition = %q，期望 attachment 且含 .db 文件名", disposition)
	}
	// SQLite 文件头固定为 "SQLite format 3\000"
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("SQLite format 3\x00")) {
		t.Fatalf("响应体不是有效的 SQLite 文件，前 16 字节：%q", rec.Body.Bytes()[:min(16, rec.Body.Len())])
	}
}

// TestMaintenanceInspect_ValidBackup 验证「导出 → 上传校验」链路可用。
func TestMaintenanceInspect_ValidBackup(t *testing.T) {
	fx := newMaintenanceFixture(t)

	backup, _ := doMaintenanceRequest(t, fx.handler, http.MethodGet, "/api/admin/maintenance/backup", fx.adminTok)
	if backup.Code != http.StatusOK {
		t.Fatalf("导出备份失败：%d", backup.Code)
	}

	rec, body := doMaintenanceUpload(t, fx.handler, "/api/admin/maintenance/backup/inspect",
		fx.adminTok, maintenanceBackupFormField, "aqua-backup.db", backup.Body.Bytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("校验接口返回 %d：%s", rec.Code, rec.Body.String())
	}
	if valid, _ := body["valid"].(bool); !valid {
		t.Errorf("valid 应为 true：%v", body)
	}
	if version, ok := body["schema_version"].(float64); !ok || int(version) != store.SupportedSchemaVersion() {
		t.Errorf("schema_version = %v，期望 %d", body["schema_version"], store.SupportedSchemaVersion())
	}

	tables, ok := body["tables"].([]any)
	if !ok || len(tables) == 0 {
		t.Fatalf("对比结果应为非空数组：%v", body["tables"])
	}
	usersFound := false
	for _, raw := range tables {
		item, _ := raw.(map[string]any)
		if item["name"] == "users" {
			usersFound = true
			if inBackup, _ := item["in_backup"].(bool); !inBackup {
				t.Errorf("备份应包含 users 表：%v", item)
			}
		}
	}
	if !usersFound {
		t.Errorf("对比结果应包含 users：%v", tables)
	}

	steps, ok := body["restore_steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Errorf("应返回恢复步骤：%v", body["restore_steps"])
	}
}

// TestMaintenanceInspect_RejectsInvalidFile 验证非 SQLite 上传被拒绝。
func TestMaintenanceInspect_RejectsInvalidFile(t *testing.T) {
	fx := newMaintenanceFixture(t)

	rec, _ := doMaintenanceUpload(t, fx.handler, "/api/admin/maintenance/backup/inspect",
		fx.adminTok, maintenanceBackupFormField, "bad.db", []byte("not a sqlite database at all"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法文件应返回 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestMaintenanceInspect_MissingField 验证缺少上传字段时给出明确 400。
func TestMaintenanceInspect_MissingField(t *testing.T) {
	fx := newMaintenanceFixture(t)

	rec, _ := doMaintenanceRequest(t, fx.handler, http.MethodPost, "/api/admin/maintenance/backup/inspect", fx.adminTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少上传文件应返回 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
}
