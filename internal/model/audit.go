// 本文件定义「管理操作审计日志」领域模型与仓储接口。
//
// 意图（Why）：
//
//	后台的写操作（建渠道、改设置、删用户、退款……）必须有据可查：
//	"谁、在什么时候、对哪个对象、做了什么、结果如何"。
//	模型层只描述这条记录的形态与查询条件，不感知 HTTP（HTTP 语义在
//	middleware.AdminAudit 里翻译成这些字段），也不写 SQL（由 store 实现）。
//
// 流转（Flow）：
//
//	middleware.AdminAudit 组装 AuditLog → AuditLogRepository.Create
//	  └─ 后台审计页：server.handleAdminListAuditLogs → List（带过滤与分页）
//	  └─ 运维清理：DeleteBefore（按保留期删除历史）
//
// 扩展（Extend）：
//
//	新增记录字段：先在迁移里加列，再同步 model.AuditLog、store 的列清单与扫描函数，
//	  最后在 middleware.AdminAudit 组装处补值，四处同步。
//	新增过滤条件：在 AuditLogQuery 加字段，并在 store 的 buildAuditWhere 实现。
package model

import (
	"context"
	"time"
)

// AuditLog 表示一条后台写操作的审计记录。
//
// 字段设计说明：
//   - AdminUsername 冗余保存：审计的意义是"多年后仍能看懂"，而用户可能被改名或删除；
//     只存 ID 会让历史记录在用户变更后失去可读性；
//   - Detail 是请求体摘要，入库前已由中间件脱敏（敏感字段值替换为 ***）并截断，
//     仓储层不再二次处理——脱敏规则属于应用层策略，改规则不应需要动数据库；
//   - LatencyMS 是包含业务处理在内的总耗时，用于发现"某个后台操作特别慢"。
type AuditLog struct {
	ID            uint64    // 主键
	AdminID       uint64    // 操作管理员 ID（0 表示未识别）
	AdminUsername string    // 操作管理员用户名（冗余保存）
	Method        string    // HTTP 方法（POST/PUT/PATCH/DELETE）
	Path          string    // 实际请求路径（含具体 ID）
	Action        string    // 可读动作描述（如"更新渠道"）
	Target        string    // 路径参数取值（如 "id=12"）
	Detail        string    // 请求体摘要（已脱敏、已截断）
	StatusCode    int       // 回写给客户端的状态码
	LatencyMS     int       // 处理耗时（毫秒）
	ClientIP      string    // 客户端 IP
	UserAgent     string    // User-Agent（已截断）
	CreatedAt     time.Time // 记录时间
}

// AuditLogQuery 描述审计日志的查询与分页条件。
//
// 各字段均为可选：零值（nil / 空串）表示不施加该条件。
// 时间范围语义：Since 为闭区间起点（created_at >= Since），
// Until 为闭区间终点（created_at <= Until），两端都包含。
type AuditLogQuery struct {
	AdminID    *uint64    // 按操作管理员过滤；nil 表示不过滤
	Method     string     // 按 HTTP 方法精确过滤（大写）；空表示不过滤
	PathPrefix string     // 按请求路径前缀过滤；空表示不过滤
	StatusCode *int       // 按响应状态码精确过滤；nil 表示不过滤
	Since      *time.Time // 起始时间（含）；nil 表示不限
	Until      *time.Time // 结束时间（含）；nil 表示不限
	Limit      int        // 条数上限（列表查询使用）
	Offset     int        // 偏移量（列表查询使用）
}

// AuditLogRepository 定义审计日志的持久化操作。
type AuditLogRepository interface {
	// Create 写入一条审计日志。
	Create(ctx context.Context, log *AuditLog) error

	// List 按条件返回审计日志（时间倒序：最新在前），同时返回符合条件的总数。
	//
	// 一次性返回 (items, total) 而非分成两次调用：审计列表总与总数成对使用，
	// 合在一起可避免调用方忘记统计总数、或两次查询条件不一致。
	List(ctx context.Context, q AuditLogQuery) ([]*AuditLog, int, error)

	// DeleteBefore 删除 cutoff 之前（created_at < cutoff）的记录，返回删除条数。
	//
	// 用途：审计表会随时间无限增长，需要一个按保留期清理的入口；
	// 具体保留多久由调用方决定（见 cmd / 定时任务），仓储层不做策略假设。
	DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error)
}
