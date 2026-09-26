// 本文件负责「按渠道类型组装上游请求」：把"怎么发这个请求"从转发主链路中抽出。
//
// 意图（Why）：
//
//	不同上游的差异集中在少数几处：地址、鉴权头、必须携带的固定头、请求路径
//	（含占位符）与类型专属查询参数。若把"拼地址 + 加鉴权头"硬编码在转发逻辑里，
//	每接一类上游都要改主链路，且极易漏改——典型后果是"地址对了但鉴权头不对"，
//	线上表现为 401 却查不出原因。因此把这些差异数据化到 channeltype 目录，
//	再用本文件把「目录数据 + 渠道配置」组装成最终可发送的请求。
//
// 流转（Flow）：
//
//	forwardChat
//	  ├─ prepareChannelUpstream(ch, ...)     取规格 + 转换请求体 + 组装 URL/头
//	  │    ├─ upstreamSpecForChannel(ch)     按 type_key 取渠道类型规格
//	  │    ├─ encodeUpstreamRequestBody(...) 请求体协议转换（Anthropic / Gemini）
//	  │    └─ buildUpstreamRequest(input)    组装最终 URL / 请求头（含鉴权）
//	  └─ http.NewRequestWithContext(...)      发送
//
// 扩展（Extend）：
//
//	新增鉴权方式：在 applyUpstreamAuth 增加分支，并在把该渠道类型标为
//	  Available 之前补一条测试；未实现的鉴权方式必须返回明确错误，
//	  绝不能静默不带凭据（那会以 401 的形式在上游暴露，极难排查）。
//	新增类型专属路径：在 upstreamPath 增加分支（按 Protocol 分派）。
package relay

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// 上游路径模板常量。
const (
	// azureChatPathTemplate 是 Azure OpenAI 的对话路径模板。
	//
	// Azure 与标准 OpenAI 的三点差异：地址里不含版本前缀、部署名进路径、
	// 接口版本走查询参数。部署名缺省时可回退为模型名（Azure 常以模型名当部署名）。
	azureChatPathTemplate = "/openai/deployments/{deployment}/chat/completions"
	// anthropicMessagesPath 是 Anthropic Messages 端点。
	//
	// 只拼 /messages 而非 /v1/messages：该类型目录里的默认地址已含 /v1，
	// 两者叠加会拼出 /v1/v1/messages。
	anthropicMessagesPath = "/messages"
	// authQueryKeyParam 是"密钥进查询参数"时的参数名（Google 系约定）。
	authQueryKeyParam = "key"
	// geminiGeneratePath 是 Gemini 非流式生成端点。
	//
	// Gemini 与 OpenAI 的两点差异：模型名与"动作"（生成/流式生成）都在路径里，
	// 而不是在请求体里；因此同一份对话请求在流式与非流式下走两个不同路径。
	geminiGeneratePath = "/v1beta/models/{model}:generateContent"
	// geminiStreamPath 是 Gemini 流式生成端点。
	//
	// 选择 streamGenerateContent 需配合查询参数 alt=sse，才会得到 SSE 分片；
	// 否则上游以 JSON 数组形式分块返回，转换层无法逐事件处理。
	geminiStreamPath = "/v1beta/models/{model}:streamGenerateContent"
)

// errUpstreamBaseURLMissing 表示渠道与类型都没提供上游地址，无法确定请求目标。
//
// 单独定义便于上层据此给出可操作提示（"请填写上游地址"），而不是含糊的 502。
var errUpstreamBaseURLMissing = errors.New("relay: 渠道未填写上游地址，且该渠道类型没有默认地址")

// errRequestBodyConversion 表示内部请求体无法转换为目标上游协议。
//
// 与"组装失败（地址缺失）"区分开：转换失败是调用方请求本身的问题（如工具类型
// 映射不了），应回 400 让使用者自助修正；而组装失败是渠道配置问题，可换渠道重试。
var errRequestBodyConversion = errors.New("relay: 请求体无法转换为上游协议")

