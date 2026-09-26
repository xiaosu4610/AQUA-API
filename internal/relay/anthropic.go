// 本文件实现 Anthropic Messages 协议与内部 OpenAI 协议的双向转换。
//
// 意图（Why）：
//
//	Claude 官方 SDK、Claude Code、以及一批客户端默认走 Anthropic 的
//	POST /v1/messages 协议。若不转换，这些工具无法接入本站；
//	转换之后，同一份上游资源可以被 OpenAI 与 Anthropic 两类客户端共用。
//
// 协议对照（只列本项目需要转换的部分）：
//
//	请求：
//	  Anthropic                              → OpenAI
//	  model                                     model
//	  system（字符串或文本块数组）                 messages[0].role=system
//	  messages[].content（string | 块数组）       messages[].content
//	    ├─ {type:text}                            纯文本
//	    ├─ {type:image, source:base64}            image_url（data: URI）
//	    ├─ {type:tool_use}                        assistant.tool_calls
//	    └─ {type:tool_result}                     role=tool 消息
//	  max_tokens / temperature / top_p / stop_sequences  同名或对应字段
//	  tools[].input_schema                      tools[].function.parameters
//	  stream                                    stream
//
//	响应：
//	  OpenAI                                 → Anthropic
//	  choices[0].message.content               content[{type:text,text}]
//	  choices[0].message.tool_calls            content[{type:tool_use,...}]
//	  choices[0].finish_reason                 stop_reason（见 mapStopReason）
//	  usage.prompt_tokens/completion_tokens    usage.input_tokens/output_tokens
//
// 流式：把上游的 OpenAI 分片重组成 Anthropic 的事件序列
//
//	（message_start → content_block_start → content_block_delta* →
//	  content_block_stop → message_delta → message_stop）。
//
// 扩展（Extend）：
//
//	Anthropic 新增内容块类型（如 thinking、document）时，在
//	toOpenAIMessages 与 anthropicContentBlocks 两处同步补齐映射。
package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// anthropicAdapter 实现 Adapter 接口。
type anthropicAdapter struct{}

// NewAnthropicAdapter 创建 Anthropic 协议适配器。
func NewAnthropicAdapter() Adapter {
	return anthropicAdapter{}
}

// Name 返回协议名。
func (anthropicAdapter) Name() string { return "anthropic" }

// ---- 请求结构（只声明本项目需要读取的字段）----

