// 本文件是 model.AuditLogRepository 的 SQL 实现（管理操作审计日志）。
//
// 意图（Why）：
//
//	把后台写操作的审计记录落库，并提供"按管理员/方法/路径/状态码/时间"过滤的
//	分页查询。所有条件一律用占位符拼接，杜绝 SQL 注入——审计表的输入来自
//	请求路径与请求体，是最不该被注入的一类数据（攻击者若能注入，等于直接改审计）。
//
// 流转（Flow）：
//
//	NewAuditLogRepository(db)
//	  ├─ 写入：middleware.AdminAudit → Create
//	  └─ 查询：server.handleAdminListAuditLogs → List（返回 items, total）
//	  └─ 清理：调用方按保留期 → DeleteBefore
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 auditLogColumns / scanAuditLog / Create 列清单。
//	新增过滤条件：在 model.AuditLogQuery 加字段，并在 buildAuditWhere 实现对应条件。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 审计日志列表的分页约束：默认 20，上限 100（与后台其他列表保持一致）。
const (
	defaultAuditListLimit = 20
	maxAuditListLimit     = 100
)

// auditLogColumns 集中定义查询列，顺序必须与 scanAuditLog 的扫描顺序严格一致。
const auditLogColumns = `id, admin_id, admin_username, method, path, action, target,
	detail, status_code, latency_ms, client_ip, user_agent, created_at`

// auditLogRepository 是 model.AuditLogRepository 的 SQL 实现，并发安全。
type auditLogRepository struct {
	db *sql.DB
}

// NewAuditLogRepository 创建审计日志仓储。
func NewAuditLogRepository(db *sql.DB) model.AuditLogRepository {
	return &auditLogRepository{db: db}
}

// Create 写入一条审计日志。
func (r *auditLogRepository) Create(ctx context.Context, log *model.AuditLog) error {
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_logs
			(admin_id, admin_username, method, path, action, target, detail,
			 status_code, latency_ms, client_ip, user_agent, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		log.AdminID, log.AdminUsername, log.Method, log.Path, log.Action, log.Target,
		log.Detail, log.StatusCode, log.LatencyMS, log.ClientIP, log.UserAgent,
		log.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: 写入审计日志失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取审计日志 ID 失败: %w", err)
	}
	log.ID = uint64(id)
	return nil
}

// List 按条件分页查询审计日志，同时返回符合条件的总数。
func (r *auditLogRepository) List(ctx context.Context, q model.AuditLogQuery) ([]*model.AuditLog, int, error) {
	where, args := buildAuditWhere(q)

	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM audit_logs"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计审计日志数量失败: %w", err)
	}

	limit := normalizeLimit(q.Limit, defaultAuditListLimit, maxAuditListLimit)
	offset := normalizeOffset(q.Offset)

	// 按自增主键倒序而非时间倒序：同秒内多条记录用 id 排序才能保证分页稳定
	// （否则翻页可能重复或漏行）。
	sqlText := "SELECT " + auditLogColumns + " FROM audit_logs" + where +
		" ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询审计日志失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]*model.AuditLog, 0, limit)
	for rows.Next() {
		entry, err := scanAuditLog(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: 遍历审计日志结果集失败: %w", err)
	}
	return items, total, nil
}

// DeleteBefore 删除 cutoff 之前的记录，返回删除条数。
func (r *auditLogRepository) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, "DELETE FROM audit_logs WHERE created_at < ?", cutoff.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: 清理审计日志失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取清理条数失败: %w", err)
	}
	return affected, nil
}

// buildAuditWhere 依据查询条件拼装 WHERE 子句与参数（全部参数化，杜绝 SQL 注入）。
func buildAuditWhere(q model.AuditLogQuery) (string, []any) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)

	if q.AdminID != nil {
		conditions = append(conditions, "admin_id = ?")
		args = append(args, *q.AdminID)
	}
	if method := strings.ToUpper(strings.TrimSpace(q.Method)); method != "" {
		conditions = append(conditions, "method = ?")
		args = append(args, method)
	}
	if prefix := strings.TrimSpace(q.PathPrefix); prefix != "" {
		// 前缀匹配；转义 % 与 _ 避免用户输入被当作通配符
		conditions = append(conditions, "path LIKE ? ESCAPE '\\'")
		args = append(args, escapeLike(prefix)+"%")
	}
	if q.StatusCode != nil {
		conditions = append(conditions, "status_code = ?")
		args = append(args, *q.StatusCode)
	}
	if q.Since != nil {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, q.Since.Unix())
	}
	if q.Until != nil {
		// 闭区间终点：<= 使结束那一刻的记录也被包含，与前端"选到某天为止"的直觉一致
		conditions = append(conditions, "created_at <= ?")
		args = append(args, q.Until.Unix())
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// scanAuditLog 把一行数据映射为审计日志对象。
func scanAuditLog(sc rowScanner) (*model.AuditLog, error) {
	var (
		id            uint64
		adminID       uint64
		adminUsername string
		method        string
		path          string
		action        string
		target        string
		detail        string
		statusCode    int
		latencyMS     int
		clientIP      string
		userAgent     string
		createdAt     int64
	)

	if err := sc.Scan(&id, &adminID, &adminUsername, &method, &path, &action, &target,
		&detail, &statusCode, &latencyMS, &clientIP, &userAgent, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取审计日志字段失败: %w", err)
	}

	return &model.AuditLog{
		ID:            id,
		AdminID:       adminID,
		AdminUsername: adminUsername,
		Method:        method,
		Path:          path,
		Action:        action,
		Target:        target,
		Detail:        detail,
		StatusCode:    statusCode,
		LatencyMS:     latencyMS,
		ClientIP:      clientIP,
		UserAgent:     userAgent,
		CreatedAt:     time.Unix(createdAt, 0),
	}, nil
}
