// Package store 负责数据库连接的建立、连接池配置与结构迁移。
//
// 意图（Why）：
//
//	把「数据库怎么连、结构怎么演进」这件事收敛到一处，让上层（model / server / relay）
//	只依赖抽象，不关心底层是 SQLite 还是 PostgreSQL。
//	另一个重要目的是让单二进制部署开箱可用：SQLite 使用纯 Go 驱动，
//	无需 CGO、无需外部数据库进程，启动时自动建表。
//
// 流转（Flow）：
//
//	cmd/aqua/main.go
//	  └─ store.Open(driver, dsn)     建立连接 + 设置连接池 + 校验可用性
//	       └─ store.Migrate(ctx)     按版本顺序执行迁移（幂等，可重复调用）
//	            └─ 供 store 下的各仓储（C4 起陆续加入）读写数据
//
// 扩展（Extend）：
//
//	新增数据表：
//	  1) 在 schema.sql 末尾追加一段建表语句（禁止改历史语句）；
//	  2) 在本文件 migrations 列表登记一个新版本号，Name 与用途对应。
//	新增数据库驱动：
//	  1) 在 Open 的 switch 中增加分支（当前仅 sqlite，postgres/mysql 为预留）；
//	  2) 注意纯 Go 约束——本机无 gcc，CGO 驱动不可用。
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	// 纯 Go 实现的 SQLite 驱动（注册驱动名 "sqlite"）。
	// 选用它而非 mattn/go-sqlite3 的原因：后者依赖 CGO，本机无 gcc 无法编译。
	_ "modernc.org/sqlite"
)

// migrationsFS 把迁移脚本目录（含各数据库方言的子目录）嵌入二进制，
// 避免运行期依赖外部文件。
//
// 使用 all: 前缀是为了把子目录一并纳入嵌入（默认规则不递归包含目录内容）。
//
//go:embed all:migrations
var migrationsFS embed.FS

// migration 描述一次结构变更。
//
// 版本号必须单调递增且永不复用；每个脚本的 SQL 应保证「可安全重复执行」
// （使用 IF NOT EXISTS 等），这样即使中途失败重跑也不会破坏数据。
type migration struct {
	Version int    // 版本号（与 schema_migrations.version 对应，取自文件名前缀）
	Name    string // 变更名称，用于人工排查（取自文件名下划线之后的部分）
	SQL     string // 该版本的 SQL 语句集
}

// canonicalMigrationsDir 是所有数据库方言共用的「版本号基准目录」。
//
// 为什么需要基准：迁移版本号跨方言一致（同一个 0011 对应同一个结构变更），
// 因此「二进制内置的最高版本号」与方言无关。这里固定取一个目录作为基准，
// 并由 TestMigrationDirs_AllDialectsShareSameVersions 保证各目录的版本号集合一致。
const canonicalMigrationsDir = "migrations/" + DriverSQLite

// canonicalMigrations 是基准目录下的迁移清单，供 SupportedSchemaVersion 使用。
//
// 各 Store 实例会用各自方言的目录重新加载（见 Open）。
var canonicalMigrations []migration

// init 在包初始化阶段加载并校验基准迁移脚本。
//
// 这里使用 panic 是刻意的：脚本已在编译期嵌入，运行期读不到说明二进制损坏，
// 属于不可恢复的编程错误，应尽早暴露，而不是带着错误的迁移集启动。
func init() {
	loaded, err := loadMigrations(canonicalMigrationsDir)
	if err != nil {
		panic(fmt.Sprintf("store: 加载迁移脚本失败: %v", err))
	}
	canonicalMigrations = loaded
}

// loadMigrations 读取指定目录下全部嵌入的迁移脚本，校验后按版本号升序返回。
//
// dir 由 Dialect.MigrationsDir() 提供，形如 "migrations/sqlite"。
func loadMigrations(dir string) ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录 %s 失败: %w", dir, err)
	}

	result := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries)) // 版本号 → 文件名，用于查重

	for _, entry := range entries {
		// 子目录（其他方言的迁移）不属于本方言，跳过
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version, name, err := parseMigrationFileName(entry.Name())
		if err != nil {
			return nil, err
		}
		// 版本号重复会导致迁移顺序不确定，必须直接拒绝
		if prev, duplicated := seen[version]; duplicated {
			return nil, fmt.Errorf("迁移版本号 %d 重复：%s 与 %s", version, prev, entry.Name())
		}
		seen[version] = entry.Name()

		raw, err := migrationsFS.ReadFile(dir + "/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("读取迁移脚本 %s 失败: %w", entry.Name(), err)
		}
		result = append(result, migration{Version: version, Name: name, SQL: string(raw)})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("未在 %s 找到任何迁移脚本（目录为空？）", dir)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	return result, nil
}

