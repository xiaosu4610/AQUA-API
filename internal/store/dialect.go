// 本文件定义「数据库方言」接缝。
//
// 意图（Why）：
//
//	同一套仓储代码要能在不同数据库上跑，真正的差异只有少数几处：
//	建表语句风格、自增主键写法、时间函数、UPSERT 语法、系统表查询、参数占位符。
//	把这些差异全部收敛到一个 Dialect 里，仓储层只写「统一 SQL」，
//	将来接入新数据库时改动被限制在本文件 + 迁移脚本目录，
//	而不是散落到十几个仓储文件里逐条改语句（那样极易漏改且无法审计）。
//
// 流转（Flow）：
//
//	store.Open(driver, dsn) → 选定 Dialect → Store{dialect, migrations}
//	  ├─ Migrate：从 dialect.MigrationsDir() 读脚本，每条语句先过 dialect.Rewrite
//	  ├─ LatestMigrationVersion：用 dialect.TableExistsSQL / MaxMigrationVersionSQL
//	  └─ 仓储：时间分桶、UPSERT 等少数方言相关语句通过 Dialect 取（不再硬编码）
//
// 扩展（Extend）：
//
//	接入新数据库时按下面清单补齐，缺一不可：
//	  1) 新增 XXDialect 并实现本接口；
//	  2) 新建 migrations/<driver>/NNNN_*.sql，版本号必须与既有目录完全一致
//	     （TestMigrationDirs_AllDialectsShareSameVersions 会校验这条不变量）；
//	  3) 在 driver.go 的注册表里把该驱动的 Available 置为 true；
//	  4) Rewrite 若要真正改写语句（PostgreSQL 的 ? → $1..$n），
//	     必须同时包装事务：仓储里有大量语句是在 *sql.Tx 上执行的，
//	     只包 *sql.DB 会漏掉事务内的语句。
package store

import "fmt"

// Dialect 描述一种数据库的方言差异。
//
// 只收录「当前代码里确实存在差异」的能力，不做抽象预判：
// 每新增一个方法都应当对应一处真实的语句差异，否则就是无用的间接层。
type Dialect interface {
	// Name 返回驱动名（sqlite / mysql / postgres），与配置 database.driver 一致。
	Name() string

	// MigrationsDir 返回该方言的迁移脚本目录（相对嵌入文件系统的根）。
	MigrationsDir() string

	// Rewrite 把仓储层书写的统一 SQL 适配为该方言可执行的形态。
	//
	// SQLite 与 MySQL 的占位符就是 ?，直接返回原串；
	// PostgreSQL 需要把 ? 依次改写为 $1..$n —— 见文件头「扩展」第 4 条。
	Rewrite(sql string) string

	// DayBucket 返回「把 Unix 秒时间列按【本地日期】分桶」的表达式。
	//
	// 为什么必须是本地日期：用量趋势图是给站长自己看的，
	// 按 UTC 分桶会让东八区凌晨 0-8 点的用量被算到前一天，
	// 站长核对当天数据时会对不上账。
	DayBucket(column string) string

	// UpsertSettingSQL 返回设置表「按键插入、已存在则更新」的语句。
	// 参数顺序固定为 (key, value, updated_at)。
	UpsertSettingSQL() string

	// TableExistsSQL 返回判断某张表是否存在的查询（返回一行计数）。
	// 表名由本包内部的常量传入，不接受外部输入，因此可直接拼接。
	TableExistsSQL(table string) string

	// MaxMigrationVersionSQL 返回查询「已登记的最高迁移版本号」的语句。
	// 全新库返回 NULL，调用方按 0 处理。
	MaxMigrationVersionSQL() string
}

// sqliteDialect 是 SQLite 的方言实现（当前唯一已落地的驱动）。
//
// SQLite 的特性决定了下面几处写法：
//   - 无原生布尔类型，布尔语义统一用 INTEGER 0/1；
//   - 自增主键写作 INTEGER PRIMARY KEY AUTOINCREMENT（迁移脚本内）；
//   - 时间函数是 strftime，且支持 'unixepoch' / 'localtime' 修饰符；
//   - UPSERT 用 ON CONFLICT ... DO UPDATE，并可用 excluded 引用待写入值；
//   - 系统表是 sqlite_master。
type sqliteDialect struct{}

func (sqliteDialect) Name() string { return DriverSQLite }

func (sqliteDialect) MigrationsDir() string { return "migrations/" + DriverSQLite }

// Rewrite 对 SQLite 是恒等变换：占位符本来就是 ?。
func (sqliteDialect) Rewrite(sql string) string { return sql }

func (sqliteDialect) DayBucket(column string) string {
	// %% 是字面百分号：这里要生成 '%Y-%m-%d' 而非被 fmt 当作动词
	return fmt.Sprintf("strftime('%%Y-%%m-%%d', %s, 'unixepoch', 'localtime')", column)
}

func (sqliteDialect) UpsertSettingSQL() string {
	return `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
}

func (sqliteDialect) TableExistsSQL(table string) string {
	return fmt.Sprintf("SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='%s'", table)
}

func (sqliteDialect) MaxMigrationVersionSQL() string {
	return "SELECT MAX(version) FROM schema_migrations"
}

// dialectFor 返回驱动对应的方言实现。
//
// 只有「已实现连接」的驱动才会在这里出现；登记了元数据但未实现的驱动
// （见 driver.go）不会走到这里——Open 会先一步给出「暂未开放」的明确提示。
func dialectFor(driver string) (Dialect, error) {
	switch driver {
	case DriverSQLite:
		return sqliteDialect{}, nil
	default:
		return nil, fmt.Errorf("store: 驱动 %q 尚未实现方言适配", driver)
	}
}
