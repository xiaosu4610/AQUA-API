// 本文件实现「内部 OpenAI chat.completions ↔ ChatGPT Codex Responses」的双向转换。
//
// 意图（Why）：
//
//	Codex 订阅账号唯一可用的对话端点是 Responses 协议，而我们的下游客户端
//	（OpenAI SDK、各类第三方工具）绝大多数只会说 chat.completions。
//	若不转换，站长必须让所有使用者换客户端——那等于这个渠道不可用。
//	因此这里做一次"翻译"：请求方向 chat.completions → Responses，
//	响应方向 Responses（SSE 事件流）→ chat.completions 分片。
//
//	转换不是"字段改名"这么简单，上游有几条硬性约束必须一并满足：
//	  - store 必须为 false（显式传 true 会被拒绝）；
//	  - stream 必须为 true（上游只以 SSE 返回，非流式需求由我们聚合）；
//	  - instructions 必须存在且为非空字符串；
//	  - temperature / top_p / max_output_tokens / metadata / stream_options 等
//	    采样与元数据字段一律不被接受，必须剥掉而不是原样透传（否则 400）。
//
// 流转（Flow）：
//
//	请求：prepareChannelUpstream → encodeUpstreamRequestBody → encodeCodexRequest
//	响应：forwardChat → normalizeUpstreamResponse → adjustCodexUpstreamResponse
//	        ├─ 错误响应：Responses 错误体 → OpenAI 错误体
//	        ├─ 流式：wrapCodexUpstreamStream（逐事件转分片 + 收尾 [DONE]）
//	        └─ 非流式：读完 SSE 聚合成一个 chat.completions 响应
//
// 扩展（Extend）：
//
//	上游新增事件类型（如新的工具调用形态）时：在 codexStreamConverter.consume
//	的 switch 里加分支，并在 codex_responses_test.go 补用例。
//	要支持 /responses/compact 等子端点时：另建转换函数，不要塞进本文件。
package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// codexDefaultInstructions 是客户端没有提供 system 提示时注入的基础指令。
//
// 为什么必须有：Codex 上游要求 instructions 字段存在且为非空字符串，
// 缺失会直接拒绝请求（不是降级，而是失败）。
//
// 刻意不复刻官方 CLI 的长提示词：那是别家的文本资产，我们既无从判断版本对应关系，
// 也不该把它搬进自己的仓库。这里只写一段自述用途的短指令——模型能力不依赖这段文字，
// 但字段不能缺；真正的行为约束来自客户端自己的 system 提示。
const codexDefaultInstructions = "You are a helpful assistant served through an OpenAI-compatible API gateway. " +
	"Answer the user's request directly, accurately and concisely."

// 转换策略：白名单式重建请求体，而不是"复制一份再删字段"。
//
// 为什么这样更安全：上游对不支持的字段返回 400（形如 "Unsupported parameter: temperature"），
// 而下游客户端带的字段五花八门（temperature / stream_options / metadata / 各种厂商私有位）。
// 逐个列举"要删的"必然漏；反过来只挑"上游认识的"重组，未知字段自动被丢弃，
// 既不会误发也不需要在每次上游变更时同步清单。
//
// 已知会被丢弃的典型字段：temperature、top_p、frequency_penalty、presence_penalty、
// logit_bias、n、seed、stop、max_tokens / max_completion_tokens / max_output_tokens、
// metadata、user、safety_identifier、stream_options、truncation、prompt_cache_retention。

// codexChatRequest 是 Codex 转换需要的 chat.completions 请求视图。
//
// 为什么不复用 openAIUpstreamRequest：那个类型是给 Anthropic / Gemini 转换用的
// 最小公共集，缺少 Codex 需要的新字段（response_format / reasoning_effort 等）。
// 各自定义能避免"为了一个协议给公共结构加字段，结果所有协议都要跟着改"。
type codexChatRequest struct {
	Model             string             `json:"model"`
	Messages          []codexChatMessage `json:"messages"`
	Tools             []codexChatTool    `json:"tools"`
	Functions         []codexChatTool    `json:"functions"`
	ToolChoice        json.RawMessage    `json:"tool_choice"`
	FunctionCall      json.RawMessage    `json:"function_call"`
	ResponseFormat    json.RawMessage    `json:"response_format"`
	ReasoningEffort   string             `json:"reasoning_effort"`
	PromptCacheKey    string             `json:"prompt_cache_key"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls"`
}

