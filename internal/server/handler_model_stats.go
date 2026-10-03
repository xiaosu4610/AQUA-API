// 本文件实现「模型实时指标」接口。
//
// 意图（Why）：
//
//	模型详情页需要回答"这个模型现在快不快"：用近 N 分钟的真实调用统计
//	给出输出速率（tokens/s）、平均耗时、请求量与可用性。数据源是
//	usage_logs（relay 每次转发落一条），与概览页/仪表盘同口径。
//
//	"连通性测试"不在后端做：它需要模拟真实 agent 调用（请求发起 → 鉴权
//	→ 流式返回），由前端直接用访问令牌调用 /v1/chat/completions 完成，
//	与 Playground 完全同路径——后端只提供统计数据，避免为"测通"引入
//	第二套转发逻辑（重复即熵）。
//
// 流转（Flow）：
//
//	GET /api/user/models/:model/stats → 近 N 分钟聚合（TPS/耗时/请求数）
//
// 边界情况：
//   - 模型离线（无启用渠道支持）：返回 available=false、channel_count=0，
//     前端据此显示"离线"，不发起测试请求；
//   - 统计口径：TPS 与 TTFB 取自 usage_logs 的 tokens_per_second 与
//     first_token_ms，与概览页/仪表盘完全一致。
package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
)

// modelStatsDefaultMinutes 是模型实时指标默认统计窗口（近 15 分钟）。
const modelStatsDefaultMinutes = 15

// modelStatsMaxMinutes 是模型实时指标允许的最大窗口（近 24 小时）。
const modelStatsMaxMinutes = 24 * 60

// modelStatsDTO 是单个模型的实时指标（模型详情页的 tokens/s 展示）。
type modelStatsDTO struct {
	// Model 是模型名。
	Model string `json:"model"`
	// Available 表示当前是否至少有一个启用渠道支持该模型。
	Available bool `json:"available"`
	// ChannelCount 是支持该模型的启用渠道数量（0 = 离线）。
	ChannelCount int `json:"channel_count"`
	// Requests 是窗口内该模型的成功请求数。
	Requests int64 `json:"requests"`
	// AvgTokensPerSecond 是窗口内平均输出速率（tokens/s，0 = 无样本）。
	//
	// 口径与 usage_logs.tokens_per_second 完全一致：输出 token 数 ÷
	// (总耗时 − 首 token 延迟)。只统计有速率样本的请求。
	AvgTokensPerSecond float64 `json:"avg_tokens_per_second"`
	// AvgLatencyMS 是窗口内平均总耗时（毫秒，0 = 无请求）。
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	// AvgFirstTokenMS 是窗口内平均首字延迟（TTFB，毫秒，0 = 无样本）。
	AvgFirstTokenMS float64 `json:"avg_first_token_ms"`
}

// handleModelStats 返回某个模型的近窗口实时指标。
func (s *Server) handleModelStats(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	_ = user // 本接口限登录用户调用（会话中间件已保障）

	modelName := strings.TrimSpace(c.Query("model"))
	if modelName == "" {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"缺少模型名", oai.TypeInvalidRequest, "invalid_model")
		return
	}

	minutes := modelStatsDefaultMinutes
	// 统计窗口上限可在后台「运行上限」页调整（默认 1440 分钟）。
	maxMinutes := s.limitSettingsCached(c.Request.Context()).ModelStatsMaxMinutes
	if raw := c.Query("minutes"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= maxMinutes {
			minutes = parsed
		}
	}
	since := time.Now().Add(-time.Duration(minutes) * time.Minute)

	ctx := c.Request.Context()

	// 可用性：启用渠道是否声明了该模型（决定 available / channel_count）。
	enabled := model.ChannelStatusEnabled
	channels, err := s.deps.Channels.List(ctx, model.ChannelQuery{Status: &enabled, Limit: 500})
	if err != nil {
		s.respondInternalError(c, "查询渠道失败")
		return
	}
	channelCount := 0
	for _, ch := range channels {
		if ch.HasModel(modelName) {
			channelCount++
		}
	}

	// 实时指标：按模型过滤的成功请求聚合（与排行榜/概览页同口径）。
	//
	// 为什么必须限定成功请求：失败请求（鉴权拒绝、上游错误等）的
	// latency 极小（网关层即返回），混入平均会把"模型快不快"系统性
	// 拉低成没有意义的数字；requests 也只显示真实成功的调用量。
	summary, err := s.deps.UsageLogs.Summary(ctx, model.UsageLogQuery{
		Model:  modelName,
		Since:  &since,
		Status: model.LogStatusSuccess,
	})
	if err != nil {
		s.respondInternalError(c, "统计模型指标失败")
		return
	}

	tps := 0.0
	if summary.TPSSamples > 0 {
		tps = summary.TPSSum / float64(summary.TPSSamples)
	}
	latency := 0.0
	if summary.Requests > 0 {
		latency = float64(summary.LatencySumMS) / float64(summary.Requests)
	}
	firstToken := 0.0
	if summary.FirstTokenSamples > 0 {
		firstToken = float64(summary.FirstTokenSumMS) / float64(summary.FirstTokenSamples)
	}

	c.JSON(http.StatusOK, modelStatsDTO{
		Model:              modelName,
		Available:          channelCount > 0,
		ChannelCount:       channelCount,
		Requests:           summary.Requests,
		AvgTokensPerSecond: tps,
		AvgLatencyMS:       latency,
		AvgFirstTokenMS:    firstToken,
	})
}
