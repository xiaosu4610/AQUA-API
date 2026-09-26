// 本文件实现 Anthropic Messages 上游（出站方向）与内部 OpenAI 协议之间的转换。
//
// 意图（Why）：
//
//	网关内部统一以 OpenAI 协议作为「中间表示」，而 Anthropic 是原生另一套协议。
//	当某渠道的类型 Protocol=anthropic 时，必须在【发往上游前】把内部 OpenAI 请求
//	改写成 Anthropic 请求，并在【收到上游响应后】把 Anthropic 响应改写回 OpenAI，
//	上层（计费 / 用量 / 下游协议适配）才能像对待普通 OpenAI 上游一样工作。
//
//	与 adapter.go 的区别：adapter.go 处理的是【下游协议 → OpenAI】（入站），
//	本文件处理的是【OpenAI → 上游协议】（出站），两者方向相反、互不干扰。
//
// 协议对照（只列需要转换的部分）：
//
//	请求（OpenAI → Anthropic）：
//	  messages[].role=system        → system（单独拎出为顶层字段）
//	  messages[].content（字符串/多段） → content（字符串或内容块数组）
//	  messages[].tool_calls         → content[{type:tool_use}]
//	  messages[].role=tool          → role=user 消息里的 [{type:tool_result}]
//	  max_tokens                    → max_tokens（Anthropic 必填，缺失时补默认值）
//	  temperature/top_p             → 同名
//	  stop                          → stop_sequences
//	  tools[].function              → tools[{name,description,input_schema}]
//	  stream                        → stream
//
//	响应（Anthropic → OpenAI）：
//	  content[].text                → choices[0].message.content
//	  content[].tool_use            → choices[0].message.tool_calls
//	  stop_reason                  → finish_reason（见 anthropicStopReasonToOpenAI）
//	  usage.input_tokens/output_tokens → usage.prompt_tokens/completion_tokens
//
//	流式：Anthropic 的 SSE 事件（message_start / content_block_start /
//	content_block_delta / message_delta / message_stop）转换为 OpenAI 的
//	chat.completion.chunk 分片，并以 data: [DONE] 收尾。
//
// 扩展（Extend）：
//
//	Anthropic 新增内容块类型时，在 openAIContentToAnthropicBlocks 与
//	anthropicUpstreamResponse 两处同步补齐；映射不了的类型必须【明确报错】，
//	不允许静默丢弃（静默丢工具会让用户以为"模型不会用工具"）。
package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// anthropicDefaultMaxTokens 是 OpenAI 请求未提供 max_tokens 时的保守默认值。
//
// 为什么必须给默认值：Anthropic 的 Messages 接口要求 max_tokens 必填，
// 缺失会被直接拒绝（invalid_request_error）。取 4096 的权衡：足以覆盖
// 绝大多数对话/代码补全，又不至于因过大而触发上游的配额或长度校验拒绝。
const anthropicDefaultMaxTokens = 4096

// maxAnthropicStreamLineBytes 是解析 Anthropic SSE 单行的字节上限。
//
// 取 1MiB：单个事件（含工具参数的 partial_json 增量）可能较长，
// 但绝不会达到这个量级；设上限用于防御异常上游撑爆内存。
const maxAnthropicStreamLineBytes = 1 << 20

// isAnthropicUpstream 判断该渠道类型是否需要走 Anthropic 出站转换。
func isAnthropicUpstream(spec channeltype.Type) bool {
	return spec.Protocol == channeltype.ProtocolAnthropic
}

// ---- 请求转换（OpenAI → Anthropic）----

// openAIUpstreamRequest 是内部 OpenAI 请求体中需要转换的字段。
//
// 用 RawMessage 接住 stop/tools/tool_choice：它们的取值形态多样（字符串或数组、
// 不同子结构），先原样接住再按形态分发，避免为每种形态定义一套类型。
type openAIUpstreamRequest struct {
	Model       string                  `json:"model"`
	Messages    []openAIUpstreamMessage `json:"messages"`
	Stream      bool                    `json:"stream,omitempty"`
	MaxTokens   int                     `json:"max_tokens,omitempty"`
	Temperature *float64                `json:"temperature,omitempty"`
	TopP        *float64                `json:"top_p,omitempty"`
	Stop        json.RawMessage         `json:"stop,omitempty"`
	Tools       json.RawMessage         `json:"tools,omitempty"`
	ToolChoice  json.RawMessage         `json:"tool_choice,omitempty"`
}