// openAICompatibleSpec 是"OpenAI 兼容"这一当前唯一启用形态的渠道类型规格。
//
// 预先解析一次：channeltype.Find 每次调用都会重建整份目录切片，
// 放在每请求的转发路径上属于无谓开销。
var openAICompatibleSpec = mustResolveTypeKey("custom_openai")

// mustResolveTypeKey 从目录里取出类型规格。
//
// 取不到说明目录被改坏——这是启动即可发现的程序错误（catalog_test 亦会钉住），
// 直接 panic 比运行期静默退化更安全：静默退化会让所有上游悄悄走错鉴权方式。
func mustResolveTypeKey(key string) channeltype.Type {
	item, ok := channeltype.Find(key)
	if !ok {
		panic("relay: 渠道目录缺少类型 " + key)
	}
	return item
}

// upstreamSpecForChannel 解析渠道对应的上游类型规格。
//
// 规则（保持向后兼容是硬要求）：
//   - 渠道填写了 type_key 且该类型已登记 → 使用它的 Protocol / AuthMode /
//     DefaultHeaders / ExtraFields，专用适配器据此工作；
//   - type_key 为空（历史渠道），或指向一个未登记的类型（理论不该出现，
//     因为领域层已校验）→ 回退为 OpenAI 兼容规格，转发行为与改造前完全一致。
func upstreamSpecForChannel(ch *model.Channel) channeltype.Type {
	if ch != nil {
		if key := strings.TrimSpace(ch.TypeKey); key != "" {
			if item, ok := channeltype.Find(key); ok {
				return item
			}
		}
	}
	return openAICompatibleSpec
}

// prepareChannelUpstream 把「渠道 + 内部 OpenAI 请求体」组装成可直接发送的上游请求。
//
// 三步：① 按渠道类型解析规格；② 按规格把请求体转换为上游协议；③ 组装 URL 与请求头。
// 之所以把三步收进一个函数而不是散在转发主链路里：它们是"接线"的关键路径，
// 集中后既好测（见 upstream_wiring_test.go），也不易漏掉 Extra / Stream 的传递。
//
// 返回的 spec 供调用方在响应阶段复用（决定是否做入站协议改写）。
// 错误分为两类：请求体转换失败（errors.Is(err, errRequestBodyConversion)，
// 属调用方问题）与其他组装失败（属渠道配置问题，可换渠道重试）。
func prepareChannelUpstream(
	ch *model.Channel, apiKey, modelName, path string,
	body []byte, headers http.Header, stream bool,
) (channeltype.Type, []byte, *UpstreamRequest, error) {
	spec := upstreamSpecForChannel(ch)

	outBody, err := encodeUpstreamRequestBody(spec, body)
	if err != nil {
		return spec, nil, nil, err
	}

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: ch.BaseURL,
		APIKey:  apiKey,
		Model:   modelName,
		Path:    path,
		Headers: headers,
		Extra:   ch.ExtraConfig,
		Stream:  stream,
	})
	if err != nil {
		return spec, nil, nil, err
	}
	return spec, outBody, built, nil
}

// upstreamRequestInput 是组装一次上游请求所需的全部输入。
type upstreamRequestInput struct {
	// Type 是渠道类型规格（默认地址、鉴权方式、固定头、专属参数定义）。
	Type channeltype.Type
	// BaseURL 是渠道配置的上游地址；为空时回退到 Type.DefaultBaseURL。
	BaseURL string
	// APIKey 是本次提交给上游的凭据，仅用于注入，绝不写入日志。
	APIKey string
	// Model 是请求的模型名，用于路径模板的 {model} 占位符。
	Model string
	// Path 是上游端点路径（OpenAI 形态）；各协议可在其基础上改写。
	Path string
	// Headers 是调用方希望携带的基础请求头（如 Accept / User-Agent）。
	Headers http.Header
	// Extra 是渠道的类型专属参数（如 Azure 的 deployment / api_version）。
	Extra map[string]string
	// Stream 表示本次请求是否要求流式返回。
	//
	// 仅少数协议需要它：Gemini 把"是否流式"写进路径与查询参数，而 OpenAI /
	// Anthropic 用请求体里的 stream 字段表达，故对它们无影响。
	Stream bool
}

