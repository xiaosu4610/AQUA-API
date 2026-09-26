// 公告接口的单元测试。
//
// 意图（Why）：
//
//	公告接口有两条必须锁死的行为：
//	  1) 公开端只暴露"当前可见"的公告——停用、未到发布时间、已过期的都必须被挡在门外，
//	     一旦放宽就会把草稿/历史内容泄露给用户；
//	  2) 后台的增删改查与校验失败提示要精确（400 而非 500），
//	     否则管理员面对"保存失败"无从判断是哪里填错了。
//
// 流转（Flow）：
//
//	go test ./internal/server/ → httptest 直接调用 Handler（公开接口免鉴权，后台带会话头）
//
// 扩展（Extend）：
//
//	新增公告字段或筛选条件时，按"正常路径 + 校验失败路径"补充用例；
//	时间窗口断言一律用直接写库的固定时间点，避免依赖真实时钟。
package server

import (
	"context"
	"encoding/json"
	"io"
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
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// announcementFixture 汇总公告接口测试所需的仓储与已登录凭据。
type announcementFixture struct {
	srv      *Server
	repo     model.AnnouncementRepository
	adminTok string
	userTok  string
}

// newAnnouncementFixture 构造一个装配完整（含会话鉴权）但不监听端口的服务。
func newAnnouncementFixture(t *testing.T) *announcementFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "announcement_server_test.db")
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
	announcements := store.NewAnnouncementRepository(st.DB())

	ctx := context.Background()
	admin := &model.User{
		Username: "announcement-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled, Quota: model.QuotaUnlimited,
	}
	if err := users.Create(ctx, admin); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	user := &model.User{
		Username: "announcement-user", PasswordHash: "test-hash",
		Role: model.UserRoleUser, Status: model.UserStatusEnabled, Quota: 1000,
	}
	if err := users.Create(ctx, user); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	srv := New(Deps{
		Config:        cfg,
		Store:         st,
		Users:         users,
		Sessions:      sessions,
		Announcements: announcements,
		Settings:      store.NewSettingRepository(st.DB(), st.Dialect()),
	})

	return &announcementFixture{
		srv:      srv,
		repo:     announcements,
		adminTok: createAnnouncementTestSession(t, sessions, admin.ID, "admin"),
		userTok:  createAnnouncementTestSession(t, sessions, user.ID, "user"),
	}
}