// parseMigrationFileName 解析形如 "0001_init.sql" 的文件名。
//
// 返回版本号与名称；命名不合规时返回错误（宁可启动失败，也不静默跳过脚本——
// 静默跳过会导致线上缺表，故障现场极难定位）。
func parseMigrationFileName(fileName string) (int, string, error) {
	base := strings.TrimSuffix(fileName, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("迁移脚本命名不合规（应形如 NNNN_名称.sql）: %s", fileName)
	}

	version, err := strconv.Atoi(parts[0])
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("迁移脚本版本号非法（应为正整数）: %s", fileName)
	}
	return version, parts[1], nil
}

// Store 是数据库访问的门面，持有连接池。
//
// 使用方式：由 main 创建一次，注入给各仓储；进程退出前调用 Close 释放。
type Store struct {
	db     *sql.DB
	driver string
	// dialect 是本次连接的方言实现：迁移执行与少数方言相关语句都经由它。
	dialect Dialect
	// migrations 是本方言的迁移清单（Open 时按 dialect.MigrationsDir() 加载）。
	migrations []migration
}

// ErrNotConfigured 表示数据库尚未配置。
//
// 典型场景：全新部署还没走安装向导。上层识别到该错误后应转入「安装模式」
// （只提供安装接口与安装页面），而不是把它当成连接失败去重试。
var ErrNotConfigured = errors.New("store: 数据库尚未配置（请先访问 /install 完成安装）")

// sqlitePragmas 是 SQLite 的关键运行参数。
//
// 逐项说明（这些设置直接影响正确性与并发表现，不要随意删除）：
//   - journal_mode(WAL)：写前日志模式，允许「一写多读」并发，显著减少锁冲突；
//   - busy_timeout(5000)：遇到锁时最多等待 5 秒再报错，避免瞬时并发直接失败；
//   - foreign_keys(1)：SQLite 默认不强制外键，需显式开启才能保证引用完整性；
//   - synchronous(NORMAL)：WAL 下的推荐档位，在安全性与写入性能间取得平衡。
//
// 注意：这些参数是「按连接」生效的，因此通过 DSN 传递，
// 而不是在某个连接上执行一次 PRAGMA（连接池会新建连接，那样设置会丢失）。
const sqlitePragmas = "_pragma=journal_mode(WAL)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)" +
	"&_pragma=synchronous(NORMAL)"

// Open 建立数据库连接并完成基础校验。
//
// 参数：
//   - driver：数据库类型，取值见 driver.go 的驱动常量；
//   - dsn：数据源。SQLite 为文件路径（如 ./data/aqua.db），
//     目录不存在时会自动创建，保证「零准备启动」。
//
// 返回的 *Store 已可直接使用；调用方应在退出前调用 Close。
func Open(driver, dsn string) (*Store, error) {
	// 未配置：全新部署还没走安装向导时会走到这里，
	// 返回可识别的哨兵错误，让上层转入安装模式，而不是抛一个看不懂的连接错误。
	if strings.TrimSpace(driver) == "" {
		return nil, ErrNotConfigured
	}

	switch driver {
	case DriverSQLite:
		return openSQLite(dsn)
	case DriverMySQL, DriverPostgres:
		// 元数据已在 driver.go 登记（安装向导不会提供该选项），但连接实现尚未落地。
		// 这里给出明确提示，避免使用者按元数据填了配置却得到一个模糊的连接失败。
		return nil, fmt.Errorf("store: 驱动 %q 的接入尚未完成，当前可用：%s", driver, AvailableDriverNames())
	default:
		return nil, fmt.Errorf("store: 未知的数据库驱动 %q（可用：%s）", driver, strings.Join(DriverKeys(), "、"))
	}
}

// newStore 装配 Store：选定方言实现并加载该方言的迁移脚本。
//
// 失败时会关闭已建立的连接，避免调用方拿到「既报错又漏连接」的半成品。
func newStore(db *sql.DB, driver string) (*Store, error) {
	dialect, err := dialectFor(driver)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	list, err := loadMigrations(dialect.MigrationsDir())
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, driver: driver, dialect: dialect, migrations: list}, nil
}

// openSQLite 打开 SQLite 数据库（含目录自动创建与连接池配置）。
func openSQLite(dsn string) (*Store, error) {
	realDSN, err := prepareSQLiteDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", realDSN)
	if err != nil {
		return nil, fmt.Errorf("store: 打开 SQLite 失败: %w", err)
	}

	// 连接池配置。
	//
	// 为什么不设成 1：网关是高并发读场景，串行化会严重影响吞吐。
	// 为什么不能设太大：SQLite 的写操作本质是串行的，连接过多只会增加锁竞争。
	// 取 4 是在「读并发」与「锁竞争」之间折中；配合 busy_timeout 可覆盖绝大多数场景。
	// 后续里程碑若出现写热点，会引入独立写通道进一步优化。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0) // 连接长期复用，不主动过期（本地文件数据库无需重连）

	// 通过一次 Ping 验证连接确实可用（例如文件权限不足等问题会在此暴露）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 连接 SQLite 失败（请检查文件路径与读写权限）: %w", err)
	}

	return newStore(db, DriverSQLite)
}