// openAIUpstreamMessage 是内部 OpenAI 的一条消息。
//
// Content 用 RawMessage：既可能是字符串，也可能是内容块数组。
// ToolCalls / ToolCallID 分别承载 assistant 的工具调用与 role=tool 的工具结果。
type openAIUpstreamMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// encodeUpstreamRequestBody 按渠道类型把内部 OpenAI 请求体转换为上游协议请求体。
//
// 非 Anthropic 类型原样返回（保证既有上游行为不变）。
func encodeUpstreamRequestBody(spec channeltype.Type, body []byte) ([]byte, error) {
	if !isAnthropicUpstream(spec) {
		return body, nil
	}
	return encodeAnthropicRequest(body)
}

// encodeAnthropicRequest 把内部 OpenAI 请求体转换为 Anthropic Messages 请求体。
func encodeAnthropicRequest(body []byte) ([]byte, error) {
	var in openAIUpstreamRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("relay: 解析待转换的 OpenAI 请求失败: %w", err)
	}

	system, messages, err := splitAnthropicMessages(in.Messages)
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"model":    in.Model,
		"messages": messages,
	}
	if system != "" {
		out["system"] = system
	}

	// max_tokens 必填：未给时补保守默认值（见 anthropicDefaultMaxTokens 的说明）。
	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	out["max_tokens"] = maxTokens

	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if stops := decodeOpenAIStop(in.Stop); len(stops) > 0 {
		out["stop_sequences"] = stops
	}
	if in.Stream {
		out["stream"] = true
	}
	if len(in.Tools) > 0 {
		tools, err := toAnthropicTools(in.Tools)
		if err != nil {
			return nil, err
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if len(in.ToolChoice) > 0 {
		choice, err := toAnthropicToolChoice(in.ToolChoice)
		if err != nil {
			return nil, err
		}
		if choice != nil {
			out["tool_choice"] = choice
		}
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("relay: 构造 Anthropic 请求失败: %w", err)
	}
	return encoded, nil
}

// splitAnthropicMessages 把 OpenAI 消息序列拆成 Anthropic 的 system 与 messages。
//
// 两大差异：
//  1. OpenAI 把 system 当一条普通消息，Anthropic 要求它是顶层字段；
//  2. OpenAI 的工具结果是独立的 role=tool 消息，Anthropic 要求它是
//     紧跟 assistant 工具调用之后的 user 消息里的 tool_result 内容块。
//     这里把【连续】的工具结果合并进同一条 user 消息（Anthropic 允许一条
//     user 消息承载多个 tool_result）。
func splitAnthropicMessages(messages []openAIUpstreamMessage) (string, []map[string]any, error) {
	systemParts := make([]string, 0)
	result := make([]map[string]any, 0, len(messages))
	pendingToolResults := make([]map[string]any, 0)

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		// 复制一份再放入，避免后续复用切片导致内容被覆盖
		blocks := make([]map[string]any, len(pendingToolResults))
		copy(blocks, pendingToolResults)
		result = append(result, map[string]any{"role": "user", "content": blocks})
		pendingToolResults = pendingToolResults[:0]
	}

	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			text, err := flattenOpenAIContent(msg.Content)
			if err != nil {
				return "", nil, err
			}
			if strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}
		case "tool", "function":
			pendingToolResults = append(pendingToolResults, openAIToolResultBlock(msg))
		default:
			flushToolResults()
			converted, err := toAnthropicMessage(msg)
			if err != nil {
				return "", nil, err
			}
			result = append(result, converted)
		}
	}
	flushToolResults()

	if len(result) == 0 {
		result = append(result, map[string]any{"role": "user", "content": ""})
	}
	return strings.Join(systemParts, "\n\n"), result, nil
}