// codexChatMessage 是 Codex 转换需要的消息视图。
type codexChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []codexToolCall `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

// codexToolCall 是 assistant 消息里的一次工具调用。
type codexToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// codexChatTool 兼容两种工具声明形态：新的 tools[] 与旧的 functions[]。
type codexChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// encodeCodexRequest 把内部 chat.completions 请求体转为 Codex Responses 请求体。
//
// 返回值保证满足三条硬约束：store=false、stream=true、instructions 为非空字符串。
func encodeCodexRequest(body []byte) ([]byte, error) {
	// 客户端本来就说 Responses（Codex CLI 等 /v1/responses 客户端）：
	// 此时不需要翻译，只需做合规性归一。判据是"有 input/instructions、没有 messages"，
	// 它比"看请求来自哪个路由"更可靠——同一份代码因此对两条入口都成立。
	if isResponsesShape(body) {
		return normalizeCodexResponsesBody(body)
	}

	var in codexChatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("relay: 解析待转换的 OpenAI 请求失败: %w", err)
	}

	// 模型名与推理强度：社区约定的 "-high"/"-medium" 后缀在这里被拆开——
	// 上游只认纯模型名，强度要写进 reasoning.effort。
	model, effortFromName := splitCodexModelEffort(in.Model)

	instructions, input := codexInputFromMessages(in.Messages)
	if strings.TrimSpace(instructions) == "" {
		instructions = codexDefaultInstructions
	}

	out := map[string]any{
		"model":        model,
		"instructions": instructions,
		"input":        input,
		// 两条硬约束：不满足上游直接拒绝，因此无条件覆盖下游取值。
		"store":  false,
		"stream": true,
	}

	if tools := codexToolsFromRequest(in); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice, ok := codexToolChoiceFromRequest(in); ok {
		out["tool_choice"] = choice
	}
	if format, ok := codexTextFormatFromRequest(in.ResponseFormat); ok {
		out["text"] = map[string]any{"format": format}
	}
	if in.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *in.ParallelToolCalls
	}
	if key := strings.TrimSpace(in.PromptCacheKey); key != "" {
		out["prompt_cache_key"] = key
	}

	effort := in.ReasoningEffort
	if strings.TrimSpace(effort) == "" {
		effort = effortFromName
	}
	if effort = normalizeCodexEffort(effort); effort != "" {
		// summary 固定 auto：上游据此在流里回传推理摘要，
		// 我们把它映射成下游的 reasoning_content，客户端的"思考过程"才看得见。
		out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
		// 带推理时必须显式请求加密的推理内容，否则上游不回传 reasoning 事件。
		out["include"] = []string{"reasoning.encrypted_content"}
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("relay: 序列化 Codex 请求失败: %w", err)
	}
	return encoded, nil
}

// isResponsesShape 判断请求体是否已经是 Responses 协议形态。
//
// 判据取"有 input 或 instructions、且没有 messages"：
//   - input / instructions 是 Responses 的专有顶层字段；
//   - messages 是 chat.completions 的专有字段，两者不会同时出现。
//
// 用字段特征而不是"请求来自哪个路由"来判定，有两个好处：
// 路由变更不会影响转换选择；同一份代码对两条入口都成立，不必分叉。
func isResponsesShape(body []byte) bool {
	var probe struct {
		Messages     json.RawMessage `json:"messages"`
		Input        json.RawMessage `json:"input"`
		Instructions json.RawMessage `json:"instructions"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	if len(probe.Messages) > 0 {
		return false
	}
	return len(probe.Input) > 0 || len(probe.Instructions) > 0
}

// codexUnsupportedResponsesFields 是 ChatGPT 内部端点不接受、但 Responses 客户端
// 可能会带的字段。
//
// 与 chat 方向的白名单重建不同，这里必须"点名删除"：客户端给的 input/tools 等
// 内容是权威的，重建会丢信息；而这些字段是上游明确拒绝的，留着就必然 400。
// 因此这个清单只包含"确定不被接受"的字段。
var codexUnsupportedResponsesFields = []string{
	"max_output_tokens", "max_completion_tokens", "temperature", "top_p",
	"frequency_penalty", "presence_penalty", "logit_bias", "n", "seed",
	"user", "metadata", "safety_identifier", "prompt_cache_retention",
	"stream_options", "truncation", "stop_sequences", "chat_template_kwargs",
	// previous_response_id 依赖上游侧的服务端会话存储，而我们强制 store=false，
	// 该字段必然无效，留着只会让请求被拒。
	"previous_response_id",
}

// normalizeCodexResponsesBody 对"已经是 Responses 形态"的请求体做合规性归一。
//
// 三件事（与 chat 方向完全一致的三条硬约束）：
//  1. store 强制 false、stream 强制 true；
//  2. instructions 必须是非空字符串（缺失时注入默认，非字符串时纠正为空串再注入）；
//  3. 删除上游不接受的字段。
//
// 其余内容（input / tools / reasoning / text 等）原样保留——
// 客户端说的就是上游的语言，我们只做合规校验，不做翻译。
func normalizeCodexResponsesBody(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("relay: 解析 Responses 请求失败: %w", err)
	}
	if payload == nil {
		return nil, fmt.Errorf("relay: Responses 请求体必须是 JSON 对象")
	}

	for _, field := range codexUnsupportedResponsesFields {
		delete(payload, field)
	}
	payload["store"] = false
	payload["stream"] = true

	callerInstructions, _ := payload["instructions"].(string)
	payload["instructions"] = strings.TrimSpace(callerInstructions)
	if strings.TrimSpace(callerInstructions) == "" {
		model, _ := payload["model"].(string)
		payload["instructions"] = codexDefaultInstructionsFor(model)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("relay: 序列化 Responses 请求失败: %w", err)
	}
	return encoded, nil
}

// codexDefaultInstructionsFor 返回按模型选择的默认指令。
//
// 目前所有模型共用一段自述用途的短指令：上游只要求该字段非空，
// 内容不影响能力；真正的行为约束来自客户端自己的 instructions。
func codexDefaultInstructionsFor(string) string {
	return codexDefaultInstructions
}

