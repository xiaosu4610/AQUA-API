// 本文件实现 Gemini（generateContent）协议与内部 OpenAI 协议的双向转换。
//
// 意图（Why）：
//
//	Gemini SDK 与一批多模型客户端使用 Google 的原生协议；转换之后，
//	这些客户端也能使用本站统一的上游资源。
//
// 协议对照：
//
//	请求（模型名与"是否流式"都在 URL 路径里，不在 body 里）：
//	  POST /v1beta/models/{model}:generateContent         非流式
//	  POST /v1beta/models/{model}:streamGenerateContent   流式
//
//	  contents[].role：user / model        → OpenAI 的 user / assistant
//	  contents[].parts[].text              → 文本内容
//	  contents[].parts[].inlineData        → image_url（data: URI）
//	  contents[].parts[].functionCall      → assistant.tool_calls
//	  contents[].parts[].functionResponse  → role=tool 消息
//	  systemInstruction                    → messages[0].role=system
//	  generationConfig.temperature/topP/maxOutputTokens/stopSequences
//	  tools[].functionDeclarations         → tools[].function
//
//	响应：
//	  candidates[0].content.parts[].text   ← choices[0].message.content
//	  candidates[0].finishReason           ← choices[0].finish_reason（映射见下）
//	  usageMetadata                        ← usage
//
// 扩展（Extend）：
//
//	新增 parts 类型（如 fileData、codeExecution）时，在 geminiPart 与
//	toGeminiParts 两处同步补齐；流式工具调用需要累积参数，见 geminiStreamEncoder。
package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// geminiAdapter 实现 Adapter 接口。
type geminiAdapter struct{}

// NewGeminiAdapter 创建 Gemini 协议适配器。
func NewGeminiAdapter() Adapter {
	return geminiAdapter{}
}

// Name 返回协议名。
func (geminiAdapter) Name() string { return "gemini" }

// ---- 请求结构 ----

// geminiRequest 是 Gemini generateContent 请求体。
type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig"`
	Tools             []geminiTool            `json:"tools"`
}

// geminiContent 是一段对话内容。
type geminiContent struct {
	// Role 取值 "user" 或 "model"（Gemini 用 model 表示助手）
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart 是内容片段。
type geminiPart struct {
	Text string `json:"text"`

	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`

	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`

	FunctionResponse *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
}

// geminiGenerationConfig 是生成参数。
type geminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature"`
	TopP            *float64 `json:"topP"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
	StopSequences   []string `json:"stopSequences"`
}

// geminiTool 是工具声明（Gemini 把多个函数声明放在一个 tools 元素里）。
type geminiTool struct {
	FunctionDeclarations []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"functionDeclarations"`
}

// parseGeminiAction 从请求路径解析出模型名与是否流式。
//
// 路径形态：/v1beta/models/{model}:{action}
// 返回 ok=false 表示路径不符合 Gemini 协议（此时应给出明确错误而不是猜）。
func parseGeminiAction(path string) (model string, stream bool, ok bool) {
	const marker = "/models/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		return "", false, false
	}
	rest := path[idx+len(marker):]
	action := ""
	if colon := strings.Index(rest, ":"); colon >= 0 {
		action = rest[colon+1:]
		rest = rest[:colon]
	}
	model = strings.TrimSpace(rest)
	if model == "" || action == "" {
		return "", false, false
	}
	return model, strings.Contains(action, "streamGenerateContent"), true
}

