// 本文件实现管理后台接口：仪表盘、渠道、令牌、用户、日志、系统设置。
//
// 意图（Why）：
//
//	管理后台是站长日常使用的主界面：配置上游渠道、给用户发令牌、看用量与故障。
//	这些接口全部需要管理员权限（路由层用 RequireAdmin 统一守卫），
//	且必须遵守两条安全铁律：
//	  1) 任何响应都不得包含明文密钥（渠道密钥只输出掩码）；
//	  2) 不得因"方便"而允许删除最后一个管理员，否则系统将无人可管理。
//
// 流转（Flow）：
//
//	/api/admin/* → SessionAuth → RequireAdmin → 本文件各处理器 → DTO → JSON
//
// 扩展（Extend）：
//
//	新增管理功能时：在本文件加处理器，在 router.go 的 admin 分组注册，
//	并同步更新 docs/06-前后端接口契约.md。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/mailer"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// 分页默认值。
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// nameLookupLimit 是批量解析名称时一次加载的最大记录数。
//
// 取舍说明：日志列表需要展示"用户名/渠道名"，而日志表只存 ID。
// 逐条查询会造成 N+1（一页 20 条就是 40 次额外查询），因此改为一次性加载映射。
// 对自托管网关（渠道与用户规模通常在数十级别）完全够用；
// 若将来规模显著增长，再改为带条件查询或 SQL JOIN。
const nameLookupLimit = 1000

// ---------------------------------------------------------------------------
// 仪表盘
// ---------------------------------------------------------------------------

// dashboardSectionCount 是仪表盘中"计数"分组的响应结构。
type dashboardSectionCount struct {
	Total        int `json:"total"`
	Enabled      int `json:"enabled"`
	AutoDisabled int `json:"auto_disabled,omitempty"`
}

// handleDashboard 返回仪表盘汇总数据。
func (s *Server) handleDashboard(c *gin.Context) {
	ctx := c.Request.Context()

	// 渠道
	channelCounts, err := s.deps.Channels.StatusCounts(ctx)
	if err != nil {
		s.respondInternalError(c, "统计渠道失败")
		return
	}
	channels := dashboardSectionCount{
		Total:        channelCounts[model.ChannelStatusEnabled] + channelCounts[model.ChannelStatusDisabled] + channelCounts[model.ChannelStatusAutoDisabled],
		Enabled:      channelCounts[model.ChannelStatusEnabled],
		AutoDisabled: channelCounts[model.ChannelStatusAutoDisabled],
	}

	// 令牌
	tokenCounts, err := s.deps.Tokens.StatusCounts(ctx)
	if err != nil {
		s.respondInternalError(c, "统计令牌失败")
		return
	}
	tokens := dashboardSectionCount{
		Total:   tokenCounts[model.TokenStatusEnabled] + tokenCounts[model.TokenStatusDisabled] + tokenCounts[model.TokenStatusExpired] + tokenCounts[model.TokenStatusExhausted],
		Enabled: tokenCounts[model.TokenStatusEnabled],
	}

	// 用户
	enabledStatus := model.UserStatusEnabled
	userTotal, err := s.deps.Users.Count(ctx, model.UserQuery{})
	if err != nil {
		s.respondInternalError(c, "统计用户失败")
		return
	}
	userActive, err := s.deps.Users.Count(ctx, model.UserQuery{Status: &enabledStatus})
	if err != nil {
		s.respondInternalError(c, "统计用户失败")
		return
	}

	// 今日用量与近 7 天趋势
	todayStart := truncateToDay(time.Now())
	recentSince := todayStart.AddDate(0, 0, -6)

	todaySummary, err := s.deps.UsageLogs.Summary(ctx, model.UsageLogQuery{Since: &todayStart})
	if err != nil {
		s.respondInternalError(c, "统计今日用量失败")
		return
	}

	series, err := s.deps.UsageLogs.DailySeries(ctx, model.UsageLogQuery{Since: &recentSince})
	if err != nil {
		s.respondInternalError(c, "统计用量趋势失败")
		return
	}

	topModels, err := s.deps.UsageLogs.TopModels(ctx, model.UsageLogQuery{Since: &recentSince}, 5)
	if err != nil {
		s.respondInternalError(c, "统计模型排行失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"channels": channels,
		"users": gin.H{
			"total":  userTotal,
			"active": userActive,
		},
		"tokens": tokens,
		"today": gin.H{
			"requests":     todaySummary.Requests,
			"tokens":       todaySummary.Tokens,
			"quota":        todaySummary.Quota,
			"success_rate": todaySummary.SuccessRate(),
		},
		"recent_days": toDailyUsageDTOList(series),
		"top_models":  toModelUsageDTOList(topModels),
	})
}

// ---------------------------------------------------------------------------
// 渠道管理
// ---------------------------------------------------------------------------