// splitCodexModelEffort 从模型名尾部拆出推理强度后缀。
//
// 例："gpt-5.4-high" → ("gpt-5.4", "high")。
//
// 为什么要做这件事：社区约定用后缀表达强度（"-high"/"-xhigh"），但上游只认
// 纯模型名——原样发出会得到"模型不存在"。拆成 model + reasoning.effort 既让后缀
// 生效，又不必要求站长在后台为每个强度各配一条模型映射。
//
// 只识别已知的强度词，因此 "-mini" / "-turbo" 这类正常后缀不会被误拆。
func splitCodexModelEffort(model string) (string, string) {
	trimmed := strings.TrimSpace(model)
	idx := strings.LastIndex(trimmed, "-")
	if idx <= 0 || idx == len(trimmed)-1 {
		return trimmed, ""
	}
	suffix := strings.ToLower(trimmed[idx+1:])
	switch suffix {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return trimmed[:idx], suffix
	}
	return trimmed, ""
}

// normalizeCodexEffort 把强度写法归一成上游接受的取值；空串表示无法识别。
func normalizeCodexEffort(effort string) string {
	switch value := strings.ToLower(strings.TrimSpace(effort)); value {
	case "none", "minimal", "low", "medium", "high":
		return value
	case "xhigh", "extrahigh", "max":
		// 上游只认 xhigh；max 是部分客户端的叫法，统一收敛过去。
		return "xhigh"
	default:
		return ""
	}
}

// codexInputFromMessages 把 messages 转成 Responses 的 input，并抽出 system 文本。
//
// 为什么 system 要"抽出来"而不是留在 input 里：Responses 协议中 system 提示的位置
// 是顶层 instructions 字段；把 role=system 的项混在 input 里，上游会按未知角色处理
// （部分实现直接 400）。抽取保持原序，多条 system 用空行连接。
// role=developer 与 system 语义相同，一并归入。
func codexInputFromMessages(messages []codexChatMessage) (string, []any) {
	systemTexts := make([]string, 0, 2)
	items := make([]any, 0, len(messages))

	for _, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system", "developer":
			if text := codexContentToText(msg.Content); strings.TrimSpace(text) != "" {
				systemTexts = append(systemTexts, text)
			}
		case "assistant":
			items = append(items, codexAssistantItems(msg)...)
		case "tool", "function":
			items = append(items, codexToolOutputItem(msg))
		default:
			// user 与未知角色：都按用户输入处理，保留内容总比整条丢弃好
			if content := codexUserContent(msg.Content); len(content) > 0 {
				items = append(items, map[string]any{"role": "user", "content": content})
			}
		}
	}

	return strings.Join(systemTexts, "\n\n"), items
}

// codexUserContent 把用户消息内容转成 Responses 的 content 数组。
//
// 支持两种形态：纯字符串与多模态数组。必须显式改写 part 类型名，
// 因为两种协议不同（text → input_text、image_url → input_image），
// 原样透传会被上游当成未知类型而拒绝。
func codexUserContent(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []any{map[string]any{"type": "input_text", "text": text}}
	}

	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	converted := make([]any, 0, len(parts))
	for _, part := range parts {
		switch kind, _ := part["type"].(string); kind {
		case "text", "input_text":
			if value, _ := part["text"].(string); strings.TrimSpace(value) != "" {
				converted = append(converted, map[string]any{"type": "input_text", "text": value})
			}
		case "image_url", "input_image":
			if url := codexImageURL(part); url != "" {
				converted = append(converted, map[string]any{"type": "input_image", "image_url": url})
			}
		}
	}
	return converted
}

// codexImageURL 从多模态 part 里取出图片地址。
//
// OpenAI 的 image_url 既可能是字符串，也可能是 {"url": "..."} 对象，
// 两种形态在实际客户端里都存在，这里一并兼容。
func codexImageURL(part map[string]any) string {
	switch value := part["image_url"].(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		url, _ := value["url"].(string)
		return strings.TrimSpace(url)
	}
	return ""
}

// codexAssistantItems 把 assistant 消息转成 Responses 的 output 项。
//
// 一条 assistant 消息可能同时含文本与工具调用，因此返回切片：
// 文本项在前、工具调用项在后——与上游回传顺序一致，便于对话续写。
func codexAssistantItems(msg codexChatMessage) []any {
	items := make([]any, 0, 1+len(msg.ToolCalls))

	if text := codexContentToText(msg.Content); strings.TrimSpace(text) != "" {
		items = append(items, map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		})
	}

	for _, call := range msg.ToolCalls {
		arguments := strings.TrimSpace(call.Function.Arguments)
		if arguments == "" {
			// 上游要求 arguments 是合法 JSON；空参数用 "{}" 表示"无参数"，
			// 空串会让部分实现解析失败。
			arguments = "{}"
		}
		items = append(items, map[string]any{
			"type":      "function_call",
			"call_id":   call.ID,
			"name":      call.Function.Name,
			"arguments": arguments,
		})
	}
	return items
}