// toAnthropicMessage 把一条普通（非 system / 非 tool）消息转换为 Anthropic 消息。
func toAnthropicMessage(msg openAIUpstreamMessage) (map[string]any, error) {
	role := "user"
	if msg.Role == "assistant" {
		role = "assistant"
	}

	content, err := openAIContentToAnthropicBlocks(msg.Content)
	if err != nil {
		return nil, err
	}
	if len(msg.ToolCalls) > 0 {
		toolUses, err := openAIToolCallsToAnthropicBlocks(msg.ToolCalls)
		if err != nil {
			return nil, err
		}
		content = append(content, toolUses...)
	}

	// 单块纯文本时退化为字符串，兼容性最好；否则用内容块数组。
	if len(content) == 1 && content[0]["type"] == "text" {
		return map[string]any{"role": role, "content": content[0]["text"]}, nil
	}
	if len(content) == 0 {
		return map[string]any{"role": role, "content": ""}, nil
	}
	return map[string]any{"role": role, "content": content}, nil
}

// openAIContentToAnthropicBlocks 把 OpenAI 的 content（字符串或内容块数组）
// 转换为 Anthropic 的内容块数组。
//
// 映射不了的块类型【明确报错】：静默丢弃会让用户拿到"莫名其妙少了张图/一段话"
// 的结果，比直接失败更难排查。
func openAIContentToAnthropicBlocks(raw json.RawMessage) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}

	// 形态 1：纯字符串
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []map[string]any{{"type": "text", "text": text}}, nil
	}

	// 形态 2：内容块数组
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, fmt.Errorf("relay: OpenAI 消息内容结构无法识别，无法转换为 Anthropic: %w", err)
	}

	result := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				result = append(result, map[string]any{"type": "text", "text": block.Text})
			}
		case "image_url":
			if block.ImageURL == nil || strings.TrimSpace(block.ImageURL.URL) == "" {
				continue
			}
			result = append(result, anthropicImageBlock(block.ImageURL.URL))
		default:
			return nil, fmt.Errorf("relay: OpenAI 内容块类型 %q 暂不支持转换为 Anthropic", block.Type)
		}
	}
	return result, nil
}

// anthropicImageBlock 把 OpenAI 的 image_url 转换为 Anthropic 的 image 块。
//
// 两种来源：data: URI（内联 base64）与 http(s) URL（外链）。
func anthropicImageBlock(rawURL string) map[string]any {
	if mediaType, data, ok := parseImageDataURI(rawURL); ok {
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": rawURL},
	}
}

// parseImageDataURI 解析 "data:<media-type>;base64,<data>" 形式的内联图片。
func parseImageDataURI(rawURL string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(rawURL, prefix) {
		return "", "", false
	}
	rest := rawURL[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	header := rest[:comma]
	payload := rest[comma+1:]
	if !strings.Contains(header, "base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(header, ";base64")
	mediaType = strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0])
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return mediaType, payload, true
}

// openAIToolCallsToAnthropicBlocks 把 OpenAI 的 assistant.tool_calls 转换为
// Anthropic 的 tool_use 内容块。
func openAIToolCallsToAnthropicBlocks(raw json.RawMessage) ([]map[string]any, error) {
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, fmt.Errorf("relay: 无法解析 OpenAI tool_calls，无法转换为 Anthropic: %w", err)
	}

	result := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		// 只支持 function 类型：其它类型（如自定义工具）在 Anthropic 无对应表示。
		if call.Type != "" && call.Type != "function" {
			return nil, fmt.Errorf("relay: OpenAI 工具调用类型 %q 暂不支持转换为 Anthropic（无法静默丢弃）", call.Type)
		}
		if strings.TrimSpace(call.Function.Name) == "" {
			return nil, fmt.Errorf("relay: OpenAI 工具调用缺少函数名，无法转换为 Anthropic")
		}
		var input any = map[string]any{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
				return nil, fmt.Errorf("relay: OpenAI 工具调用参数不是合法 JSON，无法转换为 Anthropic: %w", err)
			}
		}
		result = append(result, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Function.Name,
			"input": input,
		})
	}
	return result, nil
}

// openAIToolResultBlock 把一条 role=tool 消息转换为 Anthropic 的 tool_result 块。
func openAIToolResultBlock(msg openAIUpstreamMessage) map[string]any {
	content, err := flattenOpenAIContent(msg.Content)
	if err != nil || content == "" {
		content = "{}"
	}
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": msg.ToolCallID,
		"content":     content,
	}
}

