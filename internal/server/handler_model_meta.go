// 本文件实现「模型实体」与「渠道模型映射」的后台管理接口，以及模型引用统计。
//
// 意图（Why）：
//
//	把"模型"从渠道上的字符串升级为可管理实体后，后台需要三类能力：
//	  1) 模型清单维护（增删改查 + 关键词/厂商/启用状态筛选）；
//	  2) 渠道级模型映射维护（对外名 ↔ 上游名，整组提交）；
//	  3) 删除前的引用统计（有多少令牌/分组/渠道还在用这个模型）。
//	第 3 点尤其重要：模型名被令牌白名单、渠道清单与映射引用，删掉前若不知道
//	影响面，很容易让线上渠道"静默失联"（表现为用户突然 404）。
//
//	映射的整组替换语义（PUT /channels/:id/mappings）：
//	后台界面通常把映射渲染成一张表一次性提交，因此接口设计为"整组替换"而非
//	逐条增删——这样才能保证"界面所见 = 落库结果"，且避免中间态与孤儿行。
//
// 流转（Flow）：
//
//	ModelsView      → GET/POST/PUT/DELETE /api/admin/models
//	引用提示        → GET /api/admin/models/:name/references（只读）
//	渠道映射编辑    → GET/PUT /api/admin/channels/:id/mappings
//
// 扩展（Extend）：
//
//	新增模型字段（如模态、最大输出长度）时：先在 model.Model 加字段并建迁移加列，
//	再在本文件的 DTO 与 upsert 请求体补充映射。
package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// ---------------------------------------------------------------------------
// 模型实体（后台）
// ---------------------------------------------------------------------------

// modelMetaDTO 是模型实体的对外表示。
type modelMetaDTO struct {
	ID            uint64   `json:"id"`
	Name          string   `json:"name"`
	DisplayName   string   `json:"display_name"`
	Label         string   `json:"label"`
	Vendor        string   `json:"vendor"`
	Description   string   `json:"description"`
	ContextLength int64    `json:"context_length"`
	Enabled       bool     `json:"enabled"`
	Capabilities  []string `json:"capabilities"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
}

// toModelMetaDTO 把领域模型转为对外 DTO。
func toModelMetaDTO(m *model.Model) modelMetaDTO {
	if m == nil {
		return modelMetaDTO{}
	}
	capabilities := m.Capabilities
	if capabilities == nil {
		// 保证 JSON 序列化为 [] 而不是 null，前端无需额外判空
		capabilities = []string{}
	}
	return modelMetaDTO{
		ID:            m.ID,
		Name:          m.Name,
		DisplayName:   m.DisplayName,
		Label:         m.Label(),
		Vendor:        m.Vendor,
		Description:   m.Description,
		ContextLength: m.ContextLength,
		Enabled:       m.Enabled,
		Capabilities:  capabilities,
		CreatedAt:     unixOrZero(m.CreatedAt),
		UpdatedAt:     unixOrZero(m.UpdatedAt),
	}
}

// handleListModelMetas 处理 GET /api/admin/models。
//
// 支持分页（page/size）与筛选（keyword/vendor/enabled）。
func (s *Server) handleListModelMetas(c *gin.Context) {
	if s.deps.Models == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"模型模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	_, size, offset := parsePagination(c)
	query := model.ModelQuery{
		Keyword: strings.TrimSpace(c.Query("keyword")),
		Vendor:  strings.TrimSpace(c.Query("vendor")),
		Limit:   size,
		Offset:  offset,
	}
	// enabled 为可选参数：仅在显式给出 true/false 时过滤，未给出则不过滤
	if raw, ok := c.GetQuery("enabled"); ok {
		if value, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			query.Enabled = &value
		}
	}

	ctx := c.Request.Context()
	total, err := s.deps.Models.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计模型数失败")
		return
	}
	models, err := s.deps.Models.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询模型列表失败")
		return
	}

	items := make([]modelMetaDTO, 0, len(models))
	for _, m := range models {
		items = append(items, toModelMetaDTO(m))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// modelMetaUpsertRequest 是新增/更新模型的请求体。
type modelMetaUpsertRequest struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"display_name"`
	Vendor        string   `json:"vendor"`
	Description   string   `json:"description"`
	ContextLength *int64   `json:"context_length"`
	Enabled       *bool    `json:"enabled"`
	Capabilities  []string `json:"capabilities"`
}

