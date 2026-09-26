// 本文件提供"上游渠道类型目录"的后台接口。
//
// 意图（Why）：
//
//	渠道类型目录（internal/channeltype）是"支持哪些上游、每种怎么连"的唯一来源。
//	把它单独开一个接口而不是塞进渠道详情里，是因为后台要多处用到它：
//	  1) 新建/编辑渠道时做**触发式渲染**：选了哪个类型，才展开它必填的字段；
//	  2) 渠道列表按类型筛选与分组展示；
//	  3) 未来做"批量导入渠道"时，按类型给不同的导入模板。
//	前端只认这个接口返回的字段描述，因此新增上游类型不需要改前端代码。
//
// 流转（Flow）：
//
//	后台页面 → GET /api/admin/channel-types → handlerChannelTypes
//	  → channeltype.Types() + 中文标签（大类/鉴权/能力位）→ JSON
//	→ 前端渲染类型选择器，并按 ExtraFields 做条件字段
//
// 扩展（Extend）：
//
//	新增渠道类型只需在 internal/channeltype/catalog.go 加一条数据；
//	本文件不需要任何改动（这正是把"上游差异"做成数据的收益）。
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
)

// channelTypeFieldDTO 是渠道类型的一个额外参数字段（供前端做条件表单）。
type channelTypeFieldDTO struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder"`
	Help        string `json:"help"`
	Default     string `json:"default"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
}

// channelTypeDTO 是一种上游渠道类型。
type channelTypeDTO struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Category string `json:"category"`
	// CategoryLabel 是大类中文名，避免前端硬编码一套中英映射。
	CategoryLabel string `json:"category_label"`
	Protocol      string `json:"protocol"`
	// AuthMode 是对外稳定的枚举值；AuthLabel 是给人看的中文说明。
	AuthMode   string `json:"auth_mode"`
	AuthLabel  string `json:"auth_label"`
	AuthHeader string `json:"auth_header"`
	// DefaultBaseURL 为空表示必须由使用者填写上游地址。
	DefaultBaseURL  string            `json:"default_base_url"`
	BaseURLEditable bool              `json:"base_url_editable"`
	DefaultHeaders  map[string]string `json:"default_headers,omitempty"`
	// Capabilities 是能力位的中文名列表（如 ["对话","流式","工具调用"]）。
	Capabilities      []string              `json:"capabilities"`
	ExtraFields       []channelTypeFieldDTO `json:"extra_fields"`
	SupportsModelList bool                  `json:"supports_model_list"`
	// Available 为 false 时，前端应显示为"即将支持"且禁止选中。
	Available bool   `json:"available"`
	Notes     string `json:"notes"`
}

// handleChannelTypes 返回全部上游渠道类型（含字段定义与中文标签）。
func (s *Server) handleChannelTypes(c *gin.Context) {
	types := channeltype.Types()

	items := make([]channelTypeDTO, 0, len(types))
	available := 0
	for _, item := range types {
		if item.Available {
			available++
		}

		fields := make([]channelTypeFieldDTO, 0, len(item.ExtraFields))
		for _, field := range item.ExtraFields {
			fields = append(fields, channelTypeFieldDTO{
				Key:         field.Key,
				Label:       field.Label,
				Placeholder: field.Placeholder,
				Help:        field.Help,
				Default:     field.Default,
				Required:    field.Required,
				Secret:      field.Secret,
			})
		}

		items = append(items, channelTypeDTO{
			Key:               item.Key,
			Label:             item.Label,
			Category:          string(item.Category),
			CategoryLabel:     channeltype.CategoryLabel(item.Category),
			Protocol:          string(item.Protocol),
			AuthMode:          string(item.AuthMode),
			AuthLabel:         channeltype.AuthLabel(item.AuthMode),
			AuthHeader:        item.AuthHeader,
			DefaultBaseURL:    item.DefaultBaseURL,
			BaseURLEditable:   item.BaseURLEditable,
			DefaultHeaders:    item.DefaultHeaders,
			Capabilities:      nonNilStrings(channeltype.CapabilityNames(item.Caps)),
			ExtraFields:       fields,
			SupportsModelList: item.SupportsModelList,
			Available:         item.Available,
			Notes:             item.Notes,
		})
	}

	// 大类汇总：前端据此渲染分组标签与计数，不必自己遍历统计。
	categories := make([]gin.H, 0, 8)
	for _, category := range []channeltype.Category{
		channeltype.CategoryText,
		channeltype.CategoryEmbedding,
		channeltype.CategoryImage,
		channeltype.CategoryVideo,
		channeltype.CategoryAudio,
		channeltype.CategorySubscription,
		channeltype.CategoryAggregator,
		channeltype.CategorySelfHosted,
	} {
		count := 0
		for _, item := range items {
			if item.Category == string(category) {
				count++
			}
		}
		categories = append(categories, gin.H{
			"key":   string(category),
			"label": channeltype.CategoryLabel(category),
			"count": count,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"items":      items,
		"categories": categories,
		"total":      len(items),
		// available_count 让界面能显示"当前可直接接入 N 种上游"，
		// 与"已登记但适配器待做"的数量区分开，避免使用者误以为全部可用。
		"available_count": available,
	})
}
