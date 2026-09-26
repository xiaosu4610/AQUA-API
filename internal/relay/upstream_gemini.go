// 本文件实现 Gemini（generateContent）上游的出站适配：把内部 OpenAI 协议
// 改写成 Gemini 原生协议，并把 Gemini 响应改写回 OpenAI。
//
// 意图（Why）：
//
//	网关内部统一以 OpenAI 协议作为「中间表示」，而 Gemini 是另一套原生协议。
//	当渠道类型 Protocol=gemini 时，必须在【发往上游前】把内部 OpenAI 请求
//	改写成 Gemini 请求，并在【收到响应后】把 Gemini 响应（含流式与错误体）
//	改写回 OpenAI，上层（计费 / 用量 / 下游协议适配）才能像对待普通 OpenAI
//	上游一样工作。
//
//	与 gemini.go 的区别：gemini.go 处理的是【下游 Gemini → OpenAI】（入站，
//	让 Gemini 客户端能用本站资源）；本文件处理的是【OpenAI → Gemini 上游】（出站，
//	让本站能调用 Gemini 上游）。两者方向相反、互不干扰。
//
// 协议对照（只列需要转换的部分）：
//
//	请求（OpenAI → Gemini）：
//	  messages[].role=system    → systemInstruction（单独拎出为顶层字段）
//	  messages[].role=user      → contents[].role=user
//	  messages[].role=assistant → contents[].role=model
//	  messages[].content        → contents[].parts[].text（或 inlineData）
//	  messages[].tool_calls     → parts[].functionCall
//	  messages[].role=tool      → user 消息里的 parts[].functionResponse
//	  max_tokens                → generationConfig.maxOutputTokens
//	  temperature/top_p         → generationConfig.temperature/topP
//	  stop                      → generationConfig.stopSequences
//	  tools[].function          → tools[].functionDeclarations[]
//	  tool_choice               → toolConfig.functionCallingConfig
//
//	响应（Gemini → OpenAI）：
//	  candidates[0].content.parts[].text     → choices[0].message.content
//	  candidates[0].content.parts[].functionCall → choices[0].message.tool_calls
//	  candidates[0].finishReason             → choices[0].finish_reason
//	  usageMetadata.promptTokenCount/candidatesTokenCount → usage 的
//	  prompt_tokens/completion_tokens
//
//	流式：Gemini 的 SSE 每个事件是一段完整的 GenerateContentResponse JSON
//	（不是 OpenAI 的 delta 分片），这里逐事件转换为 chat.completion.chunk，
//	并在流结束时以 data: [DONE] 收尾。
//
// 扩展（Extend）：
//
//	Gemini 新增 parts 类型（如 fileData、codeExecution）时，在
//	openAIContentToGeminiParts 与 geminiUpstreamResponse 两处同步补齐；
//	映射不了的类型必须【明确报错】，不允许静默丢弃（静默丢工具会让用户
//	以为"模型不会用工具"）。
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

	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// maxGeminiStreamLineBytes 是解析 Gemini SSE 单行的字节上限。
//
// 与 Anthropic 同理：单个事件可能较长，设上限用于防御异常上游撑爆内存。
const maxGeminiStreamLineBytes = 1 << 20

// ---- 请求转换（OpenAI → Gemini）----