// anthropicRequest 是 Anthropic Messages 请求体。
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system"` // 字符串或文本块数组
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Stream        bool               `json:"stream"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
}

// anthropicMessage 是 Anthropic 的一条消息。
//
// Content 用 RawMessage：它既可能是字符串，也可能是内容块数组，
// 先原样接住再按形态分发，避免为两种结构定义两套消息类型。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicTool 是 Anthropic 的工具声明。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicContentBlock 是 Anthropic 的内容块（只声明用到的类型）。
type anthropicContentBlock struct {
	Type string `json:"type"`

	// type=text
	Text string `json:"text"`

	// type=image
	Source *struct {
		Type      string `json:"type"`       // "base64"
		MediaType string `json:"media_type"` // "image/png"
		Data      string `json:"data"`
	} `json:"source"`

	// type=tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// type=tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// DecodeRequest 把 Anthropic 请求转换为 OpenAI 请求。
//
// pathModel 参数被忽略：Anthropic 协议把模型名放在请求体里，
// 路径只是固定的 /v1/messages，不含模型信息。
func (a anthropicAdapter) DecodeRequest(body []byte, _ string) (string, []byte, error) {
	var req anthropicRequest
	if err := decodeJSONStrict(body, &req, "Anthropic Messages"); err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(req.Model) == "" {
		return "", nil, fmt.Errorf("%w（缺少 model 字段）", ErrUnsupportedProtocol)
	}

	messages := make([]map[string]any, 0, len(req.Messages)+1)
	if system := decodeAnthropicSystem(req.System); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for _, msg := range req.Messages {
		messages = append(messages, toOpenAIMessages(msg)...)
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": messages,
	}
	if req.Stream {
		out["stream"] = true
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		out["tools"] = toOpenAITools(req.Tools)
	}
	if len(req.ToolChoice) > 0 {
		if mapped := toOpenAIToolChoice(req.ToolChoice); mapped != nil {
			out["tool_choice"] = mapped
		}
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return "", nil, fmt.Errorf("relay: 构造上游请求失败: %w", err)
	}
	return req.Model, encoded, nil
}

// StreamRequested 判断是否为流式请求。
func (anthropicAdapter) StreamRequested(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Stream
}

// decodeAnthropicSystem 把 system 字段（字符串或文本块数组）拍平为纯文本。
//
// 为什么要拍平：OpenAI 的 system 消息只接受字符串（或内容块数组，
// 但上游兼容实现的支持度参差不齐）。拍平为纯文本兼容性最好。
func decodeAnthropicSystem(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}

	// 形态 1：字符串
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return strings.TrimSpace(text)
	}

	// 形态 2：文本块数组
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toOpenAIMessages 把一条 Anthropic 消息转换为一条或多条 OpenAI 消息。
//
// 需要"一变多"的情形：Anthropic 把 tool_result 放在 user 消息的内容块里，
// 而 OpenAI 要求工具结果必须是独立的 role=tool 消息。
func toOpenAIMessages(msg anthropicMessage) []map[string]any {
	trimmed := bytes.TrimSpace(msg.Content)
	if len(trimmed) == 0 {
		return []map[string]any{{"role": msg.Role, "content": ""}}
	}

	// 形态 1：纯字符串内容
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return []map[string]any{{"role": msg.Role, "content": text}}
	}

	// 形态 2：内容块数组
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return []map[string]any{{"role": msg.Role, "content": string(trimmed)}}
	}

	result := make([]map[string]any, 0, len(blocks)+1)
	// 普通内容块（文本 / 图片）聚合为一条消息
	parts := make([]map[string]any, 0, len(blocks))
	// 工具调用（仅 assistant 消息会出现）
	toolCalls := make([]map[string]any, 0)

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": block.Text})
			}
		case "image":
			if block.Source != nil && block.Source.Data != "" {
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:" + block.Source.MediaType + ";base64," + block.Source.Data,
					},
				})
			}
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
		case "tool_result":
			// 工具结果必须单独成一条 role=tool 消息
			content := ""

			// tool_result 的 content 也可能是块数组
			var nested []anthropicContentBlock
			if len(block.Content) > 0 {
				if err := json.Unmarshal(block.Content, &nested); err == nil {
					texts := make([]string, 0, len(nested))
					for _, item := range nested {
						if item.Type == "text" && item.Text != "" {
							texts = append(texts, item.Text)
						}
					}
					content = strings.Join(texts, "\n")
				} else {
					content = strings.Trim(string(block.Content), `"`)
				}
			}
			result = append(result, map[string]any{
				"role":         "tool",
				"tool_call_id": block.ToolUseID,
				"content":      content,
			})
		}
	}

	// 先放工具结果（对应上面的 tool_result 块），再放普通内容
	if len(parts) > 0 || len(toolCalls) > 0 {
		message := map[string]any{"role": msg.Role}
		// 只有单个纯文本块时退化为字符串，兼容那些不支持内容块数组的上游
		if len(parts) == 1 && parts[0]["type"] == "text" {
			if _, hasTools := parts[0]["image_url"]; !hasTools {
				message["content"] = parts[0]["text"]
			}
		}
		if _, exists := message["content"]; !exists && len(parts) > 0 {
			message["content"] = parts
		}
		if _, exists := message["content"]; !exists {
			message["content"] = ""
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		result = append([]map[string]any{message}, result...)
	}

	if len(result) == 0 {
		result = append(result, map[string]any{"role": msg.Role, "content": ""})
	}
	return result
}

// toOpenAITools 把 Anthropic 工具声明转换为 OpenAI 的 function 形式。
func toOpenAITools(tools []anthropicTool) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.InputSchema) > 0 {
			if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
				// schema 解析失败时退化为"任意对象"，避免整个请求失败
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
		}
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return result
}

// toOpenAIToolChoice 把 Anthropic 的 tool_choice 映射为 OpenAI 形式。
//
// 映射关系：auto / any → auto / required；{type:tool,name} → 指定 function。
func toOpenAIToolChoice(raw json.RawMessage) any {
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		switch simple {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		}
		return nil
	}

	var shaped struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &shaped); err != nil {
		return nil
	}
	if shaped.Type == "tool" && shaped.Name != "" {
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": shaped.Name},
		}
	}
	if shaped.Type == "any" {
		return "required"
	}
	return nil
}

// ---- 响应转换（OpenAI → Anthropic）----