// channelUpsertRequest 是创建/更新渠道的请求体。
//
// 关键设计：APIKey 使用指针类型。
//   - nil  表示"未提交该字段" → 更新时保留原密钥（前端编辑时留空即为这种情况）；
//   - ""   表示"显式清空密钥"；
//   - 其他 表示"设置新密钥"。
//
// 若用普通 string，就无法区分"没传"与"传了空串"，
// 结果是管理员只改个名字就把渠道密钥清空了——这类事故很难排查。
type channelUpsertRequest struct {
	Name     string   `json:"name"`
	Type     int      `json:"type"`
	BaseURL  string   `json:"base_url"`
	APIKey   *string  `json:"api_key"`
	Models   []string `json:"models"`
	Group    string   `json:"group"`
	Priority int      `json:"priority"`
	Weight   int      `json:"weight"`
	Status   int      `json:"status"`
	// KeysText 是"批量密钥"文本框内容：每行一把密钥，行内可用空格或逗号附加备注。
	//
	// 为什么用文本而不是 []string：
	//   - 使用者通常从表格/记事本里直接粘几百行，文本是最自然的输入形态；
	//   - 后端解析（model.ParseKeyList）能统一处理空行、注释、备注与去重，
	//     前端只需原样提交，不必自己实现一遍解析规则。
	//
	// 语义：非空时把该渠道的密钥池整体替换为这批密钥（差集增删，幂等）；
	// 为空时不动密钥池（避免"只改个名字却把 500 把密钥清空"）。
	KeysText string `json:"keys_text"`
}

// handleListChannels 返回渠道列表（统一分页格式）。
func (s *Server) handleListChannels(c *gin.Context) {
	page, size, offset := parsePagination(c)

	query := model.ChannelQuery{Limit: size, Offset: offset}
	if group := c.Query("group"); group != "" {
		query.Group = group
	}
	if statusRaw := c.Query("status"); statusRaw != "" {
		if parsed, err := strconv.Atoi(statusRaw); err == nil {
			status := model.ChannelStatus(parsed)
			query.Status = &status
		}
	}

	ctx := c.Request.Context()
	channels, err := s.deps.Channels.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询渠道列表失败")
		return
	}
	total, err := s.deps.Channels.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计渠道总数失败")
		return
	}

	dtos := toChannelDTOList(channels)
	s.attachKeyPoolSummaries(ctx, channels, dtos)

	c.JSON(http.StatusOK, newPagedResponse(dtos, total, page, size))
}

// attachKeyPoolSummaries 为一批渠道补充密钥池概览。
//
// 用一次 GROUP BY 查询覆盖全部渠道，避免"每个渠道查一次"的 N+1 问题
// （渠道列表页通常有几十行，N+1 会让响应时间随渠道数线性增长）。
//
// 查询失败时不阻断列表：密钥池只是展示信息，缺少它不应导致管理页面打不开。
func (s *Server) attachKeyPoolSummaries(ctx context.Context, channels []*model.Channel, dtos []channelDTO) {
	if s.deps.ChannelKeys == nil || len(channels) == 0 {
		return
	}

	ids := make([]uint64, 0, len(channels))
	for _, ch := range channels {
		ids = append(ids, ch.ID)
	}
	summaries, err := s.deps.ChannelKeys.Summary(ctx, ids)
	if err != nil {
		return
	}
	for i, ch := range channels {
		if summary, ok := summaries[ch.ID]; ok {
			applyKeyPool(&dtos[i], summary)
		}
	}
}

// handleGetChannel 返回单个渠道详情。
func (s *Server) handleGetChannel(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	channel, err := s.deps.Channels.GetByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, model.ErrChannelNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "渠道不存在", oai.TypeInvalidRequest, "channel_not_found")
			return
		}
		s.respondInternalError(c, "查询渠道失败")
		return
	}
	c.JSON(http.StatusOK, toChannelDTO(channel))
}

// handleCreateChannel 新建渠道。
func (s *Server) handleCreateChannel(c *gin.Context) {
	var req channelUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	// 先解析批量密钥：解析失败时直接返回，避免"渠道建好了但密钥没进去"的半成品状态
	keys, labels, err := parseKeysText(req.KeysText)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_keys")
		return
	}

	channel := &model.Channel{
		Name:     strings.TrimSpace(req.Name),
		Type:     req.Type,
		BaseURL:  strings.TrimSpace(req.BaseURL),
		Models:   req.Models,
		Group:    defaultIfEmpty(strings.TrimSpace(req.Group), defaultChannelGroup),
		Priority: req.Priority,
		Weight:   defaultIfZero(req.Weight, 1),
		Status:   model.ChannelStatus(defaultIfZero(req.Status, int(model.ChannelStatusEnabled))),
	}
	if req.APIKey != nil {
		channel.APIKey = *req.APIKey
	}

	ctx := c.Request.Context()
	if err := s.deps.Channels.Create(ctx, channel); err != nil {
		// 领域校验的错误信息（如"base_url 必须以 http:// 开头"）对使用者有直接帮助，
		// 且不包含内部细节，因此原样回传，便于管理员自助修正。
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_channel")
		return
	}

	if err := s.replaceChannelKeys(ctx, channel.ID, keys, labels); err != nil {
		s.respondInternalError(c, "导入渠道密钥失败")
		return
	}

	dto, err := s.channelDTOWithPool(ctx, channel)
	if err != nil {
		s.respondInternalError(c, "读取渠道信息失败")
		return
	}
	c.JSON(http.StatusOK, dto)
}