// prepareSQLiteDSN 规范化 SQLite 的 DSN，并在必要时创建数据目录。
//
// 处理逻辑：
//   - 若调用方已提供 "file:" 形式的 DSN，则原样使用（尊重高级用法）；
//   - 否则视为文件路径：自动创建其父目录，并补上 pragma 参数。
func prepareSQLiteDSN(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "file:") {
		return dsn, nil
	}

	// 自动创建父目录：用户写 ./data/aqua.db 时无需手动 mkdir
	dir := filepath.Dir(dsn)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("store: 创建数据目录 %s 失败: %w", dir, err)
		}
	}

	// 统一使用斜杠，保证 Windows 路径也能被 URI 正确解析
	return "file:" + filepath.ToSlash(dsn) + "?" + sqlitePragmas, nil
}

// DB 返回底层连接池，供同包内的仓储实现使用。
//
// 设计说明：不导出给包外使用，避免上层绕过仓储直接拼 SQL（破坏分层）。
func (s *Store) DB() *sql.DB {
	return s.db
}

// Driver 返回当前数据库类型，便于上层做少量差异化处理（如占位符风格）。
func (s *Store) Driver() string {
	return s.driver
}

// Dialect 返回当前的方言实现。
//
// 为什么暴露它：极少数仓储里存在与方言相关的语句（按天分桶、UPSERT），
// 这些仓储构造时需要方言；只写通用 SQL 的仓储不需要，也就不传。
func (s *Store) Dialect() Dialect {
	return s.dialect
}

// Close 关闭连接池。进程退出前应调用。
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// migrationsTableDDL 用于记录已执行的迁移版本。
//
// 单独定义而不放进 schema.sql，是因为它必须先于任何迁移存在——
// 否则第一次迁移时就没有地方登记版本号。
const migrationsTableDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
);`

// Migrate 按版本顺序执行尚未应用的迁移。
//
// 幂等性保证：
//   - 每个版本执行前先查询 schema_migrations，已存在则跳过；
//   - 单次迁移在事务中执行（DDL 与版本登记同成败），避免出现
//     「表建好了但版本没登记」导致下次重复执行的不一致状态。
//
// 可在每次启动时安全调用。
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, migrationsTableDDL); err != nil {
		return fmt.Errorf("store: 创建迁移记录表失败: %w", err)
	}

	for _, m := range s.migrations {
		applied, err := s.isMigrationApplied(ctx, m.Version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// isMigrationApplied 查询指定版本是否已执行。
func (s *Store) isMigrationApplied(ctx context.Context, version int) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(1) FROM schema_migrations WHERE version = ?", version).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: 查询迁移版本 %d 失败: %w", version, err)
	}
	return count > 0, nil
}

// applyMigration 在单个事务内执行一个版本的迁移并登记版本号。
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启迁移事务失败（版本 %d）: %w", m.Version, err)
	}
	// 失败时回滚；成功时 Commit 后此调用为无操作
	defer func() { _ = tx.Rollback() }()

	// 语句先过方言改写：SQLite / MySQL 是恒等变换，
	// PostgreSQL 需要把 ? 变成 $1..$n（见 dialect.go 的扩展说明）。
	if _, err := tx.ExecContext(ctx, s.dialect.Rewrite(m.SQL)); err != nil {
		return fmt.Errorf("store: 执行迁移 %s（版本 %d）失败: %w", m.Name, m.Version, err)
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		m.Version, m.Name, time.Now().Unix()); err != nil {
		return fmt.Errorf("store: 登记迁移版本 %d 失败: %w", m.Version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交迁移（版本 %d）失败: %w", m.Version, err)
	}
	return nil
}

// SupportedSchemaVersion 返回当前二进制内置的最高迁移版本号。
//
// 用途：让调用方（启动日志、健康检查、测试）能判断"数据库结构是否已追平程序"，
// 避免在测试或代码中写死版本号——每新增一个迁移都要改多处，很容易漏改。
//
// 注意：迁移清单在 init() 中加载，因此本函数在包加载完成后始终可用。
func SupportedSchemaVersion() int {
	if len(canonicalMigrations) == 0 {
		return 0
	}
	return canonicalMigrations[len(canonicalMigrations)-1].Version
}

// LatestMigrationVersion 返回已登记的最高迁移版本号，供健康检查展示。
//
// 说明：全新数据库在 Migrate 之前调用会返回 0。
func (s *Store) LatestMigrationVersion(ctx context.Context) (int, error) {
	// 表可能尚未创建，此时视为版本 0（不是错误）。
	// 判断表是否存在必须走方言：SQLite 查 sqlite_master，其他库查各自的系统表。
	var exists int
	if err := s.db.QueryRowContext(ctx, s.dialect.TableExistsSQL("schema_migrations")).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: 检查迁移表是否存在失败: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}

	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, s.dialect.MaxMigrationVersionSQL()).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: 查询最高迁移版本失败: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}
