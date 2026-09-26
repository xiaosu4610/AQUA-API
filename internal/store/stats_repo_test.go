// 运维统计与备份校验查询的单元测试。
//
// 意图（Why）：
//
//	运维概览的真实性依赖这些聚合查询：一旦「各表行数」「失败率」「备份校验」
//	出错，站长会据此做出错误判断（例如以为备份可用而实际不可恢复）。
//	因此用真实 SQLite 库验证「行数正确」「空库不 panic」「非法文件被识别」。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 临时库写入样本 → 断言聚合结果
//
// 扩展（Extend）：
//
//	新增统计维度时，在此补充对应断言。
package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTableRowCounts_ReturnsExistingTables 验证行数统计覆盖核心表且只返回存在的表。
func TestTableRowCounts_ReturnsExistingTables(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.DB().ExecContext(ctx,
		"INSERT INTO users (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)",
		"stats-user", "hash", time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatalf("写入样本用户失败: %v", err)
	}

	stats, err := st.TableRowCounts(ctx)
	if err != nil {
		t.Fatalf("统计表行数失败: %v", err)
	}

	byName := make(map[string]int64, len(stats))
	for _, item := range stats {
		byName[item.Name] = item.Rows
	}
	if rows, ok := byName["users"]; !ok || rows != 1 {
		t.Fatalf("users 行数应为 1，实际 ok=%v rows=%d", ok, rows)
	}
	if _, ok := byName["usage_logs"]; !ok {
		t.Fatalf("统计结果应包含 usage_logs 表：%v", byName)
	}
}

// TestUsageHealth_CountsFailureAndLatency 验证窗口统计的次数、失败数与平均耗时。
func TestUsageHealth_CountsFailureAndLatency(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// 三条近 24 小时记录：两条成功（200/201，耗时 100/300），一条失败（500，耗时 200）；
	// 再加一条 3 天前的失败记录（只应计入 7 天窗口，不计入 24 小时）。
	insertLog := func(status, latency int, age time.Duration) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO usage_logs (status_code, latency_ms, created_at) VALUES (?, ?, ?)`,
			status, latency, now.Add(-age).Unix()); err != nil {
			t.Fatalf("写入样本日志失败: %v", err)
		}
	}
	insertLog(200, 100, time.Hour)
	insertLog(201, 300, 2*time.Hour)
	insertLog(500, 200, 3*time.Hour)
	insertLog(500, 400, 3*24*time.Hour)

	health, err := st.UsageHealth(ctx, now)
	if err != nil {
		t.Fatalf("统计调用健康度失败: %v", err)
	}

	if health.Last24h.Requests != 3 || health.Last24h.Failures != 1 {
		t.Errorf("近 24 小时 requests=%d failures=%d，期望 3/1",
			health.Last24h.Requests, health.Last24h.Failures)
	}
	if got := health.Last24h.AvgLatencyMS; got < 199 || got > 201 {
		t.Errorf("近 24 小时平均耗时 = %v，期望约 200", got)
	}
	if health.Last7d.Requests != 4 || health.Last7d.Failures != 2 {
		t.Errorf("近 7 天 requests=%d failures=%d，期望 4/2",
			health.Last7d.Requests, health.Last7d.Failures)
	}
}

// TestUsageHealth_EmptyDatabase 验证空库统计返回零值而非报错。
func TestUsageHealth_EmptyDatabase(t *testing.T) {
	st := newTestStore(t)

	health, err := st.UsageHealth(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("空库统计不应报错: %v", err)
	}
	if health.Last24h.Requests != 0 || health.Last7d.Requests != 0 {
		t.Errorf("空库应返回 0 次调用，实际 %+v", health)
	}
}

// TestDatabaseSizeBytes_SQLite 验证 SQLite 能给出正数的数据库体积。
func TestDatabaseSizeBytes_SQLite(t *testing.T) {
	st := newTestStore(t)

	size, ok, err := st.DatabaseSizeBytes(context.Background())
	if err != nil {
		t.Fatalf("读取数据库体积失败: %v", err)
	}
	if !ok || size <= 0 {
		t.Fatalf("SQLite 应返回正数体积，实际 ok=%v size=%d", ok, size)
	}
}

// TestDiskUsage_NoError 验证磁盘水位查询在任何平台都不报错。
//
// 说明：Windows 开发环境下返回 Available=false（见 stats_disk_windows.go），
// 因此不断言 Available 的具体取值，只要求「不报错」且「可用时数据自洽」。
func TestDiskUsage_NoError(t *testing.T) {
	st := newTestStore(t)

	disk, err := st.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("磁盘水位查询不应报错: %v", err)
	}
	if disk.Available && disk.FreeBytes > disk.TotalBytes {
		t.Errorf("可用空间不应大于总空间: %+v", disk)
	}
}

// TestInspectSQLiteBackup_RoundTrip 验证对一个真实快照的只读校验。
func TestInspectSQLiteBackup_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.DB().ExecContext(ctx,
		"INSERT INTO users (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)",
		"backup-user", "hash", time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatalf("写入样本用户失败: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := st.DB().ExecContext(ctx, "VACUUM INTO ?", backupPath); err != nil {
		t.Fatalf("生成快照失败: %v", err)
	}

	inspection, err := InspectSQLiteBackup(ctx, backupPath)
	if err != nil {
		t.Fatalf("校验快照失败: %v", err)
	}
	if !inspection.HasSchemaTable {
		t.Error("快照应包含 schema_migrations 表")
	}
	if inspection.SchemaVersion != SupportedSchemaVersion() {
		t.Errorf("快照 schema 版本 = %d，期望 %d", inspection.SchemaVersion, SupportedSchemaVersion())
	}

	var userRows int64 = -1
	for _, item := range inspection.Tables {
		if item.Name == "users" {
			userRows = item.Rows
		}
	}
	if userRows != 1 {
		t.Errorf("快照中 users 行数 = %d，期望 1", userRows)
	}
}

// TestInspectSQLiteBackup_RejectsNonSQLite 验证非 SQLite 文件被明确拒绝。
func TestInspectSQLiteBackup_RejectsNonSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-db.db")
	if err := os.WriteFile(path, []byte("this is definitely not a sqlite database"), 0o600); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	if _, err := InspectSQLiteBackup(context.Background(), path); err == nil {
		t.Fatal("非 SQLite 文件应返回错误，实际返回 nil")
	}
}

// TestSQLiteMainFilePath_ReturnsFile 验证能定位主库文件路径。
func TestSQLiteMainFilePath_ReturnsFile(t *testing.T) {
	st := newTestStore(t)

	path, err := st.SQLiteMainFilePath(context.Background())
	if err != nil {
		t.Fatalf("读取主库文件路径失败: %v", err)
	}
	if path == "" {
		t.Fatal("主库文件路径不应为空")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("主库文件应存在: %v", err)
	}
}