// encodeGeminiRequest 把内部 OpenAI 请求体转换为 Gemini generateContent 请求体。
func encodeGeminiRequest(body []byte) ([]byte, error) {
	var in openAIUpstreamRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("relay: 解析待转换的 OpenAI 请求失败: %w", err)
	}

	system, contents, err := splitGeminiMessages(in.Messages)
	if err != nil {
		return nil, err
	}

	out := map[string]any{"contents": contents}
	if system != "" {
		// Gemini 把系统提示作为顶层 systemInstruction（无 role 字段）。
		out["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}

	// 生成参数统一放进 generationConfig；为空时不写该字段（避免下发空对象）。
	gen := map[string]any{}
	if in.MaxTokens > 0 {
		gen["maxOutputTokens"] = in.MaxTokens
	}
	if in.Temperature != nil {
		gen["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		gen["topP"] = *in.TopP
	}
	if stops := decodeOpenAIStop(in.Stop); len(stops) > 0 {
		gen["stopSequences"] = stops
	}
	if len(gen) > 0 {
		out["generationConfig"] = gen
	}

	if len(in.Tools) > 0 {
		declarations, err := toGeminiFunctionDeclarations(in.Tools)
		if err != nil {
			return nil, err
		}
		if len(declarations) > 0 {
			out["tools"] = []map[string]any{{"functionDeclarations": declarations}}
			// toolConfig 只有在声明了工具时才有意义，故放在该分支内。
			if len(in.ToolChoice) > 0 {
				config, err := toGeminiToolConfig(in.ToolChoice)
				if err != nil {
					return nil, err
				}
				if config != nil {
					out["toolConfig"] = config
				}
			}
		}
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("relay: 构造 Gemini 请求失败: %w", err)
	}
	return encoded, nil
}

// splitGeminiMessages 把 OpenAI 消息序列拆成 Gemini 的 systemInstruction 与 contents。
//
// 两大差异：
//  1. OpenAI 把 system 当普通消息，Gemini 要求它是顶层字段，因此先收集再拼接；
//  2. OpenAI 的工具结果是独立的 role=tool 消息，Gemini 要求它是 user 内容里的
//     functionResponse 片段；这里把【连续】的工具结果合并进同一条 user 内容。
//
// 工具结果的 functionResponse 需要"函数名"，而 OpenAI 的 role=tool 消息只带
// tool_call_id，故维护 id→name 映射（由前面的 assistant.tool_calls 填充）；
// 映射不到时明确报错，绝不静默丢弃。
func splitGeminiMessages(messages []openAIUpstreamMessage) (string, []map[string]any, error) {
	systemParts := make([]string, 0)
	contents := make([]map[string]any, 0, len(messages))
	toolNames := make(map[string]string)
	pendingToolParts := make([]map[string]any, 0)

	flushToolResults := func() {
		if len(pendingToolParts) == 0 {
			return
		}
		parts := make([]map[string]any, len(pendingToolParts))
		copy(parts, pendingToolParts)
		contents = append(contents, map[string]any{"role": "user", "parts": parts})
		pendingToolParts = pendingToolParts[:0]
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
			part, err := geminiFunctionResponsePart(msg, toolNames)
			if err != nil {
				return "", nil, err
			}
			pendingToolParts = append(pendingToolParts, part)
		default:
			flushToolResults()
			content, err := toGeminiContent(msg, toolNames)
			if err != nil {
				return "", nil, err
			}
			contents = append(contents, content)
		}
	}
	flushToolResults()

	// contents 不能为空：某些上游对空数组直接报错，给一条占位 user 内容兜底。
	if len(contents) == 0 {
		contents = append(contents, map[string]any{
			"role":  "user",
			"parts": []map[string]any{{"text": ""}},
		})
	}
	return strings.Join(systemParts, "\n\n"), contents, nil
}

// toGeminiContent 把一条普通（非 system / 非 tool）消息转换为 Gemini 内容。
//
// role 映射：user→user、assistant→model；其余角色按 user 处理（Gemini 只有两端）。
func toGeminiContent(msg openAIUpstreamMessage, toolNames map[string]string) (map[string]any, error) {
	role := "user"
	if msg.Role == "assistant" {
		role = "model"
	}

	parts, err := openAIContentToGeminiParts(msg.Content)
	if err != nil {
		return nil, err
	}
	if len(msg.ToolCalls) > 0 {
		callParts, names, err := openAIToolCallsToGeminiParts(msg.ToolCalls)
		if err != nil {
			return nil, err
		}
		for id, name := range names {
			toolNames[id] = name
		}
		parts = append(parts, callParts...)
	}
	if len(parts) == 0 {
		parts = []map[string]any{{"text": ""}}
	}
	return map[string]any{"role": role, "parts": parts}, nil
}

// openAIContentToGeminiParts 把 OpenAI 的 content（字符串或内容块数组）转换为
// Gemini 的 parts 数组。
//
// 映射不了的块类型【明确报错】：静默丢弃会让用户拿到"莫名其妙少了张图/一段话"
// 的结果，比直接失败更难排查。
func openAIContentToGeminiParts(raw json.RawMessage) ([]map[string]any, error) {
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
		return []map[string]any{{"text": text}}, nil
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
		return nil, fmt.Errorf("relay: OpenAI 消息内容结构无法识别，无法转换为 Gemini: %w", err)
	}

	result := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				result = append(result, map[string]any{"text": block.Text})
			}
		case "image_url":
			if block.ImageURL == nil || strings.TrimSpace(block.ImageURL.URL) == "" {
				continue
			}
			mediaType, data, ok := parseImageDataURI(block.ImageURL.URL)
			if !ok {
				// Gemini 的 inlineData 只能承载内联字节；外链图片需要先上传为
				// File 再用 fileData 引用，本适配器未实现，故明确报错而非丢弃。
				return nil, fmt.Errorf("relay: Gemini 仅支持 data: URI 形式的内联图片，无法映射该外链图片")
			}
			result = append(result, map[string]any{
				"inlineData": map[string]any{"mimeType": mediaType, "data": data},
			})
		default:
			return nil, fmt.Errorf("relay: OpenAI 内容块类型 %q 暂不支持转换为 Gemini（无法静默丢弃）", block.Type)
		}
	}
	return result, nil
}

