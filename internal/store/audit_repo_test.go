// 本文件是 auditLogRepository 的单元测试。
//
// 意图（Why）：
//
//	审计查询是"事后追责"的唯一入口，筛选条件写错（例如时间范围边界、
//	路径前缀匹配）会让记录"看起来不存在"，直接摧毁审计的可信度。
//	因此对写入、过滤、分页与清理逐一验证。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 在临时 SQLite 库上真实读写审计表
//
// 扩展（Extend）：
//
//	新增过滤条件时，在此补充对应场景；新增字段时同步更新 sampleAuditLog 的赋值。
package store

import (
	"context"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// sampleAuditLog 构造一条用于测试的审计记录。
func sampleAuditLog(adminID uint64, method, path string, statusCode int, createdAt time.Time) *model.AuditLog {
	return &model.AuditLog{
		AdminID:       adminID,
		AdminUsername: "admin",
		Method:        method,
		Path:          path,
		Action:        method + " " + path,
		Target:        "id=1",
		Detail:        `{"name":"x"}`,
		StatusCode:    statusCode,
		LatencyMS:     12,
		ClientIP:      "10.0.0.1",
		UserAgent:     "test-agent",
		CreatedAt:     createdAt,
	}
}

// TestAuditLogRepository_CreateAndList_场景_预期 验证写入后可按条件过滤并分页。
func TestAuditLogRepository_CreateAndList_场景_预期(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	repo := NewAuditLogRepository(st.DB())

	base := time.Now()
	entries := []*model.AuditLog{
		sampleAuditLog(1, "POST", "/api/admin/channels", 200, base.Add(-3*time.Hour)),
		sampleAuditLog(1, "DELETE", "/api/admin/users/5", 500, base.Add(-2*time.Hour)),
		sampleAuditLog(2, "PUT", "/api/admin/settings", 200, base.Add(-1*time.Hour)),
	}
	for _, entry := range entries {
		if err := repo.Create(ctx, entry); err != nil {
			t.Fatalf("写入审计日志失败: %v", err)
		}
		if entry.ID == 0 {
			t.Fatal("写入后应回填自增 ID")
		}
	}

	// 无过滤：应返回全部，且按 id 倒序（最新写入的在前）
	all, total, err := repo.List(ctx, model.AuditLogQuery{})
	if err != nil {
		t.Fatalf("查询全部审计日志失败: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("无过滤应返回 3 条，实际 total=%d len=%d", total, len(all))
	}
	if all[0].Path != "/api/admin/settings" {
		t.Errorf("应按 id 倒序返回，首条 = %q", all[0].Path)
	}

	adminOne := uint64(1)
	filtered, total, err := repo.List(ctx, model.AuditLogQuery{AdminID: &adminOne})
	if err != nil {
		t.Fatalf("按管理员过滤失败: %v", err)
	}
	if total != 2 || len(filtered) != 2 {
		t.Errorf("按管理员过滤应返回 2 条，实际 total=%d len=%d", total, len(filtered))
	}

	// 路径前缀过滤：应只命中渠道相关记录
	byPrefix, total, err := repo.List(ctx, model.AuditLogQuery{PathPrefix: "/api/admin/channels"})
	if err != nil {
		t.Fatalf("按路径前缀过滤失败: %v", err)
	}
	if total != 1 || byPrefix[0].Path != "/api/admin/channels" {
		t.Errorf("路径前缀过滤异常：total=%d", total)
	}

	// 方法过滤：GET 无记录，应返回 0
	byMethod, total, err := repo.List(ctx, model.AuditLogQuery{Method: "get"})
	if err != nil {
		t.Fatalf("按方法过滤失败: %v", err)
	}
	if total != 0 || len(byMethod) != 0 {
		t.Errorf("GET 应无记录，实际 total=%d", total)
	}

	// 状态码过滤
	statusCode := 500
	byStatus, total, err := repo.List(ctx, model.AuditLogQuery{StatusCode: &statusCode})
	if err != nil {
		t.Fatalf("按状态码过滤失败: %v", err)
	}
	if total != 1 || byStatus[0].Method != "DELETE" {
		t.Errorf("按状态码 500 过滤异常：total=%d", total)
	}

	// 时间范围（闭区间）：[base-2h, base-1h] 应命中后两条
	since := base.Add(-2 * time.Hour)
	until := base.Add(-1 * time.Hour)
	byRange, total, err := repo.List(ctx, model.AuditLogQuery{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("按时间范围过滤失败: %v", err)
	}
	if total != 2 {
		t.Errorf("时间范围过滤应返回 2 条，实际 %d", total)
	}
	if len(byRange) != 2 {
		t.Errorf("时间范围过滤应返回 2 行，实际 %d", len(byRange))
	}

	// 分页：limit=1 应只返回一条，但 total 仍为全量
	paged, total, err := repo.List(ctx, model.AuditLogQuery{Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("分页查询失败: %v", err)
	}
	if total != 3 || len(paged) != 1 {
		t.Errorf("分页查询异常：total=%d len=%d", total, len(paged))
	}
}

// TestAuditLogRepository_DeleteBefore_场景_预期 验证按保留期清理历史记录。
func TestAuditLogRepository_DeleteBefore_场景_预期(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	repo := NewAuditLogRepository(st.DB())

	base := time.Now()
	if err := repo.Create(ctx, sampleAuditLog(1, "POST", "/api/admin/tokens", 200, base.Add(-48*time.Hour))); err != nil {
		t.Fatalf("写入历史记录失败: %v", err)
	}
	if err := repo.Create(ctx, sampleAuditLog(1, "PUT", "/api/admin/tokens/1", 200, base)); err != nil {
		t.Fatalf("写入近期记录失败: %v", err)
	}

	deleted, err := repo.DeleteBefore(ctx, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("清理历史审计日志失败: %v", err)
	}
	if deleted != 1 {
		t.Errorf("应清理 1 条历史记录，实际 %d", deleted)
	}

	_, total, err := repo.List(ctx, model.AuditLogQuery{})
	if err != nil {
		t.Fatalf("清理后查询失败: %v", err)
	}
	if total != 1 {
		t.Errorf("清理后应剩 1 条，实际 %d", total)
	}
}