// toAnthropicTools 把 OpenAI 的 tools 声明转换为 Anthropic 的 tools 声明。
func toAnthropicTools(raw json.RawMessage) ([]map[string]any, error) {
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("relay: 无法解析 OpenAI tools，无法转换为 Anthropic: %w", err)
	}

	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		// 只支持 function 类型：OpenAI 的其它内置工具（如 code_interpreter）
		// 在 Anthropic 无对应表示，必须报错而不是静默丢弃。
		if tool.Type != "" && tool.Type != "function" {
			return nil, fmt.Errorf("relay: OpenAI 工具类型 %q 暂不支持转换为 Anthropic（无法静默丢弃）", tool.Type)
		}
		if strings.TrimSpace(tool.Function.Name) == "" {
			return nil, fmt.Errorf("relay: OpenAI 工具声明缺少函数名，无法转换为 Anthropic")
		}
		schema := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Function.Parameters) > 0 {
			if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
				return nil, fmt.Errorf("relay: OpenAI 工具参数 schema 不是合法 JSON，无法转换为 Anthropic: %w", err)
			}
		}
		result = append(result, map[string]any{
			"name":         tool.Function.Name,
			"description":  tool.Function.Description,
			"input_schema": schema,
		})
	}
	return result, nil
}

// toAnthropicToolChoice 把 OpenAI 的 tool_choice 映射为 Anthropic 形式。
//
// 映射关系：auto → auto；required → any；{function:{name}} → {type:tool,name}。
// Anthropic 没有 "none"，遇到时明确报错而不是静默丢弃（丢弃会改变工具语义）。
func toAnthropicToolChoice(raw json.RawMessage) (any, error) {
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		switch simple {
		case "auto":
			return "auto", nil
		case "required":
			return "any", nil
		case "none":
			return nil, fmt.Errorf("relay: Anthropic 不支持 tool_choice=none，无法静默丢弃该语义")
		default:
			return nil, fmt.Errorf("relay: 无法识别的 OpenAI tool_choice %q", simple)
		}
	}

	var shaped struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &shaped); err != nil {
		return nil, fmt.Errorf("relay: 无法解析 OpenAI tool_choice，无法转换为 Anthropic: %w", err)
	}
	if shaped.Type == "function" && strings.TrimSpace(shaped.Function.Name) != "" {
		return map[string]any{"type": "tool", "name": shaped.Function.Name}, nil
	}
	return nil, fmt.Errorf("relay: OpenAI tool_choice 结构无法转换为 Anthropic")
}

// decodeOpenAIStop 解析 OpenAI 的 stop（字符串或字符串数组）为切片。
func decodeOpenAIStop(raw json.RawMessage) []string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var single string
	if err := json.Unmarshal(trimmed, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return nil
		}
		return []string{single}
	}
	var list []string
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return nil
	}
	return list
}

// flattenOpenAIContent 把 content（字符串或内容块数组）拍平为纯文本。
func flattenOpenAIContent(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return "", fmt.Errorf("relay: OpenAI 消息内容结构无法识别，无法转换为 Anthropic: %w", err)
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// ---- 响应转换（Anthropic → OpenAI）----

// anthropicUpstreamResponse 是 Anthropic 的非流式响应体（只取需要的字段）。
type anthropicUpstreamResponse struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// decodeAnthropicUpstreamResponse 把 Anthropic 非流式响应转换为 OpenAI 响应。
func decodeAnthropicUpstreamResponse(body []byte) []byte {
	var upstream anthropicUpstreamResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		// 结构异常：返回一个合法的空响应，避免下游客户端解析失败
		return marshalOpenAIPayload(openAICompletionPayload(
			"", "", "", nil, "stop", 0, 0))
	}

	var text strings.Builder
	toolCalls := make([]map[string]any, 0)
	for _, block := range upstream.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			arguments := "{}"
			if len(block.Input) > 0 {
				arguments = string(block.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": arguments,
				},
			})
		}
	}

	inputTokens, outputTokens := 0, 0
	if upstream.Usage != nil {
		inputTokens = upstream.Usage.InputTokens
		outputTokens = upstream.Usage.OutputTokens
	}

	return marshalOpenAIPayload(openAICompletionPayload(
		normalizeOpenAIID(upstream.ID),
		upstream.Model,
		text.String(),
		toolCalls,
		anthropicStopReasonToOpenAI(upstream.StopReason, len(toolCalls) > 0),
		inputTokens,
		outputTokens,
	))
}

// openAICompletionPayload 组装一个 OpenAI chat.completion 响应体。
func openAICompletionPayload(id, model, content string, toolCalls []map[string]any,
	finishReason string, promptTokens, completionTokens int) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return map[string]any{
		"id":      normalizeOpenAIID(id),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
}

