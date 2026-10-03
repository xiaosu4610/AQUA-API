// 本文件实现「运行上限」的后台读写接口与进程内缓存访问器。
//
// 意图（Why）：
//
//	本站有一批保护性上限（请求体大小、批量导入条数、统计窗口、试用时长、公告条数，
//	以及 SSE 流式解析的单行/尾部内存上限），站长要求"上限要支持超级管理员在后台可调"。
//	领域模型与默认值/区间见 internal/model/limit_settings.go；本文件负责四件事：
//	  1) 给运行期提供带 TTL 缓存的读取入口（热路径不每请求查库）；
//	  2) 提供后台读取接口（当前值 + 每项默认值与合法区间）；
//	  3) 提供后台保存接口（校验后落库并主动失效缓存，改完立即生效）；
//	  4) 把设置映射为 relay.StreamLimits 注入转发层（SSE 上限是进程级状态，
//	     需在启动与保存后主动推送给 relay，见 applyStreamLimits）。
//
// 流转（Flow）：
//
//	运行期：bodyLimit 等读取点 → s.limitSettingsCached(ctx)
//	          ├─ 命中 limitCache → 直接返回
//	          └─ 未命中 → model.LoadLimitSettings → 写缓存
//	SSE 上限：启动 / 保存后 → applyStreamLimits(ctx) → relay.SetStreamLimits
//	后台读取：GET /api/admin/limits → LoadLimitSettings → items（值 + 默认 + 区间）
//	后台保存：PUT /api/admin/limits → 覆盖提交项 → Validate → SetMany
//	          → invalidateLimitSettingsCache → applyStreamLimits（下一请求即读到新值）
//
// 扩展（Extend）：
//
//	新增一项上限时：在 model.LimitSettings 与默认值/区间里加字段，
//	在本文件补 DTO 字段与 buildLimitItems 的一行，再在读取点替换硬编码常量；
//	若该项属于 relay 侧进程级状态，另在 streamLimitsFromSettings 补一行映射。
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
	"github.com/LTZY-ACU/ltzy-api/internal/relay"
)

// limitSettingsCacheKey 是运行上限设置在 server 侧缓存的键。
const limitSettingsCacheKey = "limits:settings"

// limitSettingsCached 返回当前生效的运行上限设置，优先命中进程内 TTL 缓存。
//
// 读库失败时不缓存（下次请求会重试），并按 LoadLimitSettings 已经完成的回退
// 返回默认值——保证设置表出问题时，服务仍以"默认且安全"的上限继续运行，
// 而不是拒绝服务或失去保护。
func (s *Server) limitSettingsCached(ctx context.Context) model.LimitSettings {
	if s == nil || s.limitCache == nil {
		// 测试或非常规装配下的兜底：直接读一次（仓储为 nil 时返回默认值）。
		settings, _ := model.LoadLimitSettings(ctx, s.settingsRepo())
		return settings
	}
	if cached, ok := s.limitCache.Get(limitSettingsCacheKey); ok {
		return cached.(model.LimitSettings)
	}

	settings, err := model.LoadLimitSettings(ctx, s.settingsRepo())
	if err != nil {
		slog.Warn("读取运行上限设置失败，本次使用默认值", "error", err)
		return settings
	}
	s.limitCache.Set(limitSettingsCacheKey, settings)
	return settings
}

// settingsRepo 返回设置仓储（可能为 nil，LoadLimitSettings 已能安全处理 nil）。
func (s *Server) settingsRepo() model.SettingRepository {
	if s == nil {
		return nil
	}
	return s.deps.Settings
}

// invalidateLimitSettingsCache 主动失效运行上限缓存，供保存后立即生效使用。
func (s *Server) invalidateLimitSettingsCache() {
	if s == nil || s.limitCache == nil {
		return
	}
	s.limitCache.Invalidate(limitSettingsCacheKey)
}

// streamLimitsFromSettings 把设置视图映射为 relay 侧的流式上限。
//
// 为什么需要单独映射：SSE 上限是 relay 包内的进程级状态（见 relay/stream_limits.go），
// 不能由读取点每请求查库，只能在"启动"与"后台保存"两个时机主动推送。
// 这里集中做 int64 → int 的显式转换（取值区间远小于 int 上限，转换安全）。
func streamLimitsFromSettings(settings model.LimitSettings) relay.StreamLimits {
	return relay.StreamLimits{
		AnthropicLineBytes: int(settings.SSEAnthropicLineBytes),
		GeminiLineBytes:    int(settings.SSEGeminiLineBytes),
		CodexLineBytes:     int(settings.SSECodexLineBytes),
		UsageTailBytes:     int(settings.SSEUsageTailBytes),
	}
}