// codexToolOutputItem 把 tool/function 角色的结果消息转成 function_call_output 项。
func codexToolOutputItem(msg codexChatMessage) any {
	callID := strings.TrimSpace(msg.ToolCallID)
	if callID == "" {
		// 少数客户端用 name 回填调用关联；上游要求必有 call_id，用它兜住。
		callID = strings.TrimSpace(msg.Name)
	}
	output := codexContentToText(msg.Content)
	if strings.TrimSpace(output) == "" {
		// 上游不接受空 output；显式写一个占位串，避免整条被拒。
		output = "(empty)"
	}
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  output,
	}
}

// codexContentToText 把消息内容拉平成纯文本（工具结果与 assistant 文本都用它）。
//
// 多模态内容里的图片不参与文本拼接：assistant 历史里的图片无法回传，
// 强行序列化只会把一串 base64 塞进上下文，既无意义又浪费 token。
func codexContentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		value, _ := part["text"].(string)
		if strings.TrimSpace(value) == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(value)
	}
	return builder.String()
}

// codexToolsFromRequest 把 tools[] / functions[] 统一转成 Responses 的工具声明。
//
// 两种形态都要支持：新客户端用 tools（OpenAI 现行规范），
// 老客户端仍在用 functions（2023 年的旧规范），只支持一种就会让一半工具调用失效。
func codexToolsFromRequest(in codexChatRequest) []any {
	converted := make([]any, 0, len(in.Tools)+len(in.Functions))

	appendTool := func(name, description string, parameters json.RawMessage, strict *bool) {
		if strings.TrimSpace(name) == "" {
			return
		}
		// Responses 的工具声明是"扁平"的（没有 function 这一层）。
		tool := map[string]any{"type": "function", "name": strings.TrimSpace(name)}
		if strings.TrimSpace(description) != "" {
			tool["description"] = description
		}
		if len(parameters) > 0 {
			tool["parameters"] = parameters
		} else {
			// 上游要求 parameters 存在；无参数工具用空对象 schema 表示。
			tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if strict != nil {
			tool["strict"] = *strict
		}
		converted = append(converted, tool)
	}

	for _, tool := range in.Tools {
		appendTool(tool.Function.Name, tool.Function.Description, tool.Function.Parameters, tool.Function.Strict)
	}
	for _, tool := range in.Functions {
		// 旧 functions[] 是扁平的，字段直接挂在元素上
		name, description, parameters := tool.Name, tool.Description, tool.Parameters
		if name == "" {
			name, description, parameters = tool.Function.Name, tool.Function.Description, tool.Function.Parameters
		}
		appendTool(name, description, parameters, tool.Function.Strict)
	}
	return converted
}

// codexToolChoiceFromRequest 转换工具选择策略。
//
// "auto"/"none"/"required" 两种协议同名，直传；
// 指定函数时形态不同（chat 用 {type:function,function:{name}}，
// Responses 用 {type:function,name}），必须改写。
func codexToolChoiceFromRequest(in codexChatRequest) (any, bool) {
	raw := in.ToolChoice
	if len(raw) == 0 {
		// 旧字段 function_call 只在没有 tool_choice 时才作数
		raw = in.FunctionCall
	}
	if len(raw) == 0 {
		return nil, false
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "auto", "none", "required":
			return strings.ToLower(strings.TrimSpace(text)), true
		default:
			return nil, false
		}
	}

	var object struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false
	}
	name := strings.TrimSpace(object.Name)
	if name == "" {
		name = strings.TrimSpace(object.Function.Name)
	}
	if name == "" {
		return nil, false
	}
	return map[string]any{"type": "function", "name": name}, true
}