// openAIToolCallsToGeminiParts 把 assistant.tool_calls 转换为 Gemini 的
// functionCall 片段，并返回"工具调用 ID → 函数名"的映射（供工具结果回填）。
func openAIToolCallsToGeminiParts(raw json.RawMessage) ([]map[string]any, map[string]string, error) {
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, nil, fmt.Errorf("relay: 无法解析 OpenAI tool_calls，无法转换为 Gemini: %w", err)
	}

	parts := make([]map[string]any, 0, len(calls))
	names := make(map[string]string, len(calls))
	for _, call := range calls {
		if call.Type != "" && call.Type != "function" {
			return nil, nil, fmt.Errorf("relay: OpenAI 工具调用类型 %q 暂不支持转换为 Gemini（无法静默丢弃）", call.Type)
		}
		if strings.TrimSpace(call.Function.Name) == "" {
			return nil, nil, fmt.Errorf("relay: OpenAI 工具调用缺少函数名，无法转换为 Gemini")
		}
		args := map[string]any{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return nil, nil, fmt.Errorf("relay: OpenAI 工具调用参数不是合法 JSON，无法转换为 Gemini: %w", err)
			}
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{"name": call.Function.Name, "args": args},
		})
		if call.ID != "" {
			names[call.ID] = call.Function.Name
		}
	}
	return parts, names, nil
}

// geminiFunctionResponsePart 把一条 role=tool 消息转换为 Gemini 的 functionResponse 片段。
//
// 参数名取自前面 assistant.tool_calls 建立的映射：Gemini 要求 functionResponse
// 必须带函数名，而 OpenAI 的工具结果只带 tool_call_id，映射不到时明确报错。
func geminiFunctionResponsePart(msg openAIUpstreamMessage, toolNames map[string]string) (map[string]any, error) {
	name := strings.TrimSpace(toolNames[msg.ToolCallID])
	if name == "" {
		return nil, fmt.Errorf("relay: 工具结果缺少可对应的函数名（tool_call_id=%q），无法转换为 Gemini functionResponse", msg.ToolCallID)
	}

	text, _ := flattenOpenAIContent(msg.Content)
	// Gemini 的 functionResponse.response 期望是 JSON 对象；工具结果文本若本身
	// 就是 JSON 对象则直接内嵌，否则包一层 content 字段承载纯文本。
	response := map[string]any{"content": text}
	if strings.TrimSpace(text) != "" {
		var structured map[string]any
		if err := json.Unmarshal([]byte(text), &structured); err == nil && structured != nil {
			response = structured
		}
	}
	return map[string]any{
		"functionResponse": map[string]any{"name": name, "response": response},
	}, nil
}