// EncodeResponse 把上游的非流式 OpenAI 响应转换为 Anthropic Messages 响应。
func (a anthropicAdapter) EncodeResponse(upstreamBody []byte) []byte {
	var upstream openAIResponse
	if err := json.Unmarshal(upstreamBody, &upstream); err != nil {
		// 上游响应结构异常：返回一个合法的空响应而不是原始 JSON，
		// 否则 Anthropic 客户端会报出难以理解的解析错误
		return mustEncode(map[string]any{
			"id": "msg_aqua_unparsable", "type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "text", "text": ""}},
			"model":   "", "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		})
	}

	content := make([]map[string]any, 0, 2)
	stopReason := "end_turn"

	if len(upstream.Choices) > 0 {
		choice := upstream.Choices[0]
		if text := choice.Message.Content; strings.TrimSpace(text) != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, call := range choice.Message.ToolCalls {
			var input any = map[string]any{}
			if strings.TrimSpace(call.Function.Arguments) != "" {
				// 参数是 JSON 字符串：解析成对象，Anthropic 要求 input 为对象
				if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
					input = map[string]any{"_raw": call.Function.Arguments}
				}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    call.ID,
				"name":  call.Function.Name,
				"input": input,
			})
		}
		if len(choice.Message.ToolCalls) > 0 {
			stopReason = "tool_use"
		} else {
			stopReason = mapStopReason(choice.FinishReason)
		}
	}

	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	inputTokens, outputTokens := 0, 0
	if upstream.Usage != nil {
		inputTokens = upstream.Usage.PromptTokens
		outputTokens = upstream.Usage.CompletionTokens
	}

	return mustEncode(map[string]any{
		"id":          normalizeAnthropicID(upstream.ID),
		"type":        "message",
		"role":        "assistant",
		"model":       upstream.Model,
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})
}

// EncodeError 把错误转换为 Anthropic 的错误格式。
func (a anthropicAdapter) EncodeError(statusCode int, upstreamBody []byte) []byte {
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(upstreamBody, &probe)

	return mustEncode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    upstreamErrorTypeToAnthropic(statusCode, probe.Type),
			"message": extractErrorMessage(upstreamBody),
		},
	})
}

// mapStopReason 把 OpenAI 的 finish_reason 映射为 Anthropic 的 stop_reason。
func mapStopReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "content_filter":
		return "end_turn"
	default:
		if finishReason == "" {
			return "end_turn"
		}
		return finishReason
	}
}