// codexTextFormatFromRequest 把 response_format 转成 Responses 的 text.format。
//
// 只有 json_object 与 json_schema 两种形态有意义（text 是默认值，不必发）。
// schema 的键名也从 json_schema 提到 format 顶层——两种协议的嵌套层级不同。
func codexTextFormatFromRequest(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}

	var object struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict *bool           `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false
	}

	switch strings.ToLower(strings.TrimSpace(object.Type)) {
	case "json_object":
		return map[string]any{"type": "json_object"}, true
	case "json_schema":
		format := map[string]any{"type": "json_schema"}
		if name := strings.TrimSpace(object.JSONSchema.Name); name != "" {
			format["name"] = name
		}
		if len(object.JSONSchema.Schema) > 0 {
			format["schema"] = object.JSONSchema.Schema
		}
		if object.JSONSchema.Strict != nil {
			format["strict"] = *object.JSONSchema.Strict
		}
		return format, true
	default:
		return nil, false
	}
}

// ---------------------------------------------------------------------------
// 响应方向：Responses 事件流 → chat.completions
// ---------------------------------------------------------------------------

// adjustCodexUpstreamResponse 把上游 Codex 响应改写为内部 OpenAI 形态。
//
// 三条分支与 Anthropic/Gemini 的同类函数保持一致：
//   - 错误（>=400）：改写为 OpenAI 错误体，便于下游按熟悉的错误结构解析；
//   - 流式成功（下游要 chat）：把 Responses SSE 逐事件转成 chat.completions 分片；
//   - 非流式成功（下游要 chat）：读完（上游总是流式的）SSE 后聚合成一个完整响应。
//
// 参数 downstreamResponses 表示"下游客户端本身就说 Responses"（走 /v1/responses）。
// 此时上游与下游同协议，绝不能改写——客户端看不懂 chat.completions 的分片，
// 改写等于把一条本来直通的链路弄坏。只需保证错误体结构一致即可。
func adjustCodexUpstreamResponse(resp *http.Response, wantStream, downstreamResponses bool) error {
	if resp == nil || resp.Body == nil {
		return nil
	}

	if resp.StatusCode >= http.StatusBadRequest {
		raw, err := readAllLimited(resp.Body, maxAdaptedBodyBytes)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		replaceResponseBody(resp, codexErrorBody(raw, resp.StatusCode))
		resp.Header.Set("Content-Type", "application/json")
		return nil
	}

	if downstreamResponses {
		// 同协议直通：不改 body、不改 Content-Type（上游已是 text/event-stream）。
		return nil
	}

	if wantStream {
		resp.Body = wrapCodexUpstreamStream(resp.Body)
		resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
		// 长度未知：置 -1 让 Go 用分块编码重新决定消息边界。
		resp.ContentLength = -1
		return nil
	}

	raw, err := readAllLimited(resp.Body, maxAdaptedBodyBytes)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	replaceResponseBody(resp, aggregateCodexStream(raw))
	resp.Header.Set("Content-Type", "application/json")
	return nil
}

// codexErrorBody 把上游错误体改写成 OpenAI 错误结构。
//
// 上游错误体的形态不止一种：Responses 的 {"error":{...}} / {"detail":"..."} /
// 纯文本。统一映射到 OpenAI 的 error.message，下游才能按同一套逻辑展示。
// 解析不出结构化内容时把原文截断后放进 message——上游的原话对排查最有价值。
func codexErrorBody(raw []byte, status int) []byte {
	message := ""
	errorType := "invalid_request_error"
	code := ""

	var envelope struct {
		Detail string `json:"detail"`
		Error  *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		switch {
		case envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "":
			message = envelope.Error.Message
			if v := strings.TrimSpace(envelope.Error.Type); v != "" {
				errorType = v
			}
			switch v := envelope.Error.Code.(type) {
			case string:
				code = v
			case float64:
				code = fmt.Sprintf("%d", int(v))
			}
		case strings.TrimSpace(envelope.Detail) != "":
			message = envelope.Detail
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if len(message) > 2000 {
		message = message[:2000]
	}

	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"code":    code,
		},
	})
	if err != nil {
		return []byte(`{"error":{"message":"上游请求失败","type":"invalid_request_error"}}`)
	}
	return body
}

// wrapCodexUpstreamStream 把上游 Responses SSE 流转成 chat.completions 分片流。
//
// 与 Anthropic / Gemini 的流式包装同构：起一个 goroutine 逐行读上游、
// 逐事件转换、通过 io.Pipe 交给转发主链路；客户端断开时 Close 会终止 goroutine。
func wrapCodexUpstreamStream(upstream io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	converter := newCodexStreamConverter()

	go func() {
		scanner := bufio.NewScanner(upstream)
		scanner.Buffer(make([]byte, 0, 64*1024), StreamLimitsNow().CodexLineBytes)

		for scanner.Scan() {
			select {
			case <-done:
				_ = writer.Close()
				return
			default:
			}

			payload, ok := codexSSEPayload(scanner.Bytes())
			if !ok {
				continue
			}
			if out := converter.consume(payload); len(out) > 0 {
				if _, err := writer.Write(out); err != nil {
					return
				}
			}
		}
		// 上游提前断流时也要收尾，否则客户端会一直等 [DONE]。
		if out := converter.finalize(); len(out) > 0 {
			_, _ = writer.Write(out)
		}
		_ = writer.Close()
	}()

	return &codexStreamReader{reader: reader, writer: writer, upstream: upstream, done: done}
}

// Codex（Responses）SSE 事件行的字节上限改为运行期可调（默认 8 MiB），
// 取值与区间定义在 model.LimitSettings，读取见 StreamLimitsNow().CodexLineBytes。
// 默认比 Anthropic 的常规事件大得多：推理事件的加密内容与图片回显都可能很长；
// 给足余量，同时仍能挡住异常上游把内存打满。

// codexSSEPayload 从一行 SSE 文本里取出 data 段。
//
// Responses 的 SSE 同时带 event: 与 data: 两行，事件类型其实就在 data 的 JSON 里
// （字段 type），因此这里忽略 event: 行，只解析 data。
func codexSSEPayload(line []byte) ([]byte, bool) {
	trimmed := strings.TrimSpace(string(line))
	payload, ok := strings.CutPrefix(trimmed, "data:")
	if !ok {
		return nil, false
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	return []byte(payload), true
}

// codexStreamEvent 是转换需要的 Responses 事件视图（只取用得到的字段）。
type codexStreamEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex *int   `json:"output_index"`
	Arguments   string `json:"arguments"`
	Item        *struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Usage  *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			PromptTokens       int `json:"prompt_tokens"`
			CompletionTokens   int `json:"completion_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// codexAggregatedToolCall 是聚合模式下累积出的一次工具调用。
type codexAggregatedToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// codexStreamConverter 把 Responses 事件流转换成 chat.completions 分片。
//
// 同一个转换器同时服务两种下游：流式时 consume 的返回值直接写给客户端；
// 非流式时忽略返回值，最后用 aggregate 把累积结果组装成一个完整响应。
// 这样"两种模式的行为差异"只体现在输出形态上，事件解析逻辑只有一份。
type codexStreamConverter struct {
	id      string
	model   string
	created int64

	sentRole bool
	nextTool int
	// toolIndexByOutput 把上游的 output_index 映射到下游的 tool_calls 下标，
	// 因为上游按"输出项序号"编号，下游按"工具调用序号"编号，两者并不相等。
	toolIndexByOutput map[int]int
	// argumentsByOutput 记录已经下发过的参数片段，用于 done 事件补差量。
	argumentsByOutput map[int]string
	usage             *openAIUsage
	finished          bool

	// 聚合态（非流式）
	content   strings.Builder
	reasoning strings.Builder
	tools     []*codexAggregatedToolCall
	finish    string
}

// newCodexStreamConverter 创建转换器。
func newCodexStreamConverter() *codexStreamConverter {
	return &codexStreamConverter{
		created:           time.Now().Unix(),
		toolIndexByOutput: map[int]int{},
		argumentsByOutput: map[int]string{},
	}
}

// consume 处理一个上游事件，返回要写给客户端的分片（可能为空）。
func (c *codexStreamConverter) consume(payload []byte) []byte {
	var event codexStreamEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		// 上游可能出现空数据行或非 JSON 心跳：忽略而不是中断整条流，
		// 否则一个畸形事件就会让用户看到回答被截断。
		return nil
	}
	// 有些实现把错误放在顶层 error 事件里（HTTP 200 + 流内错误）。
	if event.Type == "error" {
		c.finish = "stop"
		return nil
	}

	switch event.Type {
	case "response.created":
		c.absorbResponseMeta(&event)
		return c.roleChunk()

	case "response.output_text.delta":
		if event.Delta == "" {
			return nil
		}
		c.content.WriteString(event.Delta)
		return c.deltaChunk(map[string]any{"content": event.Delta})

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if event.Delta == "" {
			return nil
		}
		c.reasoning.WriteString(event.Delta)
		return c.deltaChunk(map[string]any{"reasoning_content": event.Delta})

	case "response.output_item.added":
		return c.handleOutputItemAdded(&event)

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return c.handleArgumentsDelta(&event)

	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		return c.handleArgumentsDone(&event)

	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return c.handleTerminal(&event)
	}
	return nil
}

// absorbResponseMeta 吸收响应级元信息（id / model / usage / 未完成原因）。
func (c *codexStreamConverter) absorbResponseMeta(event *codexStreamEvent) {
	if event.Response == nil {
		return
	}
	if id := strings.TrimSpace(event.Response.ID); id != "" {
		c.id = id
	}
	if model := strings.TrimSpace(event.Response.Model); model != "" {
		c.model = model
	}
	if event.Response.Usage != nil {
		c.usage = normalizeCodexUsage(event.Response.Usage)
	}
}

// handleOutputItemAdded 处理"新增输出项"：只有函数调用需要下发首个 tool_calls 分片。
func (c *codexStreamConverter) handleOutputItemAdded(event *codexStreamEvent) []byte {
	if event.Item == nil || event.Item.Type != "function_call" {
		return nil
	}
	// 上游若直接给了完整参数（部分实现只在 added 里给），先按增量记下来，
	// 后续 done 事件会据此少发或不发。
	if event.Item.Arguments != "" {
		c.appendArguments(c.outputIndex(event), event.Item.Arguments)
	}

	index := c.toolIndex(event)
	tool := map[string]any{
		"index": index,
		"id":    event.Item.CallID,
		"type":  "function",
		"function": map[string]any{
			"name": event.Item.Name,
		},
	}
	return c.deltaChunk(map[string]any{"tool_calls": []any{tool}})
}

// handleArgumentsDelta 处理工具参数增量。
func (c *codexStreamConverter) handleArgumentsDelta(event *codexStreamEvent) []byte {
	if event.Delta == "" {
		return nil
	}
	index := c.toolIndex(event)
	c.appendArguments(c.outputIndex(event), event.Delta)

	tool := map[string]any{
		"index":    index,
		"function": map[string]any{"arguments": event.Delta},
	}
	return c.deltaChunk(map[string]any{"tool_calls": []any{tool}})
}

// handleArgumentsDone 处理工具参数完成事件。
//
// 上游的 done 里带的是【完整参数】，而我们（可能）已经按 delta 下发过一部分。
// 只有当我们记录的累计值是完整值的前缀时，才补发剩余部分——
// 否则会把已经发过的参数重复下发一遍，客户端拼出来的 JSON 就坏了。
func (c *codexStreamConverter) handleArgumentsDone(event *codexStreamEvent) []byte {
	complete := event.Arguments
	if complete == "" {
		complete = event.Delta
	}
	if complete == "" {
		complete = map[bool]string{true: "{}"}[true]
	}

	outputIndex := c.outputIndex(event)
	sent := c.argumentsByOutput[outputIndex]
	if complete == sent {
		return nil
	}
	if !strings.HasPrefix(complete, sent) {
		// 完整值不是已发值的前缀：说明我们发过不该发的内容（上游行为异常），
		// 此时宁可不补发，也不要构造出客户端无法解析的参数。
		return nil
	}

	remainder := complete[len(sent):]
	c.appendArguments(outputIndex, remainder)
	tool := map[string]any{
		"index":    c.toolIndex(event),
		"function": map[string]any{"arguments": remainder},
	}
	return c.deltaChunk(map[string]any{"tool_calls": []any{tool}})
}

