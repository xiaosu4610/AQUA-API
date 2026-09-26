// 本文件实现管理后台的操作审计查询接口。
//
// 意图（Why）：
//
//	审计日志被写入后必须能查，否则等于没记。后台审计页需要按"谁（admin_id）、
//	做了什么（method / path）、结果如何（status_code）、什么时间（start / end）"
//	组合筛选，并按时间倒序分页浏览。
//
// 流转（Flow）：
//
//	/api/admin/audit-logs → SessionAuth → RequireAdmin → handleAdminListAuditLogs
//	  └─ 解析分页与筛选 → s.deps.Audit.List → {items, total, page, page_size}
//
// 扩展（Extend）：
//
//	新增筛选条件：在此解析查询参数，并在 model.AuditLogQuery 与 store.buildAuditWhere 同步。
//	新增展示字段：在 auditLogDTO 与其组装函数 toAuditLogDTO 补字段。
package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// 审计日志分页默认值与上限（与后台其他列表保持一致：默认 20，上限 100）。
const (
	defaultAuditPageSize = 20
	maxAuditPageSize     = 100
)

// auditLogDTO 是审计日志的对外表示。
//
// 时间统一输出 Unix 秒（与契约中其他时间字段一致），由前端本地化格式化。
type auditLogDTO struct {
	ID            uint64 `json:"id"`
	AdminID       uint64 `json:"admin_id"`
	AdminUsername string `json:"admin_username"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Action        string `json:"action"`
	Target        string `json:"target"`
	Detail        string `json:"detail"`
	StatusCode    int    `json:"status_code"`
	LatencyMS     int    `json:"latency_ms"`
	ClientIP      string `json:"client_ip"`
	UserAgent     string `json:"user_agent"`
	CreatedAt     int64  `json:"created_at"`
}

// handleAdminListAuditLogs 返回后台操作审计日志（分页 + 多条件筛选）。
func (s *Server) handleAdminListAuditLogs(c *gin.Context) {
	page, size, offset := parseAuditPagination(c)

	query := model.AuditLogQuery{Limit: size, Offset: offset}

	if raw := strings.TrimSpace(c.Query("admin_id")); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			query.AdminID = &parsed
		}
	}
	if method := strings.ToUpper(strings.TrimSpace(c.Query("method"))); method != "" {
		query.Method = method
	}
	if pathPrefix := strings.TrimSpace(c.Query("path")); pathPrefix != "" {
		query.PathPrefix = pathPrefix
	}
	if raw := strings.TrimSpace(c.Query("status_code")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			query.StatusCode = &parsed
		}
	}
	// start / end 接受 Unix 秒、RFC3339，以及界面 datetime-local 产生的
	// "2006-01-02T15:04" / "2006-01-02" 形式。解析失败时明确报错，
	// 避免"筛选条件被静默忽略、管理员却以为筛过了"。
	if raw := strings.TrimSpace(c.Query("start")); raw != "" {
		parsed, err := parseAuditTime(raw)
		if err != nil {
			oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_start_time")
			return
		}
		query.Since = &parsed
	}
	if raw := strings.TrimSpace(c.Query("end")); raw != "" {
		parsed, err := parseAuditTime(raw)
		if err != nil {
			oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_end_time")
			return
		}
		query.Until = &parsed
	}

	items, total, err := s.deps.Audit.List(c.Request.Context(), query)
	if err != nil {
		s.respondInternalError(c, "查询审计日志失败")
		return
	}

	dtos := make([]auditLogDTO, 0, len(items))
	for _, item := range items {
		dtos = append(dtos, toAuditLogDTO(item))
	}

	// 响应字段用 page_size（本接口与任务约定一致）；其余后台列表接口沿用 size，
	// 二者语义相同，此处不再为统一命名而改动既有接口契约。
	c.JSON(http.StatusOK, gin.H{
		"items":     dtos,
		"total":     total,
		"page":      page,
		"page_size": size,
	})
}

// parseAuditPagination 解析审计列表的分页参数。
//
// 主参数名为 page_size；同时兼容项目其他列表接口惯用的 size，
// 便于前端复用既有请求构造逻辑。
func parseAuditPagination(c *gin.Context) (page, size, offset int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}

	raw := c.Query("page_size")
	if raw == "" {
		raw = c.Query("size")
	}
	size, _ = strconv.Atoi(raw)
	if size < 1 {
		size = defaultAuditPageSize
	}
	if size > maxAuditPageSize {
		size = maxAuditPageSize
	}
	return page, size, (page - 1) * size
}

// parseAuditTime 解析时间筛选参数。
//
// 支持多种写法是为了"同一接口既能被前端 datetime-local 调用、
// 也能被脚本用 Unix 秒或 RFC3339 调用"：不返回具体类型，
// 调用方只需拿到可比较的时间点。
func parseAuditTime(raw string) (time.Time, error) {
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(seconds, 0), nil
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, nil
	}
	// 界面 datetime-local 输入不带时区，按本地时区解释（符合管理员"我这边的时间"直觉）
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("时间格式非法：%q（应为 Unix 秒、RFC3339 或 YYYY-MM-DDTHH:mm）", raw)
}

// toAuditLogDTO 把审计日志实体转为对外 DTO。
func toAuditLogDTO(entry *model.AuditLog) auditLogDTO {
	return auditLogDTO{
		ID:            entry.ID,
		AdminID:       entry.AdminID,
		AdminUsername: entry.AdminUsername,
		Method:        entry.Method,
		Path:          entry.Path,
		Action:        entry.Action,
		Target:        entry.Target,
		Detail:        entry.Detail,
		StatusCode:    entry.StatusCode,
		LatencyMS:     entry.LatencyMS,
		ClientIP:      entry.ClientIP,
		UserAgent:     entry.UserAgent,
		CreatedAt:     unixOrZero(entry.CreatedAt),
	}
}