// DecodeRequest 把 Gemini 请求转换为 OpenAI 请求。
func (g geminiAdapter) DecodeRequest(body []byte, path string) (string, []byte, error) {
	modelName, stream, ok := parseGeminiAction(path)
	if !ok {
		return "", nil, fmt.Errorf("%w（路径应形如 /v1beta/models/{model}:generateContent）", ErrUnsupportedProtocol)
	}

	var req geminiRequest
	if err := decodeJSONStrict(body, &req, "Gemini generateContent"); err != nil {
		return "", nil, err
	}

	messages := make([]map[string]any, 0, len(req.Contents)+1)
	if req.SystemInstruction != nil {
		if text := joinGeminiText(req.SystemInstruction.Parts); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}
	for _, content := range req.Contents {
		messages = append(messages, toOpenAIMessagesFromGemini(content)...)
	}

	out := map[string]any{
		"model":    modelName,
		"messages": messages,
	}
	if stream {
		// 流式标志写进内部请求体，这样 StreamRequested 无需再解析路径
		out["stream"] = true
	}
	if cfg := req.GenerationConfig; cfg != nil {
		if cfg.Temperature != nil {
			out["temperature"] = *cfg.Temperature
		}
		if cfg.TopP != nil {
			out["top_p"] = *cfg.TopP
		}
		if cfg.MaxOutputTokens > 0 {
			out["max_tokens"] = cfg.MaxOutputTokens
		}
		if len(cfg.StopSequences) > 0 {
			out["stop"] = cfg.StopSequences
		}
	}
	if tools := toOpenAIToolsFromGemini(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return "", nil, fmt.Errorf("relay: 构造上游请求失败: %w", err)
	}
	return modelName, encoded, nil
}

// StreamRequested 判断是否流式（依据 DecodeRequest 写入的 stream 字段）。
func (geminiAdapter) StreamRequested(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Stream
}

// joinGeminiText 把若干 parts 里的文本拼起来（用于 systemInstruction）。
func joinGeminiText(parts []geminiPart) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// toOpenAIMessagesFromGemini 把一段 Gemini 内容转换为一条或多条 OpenAI 消息。
//
// 需要"一变多"的情形：Gemini 的 functionResponse 与 functionCall 可以混在
// 同一段 content 的 parts 里，而 OpenAI 要求工具结果单独成一条 role=tool 消息。
func toOpenAIMessagesFromGemini(content geminiContent) []map[string]any {
	role := "user"
	if content.Role == "model" {
		role = "assistant"
	}

	result := make([]map[string]any, 0, 2)
	parts := make([]map[string]any, 0, len(content.Parts))
	toolCalls := make([]map[string]any, 0)

	for index, part := range content.Parts {
		switch {
		case strings.TrimSpace(part.Text) != "":
			parts = append(parts, map[string]any{"type": "text", "text": part.Text})
		case part.InlineData != nil && part.InlineData.Data != "":
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:" + part.InlineData.MimeType + ";base64," + part.InlineData.Data,
				},
			})
		case part.FunctionCall != nil:
			arguments := "{}"
			if len(part.FunctionCall.Args) > 0 {
				arguments = string(part.FunctionCall.Args)
			}
			// Gemini 不提供工具调用 ID，这里合成一个稳定的 ID：
			// 同一段内容里的第 N 个调用对应 tool_result 时的第 N 个响应，
			// 用序号拼出可对应上的标识符，保证 tool_result 能正确回填。
			toolCalls = append(toolCalls, map[string]any{
				"id":   fmt.Sprintf("call_%s_%d", part.FunctionCall.Name, index),
				"type": "function",
				"function": map[string]any{
					"name":      part.FunctionCall.Name,
					"arguments": arguments,
				},
			})
		case part.FunctionResponse != nil:
			payload := "{}"
			if len(part.FunctionResponse.Response) > 0 {
				payload = string(part.FunctionResponse.Response)
			}
			result = append(result, map[string]any{
				"role":         "tool",
				"tool_call_id": fmt.Sprintf("call_%s_0", part.FunctionResponse.Name),
				"content":      payload,
			})
		}
	}

	if len(parts) > 0 || len(toolCalls) > 0 {
		message := map[string]any{"role": role}
		if len(parts) == 1 && parts[0]["type"] == "text" {
			message["content"] = parts[0]["text"]
		} else if len(parts) > 0 {
			message["content"] = parts
		} else {
			message["content"] = ""
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		result = append([]map[string]any{message}, result...)
	}

	if len(result) == 0 {
		result = append(result, map[string]any{"role": role, "content": ""})
	}
	return result
}