// decodeAnthropicUpstreamError 把 Anthropic 错误体转换为 OpenAI 错误体。
//
// 这样下游（无论 OpenAI 还是 Anthropic 客户端，后者再由入站适配器改写）
// 都能按各自熟悉的错误结构解析，而不是拿到一份陌生的 Anthropic 原文。
func decodeAnthropicUpstreamError(body []byte, status int) []byte {
	message := "上游请求失败"
	errType := oai.TypeServer

	var envelope struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil {
		if strings.TrimSpace(envelope.Error.Message) != "" {
			message = envelope.Error.Message
		}
		errType = anthropicErrorTypeToOpenAI(envelope.Error.Type, status)
	} else {
		errType = anthropicStatusToOpenAIErrorType(status)
	}

	return marshalOpenAIPayload(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    oai.CodeUpstreamRequestFailed,
		},
	})
}

// anthropicErrorTypeToOpenAI 把 Anthropic 的错误类型映射为 OpenAI 的错误类型。
func anthropicErrorTypeToOpenAI(anthropicType string, status int) string {
	switch anthropicType {
	case "invalid_request_error":
		return oai.TypeInvalidRequest
	case "authentication_error":
		return oai.TypeAuthentication
	case "permission_error":
		return oai.TypePermission
	case "not_found_error":
		return oai.TypeInvalidRequest
	case "rate_limit_error":
		return oai.TypeRateLimit
	case "overloaded_error", "api_error":
		return oai.TypeServer
	default:
		return anthropicStatusToOpenAIErrorType(status)
	}
}

// anthropicStatusToOpenAIErrorType 按状态码兜底映射错误类型。
func anthropicStatusToOpenAIErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge:
		return oai.TypeInvalidRequest
	case http.StatusUnauthorized:
		return oai.TypeAuthentication
	case http.StatusForbidden:
		return oai.TypePermission
	case http.StatusTooManyRequests:
		return oai.TypeRateLimit
	default:
		return oai.TypeServer
	}
}

// anthropicStopReasonToOpenAI 把 Anthropic 的 stop_reason 映射为 OpenAI 的 finish_reason。
func anthropicStopReasonToOpenAI(stopReason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence", "end_turn":
		return "stop"
	default:
		if stopReason == "" {
			return "stop"
		}
		return stopReason
	}
}

// normalizeOpenAIID 保证响应 id 形如 chatcmpl-xxx。
//
// 上游 Anthropic 的 id 形如 msg_xxx；直接透传虽不致命，但部分 OpenAI 客户端
// 会据此判断消息来源，统一前缀可减少非预期差异。
func normalizeOpenAIID(upstreamID string) string {
	id := strings.TrimSpace(upstreamID)
	if id == "" {
		return "chatcmpl-aqua"
	}
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	return "chatcmpl-" + id
}

// marshalOpenAIPayload 序列化一个 OpenAI 响应体；失败时给出兜底错误体。
func marshalOpenAIPayload(payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":{"message":"响应序列化失败","type":"server_error","code":"internal_error"}}`)
	}
	return encoded
}

// ---- 响应改写入口（就地修改 http.Response）----

// normalizeUpstreamResponse 就地把上游响应改写为内部 OpenAI 协议。
//
// 非 Anthropic 上游直接返回，不做任何改动——这样既有 OpenAI 渠道的
// 响应回写路径（含响应头、流式行为）与改动前完全一致。
func normalizeUpstreamResponse(spec channeltype.Type, resp *http.Response, wantStream bool) error {
	if !isAnthropicUpstream(spec) {
		return nil
	}
	return adjustAnthropicUpstreamResponse(resp, wantStream)
}