// handleCreateModelMeta 处理 POST /api/admin/models。
func (s *Server) handleCreateModelMeta(c *gin.Context) {
	if s.deps.Models == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"模型模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req modelMetaUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	m := &model.Model{
		Name:         strings.TrimSpace(req.Name),
		DisplayName:  strings.TrimSpace(req.DisplayName),
		Vendor:       strings.TrimSpace(req.Vendor),
		Description:  strings.TrimSpace(req.Description),
		Enabled:      true, // 默认启用：新建即想用，停用应是显式动作
		Capabilities: req.Capabilities,
	}
	if req.ContextLength != nil {
		m.ContextLength = *req.ContextLength
	}
	if req.Enabled != nil {
		m.Enabled = *req.Enabled
	}

	if err := s.deps.Models.Create(c.Request.Context(), m); err != nil {
		if errors.Is(err, model.ErrModelMetaDuplicated) {
			oai.WriteError(c.Writer, http.StatusConflict,
				"该模型名已存在", oai.TypeInvalidRequest, "model_duplicated")
			return
		}
		// 领域校验信息（如"模型名不能包含空白字符"）对使用者有直接帮助
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_model")
		return
	}

	c.JSON(http.StatusOK, toModelMetaDTO(m))
}

// handleUpdateModelMeta 处理 PUT /api/admin/models/{id}。
func (s *Server) handleUpdateModelMeta(c *gin.Context) {
	if s.deps.Models == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"模型模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req modelMetaUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	m, err := s.deps.Models.GetByID(ctx, id)
	if err != nil {
		s.respondModelMetaLookupError(c, err)
		return
	}

	// 模型名不可改：它被令牌白名单、渠道清单与映射引用，改名等于让历史配置失联
	if name := strings.TrimSpace(req.Name); name != "" && name != m.Name {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"模型名不可修改（令牌、渠道与映射通过它关联）；如需改名请新建模型后迁移引用",
			oai.TypeInvalidRequest, "model_name_immutable")
		return
	}
	// 仅覆盖显式提供的字段：未提供时保留原值，避免"只改一个字段"把其它字段清空
	if req.DisplayName != "" {
		m.DisplayName = strings.TrimSpace(req.DisplayName)
	}
	if req.Vendor != "" {
		m.Vendor = strings.TrimSpace(req.Vendor)
	}
	if req.Description != "" {
		m.Description = strings.TrimSpace(req.Description)
	}
	if req.ContextLength != nil {
		m.ContextLength = *req.ContextLength
	}
	if req.Enabled != nil {
		m.Enabled = *req.Enabled
	}
	if req.Capabilities != nil {
		// 允许用 [] 显式清空能力标签（与 nil=不修改 区分）
		m.Capabilities = req.Capabilities
	}

	if err := s.deps.Models.Update(ctx, m); err != nil {
		if errors.Is(err, model.ErrModelMetaNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "模型不存在",
				oai.TypeInvalidRequest, "model_not_found")
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_model")
		return
	}

	c.JSON(http.StatusOK, toModelMetaDTO(m))
}

// handleDeleteModelMeta 处理 DELETE /api/admin/models/{id}。
//
// 说明：不在此处硬拦"仍被引用"的模型——是否强行删除由管理员决定；
// 前端应先调 /references 拿到影响面再提示。
func (s *Server) handleDeleteModelMeta(c *gin.Context) {
	if s.deps.Models == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"模型模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	if err := s.deps.Models.Delete(c.Request.Context(), id); err != nil {
		if errors.Is(err, model.ErrModelMetaNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "模型不存在",
				oai.TypeInvalidRequest, "model_not_found")
			return
		}
		s.respondInternalError(c, "删除模型失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// respondModelMetaLookupError 统一处理模型查询失败。
func (s *Server) respondModelMetaLookupError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrModelMetaNotFound) {
		oai.WriteError(c.Writer, http.StatusNotFound, "模型不存在",
			oai.TypeInvalidRequest, "model_not_found")
		return
	}
	s.respondInternalError(c, "查询模型失败")
}

