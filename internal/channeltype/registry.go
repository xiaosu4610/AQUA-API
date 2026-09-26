// Package channeltype 是上游渠道类型注册表：描述"支持哪些上游、每种上游怎么连、
// 需要填什么、具备哪些能力"。
//
// 意图（Why）：
//
//	上游厂商会不断增加（OpenAI / Azure / Anthropic / Gemini / 国内各家 /
//	聚合平台 / 自建推理框架 / 图像视频音频 / 订阅账号…），每家的差异集中在
//	少数几处：默认地址、鉴权方式、必填的额外参数、请求路径、是否支持拉模型清单。
//	若把这些差异硬编码在路由逻辑与前端表单里，加一个上游就要动多处代码，
//	且极易漏改（典型后果：地址对了但鉴权头不对，表现为 401 却查不出原因）。
//
//	因此把"上游差异"做成数据（本包），带来三个直接好处：
//	  1) 后台界面对每种类型做**触发式渲染**：选了哪个类型，才展开它必填的字段
//	     （选 Azure 出现"部署名 + api-version"，选 OpenAI 什么都不出现）；
//	  2) 新增上游往往只需在目录里加一条数据 —— 若它复用已有的协议适配器，
//	     连代码都不用写；
//	  3) 校验能前置：保存渠道时就检查"这种类型必须有部署名"，而不是等线上 401。
//
// 关键设计：把「渠道类型」与「协议适配器」解耦。
//
//	渠道类型回答"上游是谁"（地址、鉴权、额外参数）；
//	协议适配器回答"用哪套请求/响应格式"（OpenAI 兼容 / Anthropic / Gemini / …）。
//	一个适配器可服务多个类型 —— 例如 DeepSeek、Kimi、智谱、硅基流动虽然厂商不同，
//	但都走 OpenAI 兼容适配器，于是"接入一家新厂商"的成本降到只填地址与密钥。
//
// 流转（Flow）：
//
//	后台：GET /api/admin/channel-types → Types() + 字段定义 → 前端触发式渲染表单
//	保存：server 校验必填字段（Validate）→ 写入渠道
//	转发：relay 按渠道的 type_key 取 Protocol 与 AuthMode → 选适配器 → 组装请求
//
// 扩展（Extend）：
//
//	新增上游类型：
//	  1) 在 catalog.go 里加一条（Key / Label / Category / 默认地址 / 鉴权 / 能力位）；
//	  2) 若它需要新协议或新鉴权方式，再实现对应适配器，然后把 Available 置为 true；
//	  3) 补一条测试用例说明它的关键差异（见 catalog_test.go 的约定）。
//	注意：Available 必须诚实 —— 未实现适配器的类型一律 false，
//	后台会显示"即将支持"而不允许选中，避免用户配好却调不通。
package channeltype

import (
	"fmt"
	"sort"
	"strings"
)

// Category 是渠道的大类，用于后台分组展示与筛选。
type Category string

const (
	// CategoryText 文本大模型（对话/补全）。
	CategoryText Category = "text"
	// CategoryImage 图像生成。
	CategoryImage Category = "image"
	// CategoryVideo 视频生成。
	CategoryVideo Category = "video"
	// CategoryAudio 音频（语音合成/识别/音乐）。
	CategoryAudio Category = "audio"
	// CategoryEmbedding 嵌入与重排。
	CategoryEmbedding Category = "embedding"
	// CategoryAggregator 聚合平台/应用编排/中转。
	CategoryAggregator Category = "aggregator"
	// CategorySelfHosted 自建或本地部署（Ollama / vLLM 等）。
	CategorySelfHosted Category = "self_hosted"
	// CategorySubscription 订阅账号池（走 OAuth，按窗口配额调度）。
	CategorySubscription Category = "subscription"
)

// CategoryLabel 返回大类的中文名（后台展示用）。
func CategoryLabel(category Category) string {
	switch category {
	case CategoryText:
		return "文本大模型"
	case CategoryImage:
		return "图像生成"
	case CategoryVideo:
		return "视频生成"
	case CategoryAudio:
		return "音频"
	case CategoryEmbedding:
		return "嵌入与重排"
	case CategoryAggregator:
		return "聚合与编排"
	case CategorySelfHosted:
		return "自建与本地部署"
	case CategorySubscription:
		return "订阅账号"
	default:
		return string(category)
	}
}

// AuthMode 是鉴权方式，决定请求头怎么带凭据。
type AuthMode string

const (
	// AuthBearer 标准 Authorization: Bearer <key>。
	AuthBearer AuthMode = "bearer"
	// AuthAPIKeyHeader 用自定义头承载，如 Azure 的 api-key。
	AuthAPIKeyHeader AuthMode = "api_key_header"
	// AuthXAPIKey 用 x-api-key 头（Anthropic 系）。
	AuthXAPIKey AuthMode = "x_api_key"
	// AuthQueryKey 把密钥放进查询参数（部分自建服务与 Google 系）。
	AuthQueryKey AuthMode = "query_key"
	// AuthSigV4 AWS 签名（需要 AK/SK 与 region）。
	AuthSigV4 AuthMode = "sigv4"
	// AuthServiceAccount 服务账号 JSON（如 Vertex AI）。
	AuthServiceAccount AuthMode = "service_account"
	// AuthOAuth 订阅账号：需要 refresh_token 换 access_token，可自动续期。
	AuthOAuth AuthMode = "oauth"
	// AuthCookie 网页端会话（部分订阅型上游只认浏览器 Cookie）。
	AuthCookie AuthMode = "cookie"
	// AuthNone 无需鉴权（本地推理服务）。
	AuthNone AuthMode = "none"
)