// parseKeysText 预解析"批量密钥"文本。
//
// 返回 (明文密钥列表, 备注列表, 错误)。文本为空时返回 (nil, nil, nil)，
// 表示"本次不修改密钥池"。
func parseKeysText(keysText string) ([]string, []string, error) {
	if strings.TrimSpace(keysText) == "" {
		return nil, nil, nil
	}
	keys, labels := model.ParseKeyList(keysText)
	if len(keys) == 0 {
		return nil, nil, errors.New("未能从提交内容中解析出任何密钥，请检查格式（每行一把）")
	}
	return keys, labels, nil
}

// replaceChannelKeys 用给定密钥整体替换渠道密钥池（差集增删，幂等）。
func (s *Server) replaceChannelKeys(ctx context.Context, channelID uint64, keys, labels []string) error {
	if s.deps.ChannelKeys == nil || len(keys) == 0 {
		return nil
	}
	_, _, err := s.deps.ChannelKeys.ReplaceAll(ctx, channelID, keys, labels)
	return err
}

// channelDTOWithPool 组装带密钥池概览的渠道 DTO。
func (s *Server) channelDTOWithPool(ctx context.Context, channel *model.Channel) (channelDTO, error) {
	dto := toChannelDTO(channel)
	if s.deps.ChannelKeys == nil {
		return dto, nil
	}
	summaries, err := s.deps.ChannelKeys.Summary(ctx, []uint64{channel.ID})
	if err != nil {
		return dto, err
	}
	if summary, ok := summaries[channel.ID]; ok {
		applyKeyPool(&dto, summary)
	}
	return dto, nil
}

// handleUpdateChannel 更新渠道。
func (s *Server) handleUpdateChannel(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req channelUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	channel, err := s.deps.Channels.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrChannelNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "渠道不存在", oai.TypeInvalidRequest, "channel_not_found")
			return
		}
		s.respondInternalError(c, "查询渠道失败")
		return
	}

	// 先解析批量密钥（与创建一致：解析失败直接返回，不留半成品状态）
	keys, labels, err := parseKeysText(req.KeysText)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_keys")
		return
	}

	// 逐字段覆盖。注意 api_key 只在请求显式提供时才替换，
	// 这样前端"编辑时留空"不会清空已配置的密钥。
	channel.Name = strings.TrimSpace(req.Name)
	channel.Type = req.Type
	channel.BaseURL = strings.TrimSpace(req.BaseURL)
	channel.Models = req.Models
	channel.Group = defaultIfEmpty(strings.TrimSpace(req.Group), defaultChannelGroup)
	channel.Priority = req.Priority
	channel.Weight = defaultIfZero(req.Weight, 1)
	channel.Status = model.ChannelStatus(defaultIfZero(req.Status, int(model.ChannelStatusEnabled)))
	if req.APIKey != nil {
		channel.APIKey = *req.APIKey
	}

	if err := s.deps.Channels.Update(ctx, channel); err != nil {
		if errors.Is(err, model.ErrChannelNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "渠道不存在", oai.TypeInvalidRequest, "channel_not_found")
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_channel")
		return
	}

	// 密钥池为空文本时不动（见 parseKeysText 的语义），避免误清空
	if err := s.replaceChannelKeys(ctx, channel.ID, keys, labels); err != nil {
		s.respondInternalError(c, "导入渠道密钥失败")
		return
	}

	dto, err := s.channelDTOWithPool(ctx, channel)
	if err != nil {
		s.respondInternalError(c, "读取渠道信息失败")
		return
	}
	c.JSON(http.StatusOK, dto)
}

