// Package store 的单元测试。
//
// 意图（Why）：
//
//	存储层是数据安全的底座，一旦迁移逻辑出错（重复执行、版本错乱、漏建索引），
//	后果难以挽回。因此对「目录自动创建」「迁移幂等」「结构正确」做显式验证。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 在临时目录创建真实 SQLite 库并执行迁移
//
// 扩展（Extend）：
//
//	新增迁移版本后，请在 TestMigrate_CreatesExpectedTables 中补充结构断言。
package store

import (
	"context"
	"path/filepath"
	"testing"
)

// newTestStore 在临时目录中创建一个已迁移完成的数据库，测试结束自动清理。
//
// 说明：使用真实 SQLite 文件而非 mock——迁移语句的语法正确性、PRAGMA 是否生效、
// 索引是否建立，只有在真实数据库上才能验证。
func newTestStore(t *testing.T) *Store {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "test.db")
	st, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	return st
}

// TestOpen_AutoCreatesDataDir 验证数据目录不存在时会自动创建。
//
// 这是「零准备启动」承诺的一部分：用户只需给出路径，无需手动 mkdir。
func TestOpen_AutoCreatesDataDir(t *testing.T) {
	// 刻意使用两层不存在的子目录
	dsn := filepath.Join(t.TempDir(), "nested", "deeper", "aqua.db")

	st, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("Open 应在目录不存在时自动创建，实际报错: %v", err)
	}
	defer func() { _ = st.Close() }()

	// 迁移能成功即证明文件确实可写
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
}

// TestOpen_UnsupportedDriver 验证未实现的驱动会明确报错而非静默降级。
func TestOpen_UnsupportedDriver(t *testing.T) {
	if _, err := Open("oracle", "whatever"); err == nil {
		t.Fatal("不支持的驱动应返回错误，实际返回 nil")
	}
}

// TestMigrate_CreatesExpectedTables 验证迁移后核心表与索引均已建立。
func TestMigrate_CreatesExpectedTables(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 表存在性
	for _, table := range []string{"schema_migrations", "channels"} {
		var count int
		err := st.DB().QueryRowContext(ctx,
			"SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name = ?", table).Scan(&count)
		if err != nil {
			t.Fatalf("查询表 %s 失败: %v", table, err)
		}
		if count != 1 {
			t.Errorf("表 %s 未创建", table)
		}
	}

	// 索引存在性（路由热路径依赖它）
	for _, idx := range []string{"idx_channels_group_status", "idx_channels_type"} {
		var count int
		err := st.DB().QueryRowContext(ctx,
			"SELECT COUNT(1) FROM sqlite_master WHERE type='index' AND name = ?", idx).Scan(&count)
		if err != nil {
			t.Fatalf("查询索引 %s 失败: %v", idx, err)
		}
		if count != 1 {
			t.Errorf("索引 %s 未创建", idx)
		}
	}

	// 版本号应已登记为 1
	version, err := st.LatestMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("查询迁移版本失败: %v", err)
	}
	if version != 1 {
		t.Errorf("迁移版本 = %d，期望 1", version)
	}
}

// TestMigrate_Idempotent 验证重复执行迁移是安全的（可每次启动都调用）。
func TestMigrate_Idempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 再次执行三次，均不应报错，且版本保持不变
	for i := 0; i < 3; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("第 %d 次重复迁移失败: %v", i+1, err)
		}
	}

	version, err := st.LatestMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("查询迁移版本失败: %v", err)
	}
	if version != 1 {
		t.Errorf("重复迁移后版本 = %d，期望仍为 1", version)
	}

	// 迁移记录不应重复插入
	var count int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(1) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("统计数据失败: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations 记录数 = %d，期望 1（不应重复登记）", count)
	}
}

// TestLatestMigrationVersion_BeforeMigrate 验证未迁移的全新库返回版本 0。
func TestLatestMigrationVersion_BeforeMigrate(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fresh.db")
	st, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	version, err := st.LatestMigrationVersion(context.Background())
	if err != nil {
		t.Fatalf("迁移前查询版本不应报错，实际: %v", err)
	}
	if version != 0 {
		t.Errorf("全新库版本 = %d，期望 0", version)
	}
}

// TestSQLitePragmas_AreApplied 验证关键 PRAGMA 通过 DSN 生效。
//
// 重点验证 WAL：它是 SQLite 支持「一写多读」并发的基础，
// 若未生效，网关在高并发下会频繁出现 database is locked。
func TestSQLitePragmas_AreApplied(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	var journalMode string
	if err := st.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("查询 journal_mode 失败: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q，期望 wal", journalMode)
	}

	var foreignKeys int
	if err := st.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("查询 foreign_keys 失败: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d，期望 1（已开启）", foreignKeys)
	}
}

// TestChannelsTable_RejectsNoRequiredFields 验证表结构约束生效。
//
// 目的：确认 NOT NULL 约束真实起作用，避免出现「写入空渠道」的脏数据。
func TestChannelsTable_RejectsNoRequiredFields(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 缺少 name/type/created_at/updated_at 等必填字段，应被拒绝
	_, err := st.DB().ExecContext(ctx, "INSERT INTO channels (base_url) VALUES ('https://example.com')")
	if err == nil {
		t.Fatal("缺少必填字段的插入应被拒绝，实际成功")
	}
}