// handleModelMetaReferences 处理 GET /api/admin/models/{name}/references（只读）。
//
// 返回该模型被多少令牌、分组、渠道与渠道映射引用，供删除前提示。
// 各计数口径：
//   - token_count   令牌白名单包含该模型的令牌数（白名单为空的令牌不算引用）；
//   - channel_count 渠道模型清单包含该模型的渠道数；
//   - group_count   计价规则中引用该模型的分组数（分组本身不直接持有模型）；
//   - mapping_count 模型映射中涉及该模型的映射条数（对外名或上游名命中）。
func (s *Server) handleModelMetaReferences(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"模型名不能为空", oai.TypeInvalidRequest, "invalid_model_name")
		return
	}

	ctx := c.Request.Context()
	tokenCount, channelCount, mappingCount := s.modelReferenceCounts(ctx, name)
	groupCount := s.modelGroupReferenceCount(ctx, name)

	c.JSON(http.StatusOK, gin.H{
		"model":         name,
		"token_count":   tokenCount,
		"channel_count": channelCount,
		"group_count":   groupCount,
		"mapping_count": mappingCount,
		"total":         tokenCount + channelCount + mappingCount,
	})
}

// modelReferenceCounts 统计令牌、渠道与映射对某模型的引用次数。
//
// 实现说明：令牌、渠道、映射总量都不大（各几百条量级），一次性全量扫描即可，
// 无需为它设计专门的聚合查询——引用统计是低频的后台操作。
func (s *Server) modelReferenceCounts(ctx context.Context, name string) (tokenCount, channelCount, mappingCount int) {
	if s.deps.Tokens != nil {
		if tokens, err := s.deps.Tokens.List(ctx, model.TokenQuery{Limit: nameLookupLimit}); err == nil {
			for _, token := range tokens {
				// 白名单为空表示"不限制"，不算作对该模型的引用
				if len(token.Models) > 0 && matchesAnyModelPattern(token.Models, name) {
					tokenCount++
				}
			}
		}
	}

	if s.deps.Channels != nil {
		channels, err := s.deps.Channels.List(ctx, model.ChannelQuery{Limit: nameLookupLimit})
		if err == nil {
			for _, channel := range channels {
				if matchesAnyModelPattern(channel.Models, name) {
					channelCount++
				}
			}
			// 映射按渠道查询，故借用上面已取到的渠道列表逐渠道统计
			if s.deps.ChannelModelMappings != nil {
				for _, channel := range channels {
					mappings, err := s.deps.ChannelModelMappings.ListByChannel(ctx, channel.ID)
					if err != nil {
						continue
					}
					for _, m := range mappings {
						if m.PublicModel == name || m.UpstreamModel == name {
							mappingCount++
						}
					}
				}
			}
		}
	}
	return tokenCount, channelCount, mappingCount
}

// modelGroupReferenceCount 统计计价规则中引用该模型的分组数。
//
// 分组通过计价规则关联模型（model_prices.group_name）：一个分组只要有一条
// 精确指向该模型的规则，就算引用。
func (s *Server) modelGroupReferenceCount(ctx context.Context, name string) int {
	if s.deps.ModelPrices == nil {
		return 0
	}
	prices, err := s.deps.ModelPrices.List(ctx, "", false)
	if err != nil {
		return 0
	}
	groups := make(map[string]struct{})
	for _, price := range prices {
		if price.PatternKind() == model.PatternExact && price.Model == name {
			groups[price.Group] = struct{}{}
		}
	}
	return len(groups)
}