// toGeminiFunctionDeclarations 把 OpenAI 的 tools 声明转换为 Gemini 的
// functionDeclarations（Gemini 把多个声明打包在一个 tools 元素里）。
func toGeminiFunctionDeclarations(raw json.RawMessage) ([]map[string]any, error) {
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("relay: 无法解析 OpenAI tools，无法转换为 Gemini: %w", err)
	}

	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "" && tool.Type != "function" {
			return nil, fmt.Errorf("relay: OpenAI 工具类型 %q 暂不支持转换为 Gemini（无法静默丢弃）", tool.Type)
		}
		if strings.TrimSpace(tool.Function.Name) == "" {
			return nil, fmt.Errorf("relay: OpenAI 工具声明缺少函数名，无法转换为 Gemini")
		}
		parameters := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Function.Parameters) > 0 {
			if err := json.Unmarshal(tool.Function.Parameters, &parameters); err != nil {
				return nil, fmt.Errorf("relay: OpenAI 工具参数 schema 不是合法 JSON，无法转换为 Gemini: %w", err)
			}
		}
		result = append(result, map[string]any{
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
			"parameters":  parameters,
		})
	}
	return result, nil
}

// toGeminiToolConfig 把 OpenAI 的 tool_choice 映射为 Gemini 的 toolConfig。
//
// 映射关系：auto → AUTO；required → ANY；none → NONE；
// {function:{name}} → ANY + allowedFunctionNames。
// 无法识别的取值明确报错，而不是静默丢弃（丢弃会改变工具语义）。
func toGeminiToolConfig(raw json.RawMessage) (map[string]any, error) {
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		mode := ""
		switch simple {
		case "auto":
			mode = "AUTO"
		case "required":
			mode = "ANY"
		case "none":
			mode = "NONE"
		default:
			return nil, fmt.Errorf("relay: 无法识别的 OpenAI tool_choice %q（无法转换为 Gemini）", simple)
		}
		return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}, nil
	}

	var shaped struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &shaped); err != nil {
		return nil, fmt.Errorf("relay: 无法解析 OpenAI tool_choice，无法转换为 Gemini: %w", err)
	}
	if shaped.Type == "function" && strings.TrimSpace(shaped.Function.Name) != "" {
		return map[string]any{
			"functionCallingConfig": map[string]any{
				"mode":                 "ANY",
				"allowedFunctionNames": []string{shaped.Function.Name},
			},
		}, nil
	}
	return nil, fmt.Errorf("relay: OpenAI tool_choice 结构无法转换为 Gemini")
}

// ---- 响应转换（Gemini → OpenAI）----

// geminiUpstreamResponse 是 Gemini 的响应体（非流式与流式分片共用同一形态）。
type geminiUpstreamResponse struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

// geminiUpstreamError 是 Gemini 的错误响应体。
type geminiUpstreamError struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// decodeGeminiUpstreamResponse 把 Gemini 非流式响应转换为 OpenAI 响应。
func decodeGeminiUpstreamResponse(body []byte) []byte {
	var upstream geminiUpstreamResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		// 结构异常：返回一个合法的空响应，避免下游客户端解析失败
		return marshalOpenAIPayload(openAICompletionPayload("", "", "", nil, "stop", 0, 0))
	}

	var text strings.Builder
	toolCalls := make([]map[string]any, 0)
	finishReason := "stop"

	if len(upstream.Candidates) > 0 {
		candidate := upstream.Candidates[0]
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				text.WriteString(part.Text)
			}
			if part.FunctionCall != nil {
				arguments := "{}"
				if len(part.FunctionCall.Args) > 0 {
					arguments = string(part.FunctionCall.Args)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   geminiToolCallID(part.FunctionCall.Name, len(toolCalls)),
					"type": "function",
					"function": map[string]any{
						"name":      part.FunctionCall.Name,
						"arguments": arguments,
					},
				})
			}
		}
		finishReason = geminiFinishReasonToOpenAI(candidate.FinishReason, len(toolCalls) > 0)
	}

	promptTokens, completionTokens := 0, 0
	if upstream.UsageMetadata != nil {
		promptTokens = upstream.UsageMetadata.PromptTokenCount
		completionTokens = upstream.UsageMetadata.CandidatesTokenCount
	}

	return marshalOpenAIPayload(openAICompletionPayload(
		"", upstream.ModelVersion, text.String(), toolCalls, finishReason,
		promptTokens, completionTokens))
}