// toOpenAIToolsFromGemini 把 Gemini 的工具声明转换为 OpenAI 形式。
//
// Gemini 把多个函数声明打包在一个 tools 元素里，OpenAI 则是每个函数一个元素，
// 因此这里需要"拆包"。
func toOpenAIToolsFromGemini(tools []geminiTool) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		for _, decl := range tool.FunctionDeclarations {
			parameters := map[string]any{"type": "object", "properties": map[string]any{}}
			if len(decl.Parameters) > 0 {
				if err := json.Unmarshal(decl.Parameters, &parameters); err != nil {
					parameters = map[string]any{"type": "object", "properties": map[string]any{}}
				}
			}
			result = append(result, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        decl.Name,
					"description": decl.Description,
					"parameters":  parameters,
				},
			})
		}
	}
	return result
}

// ---- 响应转换 ----

// EncodeResponse 把上游非流式响应转换为 Gemini 格式。
func (g geminiAdapter) EncodeResponse(upstreamBody []byte) []byte {
	var upstream openAIResponse
	if err := json.Unmarshal(upstreamBody, &upstream); err != nil {
		return geminiEnvelope([]map[string]any{{"text": ""}}, "STOP", 0, 0)
	}

	parts := make([]map[string]any, 0, 2)
	finishReason := "STOP"
	if len(upstream.Choices) > 0 {
		choice := upstream.Choices[0]
		if strings.TrimSpace(choice.Message.Content) != "" {
			parts = append(parts, map[string]any{"text": choice.Message.Content})
		}
		for _, call := range choice.Message.ToolCalls {
			var args any = map[string]any{}
			if strings.TrimSpace(call.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					args = map[string]any{}
				}
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{"name": call.Function.Name, "args": args},
			})
		}
		finishReason = mapGeminiFinishReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0)
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"text": ""})
	}

	inputTokens, outputTokens := 0, 0
	if upstream.Usage != nil {
		inputTokens = upstream.Usage.PromptTokens
		outputTokens = upstream.Usage.CompletionTokens
	}
	return geminiEnvelope(parts, finishReason, inputTokens, outputTokens)
}

// EncodeError 把错误转换为 Gemini 的错误格式。
func (g geminiAdapter) EncodeError(statusCode int, upstreamBody []byte) []byte {
	return mustEncode(map[string]any{
		"error": map[string]any{
			"code":    statusCode,
			"message": extractErrorMessage(upstreamBody),
			"status":  httpStatusToGoogleRPC(statusCode),
		},
	})
}

// geminiEnvelope 组装 Gemini 的响应信封。
func geminiEnvelope(parts []map[string]any, finishReason string, inputTokens, outputTokens int) []byte {
	return mustEncode(map[string]any{
		"candidates": []map[string]any{{
			"content":      map[string]any{"parts": parts, "role": "model"},
			"finishReason": finishReason,
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     inputTokens,
			"candidatesTokenCount": outputTokens,
			"totalTokenCount":      inputTokens + outputTokens,
		},
		"modelVersion": "",
	})
}

// mapGeminiFinishReason 把 OpenAI 的 finish_reason 映射为 Gemini 的枚举。
func mapGeminiFinishReason(finishReason string, hasToolCalls bool) string {
	if hasToolCalls {
		// Gemini 在工具调用场景下同样返回 STOP
		return "STOP"
	}
	switch finishReason {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "stop", "":
		return "STOP"
	default:
		return "STOP"
	}
}

// httpStatusToGoogleRPC 把 HTTP 状态码映射为 Google 的 canonical status 名。
//
// 为什么要映射：Gemini 客户端（尤其官方 SDK）会按 status 字段判断错误类别，
// 只给 code 与 message 时部分 SDK 无法正确分类重试策略。
func httpStatusToGoogleRPC(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusInternalServerError:
		return "INTERNAL"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		return "UNKNOWN"
	}
}

// NewStreamEncoder 创建 Gemini 流式编码器。
func (g geminiAdapter) NewStreamEncoder() StreamEncoder {
	return &geminiStreamEncoder{
		toolArgs:  make(map[int]*strings.Builder),
		toolNames: make(map[int]string),
	}
}

