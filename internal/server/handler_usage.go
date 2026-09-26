// 本文件实现用量统计与调用日志查询，供管理后台与用户门户共用。
//
// 意图（Why）：
//
//	"谁、什么时候、用了哪个模型、消耗多少、成功还是失败"是运营最核心的观测需求。
//	管理员看全站，普通用户只看自己——差别仅在于是否强制按用户过滤，
//	因此共用同一套查询构造逻辑，避免两处筛选条件出现语义漂移。
//
// 流转（Flow）：
//
//	/api/admin/logs  → buildLogQuery(forcedUserID=0)    → respondLogs
//	/api/user/logs   → buildLogQuery(forcedUserID=当前用户) → respondLogs
//	/api/user/usage  → 直接以当前用户为条件做 Summary/DailySeries/TopModels
//
// 扩展（Extend）：
//
//	新增筛选条件（如按令牌）时：在 buildLogQuery 中解析，并在
//	docs/06-前后端接口契约.md 同步登记参数名。
package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// buildLogQuery 解析日志查询条件。
//
// 参数 forcedUserID > 0 时强制按该用户过滤（用户门户使用），
// 忽略请求中的任何用户筛选参数——这是防止越权查看他人日志的关键。
//
// 返回的 ok 为 false 表示已写出错误响应（如指定的用户名不存在）。
func (s *Server) buildLogQuery(c *gin.Context, forcedUserID uint64) (model.UsageLogQuery, int, int, bool) {
	page, size, offset := parsePagination(c)

	query := model.UsageLogQuery{
		Limit:  size,
		Offset: offset,
		Model:  strings.TrimSpace(c.Query("model")),
		Status: strings.TrimSpace(c.Query("status")),
	}

	// 状态只接受契约定义的两个语义值；其他值一律忽略（不报错，
	// 避免前端拼错参数时整个列表不可用）
	if query.Status != model.LogStatusSuccess && query.Status != model.LogStatusError {
		query.Status = ""
	}

	if forcedUserID > 0 {
		userID := forcedUserID
		query.UserID = &userID
	} else {
		// 管理端：支持按 user_id 或用户名筛选
		if raw := c.Query("user_id"); raw != "" {
			if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
				query.UserID = &parsed
			}
		} else if username := strings.TrimSpace(c.Query("user")); username != "" {
			user, err := s.deps.Users.GetByUsername(c.Request.Context(), username)
			if err != nil {
				// 用户不存在时返回空结果而不是报错：筛选条件本身合法，
				// 只是没有任何匹配记录，这与"查无数据"语义一致。
				return query, page, size, true
			}
			userID := user.ID
			query.UserID = &userID
		}
	}

	if raw := c.Query("channel_id"); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			query.ChannelID = &parsed
		}
	}

	if raw := c.Query("days"); raw != "" {
		if days, err := strconv.Atoi(raw); err == nil && days > 0 && days <= 365 {
			since := truncateToDay(time.Now()).AddDate(0, 0, -days+1)
			query.Since = &since
		}
	}

	return query, page, size, true
}

// respondLogs 查询并输出日志列表。
func (s *Server) respondLogs(c *gin.Context, query model.UsageLogQuery, page, size int) {
	ctx := c.Request.Context()

	logs, err := s.deps.UsageLogs.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询调用日志失败")
		return
	}
	total, err := s.deps.UsageLogs.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计日志总数失败")
		return
	}

	// 批量解析名称映射，避免逐条查询造成 N+1
	usernames := s.loadUsernames(ctx)
	channelNames := s.loadChannelNames(ctx)

	items := make([]usageLogDTO, 0, len(logs))
	for _, entry := range logs {
		items = append(items, toUsageLogDTO(entry, usernames, channelNames))
	}

	c.JSON(http.StatusOK, newPagedResponse(items, total, page, size))
}

// handleMyLogs 返回当前用户的调用日志。
func (s *Server) handleMyLogs(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}

	query, page, size, ok := s.buildLogQuery(c, user.ID)
	if !ok {
		return
	}
	s.respondLogs(c, query, page, size)
}

// handleMyUsage 返回当前用户的用量统计（概览卡片 + 趋势 + 模型分布）。
func (s *Server) handleMyUsage(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	userID := user.ID

	// 统计区间：默认 7 天，可由前端指定（用于概览页的 7/14/30 天切换）
	days := 7
	if raw := c.Query("days"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 365 {
			days = parsed
		}
	}
	since := truncateToDay(time.Now()).AddDate(0, 0, -days+1)

	summary, err := s.deps.UsageLogs.Summary(ctx, model.UsageLogQuery{UserID: &userID, Since: &since})
	if err != nil {
		s.respondInternalError(c, "统计用量失败")
		return
	}

	series, err := s.deps.UsageLogs.DailySeries(ctx, model.UsageLogQuery{UserID: &userID, Since: &since})
	if err != nil {
		s.respondInternalError(c, "统计用量趋势失败")
		return
	}

	byModel, err := s.deps.UsageLogs.TopModels(ctx, model.UsageLogQuery{UserID: &userID, Since: &since}, 10)
	if err != nil {
		s.respondInternalError(c, "统计模型分布失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"range_days":     days,
		"total_requests": summary.Requests,
		"total_tokens":   summary.Tokens,
		"total_quota":    summary.Quota,
		"success_rate":   summary.SuccessRate(),
		// 账户额度信息一并返回，便于概览页一次请求渲染完整
		"quota":           user.Quota,
		"used_quota":      user.UsedQuota,
		"remaining_quota": user.RemainingQuota(),
		"series":          toDailyUsageDTOList(series),
		"by_model":        toModelUsageDTOList(byModel),
	})
}