// decodeGeminiUpstreamError 把 Gemini 错误体转换为 OpenAI 错误体。
func decodeGeminiUpstreamError(body []byte, status int) []byte {
	message := "上游请求失败"

	var envelope geminiUpstreamError
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil &&
		strings.TrimSpace(envelope.Error.Message) != "" {
		message = envelope.Error.Message
	}

	return marshalOpenAIPayload(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    geminiStatusToOpenAIErrorType(status),
			"code":    oai.CodeUpstreamRequestFailed,
		},
	})
}

// geminiToolCallID 合成一个稳定的工具调用 ID。
//
// Gemini 不提供工具调用 ID；OpenAI 客户端需要它来关联 tool 结果，
// 因此用"函数名 + 序号"拼出一个可对应上的标识符。
func geminiToolCallID(name string, index int) string {
	return fmt.Sprintf("call_%s_%d", name, index)
}

// geminiFinishReasonToOpenAI 把 Gemini 的 finishReason 映射为 OpenAI 的 finish_reason。
func geminiFinishReasonToOpenAI(reason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "STOP", "":
		return "stop"
	default:
		return "stop"
	}
}

// geminiStatusToOpenAIErrorType 按状态码把 Gemini 错误映射为 OpenAI 错误类型。
func geminiStatusToOpenAIErrorType(status int) string {
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

// ---- 响应改写入口（就地修改 http.Response）----

// adjustGeminiUpstreamResponse 把上游 Gemini 响应改写为 OpenAI 形态。
//
// 三条分支：
//   - 错误响应（>=400）：改写为 OpenAI 错误体；
//   - 流式成功：把 Gemini SSE 转换为 OpenAI SSE；
//   - 非流式成功：整体读取并改写为 OpenAI 响应体。
func adjustGeminiUpstreamResponse(resp *http.Response, wantStream bool) error {
	if resp == nil || resp.Body == nil {
		return nil
	}

	if resp.StatusCode >= http.StatusBadRequest {
		raw, err := readAllLimited(resp.Body, maxAdaptedBodyBytes)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		replaceResponseBody(resp, decodeGeminiUpstreamError(raw, resp.StatusCode))
		return nil
	}

	if wantStream {
		resp.Body = wrapGeminiUpstreamStream(resp.Body)
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
	replaceResponseBody(resp, decodeGeminiUpstreamResponse(raw))
	return nil
}

// ---- 流式转换（Gemini SSE → OpenAI SSE）----

// wrapGeminiUpstreamStream 把上游 Gemini 的 SSE 字节流转换为 OpenAI 的 SSE 流。
//
// 实现方式：io.Pipe + 后台 goroutine 逐行读取上游 SSE、逐事件转换后写入管道；
// 调用方读到的是 OpenAI 形态的 SSE，流式回写与 usage 抓取等既有逻辑无需改动。
func wrapGeminiUpstreamStream(upstream io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	converter := newGeminiStreamConverter()

	go func() {
		scanner := bufio.NewScanner(upstream)
		scanner.Buffer(make([]byte, 0, 64*1024), maxGeminiStreamLineBytes)

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
			payload, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			payload = strings.TrimSpace(payload)
			if payload == "" || payload == "[DONE]" {
				continue
			}
			if out := converter.consume([]byte(payload)); len(out) > 0 {
				if _, err := writer.Write(out); err != nil {
					return
				}
			}
		}
		// 上游未显式结束（Gemini 不保证发 [DONE]）时也要收尾，否则客户端一直等待
		if out := converter.finish(); len(out) > 0 {
			_, _ = writer.Write(out)
		}
		_ = writer.Close()
	}()

	return &geminiStreamReader{reader: reader, writer: writer, upstream: upstream, done: done}
}

// geminiStreamReader 包装 io.Pipe，确保 Close 时同时结束后台 goroutine 与上游连接。
//
// 为什么需要它：客户端断开时转发主链路会关闭响应体，若只关管道，
// 后台 goroutine 会阻塞在上游读取上永不退出（goroutine 泄漏）。
type geminiStreamReader struct {
	reader   *io.PipeReader
	writer   *io.PipeWriter
	upstream io.ReadCloser
	done     chan struct{}
}

// Read 读取转换后的 OpenAI SSE 字节。
func (r *geminiStreamReader) Read(p []byte) (int, error) { return r.reader.Read(p) }

// Close 结束转换并关闭上游连接（幂等）。
func (r *geminiStreamReader) Close() error {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	_ = r.reader.CloseWithError(io.EOF)
	return r.upstream.Close()
}

// geminiStreamConverter 把 Gemini 的 SSE 事件转换为 OpenAI 的流式分片。
type geminiStreamConverter struct {
	id      string
	model   string
	created int64

	started bool
	done    bool

	inputTokens  int
	outputTokens int
	finishReason string
	toolIndex    int
}

// newGeminiStreamConverter 创建流式转换器。
func newGeminiStreamConverter() *geminiStreamConverter {
	return &geminiStreamConverter{
		id:      "chatcmpl-aqua",
		created: time.Now().Unix(),
	}
}

// consume 处理一个 Gemini SSE 事件，返回需要写给客户端的 OpenAI SSE 字节。
func (e *geminiStreamConverter) consume(payload []byte) []byte {
	if e.done {
		return nil
	}

	var upstream geminiUpstreamResponse
	if err := json.Unmarshal(payload, &upstream); err != nil {
		// 可能是错误事件（Gemini 流式错误以一个 error 对象结束）
		var envelope geminiUpstreamError
		if json.Unmarshal(payload, &envelope) == nil && envelope.Error != nil {
			return e.errorOut(envelope)
		}
		return nil
	}

	if upstream.UsageMetadata != nil {
		e.inputTokens = upstream.UsageMetadata.PromptTokenCount
		e.outputTokens = upstream.UsageMetadata.CandidatesTokenCount
	}
	if upstream.ModelVersion != "" && e.model == "" {
		e.model = upstream.ModelVersion
	}

	var out bytes.Buffer
	if !e.started {
		// 首个分片：给出 role=assistant，OpenAI 客户端据此初始化消息
		e.started = true
		out.Write(e.chunk(map[string]any{"role": "assistant"}, nil, false))
	}

	if len(upstream.Candidates) > 0 {
		candidate := upstream.Candidates[0]
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				out.Write(e.chunk(map[string]any{"content": part.Text}, nil, false))
			}
			if part.FunctionCall != nil {
				arguments := "{}"
				if len(part.FunctionCall.Args) > 0 {
					arguments = string(part.FunctionCall.Args)
				}
				index := e.toolIndex
				e.toolIndex++
				delta := map[string]any{"tool_calls": []map[string]any{{
					"index": index,
					"id":    geminiToolCallID(part.FunctionCall.Name, index),
					"type":  "function",
					"function": map[string]any{
						"name":      part.FunctionCall.Name,
						"arguments": arguments,
					},
				}}}
				out.Write(e.chunk(delta, nil, false))
			}
		}
		if candidate.FinishReason != "" {
			e.finishReason = geminiFinishReasonToOpenAI(candidate.FinishReason, e.toolIndex > 0)
		}
	}
	return out.Bytes()
}

// chunk 组装一个 OpenAI 的 chat.completion.chunk 分片。
//
// finishReason 为 nil 表示"尚未结束"；非 nil 时写入 finish_reason。
// withUsage 为 true 时附带 usage（OpenAI 的用法是单独一个带用量的事件）。
func (e *geminiStreamConverter) chunk(delta map[string]any, finishReason *string, withUsage bool) []byte {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason == nil {
		choice["finish_reason"] = nil
	} else {
		choice["finish_reason"] = *finishReason
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
func (e *geminiStreamConverter) finish() []byte {
	if e.done {
		return nil
	}
	e.done = true

	reason := e.finishReason
	if reason == "" {
		reason = "stop"
	}
	out := e.chunk(map[string]any{}, &reason, true)
	out = append(out, "data: [DONE]\n\n"...)
	return out
}

// errorOut 把上游的流式错误转换为 OpenAI 的错误分片并结束流。
func (e *geminiStreamConverter) errorOut(envelope geminiUpstreamError) []byte {
	if e.done {
		return nil
	}
	e.done = true

	message := "上游流式响应出错"
	if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
		message = envelope.Error.Message
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