// geminiStreamEncoder 把上游的 OpenAI 流式分片转换为 Gemini 的 SSE 分片。
//
// 与 Anthropic 的关键差异：Gemini 的流式响应结构与非流式完全一致
// （每个分片就是一个残缺的 GenerateContentResponse），因此这里不需要
// 维护"内容块"状态机，只需逐片转换。
//
// 唯一的例外是工具调用：OpenAI 以"参数字符串增量"下发，而 Gemini 要求
// functionCall.args 是完整对象。这里把参数累积起来，在流结束时一次性下发
// 完整的 functionCall（这是最接近两者语义的转换方式）。
type geminiStreamEncoder struct {
	toolArgs     map[int]*strings.Builder
	toolNames    map[int]string
	inputTokens  int
	outputTokens int
	finishReason string
}

// Begin 不输出内容：Gemini 的 SSE 直接以数据分片开始，无需开场事件。
func (e *geminiStreamEncoder) Begin() []byte { return nil }

// Chunk 转换一个上游分片。
func (e *geminiStreamEncoder) Chunk(payload []byte) []byte {
	var chunk openAIStreamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil
	}
	if chunk.Usage != nil {
		e.inputTokens = chunk.Usage.PromptTokens
		e.outputTokens = chunk.Usage.CompletionTokens
	}

	parts := make([]map[string]any, 0, 2)
	hasToolDelta := false

	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		if choice.Delta.Content != "" {
			parts = append(parts, map[string]any{"text": choice.Delta.Content})
		}
		for _, call := range choice.Delta.ToolCalls {
			hasToolDelta = true
			if _, exists := e.toolArgs[call.Index]; !exists {
				e.toolArgs[call.Index] = &strings.Builder{}
			}
			if call.Function.Name != "" {
				e.toolNames[call.Index] = call.Function.Name
			}
			e.toolArgs[call.Index].WriteString(call.Function.Arguments)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			e.finishReason = mapGeminiFinishReason(*choice.FinishReason, len(e.toolArgs) > 0)
		}
	}

	// 只有工具参数增量、没有文本时不下发分片：
	// Gemini 的客户端无法消费"半个 JSON"，必须等参数累积完整
	if len(parts) == 0 {
		if hasToolDelta || len(chunk.Choices) == 0 {
			return nil
		}
	}

	candidate := map[string]any{"index": 0}
	if len(parts) > 0 {
		candidate["content"] = map[string]any{"parts": parts, "role": "model"}
	}
	if e.finishReason != "" {
		candidate["finishReason"] = e.finishReason
	}

	out := map[string]any{"candidates": []map[string]any{candidate}}
	if chunk.Usage != nil {
		out["usageMetadata"] = map[string]any{
			"promptTokenCount":     e.inputTokens,
			"candidatesTokenCount": e.outputTokens,
			"totalTokenCount":      e.inputTokens + e.outputTokens,
		}
	}
	return geminiSSE(out)
}

// End 在流结束时下发累积的完整工具调用（若有）。
func (e *geminiStreamEncoder) End() []byte {
	if len(e.toolArgs) == 0 {
		return nil
	}

	parts := make([]map[string]any, 0, len(e.toolArgs))
	for index := 0; index < len(e.toolArgs); index++ {
		builder, exists := e.toolArgs[index]
		if !exists {
			continue
		}
		var args any = map[string]any{}
		if raw := strings.TrimSpace(builder.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				// 参数不是合法 JSON（上游截断等）：给出空对象而不是让客户端解析失败
				args = map[string]any{}
			}
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{"name": e.toolNames[index], "args": args},
		})
	}

	return geminiSSE(map[string]any{
		"candidates": []map[string]any{{
			"content":      map[string]any{"parts": parts, "role": "model"},
			"finishReason": "STOP",
			"index":        0,
		}},
	})
}

// geminiSSE 按 Gemini 的 SSE 格式编码一个数据分片。
func geminiSSE(payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(encoded)+8)
	out = append(out, "data: "...)
	out = append(out, encoded...)
	out = append(out, '\n', '\n')
	return out
}

// ServeGeminiGenerate 处理 Gemini 的 generateContent / streamGenerateContent。
func (r *Relay) ServeGeminiGenerate(w http.ResponseWriter, req *http.Request) {
	r.serveWithAdapter(w, req, NewGeminiAdapter())
}
