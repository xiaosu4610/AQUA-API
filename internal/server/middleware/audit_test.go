// 本文件是 AdminAudit 中间件的单元测试。
//
// 意图（Why）：
//
//	审计中间件有三条最容易出错、后果又最严重的性质，必须用测试钉死：
//	  1) 写操作要记录、GET 不能记录（否则审计表被淹没）；
//	  2) 请求体摘要必须脱敏（否则密钥随审计日志泄露，等于把问题从一处搬到另一处）；
//	  3) 读取摘要后必须还原请求体（否则业务处理器读不到 body，
//	     表现为"一装审计，后台接口全部报请求体格式错误"）。
//
// 流转（Flow）：
//
//	go test ./internal/server/middleware/ → 用 gin 最小路由 + 假仓储验证上述三条
//
// 扩展（Extend）：
//
//	新增脱敏字段名时，在此补一个断言；新增被审计方法时补一个用例。
package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// fakeAuditRepo 是 model.AuditLogRepository 的内存假实现，并发安全。
type fakeAuditRepo struct {
	mu   sync.Mutex
	logs []*model.AuditLog
}

func (f *fakeAuditRepo) Create(_ context.Context, log *model.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, log)
	return nil
}

func (f *fakeAuditRepo) List(_ context.Context, _ model.AuditLogQuery) ([]*model.AuditLog, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs, len(f.logs), nil
}

func (f *fakeAuditRepo) DeleteBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeAuditRepo) snapshot() []*model.AuditLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*model.AuditLog, len(f.logs))
	copy(out, f.logs)
	return out
}

// newAuditTestRouter 组装一个最小路由：前置中间件注入当前用户，再挂审计中间件。
func newAuditTestRouter(repo model.AuditLogRepository, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 模拟 SessionAuth 之后的上下文：审计需要从中取管理员身份
	r.Use(func(c *gin.Context) {
		SetUser(c, &model.User{ID: 7, Username: "root"})
		c.Next()
	})
	r.Use(AdminAudit(repo))
	r.POST("/api/admin/channels/:id", handler)
	r.DELETE("/api/admin/channels/:id", handler)
	r.GET("/api/admin/channels/:id", handler)
	return r
}

// TestAdminAudit_写操作_记录且脱敏且不影响业务读取 覆盖三条核心性质中的第 1、2、3 条。
func TestAdminAudit_写操作_记录且脱敏且不影响业务读取(t *testing.T) {
	repo := &fakeAuditRepo{}

	var gotBody string
	handler := func(c *gin.Context) {
		// 模拟业务处理器读取请求体（若中间件未还原，这里会读到空串）
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("业务处理器读取请求体失败: %v", err)
		}
		gotBody = string(body)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}

	router := newAuditTestRouter(repo, handler)

	payload := `{"name":"渠道A","api_key":"sk-super-secret","password":"p@ssw0rd","models":["m1","m2"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/channels/12", strings.NewReader(payload))
	req.Header.Set("User-Agent", "audit-test-agent")
	req.Header.Set("X-Real-IP", "10.0.0.9")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// 性质 3：业务处理器必须仍能读到完整、未改动的请求体
	if gotBody != payload {
		t.Fatalf("业务处理器读到的请求体被中间件破坏:\n 实际: %s\n 期望: %s", gotBody, payload)
	}

	logs := repo.snapshot()
	if len(logs) != 1 {
		t.Fatalf("写操作应记录 1 条审计日志，实际 %d 条", len(logs))
	}
	entry := logs[0]

	if entry.AdminID != 7 || entry.AdminUsername != "root" {
		t.Errorf("管理员身份记录错误：id=%d name=%q", entry.AdminID, entry.AdminUsername)
	}
	if entry.Method != http.MethodPost {
		t.Errorf("方法记录错误：%q", entry.Method)
	}
	if entry.Path != "/api/admin/channels/12" {
		t.Errorf("路径记录错误：%q", entry.Path)
	}
	if entry.Action == "" {
		t.Errorf("动作描述不应为空")
	}
	if entry.Target != "id=12" {
		t.Errorf("目标记录错误：%q", entry.Target)
	}
	if entry.StatusCode != http.StatusOK {
		t.Errorf("状态码记录错误：%d", entry.StatusCode)
	}
	if entry.ClientIP != "10.0.0.9" {
		t.Errorf("客户端 IP 记录错误：%q", entry.ClientIP)
	}

	// 性质 2：敏感字段被掩码，非敏感字段保留
	if strings.Contains(entry.Detail, "sk-super-secret") || strings.Contains(entry.Detail, "p@ssw0rd") {
		t.Errorf("审计摘要未脱敏，泄露了敏感值：%s", entry.Detail)
	}
	if !strings.Contains(entry.Detail, maskPlaceholder) {
		t.Errorf("审计摘要应包含掩码 %q：%s", maskPlaceholder, entry.Detail)
	}
	if !strings.Contains(entry.Detail, "渠道A") || !strings.Contains(entry.Detail, "m1") {
		t.Errorf("非敏感字段应保留在摘要中：%s", entry.Detail)
	}
}

// TestAdminAudit_GET_不记录 验证只读请求不进入审计。
func TestAdminAudit_GET_不记录(t *testing.T) {
	repo := &fakeAuditRepo{}
	router := newAuditTestRouter(repo, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/channels/3", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := repo.snapshot(); len(got) != 0 {
		t.Fatalf("GET 请求不应被记录，实际记录 %d 条", len(got))
	}
}

// TestAdminAudit_超长请求体_摘要被截断 验证摘要截断到上限以内。
func TestAdminAudit_超长请求体_摘要被截断(t *testing.T) {
	repo := &fakeAuditRepo{}
	router := newAuditTestRouter(repo, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	longValue := strings.Repeat("a", maxAuditDetailRunes*2)
	payload := `{"name":"` + longValue + `"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/channels/99", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	logs := repo.snapshot()
	if len(logs) != 1 {
		t.Fatalf("DELETE 应记录 1 条，实际 %d 条", len(logs))
	}
	if runes := []rune(logs[0].Detail); len(runes) > maxAuditDetailRunes {
		t.Errorf("摘要长度 = %d，应不超过 %d", len(runes), maxAuditDetailRunes)
	}
}