// AuthLabel 返回鉴权方式的中文说明（后台展示用）。
func AuthLabel(mode AuthMode) string {
	switch mode {
	case AuthBearer:
		return "Authorization: Bearer"
	case AuthAPIKeyHeader:
		return "自定义请求头（如 api-key）"
	case AuthXAPIKey:
		return "x-api-key 请求头"
	case AuthQueryKey:
		return "查询参数携带密钥"
	case AuthSigV4:
		return "AWS 签名（AK/SK）"
	case AuthServiceAccount:
		return "服务账号 JSON"
	case AuthOAuth:
		return "OAuth 订阅账号（可自动续期）"
	case AuthCookie:
		return "浏览器会话 Cookie"
	case AuthNone:
		return "无需鉴权"
	default:
		return string(mode)
	}
}

// Capability 是能力位（用位掩码表达，便于在目录里紧凑声明）。
type Capability uint32

const (
	// CapChat 文本对话。
	CapChat Capability = 1 << iota
	// CapStream 流式输出。
	CapStream
	// CapTools 工具调用（Function Calling）。
	CapTools
	// CapVision 图像输入理解。
	CapVision
	// CapReasoning 推理/思考型模型。
	CapReasoning
	// CapEmbedding 文本嵌入。
	CapEmbedding
	// CapRerank 重排。
	CapRerank
	// CapImage 图像生成。
	CapImage
	// CapVideo 视频生成。
	CapVideo
	// CapAudio 音频（TTS/ASR）。
	CapAudio
	// CapMusic 音乐生成。
	CapMusic
	// CapAsyncTask 走异步任务（提交-轮询-取产物）。
	CapAsyncTask
)

// CapabilityNames 返回能力位对应的中文名列表（后台展示用）。
func CapabilityNames(caps Capability) []string {
	pairs := []struct {
		bit  Capability
		name string
	}{
		{CapChat, "对话"}, {CapStream, "流式"}, {CapTools, "工具调用"},
		{CapVision, "图像理解"}, {CapReasoning, "推理"}, {CapEmbedding, "嵌入"},
		{CapRerank, "重排"}, {CapImage, "图像生成"}, {CapVideo, "视频生成"},
		{CapAudio, "音频"}, {CapMusic, "音乐"}, {CapAsyncTask, "异步任务"},
	}
	names := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if caps&pair.bit != 0 {
			names = append(names, pair.name)
		}
	}
	return names
}

// Has 判断是否具备某个能力位。
func (c Capability) Has(bit Capability) bool { return c&bit != 0 }

// Protocol 是协议适配器名，决定用哪套请求/响应转换。
type Protocol string

const (
	// ProtocolOpenAI OpenAI 兼容（覆盖面最广，多数上游与中转站都支持）。
	ProtocolOpenAI Protocol = "openai"
	// ProtocolAzure Azure OpenAI（部署名 + api-version 在路径与查询里）。
	ProtocolAzure Protocol = "azure"
	// ProtocolAnthropic Anthropic Messages。
	ProtocolAnthropic Protocol = "anthropic"
	// ProtocolGemini Gemini 原生（模型名与动作在路径里）。
	ProtocolGemini Protocol = "gemini"
	// ProtocolVertex Vertex AI（服务账号 + 项目/区域）。
	ProtocolVertex Protocol = "vertex"
	// ProtocolBedrock AWS Bedrock（SigV4 签名）。
	ProtocolBedrock Protocol = "bedrock"
	// ProtocolPaLM Google PaLM 旧协议。
	ProtocolPaLM Protocol = "palm"
	// ProtocolOllama Ollama 原生（可切 OpenAI 兼容）。
	ProtocolOllama Protocol = "ollama"
	// ProtocolCustom 自定义（由额外参数与路径模板驱动）。
	ProtocolCustom Protocol = "custom"
)

// ExtraField 是某种渠道类型**必填或常用**的额外参数。
//
// 为什么要在注册表里声明：这些参数（部署名、api-version、region、项目 ID…）
// 是"该上游能不能连通"的必要信息，但只属于少数类型。声明的价值是
// 后台只在选中该类型时才展示它们，并在保存时做必填校验 ——
// 而不是把十几个字段平铺给使用者（小白站长根本不知道该填哪个）。
type ExtraField struct {
	// Key 字段标识（与渠道的扩展配置同键）。
	Key string
	// Label 中文标签。
	Label string
	// Placeholder 输入示例。
	Placeholder string
	// Help 一行说明：这个值去哪儿拿。
	Help string
	// Default 默认值（为空表示没有默认值）。
	Default string
	// Required 是否必填（该类型没有它必然连不通）。
	Required bool
	// Secret 是否敏感（如 AK/SK、服务账号 JSON），敏感值只走环境变量或加密存储。
	Secret bool
}

