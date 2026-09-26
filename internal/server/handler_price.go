// 本文件实现模型计价规则的后台管理接口。
//
// 意图（Why）：
//
//	价格是"用量 → 费用"的唯一换算依据。放在后台可维护，是因为不同部署的
//	上游采购成本差别极大（官方直采、代理、免费额度），任何硬编码的价格表
//	都必然与实际成本脱节。
//
//	开源交付考虑：本接口不预设任何厂商价格，全部由使用者自行录入，
//	既避免价格过时带来的误导，也避免把第三方的定价表当作本项目的资产分发。
//
// 流转（Flow）：
//
//	后台价格页 → GET  /api/admin/prices            列出全部规则
//	           → POST /api/admin/prices            新增（改完清空计费缓存）
//	           → PUT  /api/admin/prices/{id}       更新（同上）
//	           → DELETE /api/admin/prices/{id}     删除（同上）
//
// 扩展（Extend）：
//
//	新增计价维度时：在 model.ModelPrice 加字段 → 建迁移加列 → 本文件的
//	DTO / 请求体 / 校验三处同步。
package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// modelPriceDTO 是计价规则的对外表示。
type modelPriceDTO struct {
	ID              uint64 `json:"id"`
	Model           string `json:"model"`
	PromptPrice     int64  `json:"prompt_price"`
	CompletionPrice int64  `json:"completion_price"`
	Group           string `json:"group"`
	Enabled         bool   `json:"enabled"`
	Remark          string `json:"remark"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// toModelPriceDTO 把领域模型转为对外 DTO。
func toModelPriceDTO(price *model.ModelPrice) modelPriceDTO {
	if price == nil {
		return modelPriceDTO{}
	}
	return modelPriceDTO{
		ID:              price.ID,
		Model:           price.Model,
		PromptPrice:     price.PromptPrice,
		CompletionPrice: price.CompletionPrice,
		Group:           price.Group,
		Enabled:         price.Enabled,
		Remark:          price.Remark,
		CreatedAt:       unixOrZero(price.CreatedAt),
		UpdatedAt:       unixOrZero(price.UpdatedAt),
	}
}

// handleListPrices 返回计价规则列表。
//
// 支持按分组过滤：多业务线部署下，管理员通常只想看自己那条线的价格。
func (s *Server) handleListPrices(c *gin.Context) {
	if s.deps.ModelPrices == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"计费模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	group := strings.TrimSpace(c.Query("group"))
	prices, err := s.deps.ModelPrices.List(c.Request.Context(), group, false)
	if err != nil {
		s.respondInternalError(c, "查询计价规则失败")
		return
	}

	items := make([]modelPriceDTO, 0, len(prices))
	for _, price := range prices {
		items = append(items, toModelPriceDTO(price))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// modelPriceUpsertRequest 是新增/更新计价规则的请求体。
type modelPriceUpsertRequest struct {
	Model           string `json:"model"`
	PromptPrice     *int64 `json:"prompt_price"`
	CompletionPrice *int64 `json:"completion_price"`
	Group           string `json:"group"`
	Enabled         *bool  `json:"enabled"`
	Remark          string `json:"remark"`
}

// handleCreatePrice 新增计价规则。
func (s *Server) handleCreatePrice(c *gin.Context) {
	if s.deps.ModelPrices == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"计费模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req modelPriceUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	price := &model.ModelPrice{
		Model:  strings.TrimSpace(req.Model),
		Group:  defaultIfEmpty(strings.TrimSpace(req.Group), defaultChannelGroup),
		Remark: strings.TrimSpace(req.Remark),
		// 默认启用：新增规则的目的通常就是立即生效
		Enabled: true,
	}
	if req.PromptPrice != nil {
		price.PromptPrice = *req.PromptPrice
	}
	if req.CompletionPrice != nil {
		price.CompletionPrice = *req.CompletionPrice
	}
	if req.Enabled != nil {
		price.Enabled = *req.Enabled
	}

	if err := s.deps.ModelPrices.Create(c.Request.Context(), price); err != nil {
		if errors.Is(err, model.ErrModelPriceDuplicated) {
			oai.WriteError(c.Writer, http.StatusConflict,
				"该分组下已存在同名规则（同一模型只能有一条价格）",
				oai.TypeInvalidRequest, "price_duplicated")
			return
		}
		// 领域校验的错误信息（如"价格不能为负数"）对使用者有直接帮助
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_price")
		return
	}

	s.invalidatePriceCache()
	c.JSON(http.StatusOK, toModelPriceDTO(price))
}

// handleUpdatePrice 更新计价规则。
func (s *Server) handleUpdatePrice(c *gin.Context) {
	if s.deps.ModelPrices == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"计费模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req modelPriceUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	price, err := s.deps.ModelPrices.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrModelPriceNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "计价规则不存在", oai.TypeInvalidRequest, "price_not_found")
			return
		}
		s.respondInternalError(c, "查询计价规则失败")
		return
	}

	if strings.TrimSpace(req.Model) != "" {
		price.Model = strings.TrimSpace(req.Model)
	}
	if req.PromptPrice != nil {
		price.PromptPrice = *req.PromptPrice
	}
	if req.CompletionPrice != nil {
		price.CompletionPrice = *req.CompletionPrice
	}
	if group := strings.TrimSpace(req.Group); group != "" {
		price.Group = group
	}
	if req.Enabled != nil {
		price.Enabled = *req.Enabled
	}
	if remark := strings.TrimSpace(req.Remark); remark != "" {
		price.Remark = remark
	}

	if err := s.deps.ModelPrices.Update(ctx, price); err != nil {
		if errors.Is(err, model.ErrModelPriceDuplicated) {
			oai.WriteError(c.Writer, http.StatusConflict,
				"该分组下已存在同名规则", oai.TypeInvalidRequest, "price_duplicated")
			return
		}
		if errors.Is(err, model.ErrModelPriceNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "计价规则不存在", oai.TypeInvalidRequest, "price_not_found")
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_price")
		return
	}

	s.invalidatePriceCache()
	c.JSON(http.StatusOK, toModelPriceDTO(price))
}

// handleDeletePrice 删除计价规则。
//
// 删除后该模型变为"未定价"：转发仍可用，但不再扣费，日志中 quota 记 0。
// 这一语义在界面上有说明，避免管理员误以为"删了价格用户就不能用了"。
func (s *Server) handleDeletePrice(c *gin.Context) {
	if s.deps.ModelPrices == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"计费模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	if err := s.deps.ModelPrices.Delete(c.Request.Context(), id); err != nil {
		if errors.Is(err, model.ErrModelPriceNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "计价规则不存在", oai.TypeInvalidRequest, "price_not_found")
			return
		}
		s.respondInternalError(c, "删除计价规则失败")
		return
	}

	s.invalidatePriceCache()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleQuotePreview 试算某模型的费用，便于管理员核对定价是否合理。
//
// 参数：model、prompt_tokens、completion_tokens（可选，默认按 1000/1000 估算）、
// group（可选，缺省用计费组件的默认分组）——价格规则按分组隔离，试算也需能指定分组。
func (s *Server) handleQuotePreview(c *gin.Context) {
	if s.deps.Billing == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"计费模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	modelName := strings.TrimSpace(c.Query("model"))
	if modelName == "" {
		oai.WriteError(c.Writer, http.StatusBadRequest, "缺少 model 参数", oai.TypeInvalidRequest, "missing_model")
		return
	}
	group := strings.TrimSpace(c.Query("group"))
	promptTokens := parseInt64Query(c, "prompt_tokens", 1000)
	completionTokens := parseInt64Query(c, "completion_tokens", 1000)

	quota := s.deps.Billing.Quote(c.Request.Context(), group, modelName, promptTokens, completionTokens)
	c.JSON(http.StatusOK, gin.H{
		"model":             modelName,
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"quota":             quota,
		"priced":            quota > 0,
	})
}

// invalidatePriceCache 清空计费价格缓存，使改价立即生效。
//
// 不做这件事的后果：管理员改完价格后，转发链路仍用旧价格计费长达 30 秒，
// 期间产生的日志会显示出"改了价但数字没变"，非常容易误判为功能失效。
func (s *Server) invalidatePriceCache() {
	if s.deps.Billing != nil {
		s.deps.Billing.Invalidate()
	}
}

// parseInt64Query 读取整型查询参数，缺省或非法时返回默认值。
func parseInt64Query(c *gin.Context, key string, fallback int64) int64 {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}