// adjustAnthropicUpstreamResponse 把上游 Anthropic 响应改写为 OpenAI 形态。
//
// 三条分支：
//   - 错误响应（>=400）：改写为 OpenAI 错误体，便于下游按熟悉的错误结构解析；
//   - 流式成功：把 Anthropic SSE 转换为 OpenAI SSE；
//   - 非流式成功：整体读取并改写为 OpenAI 响应体。
func adjustAnthropicUpstreamResponse(resp *http.Response, wantStream bool) error {
	if resp == nil || resp.Body == nil {
		return nil
	}

	if resp.StatusCode >= http.StatusBadRequest {
		raw, err := readAllLimited(resp.Body, maxAdaptedBodyBytes)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		converted := decodeAnthropicUpstreamError(raw, resp.StatusCode)
		replaceResponseBody(resp, converted)
		return nil
	}

	if wantStream {
		resp.Body = wrapAnthropicUpstreamStream(resp.Body)
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
	replaceResponseBody(resp, decodeAnthropicUpstreamResponse(raw))
	return nil
}

// replaceResponseBody 用新的字节内容替换响应体，并同步内容长度与类型。
func replaceResponseBody(resp *http.Response, content []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(content))
	resp.Header.Set("Content-Type", "application/json")
	resp.ContentLength = int64(len(content))
}

// ---- 流式转换（Anthropic SSE → OpenAI SSE）----

// wrapAnthropicUpstreamStream 把上游 Anthropic 的 SSE 字节流转换为 OpenAI 的 SSE 流。
//
// 实现方式：io.Pipe + 后台 goroutine 逐行读取上游 SSE、逐事件转换后写入管道；
// 调用方（转发主链路）读到的是 OpenAI 形态的 SSE，因此流式回写、usage 抓取
// 等既有逻辑无需任何改动。
func wrapAnthropicUpstreamStream(upstream io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	converter := newAnthropicStreamConverter()

	go func() {
		scanner := bufio.NewScanner(upstream)
		scanner.Buffer(make([]byte, 0, 64*1024), maxAnthropicStreamLineBytes)

		eventName := ""
		for scanner.Scan() {
			select {
			case <-done:
				_ = writer.Close()
				return
			default:
			}

			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if name, ok := strings.CutPrefix(line, "event:"); ok {
				eventName = strings.TrimSpace(name)
				continue
			}
			payload, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			out := converter.consume(eventName, []byte(strings.TrimSpace(payload)))
			if len(out) > 0 {
				if _, err := writer.Write(out); err != nil {
					return
				}
			}
		}
		// 上游未发 message_stop 时也要收尾，否则客户端会一直等待
		if out := converter.finish(); len(out) > 0 {
			_, _ = writer.Write(out)
		}
		_ = writer.Close()
	}()

	return &anthropicStreamReader{reader: reader, writer: writer, upstream: upstream, done: done}
}

// anthropicStreamReader 包装 io.Pipe，确保 Close 时同时结束后台 goroutine 与上游连接。
//
// 为什么需要它：客户端断开时转发主链路会关闭响应体，若只关管道，
// 后台 goroutine 会阻塞在上游读取上永不退出（goroutine 泄漏）。
type anthropicStreamReader struct {
	reader   *io.PipeReader
	writer   *io.PipeWriter
	upstream io.ReadCloser
	done     chan struct{}
}

// Read 读取转换后的 OpenAI SSE 字节。
func (r *anthropicStreamReader) Read(p []byte) (int, error) { return r.reader.Read(p) }

// Close 结束转换并关闭上游连接（幂等）。
func (r *anthropicStreamReader) Close() error {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	_ = r.reader.CloseWithError(io.EOF)
	return r.upstream.Close()
}

// anthropicStreamConverter 把 Anthropic 的 SSE 事件转换为 OpenAI 的分片。
type anthropicStreamConverter struct {
	id      string
	model   string
	created int64

	started bool
	done    bool

	inputTokens  int
	outputTokens int
	finishReason string

	// toolIndexByBlock 记录"Anthropic 内容块序号 → OpenAI tool_calls 序号"的映射。
	// Anthropic 的 content_block_delta 只带内容块序号，而 OpenAI 的增量按工具序号归位。
	toolIndexByBlock map[int]int
	nextToolIndex    int
}

// newAnthropicStreamConverter 创建流式转换器。
func newAnthropicStreamConverter() *anthropicStreamConverter {
	return &anthropicStreamConverter{
		id:               "chatcmpl-aqua",
		created:          time.Now().Unix(),
		toolIndexByBlock: make(map[int]int),
	}
}