// applyStreamLimits 读取当前设置并注入 relay 的流式上限。
//
// 调用时机：进程启动（server.Run 监听前）与后台保存之后。
// 读到的值可能来自 TTL 缓存，但保存路径会先 invalidate 再调用，故必定是最新值。
// relay.SetStreamLimits 内部会再做一次区间夹取，即便设置表被手工写坏也不会
// 让 0 或天文数字进入 bufio.Scanner。
func (s *Server) applyStreamLimits(ctx context.Context) {
	relay.SetStreamLimits(streamLimitsFromSettings(s.limitSettingsCached(ctx)))
}

// limitItemDTO 描述一项运行上限（供后台渲染与回填）。
//
// 一次性给出当前值 / 默认值 / 合法区间，前端据此限制输入范围并提示
// "当前离默认值差多少"，无需在前端硬编码任何数值。
type limitItemDTO struct {
	// Field 是字段标识（与 PUT 请求体的 JSON 键一致），前端据此映射文案。
	Field string `json:"field"`
	// Value 是当前生效值。
	Value int64 `json:"value"`
	// Default 是默认值（与改动前的硬编码常量一致）。
	Default int64 `json:"default"`
	// Min / Max 是合法区间（含端点）。
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// buildLimitItems 把当前值与默认值组装为有序的条目列表。
//
// 顺序固定（与后台表单一致）：请求体、敏感词导入、排行榜、模型统计、试用时长、公告、
// SSE 单行（Anthropic / Gemini / Codex）、SSE usage 尾部。
// 前端按 field 映射文案与单位，不依赖顺序，但固定顺序便于人工核对接口输出。
func buildLimitItems(current, defaults model.LimitSettings) []limitItemDTO {
	return []limitItemDTO{
		{
			Field: "body_max_bytes", Value: current.BodyMaxBytes, Default: defaults.BodyMaxBytes,
			Min: model.MinLimitBodyMaxBytes, Max: model.MaxLimitBodyMaxBytes,
		},
		{
			Field: "sensitive_import_max_words", Value: int64(current.SensitiveImportMaxWords), Default: int64(defaults.SensitiveImportMaxWords),
			Min: model.MinLimitSensitiveImportMaxWords, Max: model.MaxLimitSensitiveImportMaxWords,
		},
		{
			Field: "leaderboard_max_days", Value: int64(current.LeaderboardMaxDays), Default: int64(defaults.LeaderboardMaxDays),
			Min: model.MinLimitLeaderboardMaxDays, Max: model.MaxLimitLeaderboardMaxDays,
		},
		{
			Field: "model_stats_max_minutes", Value: int64(current.ModelStatsMaxMinutes), Default: int64(defaults.ModelStatsMaxMinutes),
			Min: model.MinLimitModelStatsMaxMinutes, Max: model.MaxLimitModelStatsMaxMinutes,
		},
		{
			Field: "trial_grant_max_hours", Value: int64(current.TrialGrantMaxHours), Default: int64(defaults.TrialGrantMaxHours),
			Min: model.MinLimitTrialGrantMaxHours, Max: model.MaxLimitTrialGrantMaxHours,
		},
		{
			Field: "announcement_active_max", Value: int64(current.AnnouncementActiveMax), Default: int64(defaults.AnnouncementActiveMax),
			Min: model.MinLimitAnnouncementActiveMax, Max: model.MaxLimitAnnouncementActiveMax,
		},
		{
			Field: "sse_anthropic_line_bytes", Value: current.SSEAnthropicLineBytes, Default: defaults.SSEAnthropicLineBytes,
			Min: model.MinLimitSSELineBytes, Max: model.MaxLimitSSELineBytes,
		},
		{
			Field: "sse_gemini_line_bytes", Value: current.SSEGeminiLineBytes, Default: defaults.SSEGeminiLineBytes,
			Min: model.MinLimitSSELineBytes, Max: model.MaxLimitSSELineBytes,
		},
		{
			Field: "sse_codex_line_bytes", Value: current.SSECodexLineBytes, Default: defaults.SSECodexLineBytes,
			Min: model.MinLimitSSELineBytes, Max: model.MaxLimitSSELineBytes,
		},
		{
			Field: "sse_usage_tail_bytes", Value: current.SSEUsageTailBytes, Default: defaults.SSEUsageTailBytes,
			Min: model.MinLimitSSEUsageTailBytes, Max: model.MaxLimitSSEUsageTailBytes,
		},
	}
}

// handleGetLimits 返回当前运行上限 + 每项默认值与合法区间（超管可见）。
func (s *Server) handleGetLimits(c *gin.Context) {
	current, err := model.LoadLimitSettings(c.Request.Context(), s.settingsRepo())
	if err != nil {
		// 读库失败不阻断后台页面：按默认值返回，管理员仍能看到可填区间。
		slog.Warn("读取运行上限设置失败，按默认值返回", "error", err)
	}
	c.JSON(http.StatusOK, gin.H{"items": buildLimitItems(current, model.DefaultLimitSettings())})
}

// limitUpdateRequest 是更新运行上限的请求体。
//
// 字段用指针：未提交的项保持原值，避免前端只改一项却把其他项清空。
type limitUpdateRequest struct {
	BodyMaxBytes            *int64 `json:"body_max_bytes"`
	SensitiveImportMaxWords *int   `json:"sensitive_import_max_words"`
	LeaderboardMaxDays      *int   `json:"leaderboard_max_days"`
	ModelStatsMaxMinutes    *int   `json:"model_stats_max_minutes"`
	TrialGrantMaxHours      *int   `json:"trial_grant_max_hours"`
	AnnouncementActiveMax   *int   `json:"announcement_active_max"`
	SSEAnthropicLineBytes   *int64 `json:"sse_anthropic_line_bytes"`
	SSEGeminiLineBytes      *int64 `json:"sse_gemini_line_bytes"`
	SSECodexLineBytes       *int64 `json:"sse_codex_line_bytes"`
	SSEUsageTailBytes       *int64 `json:"sse_usage_tail_bytes"`
}

// handleUpdateLimits 校验并保存运行上限（部分字段更新）。
//
// 校验失败返回 400 与明确的中文原因（含合法区间）；保存成功后先失效缓存，
// 再把新的 SSE 上限推送给 relay，让改动对下一个请求立即生效。
func (s *Server) handleUpdateLimits(c *gin.Context) {
	if s.deps.Settings == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"设置模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req limitUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	current, err := model.LoadLimitSettings(ctx, s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取运行上限设置失败")
		return
	}

	// 只覆盖请求中出现的字段，未提交项保持原值。
	if req.BodyMaxBytes != nil {
		current.BodyMaxBytes = *req.BodyMaxBytes
	}
	if req.SensitiveImportMaxWords != nil {
		current.SensitiveImportMaxWords = *req.SensitiveImportMaxWords
	}
	if req.LeaderboardMaxDays != nil {
		current.LeaderboardMaxDays = *req.LeaderboardMaxDays
	}
	if req.ModelStatsMaxMinutes != nil {
		current.ModelStatsMaxMinutes = *req.ModelStatsMaxMinutes
	}
	if req.TrialGrantMaxHours != nil {
		current.TrialGrantMaxHours = *req.TrialGrantMaxHours
	}
	if req.AnnouncementActiveMax != nil {
		current.AnnouncementActiveMax = *req.AnnouncementActiveMax
	}
	if req.SSEAnthropicLineBytes != nil {
		current.SSEAnthropicLineBytes = *req.SSEAnthropicLineBytes
	}
	if req.SSEGeminiLineBytes != nil {
		current.SSEGeminiLineBytes = *req.SSEGeminiLineBytes
	}
	if req.SSECodexLineBytes != nil {
		current.SSECodexLineBytes = *req.SSECodexLineBytes
	}
	if req.SSEUsageTailBytes != nil {
		current.SSEUsageTailBytes = *req.SSEUsageTailBytes
	}

	// 校验（含上界）：越界一律拒绝保存，而不是静默夹取——
	// 静默夹取会让管理员以为"存成了我填的值"，实际却不是。
	if err := current.Validate(); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_limit_settings")
		return
	}

	if err := s.deps.Settings.SetMany(ctx, current.ToMap()); err != nil {
		s.respondInternalError(c, "保存运行上限设置失败")
		return
	}

	s.invalidateLimitSettingsCache()
	// 失效缓存后立即把 SSE 上限推送给 relay：这些是进程级状态，
	// 不推送的话后台改了也只对新进程生效（等于要重启才生效）。
	s.applyStreamLimits(ctx)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
