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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/mailer"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/payment"
	"gitee.com/xiaosu4610/aqua-api/internal/relay"
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
	// KeyStrategy 是渠道凭据池的调度策略标识。
	//
	// 留空表示"不修改"：更新接口据此刻意不覆盖已有策略，
	// 否则前端只提交部分字段（如仅切换状态）就会把策略重置回默认值。
	KeyStrategy string `json:"key_strategy"`
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

	// OAuthTokensText 是"批量订阅账号"文本框内容：每行一条 refresh_token，
	// 行内可用空格或逗号附加账号标识（如邮箱）。
	//
	// 与 KeysText 分开的原因：两类凭据的去重标识不同
	// （API Key 用密钥本身，订阅账号用 refresh_token），
	// 混在一个框里会让使用者无法预知"这次导入会替换掉什么"。
	OAuthTokensText string `json:"oauth_tokens_text"`
	// OAuthProvider 是这批订阅账号对应的 OAuth 提供方名称（如 claude）。
	OAuthProvider string `json:"oauth_provider"`
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

	// 调度策略：空值取默认；非法值直接 400 并告知可选值（不静默兜底，
	// 否则管理员填错也"保存成功"，却在转发时按另一套策略调度）。
	strategy, err := parseKeyStrategy(req.KeyStrategy)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_key_strategy")
		return
	}

	channel := &model.Channel{
		Name:        strings.TrimSpace(req.Name),
		Type:        req.Type,
		BaseURL:     strings.TrimSpace(req.BaseURL),
		Models:      req.Models,
		Group:       defaultIfEmpty(strings.TrimSpace(req.Group), defaultChannelGroup),
		Priority:    req.Priority,
		Weight:      defaultIfZero(req.Weight, 1),
		Status:      model.ChannelStatus(defaultIfZero(req.Status, int(model.ChannelStatusEnabled))),
		KeyStrategy: strategy,
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

	if err := s.replaceChannelOAuthCredentials(ctx, channel.ID, req.OAuthTokensText, req.OAuthProvider); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_oauth_tokens")
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

// replaceChannelOAuthCredentials 按提交的文本同步订阅账号（OAuth）凭据。
//
// 语义与 replaceChannelKeys 一致：文本为空表示"不修改"，
// 避免管理员只改渠道名却把订阅账号清空。
//
// 两类凭据分开导入的额外好处：ReplaceCredentials 只会增删"本次涉及的类型"，
// 因此导入 API Key 不会影响已有的订阅账号，反之亦然。
func (s *Server) replaceChannelOAuthCredentials(ctx context.Context, channelID uint64, tokensText, provider string) error {
	if strings.TrimSpace(tokensText) == "" {
		return nil
	}
	if s.deps.ChannelKeys == nil {
		return errors.New("凭据池功能未启用")
	}

	credentials := model.ParseCredentialList(tokensText, strings.TrimSpace(provider))
	if len(credentials) == 0 {
		return errors.New("未能从提交内容中解析出任何订阅账号凭据，请检查格式（每行一条 refresh_token）")
	}
	_, _, err := s.deps.ChannelKeys.ReplaceCredentials(ctx, channelID, credentials)
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
	// 策略留空表示"不修改"：这样只提交部分字段（如启停）也不会把策略重置。
	// 非空时校验合法性，非法值直接 400。
	if strings.TrimSpace(req.KeyStrategy) != "" {
		strategy, err := parseKeyStrategy(req.KeyStrategy)
		if err != nil {
			oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_key_strategy")
			return
		}
		channel.KeyStrategy = strategy
	}
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

	if err := s.replaceChannelOAuthCredentials(ctx, channel.ID, req.OAuthTokensText, req.OAuthProvider); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_oauth_tokens")
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

// channelKeyUpdateRequest 是修改单把凭据的请求体。
//
// 路径保持 PUT /api/admin/keys/:keyId，但语义已从"改状态"扩展为"改状态与调度参数"：
//   - Status 为空表示不改状态；
//   - weight / priority / rpm_limit 为调度参数，三者需同时提供
//     （仓储的 UpdateScheduling 是整组覆盖，缺项会把它误写成 0）。
type channelKeyUpdateRequest struct {
	Status   *int `json:"status"`
	Weight   *int `json:"weight"`
	Priority *int `json:"priority"`
	RPMLimit *int `json:"rpm_limit"`
}

// handleUpdateChannelKeyStatus 更新某把密钥的状态与调度参数。
//
// 典型场景：
//   - 恢复被误杀（连续失败自动摘除）的密钥，或临时禁用正在被限流的密钥；
//   - 调整该凭据的权重 / 优先级 / 每分钟上限（配合渠道级的调度策略）。
func (s *Server) handleUpdateChannelKeyStatus(c *gin.Context) {
	keyID, err := strconv.ParseUint(c.Param("keyId"), 10, 64)
	if err != nil || keyID == 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest, "密钥 ID 非法", oai.TypeInvalidRequest, "invalid_id")
		return
	}

	var req channelKeyUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	if req.Status == nil && req.Weight == nil && req.Priority == nil && req.RPMLimit == nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "未提供任何可更新字段", oai.TypeInvalidRequest, "empty_update")
		return
	}
	if s.deps.ChannelKeys == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "密钥池功能未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	ctx := c.Request.Context()
	resp := gin.H{"ok": true}

	// 1) 状态（可选）
	if req.Status != nil {
		status := model.ChannelKeyStatus(*req.Status)
		if !status.IsValid() {
			oai.WriteError(c.Writer, http.StatusBadRequest, "密钥状态非法", oai.TypeInvalidRequest, "invalid_status")
			return
		}
		if err := s.deps.ChannelKeys.UpdateStatus(ctx, keyID, status); err != nil {
			s.respondKeyUpdateError(c, err)
			return
		}
		resp["status"] = int(status)
		resp["status_text"] = status.String()
	}

	// 2) 调度参数（可选）：三项必须同时给，否则未给项会被写 0 造成意外停用
	if req.Weight != nil || req.Priority != nil || req.RPMLimit != nil {
		if req.Weight == nil || req.Priority == nil || req.RPMLimit == nil {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"更新调度参数时需同时提供 weight / priority / rpm_limit 三项",
				oai.TypeInvalidRequest, "incomplete_scheduling")
			return
		}
		if *req.Weight < 0 || *req.Priority < 0 || *req.RPMLimit < 0 {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"权重 / 优先级 / 每分钟上限不能为负数", oai.TypeInvalidRequest, "invalid_scheduling")
			return
		}
		if err := s.deps.ChannelKeys.UpdateScheduling(ctx, keyID, *req.Weight, *req.Priority, *req.RPMLimit); err != nil {
			s.respondKeyUpdateError(c, err)
			return
		}
		resp["weight"] = *req.Weight
		resp["priority"] = *req.Priority
		resp["rpm_limit"] = *req.RPMLimit
	}

	c.JSON(http.StatusOK, resp)
}