// normalizeAnthropicID 保证消息 id 形如 msg_xxx。
//
// 为什么必须处理：Anthropic 客户端会校验 id 前缀并据此判断响应是否合法，
// 直接透传上游的 "chatcmpl-xxx" 会导致部分客户端报错。
func normalizeAnthropicID(upstreamID string) string {
	id := strings.TrimSpace(upstreamID)
	if id == "" {
		return "msg_aqua"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

// mustEncode 序列化 JSON；失败时返回一个极简的合法响应。
//
// 为什么不返回错误：这些结构都是我们自己构造的 map，序列化失败只可能是
// 出现了不可序列化的值（编程错误）。此时返回错误会影响用户体验，
// 而不返回又难以发现——折中做法是返回兜底内容并保持接口签名简洁。
func mustEncode(payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"响应序列化失败"}}`)
	}
	return encoded
}

// NewStreamEncoder 创建 Anthropic 流式编码器。
func (a anthropicAdapter) NewStreamEncoder() StreamEncoder {
	return &anthropicStreamEncoder{
		toolBlocks: make(map[int]int),
	}
}

// anthropicStreamEncoder 把上游的 OpenAI 流式分片转换为 Anthropic 事件序列。
//
// 状态机说明：
//   - 上游第一个包含内容的 chunk 到达时，才发 message_start 与首个 content_block_start
//     （因为 message_start 里需要 model 与 id，这些来自上游分片）；
//   - 文本增量直接映射为 text_delta；
//   - 一旦出现工具调用，先关闭文本块，再为每个工具开一个 tool_use 块，
//     其参数以 input_json_delta 增量下发（与 Anthropic 官方行为一致）；
//   - 收到 finish_reason 时记录停止原因，在 End 中通过 message_delta 下发。
type anthropicStreamEncoder struct {
	buffer       bytes.Buffer
	started      bool
	blockOpen    bool
	blockIndex   int
	messageID    string
	model        string
	inputTokens  int
	outputTokens int
	stopReason   string

	// toolBlocks 记录"上游工具序号 → Anthropic 内容块序号"的映射。
	// 上游可能并发返回多个工具调用的增量，必须按序号分别归位。
	toolBlocks map[int]int
}

// Begin 不输出任何内容：Anthropic 要求 message_start 是最早的事件，
// 而其中需要 model/id，这些只有收到上游首个分片后才知道。
func (e *anthropicStreamEncoder) Begin() []byte { return nil }

// Chunk 转换一个上游分片。
func (e *anthropicStreamEncoder) Chunk(payload []byte) []byte {
	var chunk openAIStreamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		// 无法解析的分片直接丢弃：这些多为上游的保活注释或私有扩展，
		// 强行透传会让 Anthropic 客户端解析失败
		return nil
	}

	if chunk.Model != "" {
		e.model = chunk.Model
	}
	if chunk.ID != "" {
		e.messageID = normalizeAnthropicID(chunk.ID)
	}
	if chunk.Usage != nil {
		e.inputTokens = chunk.Usage.PromptTokens
		e.outputTokens = chunk.Usage.CompletionTokens
	}

	// 首个分片：补发 message_start 与首个文本块起始事件
	if !e.started {
		e.started = true
		e.writeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      e.messageID,
				"type":    "message",
				"role":    "assistant",
				"model":   e.model,
				"content": []any{},
				"usage":   map[string]any{"input_tokens": e.inputTokens, "output_tokens": 0},
			},
		})
		e.openTextBlock()
	}

	if len(chunk.Choices) == 0 {
		return e.flush()
	}
	choice := chunk.Choices[0]

	// 文本增量
	if text := choice.Delta.Content; text != "" {
		if !e.blockOpen {
			e.openTextBlock()
		}
		e.writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}

	// 工具调用增量
	for _, call := range choice.Delta.ToolCalls {
		blockIndex, exists := e.toolBlocks[call.Index]
		if !exists {
			// 首次出现该工具调用：关闭文本块，开一个 tool_use 块
			e.closeCurrentBlock()
			blockIndex = e.blockIndex + 1
			e.blockIndex = blockIndex
			e.blockOpen = true
			e.toolBlocks[call.Index] = blockIndex
			e.writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Function.Name,
					"input": map[string]any{},
				},
			})
		}
		if call.Function.Arguments != "" {
			e.writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": call.Function.Arguments,
				},
			})
		}
	}

	// 结束原因
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		if len(e.toolBlocks) > 0 {
			e.stopReason = "tool_use"
		} else {
			e.stopReason = mapStopReason(*choice.FinishReason)
		}
	}

	return e.flush()
}

// End 收尾：关闭内容块、发 message_delta 与 message_stop。
func (e *anthropicStreamEncoder) End() []byte {
	if !e.started {
		// 上游一个分片都没产出：仍要给出一个完整的事件序列，
		// 否则客户端会一直等待（表现为"请求卡住"）
		e.started = true
		e.writeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": e.messageID, "type": "message", "role": "assistant",
				"model": e.model, "content": []any{},
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
		e.openTextBlock()
	}

	e.closeCurrentBlock()
	if e.stopReason == "" {
		e.stopReason = "end_turn"
	}
	e.writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": e.stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": e.outputTokens},
	})
	e.writeEvent("message_stop", map[string]any{"type": "message_stop"})
	return e.flush()
}

// openTextBlock 开启一个文本内容块。
func (e *anthropicStreamEncoder) openTextBlock() {
	if e.blockOpen {
		return
	}
	if !e.started {
		return
	}
	e.blockOpen = true
	e.writeEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         e.blockIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// closeCurrentBlock 关闭当前内容块（若处于开启状态）。
func (e *anthropicStreamEncoder) closeCurrentBlock() {
	if !e.blockOpen {
		return
	}
	e.writeEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": e.blockIndex,
	})
	e.blockOpen = false
}

// writeEvent 按 Anthropic 的 SSE 格式写入一个事件。
//
// 格式要求：必须同时给出 event: 行与 data: 行，且以空行结尾。
// 只给 data 不给 event 时，部分 SDK 会拿不到事件类型。
func (e *anthropicStreamEncoder) writeEvent(eventName string, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	e.buffer.WriteString("event: ")
	e.buffer.WriteString(eventName)
	e.buffer.WriteString("\ndata: ")
	e.buffer.Write(encoded)
	e.buffer.WriteString("\n\n")
}

// flush 取出并清空缓冲区。
func (e *anthropicStreamEncoder) flush() []byte {
	if e.buffer.Len() == 0 {
		return nil
	}
	out := make([]byte, e.buffer.Len())
	copy(out, e.buffer.Bytes())
	e.buffer.Reset()
	return out
}

// ServeAnthropicMessages 处理 POST /v1/messages（Anthropic Messages 协议）。
func (r *Relay) ServeAnthropicMessages(w http.ResponseWriter, req *http.Request) {
	r.serveWithAdapter(w, req, NewAnthropicAdapter())
}