// handleDeleteChannel 删除渠道。
func (s *Server) handleDeleteChannel(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	if err := s.deps.Channels.Delete(ctx, id); err != nil {
		if errors.Is(err, model.ErrChannelNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "渠道不存在", oai.TypeInvalidRequest, "channel_not_found")
			return
		}
		s.respondInternalError(c, "删除渠道失败")
		return
	}

	// 级联清理密钥池：渠道已不存在，残留的密钥既无用又属于敏感数据，不应留在库里
	if s.deps.ChannelKeys != nil {
		_ = s.deps.ChannelKeys.DeleteByChannel(ctx, id)
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// channelTestResponse 是测活结果。
type channelTestResponse struct {
	OK         bool   `json:"ok"`
	LatencyMS  int    `json:"latency_ms"`
	Model      string `json:"model"`
	Message    string `json:"message"`
	StatusCode int    `json:"status_code"`
}

// handleTestChannel 对渠道发起一次真实请求以验证连通性。
func (s *Server) handleTestChannel(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	channel, err := s.deps.Channels.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrChannelNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "渠道不存在", oai.TypeInvalidRequest, "channel_not_found")
			return
		}
		s.respondInternalError(c, "查询渠道失败")
		return
	}

	// 若渠道配置了密钥池，测活要用池中的一把密钥。
	//
	// 为什么必须这样做：密钥池渠道通常【不填】单密钥（api_key 为空），
	// 若测活仍用 channel.APIKey，这类渠道会永远显示"测活失败"，
	// 管理员会误以为渠道配错了。
	if s.deps.ChannelKeys != nil {
		if pool, err := s.deps.ChannelKeys.ListUsable(ctx, channel.ID); err == nil && len(pool) > 0 {
			channel.APIKey = model.PickKey(pool).Key
		}
	}

	result := probeChannel(ctx, channel)

	// 记录测活结果（失败不影响本次响应：测活结果本身就是"可能失败"的信息）
	_ = s.deps.Channels.RecordTestResult(ctx, channel.ID, time.Now(), result.OK)

	c.JSON(http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// 上游模型列表
// ---------------------------------------------------------------------------

// fetchModelsRequest 是拉取上游模型列表的请求体。
//
// 两种用法：
//   - ChannelID > 0：用该渠道已保存的 base_url 与密钥。适用于"渠道已配好，只是想同步模型清单"；
//   - ChannelID = 0：用请求体里的 base_url 与 api_key。
//     适用于"还没保存渠道，先看看这个上游有哪些模型可勾选"。
type fetchModelsRequest struct {
	ChannelID uint64 `json:"channel_id"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
}

// fetchModelsResponse 是模型列表响应。
type fetchModelsResponse struct {
	Models []string `json:"models"`
	Count  int      `json:"count"`
}

// handleFetchModels 向上游拉取可用模型列表。
//
// 存在的意义：NIM 这类平台上架了几百个模型，人工抄写模型名必然出错
// （名字形如 "meta/llama-3.1-70b-instruct"，错一个字符整条路由就失效）。
func (s *Server) handleFetchModels(c *gin.Context) {
	var req fetchModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	if s.deps.Relay == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "转发引擎未就绪", oai.TypeServer, oai.CodeInternal)
		return
	}

	ctx := c.Request.Context()
	baseURL := strings.TrimSpace(req.BaseURL)
	apiKey := strings.TrimSpace(req.APIKey)

	if req.ChannelID > 0 {
		channel, err := s.deps.Channels.GetByID(ctx, req.ChannelID)
		if err != nil {
			if errors.Is(err, model.ErrChannelNotFound) {
				oai.WriteError(c.Writer, http.StatusNotFound, "渠道不存在", oai.TypeInvalidRequest, "channel_not_found")
				return
			}
			s.respondInternalError(c, "查询渠道失败")
			return
		}
		// 以库中数据为准：避免"编辑现有渠道时用错地址/密钥"，导致拉回来的清单
		// 与实际渠道配置不一致——那比不拉取更危险（会配错模型）
		baseURL = channel.BaseURL
		if apiKey == "" {
			apiKey = channel.APIKey
		}
		// 渠道"只有密钥池、没有单密钥"是常态，此时从池里取一把可用密钥
		if apiKey == "" && s.deps.ChannelKeys != nil {
			if pool, err := s.deps.ChannelKeys.ListUsable(ctx, channel.ID); err == nil && len(pool) > 0 {
				apiKey = model.PickKey(pool).Key
			}
		}
	}

	if baseURL == "" {
		oai.WriteError(c.Writer, http.StatusBadRequest, "缺少上游地址（base_url）", oai.TypeInvalidRequest, "missing_base_url")
		return
	}

	models, err := s.deps.Relay.FetchModels(ctx, baseURL, apiKey)
	if err != nil {
		// 上游返回的错误信息（如 "invalid api key"）对管理员排查有直接价值，
		// 且不包含网关内部细节，因此原样回传。
		oai.WriteError(c.Writer, http.StatusBadGateway, err.Error(), oai.TypeServer, "fetch_models_failed")
		return
	}

	c.JSON(http.StatusOK, fetchModelsResponse{Models: models, Count: len(models)})
}

// ---------------------------------------------------------------------------
// 渠道密钥池
// ---------------------------------------------------------------------------

// handleListChannelKeys 返回某渠道的密钥池明细（只含掩码，绝不返回明文）。
func (s *Server) handleListChannelKeys(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	if s.deps.ChannelKeys == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "密钥池功能未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	keys, err := s.deps.ChannelKeys.ListByChannel(c.Request.Context(), id)
	if err != nil {
		s.respondInternalError(c, "查询渠道密钥失败")
		return
	}

	items := toChannelKeyDTOList(keys)
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// channelKeyStatusRequest 是修改密钥状态的请求体。
type channelKeyStatusRequest struct {
	Status int `json:"status"`
}

// handleUpdateChannelKeyStatus 手动启用 / 禁用 / 恢复某把密钥。
//
// 典型场景：
//   - 恢复被误杀（连续失败自动摘除）的密钥；
//   - 临时禁用一个正在被上游限流的密钥，避免它继续拖慢请求。
func (s *Server) handleUpdateChannelKeyStatus(c *gin.Context) {
	keyID, err := strconv.ParseUint(c.Param("keyId"), 10, 64)
	if err != nil || keyID == 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest, "密钥 ID 非法", oai.TypeInvalidRequest, "invalid_id")
		return
	}

	var req channelKeyStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	status := model.ChannelKeyStatus(req.Status)
	if !status.IsValid() {
		oai.WriteError(c.Writer, http.StatusBadRequest, "密钥状态非法", oai.TypeInvalidRequest, "invalid_status")
		return
	}
	if s.deps.ChannelKeys == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "密钥池功能未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	if err := s.deps.ChannelKeys.UpdateStatus(c.Request.Context(), keyID, status); err != nil {
		if errors.Is(err, model.ErrChannelKeyNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "密钥不存在", oai.TypeInvalidRequest, "key_not_found")
			return
		}
		s.respondInternalError(c, "更新密钥状态失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": int(status), "status_text": status.String()})
}

// probeChannel 向渠道发起一次最小请求，用于验证连通与凭据有效性。
//
// 实现要点：
//   - 只发 max_tokens=1 的最小请求，尽量少消耗上游额度；
//   - 独立超时（15 秒），避免慢上游把管理后台拖住；
//   - 不把上游返回的原始报错全文透给前端（可能含地址等内部信息），只给简短结论。
func probeChannel(ctx context.Context, channel *model.Channel) channelTestResponse {
	probeModel := ""
	if len(channel.Models) > 0 {
		probeModel = channel.Models[0]
	}
	if probeModel == "" {
		// 未声明模型时无法构造有效请求：明确告知管理员先补模型列表，
		// 而不是随便挑一个模型去撞（那会产生"测活失败但渠道其实正常"的误导）。
		return channelTestResponse{
			OK:      false,
			Model:   "",
			Message: "该渠道未声明任何模型，请先在渠道配置中填写模型列表后再测活",
		}
	}

	payload, err := json.Marshal(map[string]any{
		"model":      probeModel,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	if err != nil {
		return channelTestResponse{OK: false, Model: probeModel, Message: "构造测活请求失败"}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	url := strings.TrimRight(channel.BaseURL, "/") + oai.ChatCompletionsPath
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return channelTestResponse{OK: false, Model: probeModel, Message: "构造测活请求失败"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+channel.APIKey)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	latency := int(time.Since(start).Milliseconds())

	if err != nil {
		return channelTestResponse{
			OK:        false,
			LatencyMS: latency,
			Model:     probeModel,
			Message:   "无法连接到上游（网络不通或地址有误）",
		}
	}
	defer func() { _ = resp.Body.Close() }()
	// 丢弃响应体前先读一小段：不读会导致连接无法复用
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	message := "连通正常"
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		message = "上游鉴权失败：渠道密钥可能无效或无权限"
	case resp.StatusCode == http.StatusTooManyRequests:
		message = "上游返回限流（密钥有效但当前受限）"
	case resp.StatusCode == http.StatusNotFound:
		message = "上游返回 404：请检查 BaseURL 与模型名是否正确"
	case !ok:
		message = fmt.Sprintf("上游返回异常状态码 %d", resp.StatusCode)
	}

	return channelTestResponse{
		OK:         ok,
		LatencyMS:  latency,
		Model:      probeModel,
		Message:    message,
		StatusCode: resp.StatusCode,
	}
}

// ---------------------------------------------------------------------------
// 令牌管理（管理员可操作全部令牌）
// ---------------------------------------------------------------------------

// adminTokenCreateRequest 是管理员创建令牌的请求体。
type adminTokenCreateRequest struct {
	UserID         uint64   `json:"user_id"`
	Name           string   `json:"name"`
	ExpiresInDays  int      `json:"expires_in_days"`
	Models         []string `json:"models"`
	UnlimitedQuota bool     `json:"unlimited_quota"`
	RemainQuota    int64    `json:"remain_quota"`
}

// handleAdminListTokens 返回全部令牌。
func (s *Server) handleAdminListTokens(c *gin.Context) {
	page, size, offset := parsePagination(c)

	query := model.TokenQuery{Limit: size, Offset: offset}
	if ownerRaw := c.Query("user_id"); ownerRaw != "" {
		if parsed, err := strconv.ParseUint(ownerRaw, 10, 64); err == nil {
			query.OwnerID = &parsed
		}
	}
	if statusRaw := c.Query("status"); statusRaw != "" {
		if parsed, err := strconv.Atoi(statusRaw); err == nil {
			status := model.TokenStatus(parsed)
			query.Status = &status
		}
	}

	ctx := c.Request.Context()
	tokens, err := s.deps.Tokens.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询令牌列表失败")
		return
	}
	total, err := s.deps.Tokens.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计令牌总数失败")
		return
	}

	// 补齐归属用户名（日志与列表都需要展示"谁的令牌"）
	usernames := s.loadUsernames(ctx)
	now := time.Now()

	items := make([]tokenDTO, 0, len(tokens))
	for _, token := range tokens {
		dto := toTokenDTO(token, token.EffectiveStatus(now))
		dto.Username = usernames[token.OwnerID]
		items = append(items, dto)
	}

	c.JSON(http.StatusOK, newPagedResponse(items, total, page, size))
}

// handleAdminCreateToken 为指定用户创建令牌。
func (s *Server) handleAdminCreateToken(c *gin.Context) {
	var req adminTokenCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()

	// 归属用户必须存在，否则令牌会变成"无主令牌"，用量无法归属到任何人
	if _, err := s.deps.Users.GetByID(ctx, req.UserID); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "归属用户不存在", oai.TypeInvalidRequest, "user_not_found")
		return
	}

	s.createTokenAndRespond(c, req.UserID, req.Name, req.ExpiresInDays, req.Models, req.UnlimitedQuota, req.RemainQuota)
}

// handleAdminUpdateToken 更新任意令牌。
func (s *Server) handleAdminUpdateToken(c *gin.Context) {
	s.updateToken(c, 0) // 0 表示不限制归属（管理员权限）
}

// handleAdminDeleteToken 删除任意令牌。
func (s *Server) handleAdminDeleteToken(c *gin.Context) {
	s.deleteToken(c, 0)
}

// ---------------------------------------------------------------------------
// 用户管理
// ---------------------------------------------------------------------------

// adminUserUpsertRequest 是创建/更新用户的请求体。
type adminUserUpsertRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	Role     int    `json:"role"`
	Status   int    `json:"status"`
	Quota    *int64 `json:"quota"`
}

// handleListUsers 返回用户列表。
func (s *Server) handleListUsers(c *gin.Context) {
	page, size, offset := parsePagination(c)

	query := model.UserQuery{Limit: size, Offset: offset, Keyword: c.Query("keyword")}
	if roleRaw := c.Query("role"); roleRaw != "" {
		if parsed, err := strconv.Atoi(roleRaw); err == nil {
			role := model.UserRole(parsed)
			query.Role = &role
		}
	}
	if statusRaw := c.Query("status"); statusRaw != "" {
		if parsed, err := strconv.Atoi(statusRaw); err == nil {
			status := model.UserStatus(parsed)
			query.Status = &status
		}
	}

	ctx := c.Request.Context()
	users, err := s.deps.Users.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询用户列表失败")
		return
	}
	total, err := s.deps.Users.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计用户总数失败")
		return
	}

	items := make([]userDTO, 0, len(users))
	for _, user := range users {
		items = append(items, toUserDTO(user))
	}
	c.JSON(http.StatusOK, newPagedResponse(items, total, page, size))
}

// handleCreateUser 新建用户。
func (s *Server) handleCreateUser(c *gin.Context) {
	var req adminUserUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	if err := crypto.ValidatePasswordStrength(req.Password); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_password")
		return
	}
	hash, err := crypto.HashPassword(req.Password)
	if err != nil {
		s.respondInternalError(c, "计算口令哈希失败")
		return
	}

	quota := int64(0)
	if req.Quota != nil {
		quota = *req.Quota
	}

	user := &model.User{
		Username:     strings.TrimSpace(req.Username),
		PasswordHash: hash,
		Email:        strings.TrimSpace(req.Email),
		Role:         model.UserRole(defaultIfZero(req.Role, int(model.UserRoleUser))),
		Status:       model.UserStatus(defaultIfZero(req.Status, int(model.UserStatusEnabled))),
		Quota:        quota,
	}
	if err := s.deps.Users.Create(c.Request.Context(), user); err != nil {
		if errors.Is(err, model.ErrUsernameTaken) {
			oai.WriteError(c.Writer, http.StatusConflict, "用户名已被占用", oai.TypeInvalidRequest, "username_taken")
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest, "创建用户失败："+err.Error(), oai.TypeInvalidRequest, "invalid_user")
		return
	}
	c.JSON(http.StatusOK, toUserDTO(user))
}

// handleUpdateUser 更新用户。
func (s *Server) handleUpdateUser(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req adminUserUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	user, err := s.deps.Users.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "用户不存在", oai.TypeInvalidRequest, "user_not_found")
			return
		}
		s.respondInternalError(c, "查询用户失败")
		return
	}

	newRole := model.UserRole(defaultIfZero(req.Role, int(user.Role)))
	newStatus := model.UserStatus(defaultIfZero(req.Status, int(user.Status)))

	// 保护性校验：不允许把最后一个管理员降级或禁用，
	// 否则系统将再无人能进入后台（这类不可逆的运维事故必须提前拦截）。
	if user.IsAdmin() && (newRole != model.UserRoleAdmin || newStatus != model.UserStatusEnabled) {
		adminCount, err := s.deps.Users.CountAdmins(ctx)
		if err != nil {
			s.respondInternalError(c, "统计管理员数量失败")
			return
		}
		if adminCount <= 1 {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"系统必须保留至少一名启用的管理员，无法降级或禁用最后一个管理员",
				oai.TypeInvalidRequest, "last_admin_protected")
			return
		}
	}

	user.Username = strings.TrimSpace(defaultIfEmpty(req.Username, user.Username))
	user.Email = strings.TrimSpace(req.Email)
	user.Role = newRole
	user.Status = newStatus
	if req.Quota != nil {
		user.Quota = *req.Quota
	}
	// 口令留空表示不修改（避免管理员只想改额度却意外重置了用户密码）
	if req.Password != "" {
		if err := crypto.ValidatePasswordStrength(req.Password); err != nil {
			oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_password")
			return
		}
		hash, err := crypto.HashPassword(req.Password)
		if err != nil {
			s.respondInternalError(c, "计算口令哈希失败")
			return
		}
		user.PasswordHash = hash
	}

	if err := s.deps.Users.Update(ctx, user); err != nil {
		if errors.Is(err, model.ErrUsernameTaken) {
			oai.WriteError(c.Writer, http.StatusConflict, "用户名已被占用", oai.TypeInvalidRequest, "username_taken")
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest, "更新用户失败："+err.Error(), oai.TypeInvalidRequest, "invalid_user")
		return
	}

	// 被禁用或改密后强制下线：否则已登录的会话仍可继续访问，使该操作形同虚设。
	// 改密场景尤其重要——若旧会话仍有效，"改密"就不能起到"踢出可疑登录"的作用。
	if newStatus == model.UserStatusDisabled || req.Password != "" {
		_ = s.deps.Sessions.DeleteByUserID(ctx, user.ID)
	}

	c.JSON(http.StatusOK, toUserDTO(user))
}

// handleDeleteUser 删除用户。
func (s *Server) handleDeleteUser(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	user, err := s.deps.Users.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "用户不存在", oai.TypeInvalidRequest, "user_not_found")
			return
		}
		s.respondInternalError(c, "查询用户失败")
		return
	}

	// 同样保护最后一个管理员
	if user.IsAdmin() {
		adminCount, err := s.deps.Users.CountAdmins(ctx)
		if err != nil {
			s.respondInternalError(c, "统计管理员数量失败")
			return
		}
		if adminCount <= 1 {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"系统必须保留至少一名管理员，无法删除最后一个管理员",
				oai.TypeInvalidRequest, "last_admin_protected")
			return
		}
	}

	// 先删会话再删用户：避免出现"用户已删、会话仍在"的残留状态
	if err := s.deps.Sessions.DeleteByUserID(ctx, user.ID); err != nil {
		s.respondInternalError(c, "清理用户会话失败")
		return
	}
	if err := s.deps.Users.Delete(ctx, user.ID); err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "用户不存在", oai.TypeInvalidRequest, "user_not_found")
			return
		}
		s.respondInternalError(c, "删除用户失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------------------
// 调用日志（管理员可见全站）
// ---------------------------------------------------------------------------

// handleAdminListLogs 返回全站调用日志。
func (s *Server) handleAdminListLogs(c *gin.Context) {
	query, page, size, ok := s.buildLogQuery(c, 0)
	if !ok {
		return
	}
	s.respondLogs(c, query, page, size)
}

// ---------------------------------------------------------------------------
// 系统设置
// ---------------------------------------------------------------------------

// handleGetSettings 返回系统设置。
func (s *Server) handleGetSettings(c *gin.Context) {
	settings, err := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取系统设置失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"site_name":                       settings.SiteName,
		"site_description":                settings.SiteDescription,
		"registration_enabled":            settings.RegistrationEnabled,
		"registration_require_email_code": settings.RegistrationRequireEmailCode,
		"default_user_quota":              settings.DefaultUserQuota,
		"default_group":                   settings.DefaultGroup,
		// 邮件通道是否就绪：让管理员在开关"注册邮箱验证码"前就知道
		// 当前是否具备发信能力，避免开启后用户全部收不到验证码。
		"email_service_ready": s.deps.Mailer != nil && s.deps.Mailer.Configured(),
		"email_from":          mailerFromAddress(s.deps.Mailer),
	})
}

// mailerFromAddress 返回发件人地址用于界面展示；未配置时返回空字符串。
//
// 只暴露地址不暴露口令：地址本身对管理员没有秘密，且能帮助定位
// "验证码发不出去"这类问题（例如发件地址写错）。
func mailerFromAddress(sender *mailer.Sender) string {
	if sender == nil {
		return ""
	}
	return sender.From()
}

// settingsUpdateRequest 是更新设置的请求体。
//
// 说明：字段用指针，未提交的项保持原值，避免前端只改一项却把其他项清空。
type settingsUpdateRequest struct {
	SiteName                     *string `json:"site_name"`
	SiteDescription              *string `json:"site_description"`
	RegistrationEnabled          *bool   `json:"registration_enabled"`
	RegistrationRequireEmailCode *bool   `json:"registration_require_email_code"`
	DefaultUserQuota             *int64  `json:"default_user_quota"`
	DefaultGroup                 *string `json:"default_group"`
}

// handleUpdateSettings 更新系统设置。
func (s *Server) handleUpdateSettings(c *gin.Context) {
	var req settingsUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	current, err := model.LoadSiteSettings(ctx, s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取系统设置失败")
		return
	}

	if req.SiteName != nil && strings.TrimSpace(*req.SiteName) != "" {
		current.SiteName = strings.TrimSpace(*req.SiteName)
	}
	if req.SiteDescription != nil {
		current.SiteDescription = strings.TrimSpace(*req.SiteDescription)
	}
	if req.RegistrationEnabled != nil {
		current.RegistrationEnabled = *req.RegistrationEnabled
	}
	if req.RegistrationRequireEmailCode != nil {
		// 开启验证码校验但邮件通道未配置：明确拒绝而不是静默保存。
		// 理由：一旦保存，所有用户注册都会卡在"收不到验证码"，
		// 而管理员从界面上看不出原因，属于极难定位的运营故障。
		if *req.RegistrationRequireEmailCode && (s.deps.Mailer == nil || !s.deps.Mailer.Configured()) {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"邮件服务未配置，无法开启邮箱验证码校验（请先配置 SMTP 环境变量）",
				oai.TypeInvalidRequest, "email_service_unavailable")
			return
		}
		current.RegistrationRequireEmailCode = *req.RegistrationRequireEmailCode
	}
	if req.DefaultUserQuota != nil {
		if *req.DefaultUserQuota < model.QuotaUnlimited {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"默认额度非法（允许 -1 表示不限）", oai.TypeInvalidRequest, "invalid_quota")
			return
		}
		current.DefaultUserQuota = *req.DefaultUserQuota
	}
	if req.DefaultGroup != nil && strings.TrimSpace(*req.DefaultGroup) != "" {
		current.DefaultGroup = strings.TrimSpace(*req.DefaultGroup)
	}

	if err := s.deps.Settings.SetMany(ctx, current.ToMap()); err != nil {
		s.respondInternalError(c, "保存系统设置失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------------------
// 共享辅助
// ---------------------------------------------------------------------------

// 渠道与分组的默认值。
const (
	defaultChannelGroup = "default"
)

// parsePagination 解析分页参数并归一化。
func parsePagination(c *gin.Context) (page, size, offset int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	size, _ = strconv.Atoi(c.DefaultQuery("size", strconv.Itoa(defaultPageSize)))
	if size < 1 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return page, size, (page - 1) * size
}

// parseIDParam 解析路径中的 id 参数；失败时已写出 400 响应，返回 false。
func parseIDParam(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"资源 ID 非法", oai.TypeInvalidRequest, "invalid_id")
		return 0, false
	}
	return id, true
}

// respondInternalError 统一输出内部错误（不暴露细节）。
func (s *Server) respondInternalError(c *gin.Context, _ string) {
	// 说明：传入的描述仅用于将来接入结构化日志时记录，当前不返回给客户端。
	// TODO(server): 接入结构化日志后在此记录 message 与 err
	oai.WriteError(c.Writer, http.StatusInternalServerError,
		"网关内部错误", oai.TypeServer, oai.CodeInternal)
}

// loadUsernames 一次性加载"用户 ID → 用户名"映射，用于避免日志/列表的 N+1 查询。
func (s *Server) loadUsernames(ctx context.Context) map[uint64]string {
	result := make(map[uint64]string)
	users, err := s.deps.Users.List(ctx, model.UserQuery{Limit: nameLookupLimit})
	if err != nil {
		// 名称解析失败不应阻断主流程：日志本身仍然有价值，最多"用户名"列为空
		return result
	}
	for _, user := range users {
		result[user.ID] = user.Username
	}
	return result
}

// loadChannelNames 一次性加载"渠道 ID → 渠道名"映射。
func (s *Server) loadChannelNames(ctx context.Context) map[uint64]string {
	result := make(map[uint64]string)
	channels, err := s.deps.Channels.List(ctx, model.ChannelQuery{Limit: nameLookupLimit})
	if err != nil {
		return result
	}
	for _, channel := range channels {
		result[channel.ID] = channel.Name
	}
	return result
}

// defaultIfEmpty 在值为空时返回兜底值。
func defaultIfEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// defaultIfZero 在值为 0 时返回兜底值。
//
// 用于"前端未提交该字段"的场景：0 在这里视为"未提供"，
// 因为业务上不存在合法的零值语义（如权重 0 无意义、状态 0 非法）。
func defaultIfZero(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

// truncateToDay 把时间截断到当天零点（本地时区）。
func truncateToDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