// createAnnouncementTestSession 为用户建立一条有效会话并返回其明文令牌。
func createAnnouncementTestSession(t *testing.T, sessions model.SessionRepository, userID uint64, tag string) string {
	t.Helper()
	token := "session-" + strconv.FormatUint(userID, 10) + "-announcement-" + tag
	if err := sessions.Create(context.Background(), &model.Session{
		UserID:    userID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	return token
}

// doAnnouncementJSON 带会话令牌发起一次请求（可选 JSON 请求体），并解析响应体。
func doAnnouncementJSON(t *testing.T, srv *Server, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	parsed := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	}
	return rec, parsed
}

// seedAnnouncement 直接写库一条公告，返回其 ID（用于构造精确的时间窗口场景）。
func seedAnnouncement(t *testing.T, repo model.AnnouncementRepository, item *model.Announcement) uint64 {
	t.Helper()
	if err := repo.Create(context.Background(), item); err != nil {
		t.Fatalf("写入公告失败: %v", err)
	}
	return item.ID
}

// TestAnnouncements_公开端只返回生效公告 覆盖 enabled 与时间窗口过滤。
func TestAnnouncements_公开端只返回生效公告(t *testing.T) {
	fx := newAnnouncementFixture(t)
	now := time.Now()

	seedAnnouncement(t, fx.repo, &model.Announcement{
		Title: "生效中的公告", Content: "正文", Level: model.AnnouncementLevelInfo, Enabled: true,
	})
	seedAnnouncement(t, fx.repo, &model.Announcement{
		Title: "尚未发布", Content: "正文", Level: model.AnnouncementLevelInfo, Enabled: true,
		PublishAt: now.Add(time.Hour),
	})
	seedAnnouncement(t, fx.repo, &model.Announcement{
		Title: "已过期", Content: "正文", Level: model.AnnouncementLevelInfo, Enabled: true,
		ExpireAt: now.Add(-time.Hour),
	})
	seedAnnouncement(t, fx.repo, &model.Announcement{
		Title: "草稿", Content: "正文", Level: model.AnnouncementLevelInfo, Enabled: false,
	})

	// 公开接口无需登录
	rec, body := doAnnouncementJSON(t, fx.srv, http.MethodGet, "/api/announcements", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询公开公告应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("公开端应只返回 1 条生效公告，实际 %d（%v）", len(items), body)
	}
	first, _ := items[0].(map[string]any)
	if first["title"] != "生效中的公告" {
		t.Fatalf("返回的公告标题不正确：%v", first["title"])
	}
	// 公开端不应透出 enabled 字段
	if _, ok := first["enabled"]; ok {
		t.Fatalf("公开端不应返回 enabled 字段：%v", first)
	}
}

// TestAnnouncements_后台增删改查 覆盖创建 → 列表 → 更新 → 删除主链路。
func TestAnnouncements_后台增删改查(t *testing.T) {
	fx := newAnnouncementFixture(t)

	// 未登录访问后台接口必须被拒绝
	rec, _ := doAnnouncementJSON(t, fx.srv, http.MethodGet, "/api/admin/announcements", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带会话应返回 401，实际 %d", rec.Code)
	}
	// 普通用户访问后台接口应被拒绝（403）
	rec, _ = doAnnouncementJSON(t, fx.srv, http.MethodGet, "/api/admin/announcements", fx.userTok, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问后台应返回 403，实际 %d", rec.Code)
	}

	// 创建
	rec, body := doAnnouncementJSON(t, fx.srv, http.MethodPost, "/api/admin/announcements",
		fx.adminTok, `{"title":"维护通知","content":"今晚 2 点维护","level":"warning","pinned":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建公告失败：%d %s", rec.Code, rec.Body.String())
	}
	id, _ := body["id"].(float64)
	if id == 0 {
		t.Fatalf("创建后应返回 ID：%v", body)
	}
	if body["level"] != "warning" || body["pinned"] != true || body["enabled"] != true {
		t.Fatalf("创建结果字段不正确：%v", body)
	}

	// 列表：管理端能看到刚创建的公告
	rec, body = doAnnouncementJSON(t, fx.srv, http.MethodGet, "/api/admin/announcements?page=1&size=20", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询后台列表失败：%d %s", rec.Code, rec.Body.String())
	}
	if total, _ := body["total"].(float64); total != 1 {
		t.Fatalf("列表总数应为 1，实际 %v", body["total"])
	}

	// 更新：只改标题与启用状态（停用即草稿）
	rec, body = doAnnouncementJSON(t, fx.srv, http.MethodPut,
		"/api/admin/announcements/"+strconv.FormatUint(uint64(id), 10),
		fx.adminTok, `{"title":"维护通知（草稿）","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新公告失败：%d %s", rec.Code, rec.Body.String())
	}
	if body["title"] != "维护通知（草稿）" || body["enabled"] != false {
		t.Fatalf("更新结果不正确：%v", body)
	}

	// 停用后应被公开端过滤掉
	_, body = doAnnouncementJSON(t, fx.srv, http.MethodGet, "/api/announcements", "", "")
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("停用的公告不应出现在公开端，实际 %d 条", len(items))
	}

	// 删除
	rec, _ = doAnnouncementJSON(t, fx.srv, http.MethodDelete,
		"/api/admin/announcements/"+strconv.FormatUint(uint64(id), 10), fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除公告失败：%d %s", rec.Code, rec.Body.String())
	}
	rec, _ = doAnnouncementJSON(t, fx.srv, http.MethodDelete,
		"/api/admin/announcements/"+strconv.FormatUint(uint64(id), 10), fx.adminTok, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应返回 404，实际 %d", rec.Code)
	}
}

// TestAnnouncements_校验失败返回400 覆盖标题缺失/超长、语气非法、时间窗口倒置。
func TestAnnouncements_校验失败返回400(t *testing.T) {
	fx := newAnnouncementFixture(t)

	cases := []struct {
		name string
		body string
	}{
		{name: "缺少标题", body: `{"content":"正文"}`},
		{name: "标题为空", body: `{"title":"   ","content":"正文"}`},
		{name: "标题超长", body: `{"title":"` + strings.Repeat("标", 201) + `"}`},
		{name: "语气非法", body: `{"title":"标题","level":"purple"}`},
		{name: "过期早于发布", body: `{"title":"标题","publish_at":2000,"expire_at":1000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := doAnnouncementJSON(t, fx.srv, http.MethodPost, "/api/admin/announcements",
				fx.adminTok, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("非法请求应返回 400，实际 %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