// Type 描述一种上游渠道类型。
type Type struct {
	// Key 类型标识（稳定不变，写入渠道记录；改名会导致老数据失去类型）。
	Key string
	// Label 展示名（如「DeepSeek 深度求索」）。
	Label string
	// Category 所属大类。
	Category Category
	// Protocol 使用的协议适配器。
	Protocol Protocol
	// AuthMode 鉴权方式。
	AuthMode AuthMode
	// AuthHeader 当 AuthMode 需要自定义头名时使用（如 api-key）。
	AuthHeader string
	// DefaultBaseURL 默认上游地址；为空表示必须由使用者填写。
	DefaultBaseURL string
	// BaseURLEditable 是否允许使用者覆盖默认地址。
	BaseURLEditable bool
	// DefaultHeaders 必须携带的固定请求头（如 Anthropic 的版本头）。
	DefaultHeaders map[string]string
	// ExtraFields 该类型需要额外填写的参数。
	ExtraFields []ExtraField
	// Caps 能力位。
	Caps Capability
	// SupportsModelList 是否支持从上游拉取模型清单。
	SupportsModelList bool
	// Available 适配器是否已实现：false 时后台显示"即将支持"且不允许选中。
	Available bool
	// Notes 一句话说明（展示在后台，帮助使用者理解这类上游的特点）。
	Notes string
}

// RequiredExtraFields 返回该类型必填的额外参数。
func (t Type) RequiredExtraFields() []ExtraField {
	result := make([]ExtraField, 0, len(t.ExtraFields))
	for _, field := range t.ExtraFields {
		if field.Required {
			result = append(result, field)
		}
	}
	return result
}

// MissingExtraFields 返回未填写的必填额外参数（全部已填时返回空切片）。
//
// values 由渠道记录里的扩展配置提供，键为 ExtraField.Key。
func (t Type) MissingExtraFields(values map[string]string) []string {
	missing := make([]string, 0, len(t.ExtraFields))
	for _, field := range t.RequiredExtraFields() {
		if strings.TrimSpace(values[field.Key]) == "" {
			missing = append(missing, field.Label)
		}
	}
	return missing
}

// Find 按类型标识查找。
func Find(key string) (Type, bool) {
	key = strings.TrimSpace(key)
	for _, item := range Types() {
		if item.Key == key {
			return item, true
		}
	}
	return Type{}, false
}

// Label 返回类型展示名；未知类型回退为标识本身。
func Label(key string) string {
	if item, ok := Find(key); ok {
		return item.Label
	}
	return key
}

// AvailableTypes 只返回适配器已实现的类型（后台允许选中的集合）。
func AvailableTypes() []Type {
	result := make([]Type, 0, len(Types()))
	for _, item := range Types() {
		if item.Available {
			result = append(result, item)
		}
	}
	return result
}

// TypesByCategory 按大类分组返回全部类型（后台分组展示用）。
//
// 组内与组间都按 Label 排序，保证界面稳定（不随目录书写顺序跳动）。
func TypesByCategory() map[Category][]Type {
	grouped := make(map[Category][]Type)
	for _, item := range Types() {
		grouped[item.Category] = append(grouped[item.Category], item)
	}
	for category, items := range grouped {
		sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
		grouped[category] = items
	}
	return grouped
}

// Validate 校验"这个类型 + 这些参数"是否可用于保存渠道。
//
// 三道闸门，缺一不可：
//  1. 类型必须已登记（防止旧渠道指向一个已被删除的类型）；
//  2. 适配器必须已实现（否则用户配好了也调不通）；
//  3. 该类型的必填额外参数必须已填（例如 Azure 没有部署名必然 401/404）。
func Validate(key, baseURL string, values map[string]string) error {
	item, ok := Find(key)
	if !ok {
		return fmt.Errorf("未知的渠道类型 %q（可用：%s）", key, strings.Join(Keys(), " / "))
	}
	if !item.Available {
		return fmt.Errorf("%s 的接入尚未完成，暂不支持选用", item.Label)
	}
	if strings.TrimSpace(baseURL) == "" && strings.TrimSpace(item.DefaultBaseURL) == "" {
		return fmt.Errorf("%s 需要填写上游地址", item.Label)
	}
	if missing := item.MissingExtraFields(values); len(missing) > 0 {
		return fmt.Errorf("%s 还需要填写：%s", item.Label, strings.Join(missing, "、"))
	}
	return nil
}

// Keys 返回全部已登记类型的标识（按字典序，供错误信息与前端筛选使用）。
func Keys() []string {
	all := Types()
	keys := make([]string, 0, len(all))
	for _, item := range all {
		keys = append(keys, item.Key)
	}
	sort.Strings(keys)
	return keys
}