// handleTerminal 处理终止事件：下发 finish_reason 与 usage，并写 [DONE]。
//
// 为什么把 usage 单独放一个 choices 为空的 chunk：与 OpenAI 在
// stream_options.include_usage 下的行为一致，客户端解析器都能接受；
// 而它同时是网关计费的唯一数据来源，因此【必须】下发。
func (c *codexStreamConverter) handleTerminal(event *codexStreamEvent) []byte {
	if c.finished {
		return nil
	}
	c.absorbResponseMeta(event)

	c.finish = codexFinishReason(event, len(c.tools) > 0 || c.hasToolOutput())
	c.finished = true

	var out []byte
	out = append(out, c.finishChunk(c.finish)...)
	if c.usage != nil {
		out = append(out, c.usageChunk()...)
	}
	out = append(out, []byte("data: [DONE]\n\n")...)
	return out
}

// hasToolOutput 判断本轮是否出现过工具调用（用于决定 finish_reason）。
func (c *codexStreamConverter) hasToolOutput() bool {
	return len(c.argumentsByOutput) > 0
}

// finalize 在流意外结束时收尾，保证客户端总能收到结束标记。
//
// 方法名刻意不叫 finish：结构体已有一个同名状态字段（记录已经推导出的
// finish_reason），同名会让 Go 编译器直接拒绝（字段与方法不能同名）。
func (c *codexStreamConverter) finalize() []byte {
	if c.finished {
		return nil
	}
	c.finished = true

	finish := c.finish
	if finish == "" {
		finish = codexFinishReason(nil, c.hasToolOutput())
	}
	var out []byte
	out = append(out, c.finishChunk(finish)...)
	if c.usage != nil {
		out = append(out, c.usageChunk()...)
	}
	out = append(out, []byte("data: [DONE]\n\n")...)
	return out
}

// outputIndex 取事件里的输出项序号（缺失时按 0 处理，与上游默认行为一致）。
func (c *codexStreamConverter) outputIndex(event *codexStreamEvent) int {
	if event.OutputIndex != nil {
		return *event.OutputIndex
	}
	return 0
}

// toolIndex 为某个输出项分配下游的 tool_calls 下标（幂等）。
func (c *codexStreamConverter) toolIndex(event *codexStreamEvent) int {
	outputIndex := c.outputIndex(event)
	if index, ok := c.toolIndexByOutput[outputIndex]; ok {
		return index
	}
	index := c.nextTool
	c.nextTool++
	c.toolIndexByOutput[outputIndex] = index

	if event.Item != nil && event.Item.CallID != "" {
		c.tools = append(c.tools, &codexAggregatedToolCall{ID: event.Item.CallID, Name: event.Item.Name})
	}
	return index
}

// appendArguments 记录并累积某个输出项已下发的参数。
func (c *codexStreamConverter) appendArguments(outputIndex int, fragment string) {
	if fragment == "" {
		return
	}
	c.argumentsByOutput[outputIndex] += fragment
	// 同步到聚合态：非流式响应里工具参数也要完整，否则客户端拿到的是空参数。
	if index, ok := c.toolIndexByOutput[outputIndex]; ok && index >= 0 && index < len(c.tools) {
		c.tools[index].Arguments.WriteString(fragment)
	}
}

// roleChunk 下发首个"角色"分片（OpenAI 流式约定的第一条）。
func (c *codexStreamConverter) roleChunk() []byte {
	if c.sentRole {
		return nil
	}
	c.sentRole = true
	return c.deltaChunk(map[string]any{"role": "assistant", "content": ""})
}

