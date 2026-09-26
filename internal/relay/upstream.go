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
//	新增鉴权方式：简单方式（头/查询参数）在 applyUpstreamAuth 增加分支；
//	  需要完整 URL 或请求体摘要的方式（如 SigV4、服务账号）在 signUpstreamRequest
//	  增加分支（见 signature.go / vertex_auth.go）。在把该渠道类型标为 Available 之前
//	  必须补一条测试；未实现的鉴权方式必须返回明确错误，绝不能静默不带凭据
//	  （那会以 401/403 的形式在上游暴露，极难排查）。
//	新增类型专属路径：在 upstreamPath 增加分支（按 Protocol 分派），
//	  需要新占位符时同步补充 fillPathTemplate。
package relay

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	// bedrockInvokePath 是 Bedrock 非流式调用端点。
	//
	// 模板里用 {model} 占位 Bedrock 的「模型 ID」（如 anthropic.claude-3-5-sonnet-...），
	// 它含冒号等字符，必须按 AWS 规则编码（见 fillBedrockPathTemplate）。
	bedrockInvokePath = "/model/{model}/invoke"
	// bedrockStreamPath 是 Bedrock 流式调用端点。
	bedrockStreamPath = "/model/{model}/invoke-with-response-stream"
	// vertexGeneratePath 是 Vertex AI 非流式生成端点。
	//
	// 与 Gemini 同为 generateContent 协议，但路径里多了项目/区域/发布方三段，
	// 因此需要 {project}/{location}/{publisher} 占位符（值来自渠道 extra_config）。
	vertexGeneratePath = "/v1/projects/{project}/locations/{location}/publishers/{publisher}/models/{model}:generateContent"
	// vertexStreamPath 是 Vertex AI 流式生成端点（配合 alt=sse）。
	vertexStreamPath = "/v1/projects/{project}/locations/{location}/publishers/{publisher}/models/{model}:streamGenerateContent"
	// defaultVertexPublisher 是 Vertex 路径里发布方的缺省值。
	//
	// 绝大多数模型由 Google 发布，缺省填 google，减少站长填写负担；
	// 第三方发布方可通过 extra_config.publisher 覆盖。
	defaultVertexPublisher = "google"
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
		Body:    outBody,
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
	// Extra 是渠道的类型专属参数（如 Azure 的 deployment / api_version、
	// Bedrock 的 region、Vertex 的 project_id / region / publisher）。
	Extra map[string]string
	// Stream 表示本次请求是否要求流式返回。
	//
	// 仅少数协议需要它：Gemini 把"是否流式"写进路径与查询参数，而 OpenAI /
	// Anthropic 用请求体里的 stream 字段表达，故对它们无影响。
	Stream bool
	// Body 是最终要发往上游的请求体（已按协议转换）。
	//
	// 只有 SigV4 需要它：签名覆盖了载荷的 SHA256 摘要，因此组装鉴权头时必须拿到
	// 逐字节的请求体。其余鉴权方式忽略该字段。
	Body []byte
	// now 覆盖签名/签发令牌所用的当前时间；零值表示用 time.Now。
	//
	// 仅供单元测试注入固定时间（SigV4 与 JWT 都依赖时间，固定时钟才能断言确定性结果）；
	// 不导出，生产路径不会设置它。
	now time.Time
}