// respondKeyUpdateError 统一翻译密钥更新的领域错误。
func (s *Server) respondKeyUpdateError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrChannelKeyNotFound) {
		oai.WriteError(c.Writer, http.StatusNotFound, "密钥不存在", oai.TypeInvalidRequest, "key_not_found")
		return
	}
	s.respondInternalError(c, "更新密钥失败")
}

// ---------------------------------------------------------------------------
// 凭据调度策略目录
// ---------------------------------------------------------------------------

// keyStrategyDTO 描述一种凭据调度策略（供后台渲染下拉与帮助文案）。
//
// 说明文案由后端下发（取自领域层），前端不硬编码：
// 这样新增策略时只需改后端一处，界面自动跟随。
type keyStrategyDTO struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// handleListKeyStrategies 返回全部凭据调度策略。
//
// 只读接口：策略取值是稳定枚举，站长只能"选用"，不能自定义，
// 因此这里不做鉴权以外的任何副作用，也不需要分页。
func (s *Server) handleListKeyStrategies(c *gin.Context) {
	items := make([]keyStrategyDTO, 0, len(model.KeyStrategyAll()))
	for _, strategy := range model.KeyStrategyAll() {
		items = append(items, keyStrategyDTO{
			Key:         string(strategy),
			Label:       strategy.String(),
			Description: strategy.Description(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// parseKeyStrategy 解析并校验渠道凭据调度策略。
//
// 空值取默认策略（least_in_flight）；非法值返回带可选值的错误，
// 由调用方转成 400 反馈给管理员。
func parseKeyStrategy(raw string) (model.KeyStrategy, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return model.DefaultKeyStrategy(), nil
	}
	strategy := model.KeyStrategy(trimmed)
	if !strategy.IsValid() {
		return "", fmt.Errorf("凭据调度策略非法: %q（可选：%s）", trimmed, model.KeyStrategyOptionText())
	}
	return strategy, nil
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

	// 超时与转发链路保持一致（relay.UpstreamTimeout = 300 秒）。
	//
	// 为什么不能像以前那样用 15 秒：NVIDIA NIM 这类平台的排队时间本身就长，
	// 15 秒几乎必然超时，管理员会看到"测活失败"从而误删一个其实完全可用的渠道。
	// 宁可让管理员多等一会儿，也不要给出一条误导性的结论。
	probeCtx, cancel := context.WithTimeout(ctx, relay.UpstreamTimeout)
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
		// 区分"超时"与"连不上"：两者的处置方式完全不同——
		// 超时往往只是上游慢（尤其 NVIDIA），换个时间或换个模型可能就正常；
		// 连不上才是地址/网络配错。混在一起报会让管理员改错地方。
		if errors.Is(err, context.DeadlineExceeded) {
			return channelTestResponse{
				OK:        false,
				LatencyMS: latency,
				Model:     probeModel,
				Message: fmt.Sprintf("等待上游响应超过 %d 秒仍未开始返回。"+
					"这不代表渠道不可用（部分平台排队时间很长），建议换一个模型再测，"+
					"或直接交给实际调用验证。", int(relay.UpstreamTimeout.Seconds())),
			}
		}
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
		// 404 有两种成因，必须区分开，否则管理员会一直去改 BaseURL：
		//   1) 地址配错 —— 但若此前能拉到模型列表，基本可排除；
		//   2) 该密钥/账号无权访问这个模型 —— NVIDIA 等平台按模型逐个授权，
		//      账号只拿了部分模型的权限。把两种可能都写出来，管理员才不会误判。
		message = "上游返回 404：可能是该渠道的密钥无权访问此模型（部分平台按模型逐个授权），" +
			"也可能是 BaseURL 填错（注意不要带 /v1）"
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

		// 充值 / 支付参数（非密钥，可在此修改并即时生效）
		"payment": toPaymentSettingsDTO(settings.Payment),
		// 支付通道清单：字段定义 + 当前已填值 + 密钥是否就绪。
		// 前端据此做「勾选启用哪个通道，才展开该通道的配置项」的触发式渲染，
		// 因此新增支付通道不需要改前端代码。
		"payment_channels": buildPaymentChannels(settings.Payment, os.LookupEnv),
		// 各支付通道的密钥是否已通过环境变量就绪。
		// 只暴露布尔值，绝不回传密钥本身——密钥一旦出过服务端就等于泄露。
		"payment_secrets": gin.H{
			model.PaymentMethodEPay: s.deps.Config.Payment.EPayKey != "",
			model.PaymentMethodStripe: s.deps.Config.Payment.StripeSecretKey != "" &&
				s.deps.Config.Payment.StripeWebhookSecret != "",
			model.PaymentMethodManual: true,
		},
	})
}

// paymentSettingsDTO 是支付运营参数的对外表示。
type paymentSettingsDTO struct {
	Enabled         bool     `json:"enabled"`
	Methods         []string `json:"methods"`
	ExchangeRate    int64    `json:"exchange_rate"`
	Currency        string   `json:"currency"`
	MinCents        int64    `json:"min_cents"`
	MaxCents        int64    `json:"max_cents"`
	OrderTTLMinutes int      `json:"order_ttl_minutes"`
	NotifyBase      string   `json:"notify_base"`
	// Params 是各支付通道的通道级参数，键形如 "<通道>.<字段>"（如 "epay.pid"）。
	//
	// 这是支持"任意多种支付通道"的承载：通道与字段由支付通道注册表声明，
	// 前端按注册表渲染表单并原样回传，后端不再为每个通道定义专用字段。
	Params map[string]string `json:"params"`
	// 以下四个字段是旧版专用字段，仅为兼容老前端保留，新前端请用 Params。
	EPayGateway string   `json:"epay_gateway"`
	EPayPID     string   `json:"epay_pid"`
	EPayTypes   []string `json:"epay_types"`
	StripeNote  string   `json:"stripe_note"`
}

// paymentChannelFieldDTO 描述支付通道的一个配置字段（供前端触发式渲染）。
type paymentChannelFieldDTO struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Kind        string  `json:"kind"`
	Source      string  `json:"source"`
	EnvVar      string  `json:"env_var"`
	Placeholder string  `json:"placeholder"`
	Help        string  `json:"help"`
	Default     string  `json:"default"`
	Required    bool    `json:"required"`
	Options     []gin.H `json:"options,omitempty"`
	Value       string  `json:"value"`
	Ready       bool    `json:"ready"`
	SettingKey  string  `json:"setting_key"`
}

// paymentChannelDTO 描述一个支付通道（含字段、密钥就绪状态与回调地址）。
type paymentChannelDTO struct {
	Key         string                   `json:"key"`
	Label       string                   `json:"label"`
	Description string                   `json:"description"`
	Available   bool                     `json:"available"`
	Enabled     bool                     `json:"enabled"`
	NotifyPath  string                   `json:"notify_path"`
	MissingEnv  []string                 `json:"missing_env"`
	Fields      []paymentChannelFieldDTO `json:"fields"`
}

// buildPaymentChannels 组装支付通道清单：字段定义 + 当前已填值 + 密钥是否就绪。
//
// 为什么要一次返回"字段定义 + 当前值 + 就绪状态"三样：
//   - 字段定义让前端做**触发式渲染**（勾选哪个通道才展开它的字段）；
//   - 当前值让界面能回显站长已配好的内容；
//   - 就绪状态让界面能明确提示"密钥还没注入、这个通道开了也用不了"。
//
// 密钥字段只返回"是否就绪"，绝不返回内容。
func buildPaymentChannels(settings model.PaymentSettings, lookup func(string) (string, bool)) []paymentChannelDTO {
	channels := payment.Channels()
	result := make([]paymentChannelDTO, 0, len(channels))

	for _, channel := range channels {
		missing := channel.MissingSecrets(lookup)

		fields := make([]paymentChannelFieldDTO, 0, len(channel.Fields))
		for _, field := range channel.Fields {
			item := paymentChannelFieldDTO{
				Key:         field.Key,
				Label:       field.Label,
				Kind:        string(field.Kind),
				Source:      string(field.Source),
				EnvVar:      field.EnvVar,
				Placeholder: field.Placeholder,
				Help:        field.Help,
				Default:     field.Default,
				Required:    field.Required,
				SettingKey:  channel.SettingKey(field.Key),
			}
			for _, option := range field.Options {
				item.Options = append(item.Options, gin.H{"value": option.Value, "label": option.Label})
			}

			if field.Source == payment.SourceSecret {
				// 密钥：只告诉前端"是否已就绪"，值永不出服务端
				item.Ready = true
				if field.EnvVar != "" {
					if value, ok := lookup(field.EnvVar); !ok || strings.TrimSpace(value) == "" {
						item.Ready = false
					}
				}
			} else {
				item.Value = settings.Param(channel.Key, field.Key)
				if item.Value == "" {
					item.Value = field.Default
				}
				item.Ready = item.Value != ""
			}
			fields = append(fields, item)
		}

		result = append(result, paymentChannelDTO{
			Key:         channel.Key,
			Label:       channel.Label,
			Description: channel.Description,
			Available:   channel.Available,
			Enabled:     settings.MethodEnabled(channel.Key),
			NotifyPath:  channel.NotifyPath,
			MissingEnv:  nonNilStrings(missing),
			Fields:      fields,
		})
	}
	return result
}

// toPaymentSettingsDTO 把支付设置转为对外 DTO。
func toPaymentSettingsDTO(settings model.PaymentSettings) paymentSettingsDTO {
	params := make(map[string]string, len(settings.Params))
	for key, value := range settings.Params {
		params[key] = value
	}
	return paymentSettingsDTO{
		Enabled:         settings.Enabled,
		Methods:         nonNilStrings(settings.Methods),
		ExchangeRate:    settings.ExchangeRate,
		Currency:        settings.Currency,
		MinCents:        settings.MinCents,
		MaxCents:        settings.MaxCents,
		OrderTTLMinutes: settings.OrderTTLMinutes,
		NotifyBase:      settings.NotifyBase,
		Params:          params,
		EPayGateway:     settings.Param(model.PaymentMethodEPay, "gateway"),
		EPayPID:         settings.Param(model.PaymentMethodEPay, "pid"),
		EPayTypes:       nonNilStrings(settings.ParamList(model.PaymentMethodEPay, "types")),
		StripeNote:      settings.Param(model.PaymentMethodStripe, "note"),
	}
}

// nonNilStrings 保证 JSON 序列化出 [] 而不是 null。
//
// 前端对数组做 .length / .map 时，null 会导致渲染报错，
// 因此在边界上统一成空数组。
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
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

	// 支付 / 充值参数（整体替换，见下方处理逻辑）
	Payment *paymentSettingsDTO `json:"payment"`
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
	if req.Payment != nil {
		updated, err := mergePaymentSettings(current.Payment, req.Payment)
		if err != nil {
			oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
				oai.TypeInvalidRequest, "invalid_payment_settings")
			return
		}
		current.Payment = updated
	}

	if err := s.deps.Settings.SetMany(ctx, current.ToMap()); err != nil {
		s.respondInternalError(c, "保存系统设置失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// mergePaymentSettings 校验并合并支付设置。
//
// 为什么不直接赋值：支付参数写错会造成真实资损
// （例如兑换比例填成 0 会让所有充值都不到账，填成 100000 会让 1 元换到十万额度），
// 因此必须在保存前拦住明显非法的取值。
func mergePaymentSettings(current model.PaymentSettings, req *paymentSettingsDTO) (model.PaymentSettings, error) {
	updated := current

	updated.Enabled = req.Enabled
	// 通道白名单：以支付通道注册表为准，而不是硬编码几个名字。
	//
	// 这里做两道闸门（防止"开了但用不了"）：
	//  1) 未实现的通道不允许启用（Available=false 的会明确报错）；
	//  2) 密钥未通过环境变量注入的通道不允许启用。
	// 否则用户会在充值页看到支付入口、点下去却下单失败 —— 最糟糕的体验。
	if req.Methods != nil {
		methods := make([]string, 0, len(req.Methods))
		seen := make(map[string]bool, len(req.Methods))
		for _, method := range req.Methods {
			trimmed := strings.TrimSpace(method)
			if trimmed == "" {
				continue // 忽略空项
			}
			if seen[trimmed] {
				continue // 去重，避免同一通道在界面上出现两次
			}
			channel, ok := payment.FindChannel(trimmed)
			if !ok {
				return updated, fmt.Errorf("不支持的支付通道 %q（可选：%s）",
					trimmed, strings.Join(payment.ChannelKeys(), " / "))
			}
			if err := payment.ValidateChannelEnabled(channel, os.LookupEnv); err != nil {
				return updated, err
			}
			seen[trimmed] = true
			methods = append(methods, trimmed)
		}
		updated.Methods = methods
	}

	if req.ExchangeRate > 0 {
		updated.ExchangeRate = req.ExchangeRate
	} else if req.Enabled {
		// 只在"启用充值"时强校验：关闭状态下留 0 也不会造成资损
		return updated, fmt.Errorf("兑换比例必须大于 0（表示 1 元可兑换多少额度）")
	}

	if strings.TrimSpace(req.Currency) != "" {
		updated.Currency = strings.TrimSpace(req.Currency)
	}
	if req.MinCents >= 0 {
		updated.MinCents = req.MinCents
	}
	if req.MaxCents < 0 {
		return updated, fmt.Errorf("单笔最大金额不能为负数")
	}
	updated.MaxCents = req.MaxCents
	if updated.MaxCents > 0 && updated.MaxCents < updated.MinCents {
		return updated, fmt.Errorf("单笔最大金额不能小于最小金额")
	}

	if req.OrderTTLMinutes > 0 {
		updated.OrderTTLMinutes = req.OrderTTLMinutes
	} else if req.Enabled {
		return updated, fmt.Errorf("订单有效期必须大于 0 分钟")
	}

	updated.NotifyBase = strings.TrimRight(strings.TrimSpace(req.NotifyBase), "/")

	// 通道级参数：以 Params 为唯一来源。
	params := make(map[string]string, len(updated.Params)+len(req.Params)+4)
	for key, value := range updated.Params {
		params[key] = value
	}
	for key, value := range req.Params {
		params[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	foldLegacyPaymentFields(params, req)
	// 只保留"已登记通道的已登记字段"，顺手丢掉误传的键，
	// 防止设置表被垃圾键塞满（例如通道改名后遗留的旧键）。
	updated.Params = sanitizePaymentParams(params)

	// 启用某通道却没把必要参数填全时直接拒绝：
	// 否则用户会看到一个"能下单却付不了款"的充值页，这是最令人困惑的故障。
	if updated.Enabled {
		for _, method := range updated.Methods {
			channel, ok := payment.FindChannel(method)
			if !ok {
				continue
			}
			for _, field := range channel.SettingFields() {
				if field.Required && updated.Param(method, field.Key) == "" {
					return updated, fmt.Errorf("启用「%s」需要填写「%s」", channel.Label, field.Label)
				}
			}
		}
	}

	return updated, nil
}

// foldLegacyPaymentFields 把旧版前端提交的专用字段折算进通道参数。
//
// 为什么需要它：升级后前端可能还没更新，仍会提交 epay_gateway / epay_pid 等旧字段。
// 若直接忽略，站长一保存就会把已配好的易支付参数清空 —— 属于升级事故。
// 因此这里把旧字段"翻译"成新的参数键，且不覆盖 Params 里已有的值。
func foldLegacyPaymentFields(params map[string]string, req *paymentSettingsDTO) {
	fold := func(key, value string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.TrimSpace(params[key]) != "" {
			return
		}
		params[key] = value
	}
	fold("epay.gateway", strings.TrimRight(req.EPayGateway, "/"))
	fold("epay.pid", req.EPayPID)
	fold("epay.types", strings.Join(req.EPayTypes, ","))
	fold("stripe.note", req.StripeNote)
}

// sanitizePaymentParams 过滤通道参数，只保留已登记通道的已登记字段。
//
// 两道过滤缺一不可：
//  1. 通道必须在注册表里（否则是拼错或已下线的通道）；
//  2. 字段必须在该通道声明过（否则是拼错或已改名的字段）。
//
// 这样即使前端被改坏，也不可能往设置表里写入任意键。
func sanitizePaymentParams(params map[string]string) map[string]string {
	result := make(map[string]string, len(params))
	for key, value := range params {
		channelKey, fieldKey, found := strings.Cut(strings.TrimSpace(key), ".")
		if !found {
			continue
		}
		channel, ok := payment.FindChannel(channelKey)
		if !ok {
			continue
		}
		declared := false
		for _, field := range channel.SettingFields() {
			if field.Key == fieldKey {
				declared = true
				break
			}
		}
		if !declared {
			continue
		}
		// 空值不落库：让"没填"与"填了空串"在库里表现一致，便于判断通道是否已配置。
		if strings.TrimSpace(value) == "" {
			continue
		}
		result[key] = strings.TrimSpace(value)
	}
	return result
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