// deltaChunk 组装一个带增量内容的 chat.completion.chunk。
func (c *codexStreamConverter) deltaChunk(delta map[string]any) []byte {
	chunk := map[string]any{
		"id":      c.chunkID(),
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	return codexSSEWrite(chunk)
}

// finishChunk 组装结束分片（delta 为空对象，finish_reason 为具体原因）。
func (c *codexStreamConverter) finishChunk(reason string) []byte {
	chunk := map[string]any{
		"id":      c.chunkID(),
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	return codexSSEWrite(chunk)
}

// usageChunk 组装用量分片（choices 为空数组，与 OpenAI 的 include_usage 行为一致）。
func (c *codexStreamConverter) usageChunk() []byte {
	chunk := map[string]any{
		"id":      c.chunkID(),
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{},
		"usage":   codexUsagePayload(c.usage),
	}
	return codexSSEWrite(chunk)
}

// chunkID 返回下游分片使用的响应 ID（上游没给时兜一个稳定值）。
func (c *codexStreamConverter) chunkID() string {
	if c.id != "" {
		return c.id
	}
	return "chatcmpl-codex"
}

// codexSSEWrite 把对象序列化成一行 SSE。
func codexSSEWrite(payload any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(encoded) + "\n\n")
}

// codexFinishReason 由上游状态推导下游的 finish_reason。
//
// 规则与 Anthropic 转换保持一致：长度截断 → length、内容过滤 → content_filter、
// 有工具调用 → tool_calls，其余 → stop。
func codexFinishReason(event *codexStreamEvent, sawToolCall bool) string {
	if event != nil && event.Response != nil {
		switch strings.ToLower(strings.TrimSpace(event.Response.Status)) {
		case "incomplete":
			if event.Response.IncompleteDetails != nil {
				switch strings.ToLower(strings.TrimSpace(event.Response.IncompleteDetails.Reason)) {
				case "max_output_tokens", "max_tokens":
					return "length"
				case "content_filter":
					return "content_filter"
				}
			}
			return "length"
		case "failed":
			// 上游明确失败：下游用 stop 收尾，错误信息已在流内提示过，
			// 这里再抛一个错误码只会让客户端把"部分成功"当成失败。
			return "stop"
		}
	}
	if sawToolCall {
		return "tool_calls"
	}
	return "stop"
}

// normalizeCodexUsage 把 Responses 的 usage 归一成 OpenAI 口径。
//
// 两种口径的字段名不同（input_tokens ↔ prompt_tokens），且明细放在不同层级；
// 归一到 openAIUsage 之后，既有的用量抓取器（sniffer）与计费链路无需任何改动。
func normalizeCodexUsage(usage *struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}) *openAIUsage {
	if usage == nil {
		return nil
	}

	prompt := usage.InputTokens
	if prompt == 0 {
		prompt = usage.PromptTokens
	}
	completion := usage.OutputTokens
	if completion == 0 {
		completion = usage.CompletionTokens
	}
	total := usage.TotalTokens
	if total == 0 {
		total = prompt + completion
	}

	result := &openAIUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
	if usage.InputTokensDetails != nil {
		result.CachedTokens = usage.InputTokensDetails.CachedTokens
	}
	if usage.OutputTokensDetails != nil {
		result.ReasoningTokens = usage.OutputTokensDetails.ReasoningTokens
	}
	return result
}

// codexUsagePayload 把归一后的用量转成下游 usage 对象。
//
// 明细放在 prompt_tokens_details / completion_tokens_details 里——
// 与 OpenAI 现行规范一致，也是我们自己的用量抓取器认识的形态。
func codexUsagePayload(usage *openAIUsage) map[string]any {
	if usage == nil {
		return nil
	}
	payload := map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
	}
	if usage.CachedTokens > 0 {
		payload["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
	}
	if usage.ReasoningTokens > 0 {
		payload["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.ReasoningTokens}
	}
	return payload
}

// aggregateCodexStream 把上游的整段 SSE 聚合成一个 chat.completions 响应。
//
// 上游只支持流式，而下游可能要非流式：此时必须把整段事件流读完再组装。
// 复用同一个转换器，因此"流式与非流式的差异"只体现在输出形态上。
func aggregateCodexStream(raw []byte) []byte {
	converter := newCodexStreamConverter()
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 0, 64*1024), StreamLimitsNow().CodexLineBytes)

	for scanner.Scan() {
		payload, ok := codexSSEPayload(scanner.Bytes())
		if !ok {
			continue
		}
		_ = converter.consume(payload)
	}

	finish := converter.finish
	if finish == "" {
		finish = codexFinishReason(nil, len(converter.tools) > 0)
	}

	message := map[string]any{
		"role":    "assistant",
		"content": converter.content.String(),
	}
	if reasoning := converter.reasoning.String(); reasoning != "" {
		// 推理内容单独放 reasoning_content：与主流 OpenAI 兼容实现的约定一致，
		// 混进 content 会让客户端把思考过程当正文显示。
		message["reasoning_content"] = reasoning
	}
	if len(converter.tools) > 0 {
		toolCalls := make([]any, 0, len(converter.tools))
		for index, tool := range converter.tools {
			arguments := tool.Arguments.String()
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"index": index,
				"id":    tool.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      tool.Name,
					"arguments": arguments,
				},
			})
		}
		message["tool_calls"] = toolCalls
	}

	response := map[string]any{
		"id":      converter.chunkID(),
		"object":  "chat.completion",
		"created": converter.created,
		"model":   converter.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage := codexUsagePayload(converter.usage); usage != nil {
		response["usage"] = usage
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		return []byte(`{"error":{"message":"上游响应解析失败","type":"server_error"}}`)
	}
	return encoded
}

// codexStreamReader 包装 io.Pipe，确保 Close 时同时结束后台 goroutine 与上游连接。
//
// 为什么需要它：客户端断开时转发主链路会关闭响应体，若只关管道，
// 后台 goroutine 会阻塞在上游读取上永不退出（goroutine 泄漏）。
type codexStreamReader struct {
	reader   *io.PipeReader
	writer   *io.PipeWriter
	upstream io.ReadCloser
	done     chan struct{}
}

// Read 从管道读取转换后的数据。
func (r *codexStreamReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

// Close 关闭管道与上游，并通知后台 goroutine 退出（幂等）。
func (r *codexStreamReader) Close() error {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	_ = r.reader.Close()
	_ = r.writer.Close()
	return r.upstream.Close()
}