// matchesAnyModelPattern 判断给定名称是否命中任一声明项（支持尾部通配符）。
//
// 与渠道/令牌清单对齐使用前缀通配语义，保证"渠道声明 deepseek-*"这类写法
// 也能被引用统计识别到，避免删模型时漏报影响面。
func matchesAnyModelPattern(patterns []string, name string) bool {
	for _, pattern := range patterns {
		p := strings.TrimSpace(pattern)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if p == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 渠道模型映射（后台）
// ---------------------------------------------------------------------------

// channelModelMappingDTO 是模型映射的对外表示。
type channelModelMappingDTO struct {
	ID            uint64 `json:"id"`
	ChannelID     uint64 `json:"channel_id"`
	UpstreamModel string `json:"upstream_model"`
	PublicModel   string `json:"public_model"`
	Priority      int    `json:"priority"`
	Enabled       bool   `json:"enabled"`
	Remark        string `json:"remark"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// toChannelModelMappingDTO 把领域映射转为对外 DTO。
func toChannelModelMappingDTO(m *model.ChannelModelMapping) channelModelMappingDTO {
	if m == nil {
		return channelModelMappingDTO{}
	}
	return channelModelMappingDTO{
		ID:            m.ID,
		ChannelID:     m.ChannelID,
		UpstreamModel: m.UpstreamModel,
		PublicModel:   m.PublicModel,
		Priority:      m.Priority,
		Enabled:       m.Enabled,
		Remark:        m.Remark,
		CreatedAt:     unixOrZero(m.CreatedAt),
		UpdatedAt:     unixOrZero(m.UpdatedAt),
	}
}

// channelModelMappingItem 是整组替换请求里的单条映射。
type channelModelMappingItem struct {
	UpstreamModel string `json:"upstream_model"`
	PublicModel   string `json:"public_model"`
	Priority      *int   `json:"priority"`
	Enabled       *bool  `json:"enabled"`
	Remark        string `json:"remark"`
}

// channelModelMappingsRequest 是整组替换的请求体。
type channelModelMappingsRequest struct {
	Items []channelModelMappingItem `json:"items"`
}

// handleListChannelMappings 处理 GET /api/admin/channels/{id}/mappings。
func (s *Server) handleListChannelMappings(c *gin.Context) {
	if s.deps.ChannelModelMappings == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"模型映射模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	if !s.channelExists(c, id) {
		return
	}

	mappings, err := s.deps.ChannelModelMappings.ListByChannel(c.Request.Context(), id)
	if err != nil {
		s.respondInternalError(c, "查询渠道模型映射失败")
		return
	}

	items := make([]channelModelMappingDTO, 0, len(mappings))
	for _, m := range mappings {
		items = append(items, toChannelModelMappingDTO(m))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleReplaceChannelMappings 处理 PUT /api/admin/channels/{id}/mappings（整组替换）。
func (s *Server) handleReplaceChannelMappings(c *gin.Context) {
	if s.deps.ChannelModelMappings == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"模型映射模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	if !s.channelExists(c, id) {
		return
	}

	var req channelModelMappingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	mappings := make([]*model.ChannelModelMapping, 0, len(req.Items))
	for _, item := range req.Items {
		m := &model.ChannelModelMapping{
			ChannelID:     id,
			UpstreamModel: strings.TrimSpace(item.UpstreamModel),
			PublicModel:   strings.TrimSpace(item.PublicModel),
			Enabled:       true, // 默认启用：录入即生效，停用应是显式动作
			Remark:        strings.TrimSpace(item.Remark),
		}
		if item.Priority != nil {
			m.Priority = *item.Priority
		}
		if item.Enabled != nil {
			m.Enabled = *item.Enabled
		}
		mappings = append(mappings, m)
	}

	ctx := c.Request.Context()
	if err := s.deps.ChannelModelMappings.ReplaceForChannel(ctx, id, mappings); err != nil {
		if errors.Is(err, model.ErrChannelModelMappingDuplicated) {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"同一渠道内上游模型名不能重复", oai.TypeInvalidRequest, "mapping_duplicated")
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_mapping")
		return
	}

	stored, err := s.deps.ChannelModelMappings.ListByChannel(ctx, id)
	if err != nil {
		s.respondInternalError(c, "读取保存后的模型映射失败")
		return
	}
	items := make([]channelModelMappingDTO, 0, len(stored))
	for _, m := range stored {
		items = append(items, toChannelModelMappingDTO(m))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// channelExists 校验渠道是否存在；不存在时已写出 404 并返回 false。
//
// 渠道仓储未注入时跳过校验（测试或精简部署下允许不装配渠道）。
func (s *Server) channelExists(c *gin.Context, id uint64) bool {
	if s.deps.Channels == nil {
		return true
	}
	if _, err := s.deps.Channels.GetByID(c.Request.Context(), id); err != nil {
		oai.WriteError(c.Writer, http.StatusNotFound, "渠道不存在",
			oai.TypeInvalidRequest, "channel_not_found")
		return false
	}
	return true
}