// UpstreamRequest 描述组装完成、可直接发送的上游请求。
type UpstreamRequest struct {
	// URL 是最终请求地址（含查询参数）。
	URL string
	// Header 是最终请求头（已含鉴权头与类型固定头）。
	Header http.Header

	// authInjected 表示是否真的注入了凭据；authMode 记录鉴权方式。
	// 二者仅用于日志与排障——密钥本身【绝不】出现在任何日志字段里。
	authInjected bool
	authMode     channeltype.AuthMode
}

// LogFields 返回可安全写入日志的字段。
//
// 关键安全约束：只输出"是否已注入凭据"与"鉴权方式"，绝不输出密钥值；
// 地址也只取 host 与 path——当鉴权走查询参数时，密钥在 RawQuery 里，
// 只要不输出 RawQuery 就不会泄露。
func (r *UpstreamRequest) LogFields() map[string]any {
	fields := map[string]any{
		"upstream_auth_mode":     string(r.authMode),
		"upstream_auth_injected": r.authInjected,
	}
	if parsed, err := url.Parse(r.URL); err == nil {
		fields["upstream_host"] = parsed.Host
		fields["upstream_path"] = parsed.Path
	}
	return fields
}

// buildUpstreamRequest 按渠道类型组装上游请求，返回最终 URL 与请求头。
//
// 组装顺序（重要，决定了"谁能覆盖谁"）：
//  1. 基础头（调用方给的 Accept / User-Agent）；
//  2. 类型固定头（DefaultHeaders）——刻意放在最后设置，使调用方无法覆盖，
//     避免模板被意外改坏（如 Anthropic 的 anthropic-version）；
//  3. 鉴权头/查询参数。
func buildUpstreamRequest(in upstreamRequestInput) (*UpstreamRequest, error) {
	base, err := resolveUpstreamBaseURL(in)
	if err != nil {
		return nil, err
	}

	req := &UpstreamRequest{
		URL:      base + upstreamPath(in),
		Header:   http.Header{},
		authMode: in.Type.AuthMode,
	}

	for key, values := range in.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	for key, value := range in.Type.DefaultHeaders {
		req.Header.Set(key, value)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	query := url.Values{}
	applyUpstreamQuery(in, query)
	injected, err := applyUpstreamAuth(in.Type, in.APIKey, req.Header, query)
	if err != nil {
		return nil, err
	}
	req.authInjected = injected

	if encoded := query.Encode(); encoded != "" {
		req.URL += "?" + encoded
	}
	return req, nil
}

// resolveUpstreamBaseURL 解析上游基础地址：渠道填写值优先，其次类型默认值。
func resolveUpstreamBaseURL(in upstreamRequestInput) (string, error) {
	base := strings.TrimSpace(in.BaseURL)
	if base == "" {
		base = strings.TrimSpace(in.Type.DefaultBaseURL)
	}
	if base == "" {
		return "", fmt.Errorf("%w（类型=%s）", errUpstreamBaseURLMissing, in.Type.Key)
	}
	// 去掉末尾多余的斜杠，避免出现 "//v1/..." 这类路径
	return strings.TrimRight(base, "/"), nil
}

// upstreamPath 返回上游端点路径：按协议改写调用方给出的 OpenAI 路径。
func upstreamPath(in upstreamRequestInput) string {
	path := in.Path
	if path == "" {
		path = oai.ChatCompletionsPath
	}
	switch in.Type.Protocol {
	case channeltype.ProtocolAzure:
		// 仅在对话端点上改写；图像/嵌入等 Azure 端点形态不同，后续按需补充。
		if path == oai.ChatCompletionsPath {
			path = azureChatPathTemplate
		}
	case channeltype.ProtocolAnthropic:
		path = anthropicMessagesPath
	case channeltype.ProtocolGemini:
		// Gemini 的模型名直接进路径（模板里已含 /models/ 前缀），
		// 因此去掉调用方可能多带的前缀，避免拼出 /models/models/...。
		in.Model = strings.TrimPrefix(in.Model, "models/")
		if in.Stream {
			path = geminiStreamPath
		} else {
			path = geminiGeneratePath
		}
	}
	return fillPathTemplate(path, in)
}

// fillPathTemplate 替换路径模板中的占位符。
//
//	{model}      → 模型名
//	{deployment} → 渠道的部署名；缺省回退为模型名（Azure 常以模型名当部署名）
//
// 占位符按路径段转义：模型名/部署名可能含 "/"（如 "meta/llama"），
// 不转义会把一个路径段拆成两段，导致请求打到不存在的端点。
func fillPathTemplate(template string, in upstreamRequestInput) string {
	deployment := ""
	if in.Extra != nil {
		deployment = strings.TrimSpace(in.Extra["deployment"])
	}
	if deployment == "" {
		deployment = in.Model
	}
	replacer := strings.NewReplacer(
		"{model}", url.PathEscape(in.Model),
		"{deployment}", url.PathEscape(deployment),
	)
	return replacer.Replace(template)
}

// applyUpstreamQuery 写入类型专属查询参数。
func applyUpstreamQuery(in upstreamRequestInput, query url.Values) {
	switch in.Type.Protocol {
	case channeltype.ProtocolAzure:
		// Azure 用查询参数选择接口版本，缺失或写错会被直接拒绝。
		if version := extraValue(in, "api_version"); version != "" {
			query.Set("api-version", version)
		}
	case channeltype.ProtocolGemini:
		// 流式生成要求 alt=sse，上游才会以 SSE 分片返回（否则是一段 JSON 数组，
		// 无法逐事件转换）。非流式不需要该参数。
		if in.Stream {
			query.Set("alt", "sse")
		}
	}
}

// extraValue 读取类型专属参数：优先渠道填写值，其次类型定义的默认值。
func extraValue(in upstreamRequestInput, key string) string {
	if value := strings.TrimSpace(in.Extra[key]); value != "" {
		return value
	}
	for _, field := range in.Type.ExtraFields {
		if field.Key == key {
			return strings.TrimSpace(field.Default)
		}
	}
	return ""
}

// applyUpstreamAuth 按鉴权方式把凭据注入请求头或查询参数。
//
// 返回 injected 表示是否真的带上了凭据（AuthNone 恒为 false）。
// 未实现的鉴权方式返回错误——绝不能"静默不带凭据"，
// 否则会以 401 的形式在上游暴露，让人误以为是密钥写错。
func applyUpstreamAuth(spec channeltype.Type, apiKey string, header http.Header, query url.Values) (bool, error) {
	switch spec.AuthMode {
	case channeltype.AuthBearer:
		header.Set("Authorization", "Bearer "+apiKey)
		return true, nil
	case channeltype.AuthAPIKeyHeader:
		name := strings.TrimSpace(spec.AuthHeader)
		if name == "" {
			name = "api-key"
		}
		header.Set(name, apiKey)
		return true, nil
	case channeltype.AuthXAPIKey:
		header.Set("x-api-key", apiKey)
		return true, nil
	case channeltype.AuthQueryKey:
		query.Set(authQueryKeyParam, apiKey)
		return true, nil
	case channeltype.AuthNone:
		return false, nil
	default:
		return false, fmt.Errorf(
			"relay: 渠道类型 %s 的鉴权方式 %q 尚未实现，无法组装上游请求",
			spec.Key, spec.AuthMode)
	}
}

// upstreamForwardHeaders 挑选出需要透传给上游的客户端请求头。
//
// 只取 Accept 与 User-Agent：
//   - Accept 是流式（text/event-stream）的协议协商依据；
//   - User-Agent 部分上游按它做风控或功能分级。
//
// 刻意【不】透传 Authorization：客户端带的是本网关令牌，上游要的是渠道密钥；
// 混用既会导致鉴权失败，又会把网关令牌泄露给第三方上游。
func upstreamForwardHeaders(src http.Header) http.Header {
	dst := http.Header{}
	if accept := src.Get("Accept"); accept != "" {
		dst.Set("Accept", accept)
	}
	if ua := src.Get("User-Agent"); ua != "" {
		dst.Set("User-Agent", ua)
	}
	return dst
}