// anthropicStreamEvent 是 Anthropic SSE 事件体（合并声明所有用到的字段）。
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// consume 处理一个 Anthropic SSE 事件，返回需要写给客户端的 OpenAI SSE 字节。
//
// 事件类型优先取 JSON 里的 type 字段（更可靠），取不到时回退到 event: 行。
func (e *anthropicStreamConverter) consume(eventName string, payload []byte) []byte {
	var event anthropicStreamEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		// 无法解析的分片直接丢弃：多为保活注释或私有扩展，
		// 强行透传会让 OpenAI 客户端解析失败。
		return nil
	}

	kind := event.Type
	if kind == "" {
		kind = eventName
	}

	switch kind {
	case "message_start":
		e.started = true
		if event.Message != nil {
			if event.Message.ID != "" {
				e.id = normalizeOpenAIID(event.Message.ID)
			}
			if event.Message.Model != "" {
				e.model = event.Message.Model
			}
			if event.Message.Usage != nil {
				e.inputTokens = event.Message.Usage.InputTokens
			}
		}
	}

	switch kind {
	case "message_start":
		// 首个分片：给出 role=assistant，OpenAI 客户端据此初始化消息
		return e.chunk(map[string]any{"role": "assistant"}, "", false)
	case "content_block_start":
		if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
			index := e.nextToolIndex
			e.nextToolIndex++
			e.toolIndexByBlock[event.Index] = index
			delta := map[string]any{"tool_calls": []map[string]any{{
				"index": index,
				"id":    event.ContentBlock.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      event.ContentBlock.Name,
					"arguments": "",
				},
			}}}
			return e.chunk(delta, "", false)
		}
		return nil
	case "content_block_delta":
		if event.Delta == nil {
			return nil
		}
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text == "" {
				return nil
			}
			return e.chunk(map[string]any{"content": event.Delta.Text}, "", false)
		case "input_json_delta":
			index, ok := e.toolIndexByBlock[event.Index]
			if !ok {
				return nil
			}
			delta := map[string]any{"tool_calls": []map[string]any{{
				"index":    index,
				"function": map[string]any{"arguments": event.Delta.PartialJSON},
			}}}
			return e.chunk(delta, "", false)
		}
		return nil
	case "message_delta":
		if event.Delta != nil && event.Delta.StopReason != "" {
			e.finishReason = anthropicStopReasonToOpenAI(event.Delta.StopReason, e.nextToolIndex > 0)
		}
		if event.Usage != nil {
			e.outputTokens = event.Usage.OutputTokens
		}
		return nil
	case "message_stop":
		return e.finish()
	case "error":
		return e.errorChunk(event)
	default:
		// ping / content_block_stop 等无需转换
		return nil
	}
}

// chunk 组装一个 OpenAI 的 chat.completion.chunk 分片。
//
// 参数 finishReason 为空串表示"尚未结束"；非空时写入 finish_reason。
// withUsage 为 true 时附带 usage（OpenAI 的用法是单独一个带用量的事件）。
func (e *anthropicStreamConverter) chunk(delta map[string]any, finishReason string, withUsage bool) []byte {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason == "" {
		choice["finish_reason"] = nil
	} else {
		choice["finish_reason"] = finishReason
	}

	payload := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []map[string]any{choice},
	}
	if withUsage {
		payload["usage"] = map[string]any{
			"prompt_tokens":     e.inputTokens,
			"completion_tokens": e.outputTokens,
			"total_tokens":      e.inputTokens + e.outputTokens,
		}
	}
	return openAISSE(payload)
}

// finish 发出结束分片（含 finish_reason 与 usage）并追加 [DONE]。
func (e *anthropicStreamConverter) finish() []byte {
	if e.done {
		return nil
	}
	e.done = true

	reason := e.finishReason
	if reason == "" {
		reason = "stop"
	}
	out := e.chunk(map[string]any{}, reason, true)
	out = append(out, "data: [DONE]\n\n"...)
	return out
}

// errorChunk 把上游的流式错误转换为 OpenAI 的错误分片并结束流。
func (e *anthropicStreamConverter) errorChunk(event anthropicStreamEvent) []byte {
	if e.done {
		return nil
	}
	e.done = true

	message := "上游流式响应出错"
	if event.Error != nil && strings.TrimSpace(event.Error.Message) != "" {
		message = event.Error.Message
	}
	out := openAISSE(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    oai.TypeServer,
			"code":    oai.CodeUpstreamRequestFailed,
		},
	})
	out = append(out, "data: [DONE]\n\n"...)
	return out
}

// openAISSE 按 OpenAI 的 SSE 格式编码一个数据分片。
func openAISSE(payload map[string]any) []byte {
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