// clock 返回本次组装使用的时间（UTC）。
func (in upstreamRequestInput) clock() time.Time {
	if in.now.IsZero() {
		return time.Now().UTC()
	}
	return in.now.UTC()
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
//
// 鉴权分两阶段（关键，别合并）：
//   - 第一阶段（applyUpstreamAuth）：只需头/查询参数即可完成，如 Bearer、api-key、
//     查询参数密钥。它们在拼最终 URL 之前处理，因为查询参数密钥属于 URL 的一部分。
//   - 第二阶段（signUpstreamRequest）：SigV4 与服务账号需要「规范化后的 host/path/query」
//     乃至请求体摘要，必须等最终 URL 拼好后再执行，否则签名覆盖的内容与实际发送的不一致。
func buildUpstreamRequest(in upstreamRequestInput) (*UpstreamRequest, error) {
	base, err := resolveUpstreamBaseURL(in)
	if err != nil {
		return nil, err
	}

	path := upstreamPath(in)
	query := url.Values{}
	applyUpstreamQuery(in, query)

	req := &UpstreamRequest{
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

	// 第一阶段鉴权：依赖完整 URL 的方式留到后面处理。
	if !needsSignedRequest(in.Type.AuthMode) {
		injected, err := applyUpstreamAuth(in.Type, in.APIKey, req.Header, query)
		if err != nil {
			return nil, err
		}
		req.authInjected = injected
	}

	rawQuery := query.Encode()
	req.URL = base + path
	if rawQuery != "" {
		req.URL += "?" + rawQuery
	}

	// 第二阶段鉴权：在最终 URL 确定之后签名/换取令牌。
	if needsSignedRequest(in.Type.AuthMode) {
		if err := signUpstreamRequest(in, base, path, rawQuery, req.Header); err != nil {
			return nil, err
		}
		req.authInjected = true
	}
	return req, nil
}

// needsSignedRequest 判断该鉴权方式是否必须拿到"最终 URL 与请求体"才能完成。
//
// SigV4 的签名覆盖规范化 host/path/query 与载荷摘要；服务账号要换取 access_token，
// 二者都不能在 URL 拼好之前完成。
func needsSignedRequest(mode channeltype.AuthMode) bool {
	switch mode {
	case channeltype.AuthSigV4, channeltype.AuthServiceAccount:
		return true
	default:
		return false
	}
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
	case channeltype.ProtocolVertex:
		// Vertex 与 Gemini 同为 generateContent 协议，同样去掉可能的 models/ 前缀；
		// 但路径里多了项目/区域/发布方三段，由 fillPathTemplate 替换占位符。
		in.Model = strings.TrimPrefix(in.Model, "models/")
		if in.Stream {
			path = vertexStreamPath
		} else {
			path = vertexGeneratePath
		}
	case channeltype.ProtocolBedrock:
		// Bedrock 的模型 ID 需按 AWS 规则编码，且签名与实发路径必须逐字节一致，
		// 因此单独走 fillBedrockPathTemplate（见其说明）。
		if in.Stream {
			path = bedrockStreamPath
		} else {
			path = bedrockInvokePath
		}
		return fillBedrockPathTemplate(path, in.Model)
	}
	return fillPathTemplate(path, in)
}

// fillPathTemplate 替换路径模板中的占位符。
//
//	{model}      → 模型名
//	{deployment} → 渠道的部署名；缺省回退为模型名（Azure 常以模型名当部署名）
//	{project}    → 渠道的 GCP 项目 ID（extra_config.project_id）
//	{location}   → Vertex 的区域段（extra_config.region）
//	{publisher}  → 模型发布方；缺省 google（extra_config.publisher 可覆盖）
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
	publisher := extraValue(in, "publisher")
	if publisher == "" {
		publisher = defaultVertexPublisher
	}
	replacer := strings.NewReplacer(
		"{model}", url.PathEscape(in.Model),
		"{deployment}", url.PathEscape(deployment),
		"{project}", url.PathEscape(extraValue(in, "project_id")),
		"{location}", url.PathEscape(extraValue(in, "region")),
		"{publisher}", url.PathEscape(publisher),
	)
	return replacer.Replace(template)
}

// fillBedrockPathTemplate 替换 Bedrock 路径里的 {model} 占位符。
//
// 与通用 fillPathTemplate 的差异：模型 ID 用 AWS 的 URI 编码规则（awsURIEncode），
// 而非 net/url.PathEscape。原因是 Bedrock 的模型 ID 含冒号（如 ...-v2:0），
// SigV4 的规范路径要求把冒号编码为 %3A；若实发路径不编码而签名按编码算，
// 二者不一致会导致 AWS 侧验签失败。这里让"签名用的路径"与"实发路径"同源。
func fillBedrockPathTemplate(template, model string) string {
	return strings.ReplaceAll(template, "{model}", awsURIEncode(model, false))
}

// applyUpstreamQuery 写入类型专属查询参数。
func applyUpstreamQuery(in upstreamRequestInput, query url.Values) {
	switch in.Type.Protocol {
	case channeltype.ProtocolAzure:
		// Azure 用查询参数选择接口版本，缺失或写错会被直接拒绝。
		if version := extraValue(in, "api_version"); version != "" {
			query.Set("api-version", version)
		}
	case channeltype.ProtocolGemini, channeltype.ProtocolVertex:
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
