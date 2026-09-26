// 协议适配器（Anthropic / Gemini ↔ OpenAI）的转换测试。
//
// 意图（Why）：
//
//	协议转换是"用户能不能接入"的关键路径：字段名写错、事件序列缺一环，
//	客户端就会报出难以定位的解析错误（而网关侧日志一切正常）。
//	因此这里逐条锁定转换结果的结构，尤其是流式事件序列。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  └─ 直接调用适配器的 DecodeRequest / EncodeResponse / StreamEncoder
package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

/* ────────────────────── Anthropic 请求转换 ────────────────────── */

func TestAnthropic_DecodeRequest_基础字段与system(t *testing.T) {
	adapter := NewAnthropicAdapter()

	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"max_tokens": 1024,
		"temperature": 0.5,
		"stop_sequences": ["\n\n"],
		"system": "你是助手",
		"messages": [
			{"role": "user", "content": "你好"},
			{"role": "assistant", "content": [{"type": "text", "text": "你好，有什么可以帮你"}]}
		]
	}`)

	modelName, openAIBody, err := adapter.DecodeRequest(body, "/v1/messages")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if modelName != "claude-3-5-sonnet" {
		t.Fatalf("模型名应为 claude-3-5-sonnet，实际 %q", modelName)
	}

	var out map[string]any
	if err := json.Unmarshal(openAIBody, &out); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}

	messages, ok := out["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("应产出 3 条消息（system + 2 条对话），实际 %v", out["messages"])
	}
	first := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Fatalf("system 消息转换错误: %v", first)
	}
	if out["max_tokens"] != float64(1024) {
		t.Fatalf("max_tokens 未透传: %v", out["max_tokens"])
	}
	if out["temperature"] != 0.5 {
		t.Fatalf("temperature 未透传: %v", out["temperature"])
	}
	// stop_sequences → stop
	if _, exists := out["stop"]; !exists {
		t.Fatalf("stop_sequences 应映射为 stop，实际键集合：%v", keysOf(out))
	}
	if _, exists := out["stop_sequences"]; exists {
		t.Fatal("不应把 Anthropic 私有字段 stop_sequences 透传给上游")
	}
}

func TestAnthropic_DecodeRequest_system支持数组形态(t *testing.T) {
	adapter := NewAnthropicAdapter()
	body := []byte(`{
		"model": "m",
		"system": [{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}],
		"messages": [{"role":"user","content":"hi"}]
	}`)

	_, openAIBody, err := adapter.DecodeRequest(body, "/v1/messages")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var out struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(openAIBody, &out); err != nil {
		t.Fatalf("解析输出失败: %v", err)
	}
	if len(out.Messages) == 0 || out.Messages[0].Role != "system" {
		t.Fatalf("应产出 system 消息，实际 %+v", out.Messages)
	}
	if !strings.Contains(out.Messages[0].Content, "第一段") || !strings.Contains(out.Messages[0].Content, "第二段") {
		t.Fatalf("system 文本块未拼接完整: %q", out.Messages[0].Content)
	}
}

func TestAnthropic_DecodeRequest_工具声明与工具结果(t *testing.T) {
	adapter := NewAnthropicAdapter()
	body := []byte(`{
		"model": "m",
		"messages": [
			{"role": "user", "content": "北京天气"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "北京"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "晴 26℃"}
			]}
		],
		"tools": [{"name": "get_weather", "description": "查天气", "input_schema": {"type":"object","properties":{"city":{"type":"string"}}}}]
	}`)

	_, openAIBody, err := adapter.DecodeRequest(body, "/v1/messages")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}

	var out struct {
		Messages []map[string]any `json:"messages"`
		Tools    []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(openAIBody, &out); err != nil {
		t.Fatalf("解析输出失败: %v", err)
	}

	// 工具声明应转换为 OpenAI 的 function 形式
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" {
		t.Fatalf("工具声明转换错误: %+v", out.Tools)
	}
	if out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("工具名应为 get_weather，实际 %q", out.Tools[0].Function.Name)
	}
	if out.Tools[0].Function.Parameters["type"] != "object" {
		t.Fatalf("input_schema 应映射为 parameters：%+v", out.Tools[0].Function.Parameters)
	}

	// assistant 消息应带 tool_calls
	var foundToolCall, foundToolResult bool
	for _, msg := range out.Messages {
		if msg["role"] == "assistant" {
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) == 1 {
				foundToolCall = true
				call := calls[0].(map[string]any)
				if call["id"] != "toolu_1" {
					t.Fatalf("工具调用 ID 应为 toolu_1，实际 %v", call["id"])
				}
			}
		}
		// tool_result 必须变成独立的 role=tool 消息（OpenAI 的硬要求）
		if msg["role"] == "tool" {
			foundToolResult = true
			if msg["tool_call_id"] != "toolu_1" {
				t.Fatalf("tool_call_id 应为 toolu_1，实际 %v", msg["tool_call_id"])
			}
			if msg["content"] != "晴 26℃" {
				t.Fatalf("工具结果内容错误: %v", msg["content"])
			}
		}
	}
	if !foundToolCall {
		t.Fatal("assistant 消息缺少 tool_calls")
	}
	if !foundToolResult {
		t.Fatal("tool_result 未转换为 role=tool 消息")
	}
}

func TestAnthropic_EncodeResponse_文本与用量(t *testing.T) {
	adapter := NewAnthropicAdapter()

	upstream := []byte(`{
		"id": "chatcmpl-abc",
		"model": "gpt-4o",
		"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"你好"}}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19}
	}`)

	var out map[string]any
	if err := json.Unmarshal(adapter.EncodeResponse(upstream), &out); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}

	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("响应头部字段错误: %+v", out)
	}
	// id 必须以 msg_ 开头，否则部分 Anthropic 客户端会判定响应非法
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "msg_") {
		t.Fatalf("消息 id 应以 msg_ 开头，实际 %q", id)
	}
	content := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("应有一个内容块，实际 %v", content)
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "你好" {
		t.Fatalf("文本内容块错误: %+v", block)
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason 应为 end_turn，实际 %v", out["stop_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(7) {
		t.Fatalf("usage 字段映射错误（应为 input_tokens/output_tokens）: %+v", usage)
	}
}

func TestAnthropic_EncodeResponse_工具调用与stop_reason(t *testing.T) {
	adapter := NewAnthropicAdapter()

	upstream := []byte(`{
		"id": "chatcmpl-x",
		"model": "gpt-4o",
		"choices": [{"index":0,"finish_reason":"tool_calls","message":{
			"role":"assistant","content":"",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]
		}}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
	}`)

	var out map[string]any
	if err := json.Unmarshal(adapter.EncodeResponse(upstream), &out); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}

	if out["stop_reason"] != "tool_use" {
		t.Fatalf("有工具调用时 stop_reason 应为 tool_use，实际 %v", out["stop_reason"])
	}
	content := out["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" {
		t.Fatalf("内容块类型应为 tool_use，实际 %v", block["type"])
	}
	if block["name"] != "get_weather" {
		t.Fatalf("工具名错误: %v", block["name"])
	}
	// input 必须是对象（Anthropic 要求），而不是 JSON 字符串
	input, ok := block["input"].(map[string]any)
	if !ok {
		t.Fatalf("input 应为对象，实际 %T", block["input"])
	}
	if input["city"] != "北京" {
		t.Fatalf("input 内容错误: %+v", input)
	}
}

func TestAnthropic_EncodeError_错误格式(t *testing.T) {
	adapter := NewAnthropicAdapter()
	raw := []byte(`{"error":{"message":"密钥无效","type":"authentication_error"}}`)

	var out map[string]any
	if err := json.Unmarshal(adapter.EncodeError(401, raw), &out); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if out["type"] != "error" {
		t.Fatalf("错误响应 type 应为 error，实际 %v", out["type"])
	}
	inner := out["error"].(map[string]any)
	if inner["message"] != "密钥无效" {
		t.Fatalf("错误信息未透传: %v", inner["message"])
	}
	if inner["type"] != "authentication_error" {
		t.Fatalf("错误类型映射错误: %v", inner["type"])
	}
}

/* ────────────────────── Anthropic 流式转换 ────────────────────── */

func TestAnthropic_StreamEncoder_事件序列(t *testing.T) {
	adapter := NewAnthropicAdapter()
	encoder := adapter.NewStreamEncoder()
	if encoder == nil {
		t.Fatal("Anthropic 适配器应支持流式")
	}

	var output strings.Builder
	if begin := encoder.Begin(); len(begin) > 0 {
		output.Write(begin)
	}

	chunks := []string{
		`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
		`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"好"}}]}`,
		`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	}
	for _, chunk := range chunks {
		output.Write(encoder.Chunk([]byte(chunk)))
	}
	output.Write(encoder.End())

	text := output.String()

	// 必须按 Anthropic 的事件序列出现，顺序错误客户端会解析失败
	expectedOrder := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	lastIndex := -1
	for _, event := range expectedOrder {
		index := strings.Index(text, event)
		if index < 0 {
			t.Fatalf("缺少事件 %q，实际输出：\n%s", event, text)
		}
		if index < lastIndex {
			t.Fatalf("事件 %q 顺序错误，实际输出：\n%s", event, text)
		}
		lastIndex = index
	}

	// 增量文本必须原样出现
	if !strings.Contains(text, `"text":"你"`) || !strings.Contains(text, `"text":"好"`) {
		t.Fatalf("文本增量未正确下发：\n%s", text)
	}
	// stop_reason 必须在 message_delta 中给出
	if !strings.Contains(text, `"stop_reason":"end_turn"`) {
		t.Fatalf("message_delta 缺少 stop_reason：\n%s", text)
	}
	// 每个事件都必须同时带 event: 与 data: 行
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "event: ") {
			continue
		}
	}
	if strings.Count(text, "event: ") != strings.Count(text, "data: ") {
		t.Fatalf("event 与 data 行数不匹配：\n%s", text)
	}
}

func TestAnthropic_StreamEncoder_空上游也要给出完整序列(t *testing.T) {
	adapter := NewAnthropicAdapter()
	encoder := adapter.NewStreamEncoder()

	// 上游一个分片都没产出（例如立即断开）：仍须给出完整事件序列，
	// 否则客户端会一直等待（表现为"请求卡住"）
	out := string(encoder.End())
	for _, event := range []string{"message_start", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(out, event) {
			t.Fatalf("空上游也应输出 %s 事件，实际：\n%s", event, out)
		}
	}
}

/* ────────────────────── Gemini 转换 ────────────────────── */

func TestGemini_ParseAction(t *testing.T) {
	cases := []struct {
		path   string
		model  string
		stream bool
		ok     bool
	}{
		{"/v1beta/models/gemini-1.5-pro:generateContent", "gemini-1.5-pro", false, true},
		{"/v1beta/models/gemini-1.5-pro:streamGenerateContent", "gemini-1.5-pro", true, true},
		{"/v1beta/models/gemini-2.0-flash:generateContent", "gemini-2.0-flash", false, true},
		{"/v1/messages", "", false, false},
		{"/v1beta/models/:generateContent", "", false, false},
	}

	for _, tc := range cases {
		model, stream, ok := parseGeminiAction(tc.path)
		if ok != tc.ok {
			t.Errorf("路径 %q 的 ok 应为 %v", tc.path, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if model != tc.model || stream != tc.stream {
			t.Errorf("路径 %q 应解析为 (%s, stream=%v)，实际 (%s, stream=%v)",
				tc.path, tc.model, tc.stream, model, stream)
		}
	}
}

func TestGemini_DecodeRequest_结构映射(t *testing.T) {
	adapter := NewGeminiAdapter()

	body := []byte(`{
		"systemInstruction": {"parts": [{"text": "你是助手"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "你好"}]},
			{"role": "model", "parts": [{"text": "你好呀"}]}
		],
		"generationConfig": {"temperature": 0.3, "maxOutputTokens": 512, "stopSequences": ["END"]}
	}`)

	modelName, openAIBody, err := adapter.DecodeRequest(body, "/v1beta/models/gemini-1.5-pro:generateContent")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if modelName != "gemini-1.5-pro" {
		t.Fatalf("模型名应来自路径，实际 %q", modelName)
	}

	var out struct {
		Model       string  `json:"model"`
		MaxTokens   int     `json:"max_tokens"`
		Temperature float64 `json:"temperature"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(openAIBody, &out); err != nil {
		t.Fatalf("解析输出失败: %v", err)
	}

	if out.Model != "gemini-1.5-pro" {
		t.Fatalf("内部请求体的 model 应补全为路径中的模型名，实际 %q", out.Model)
	}
	if out.MaxTokens != 512 || out.Temperature != 0.3 {
		t.Fatalf("生成参数映射错误: max_tokens=%d temperature=%v", out.MaxTokens, out.Temperature)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("应产出 3 条消息（system + 2 条对话），实际 %d", len(out.Messages))
	}
	if out.Messages[0].Role != "system" {
		t.Fatalf("systemInstruction 应映射为 system 消息，实际 %q", out.Messages[0].Role)
	}
	// Gemini 的 model 角色必须映射为 assistant
	if out.Messages[2].Role != "assistant" {
		t.Fatalf("Gemini 的 model 角色应映射为 assistant，实际 %q", out.Messages[2].Role)
	}
}

func TestGemini_DecodeRequest_流式标志来自路径(t *testing.T) {
	adapter := NewGeminiAdapter()
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	_, openAIBody, err := adapter.DecodeRequest(body, "/v1beta/models/m:streamGenerateContent")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !adapter.StreamRequested(openAIBody) {
		t.Fatal("streamGenerateContent 路径应使内部请求带上 stream=true")
	}

	_, plainBody, err := adapter.DecodeRequest(body, "/v1beta/models/m:generateContent")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if adapter.StreamRequested(plainBody) {
		t.Fatal("generateContent 不应被判定为流式")
	}
}

func TestGemini_DecodeRequest_路径非法应报错(t *testing.T) {
	adapter := NewGeminiAdapter()
	if _, _, err := adapter.DecodeRequest([]byte(`{}`), "/v1/chat/completions"); err == nil {
		t.Fatal("非法路径应返回明确错误，而不是静默产生错误请求")
	}
}

func TestGemini_EncodeResponse_结构与用量(t *testing.T) {
	adapter := NewGeminiAdapter()

	upstream := []byte(`{
		"id":"chatcmpl-1","model":"gpt-4o",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"你好"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}
	}`)

	var out map[string]any
	if err := json.Unmarshal(adapter.EncodeResponse(upstream), &out); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}

	candidates := out["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("应有一个 candidate，实际 %d", len(candidates))
	}
	candidate := candidates[0].(map[string]any)
	if candidate["finishReason"] != "STOP" {
		t.Fatalf("finishReason 应映射为 STOP，实际 %v", candidate["finishReason"])
	}
	content := candidate["content"].(map[string]any)
	if content["role"] != "model" {
		t.Fatalf("角色应为 model，实际 %v", content["role"])
	}
	parts := content["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "你好" {
		t.Fatalf("文本内容错误: %v", parts)
	}

	usage := out["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != float64(10) || usage["candidatesTokenCount"] != float64(4) {
		t.Fatalf("usageMetadata 字段映射错误: %+v", usage)
	}
	if usage["totalTokenCount"] != float64(14) {
		t.Fatalf("totalTokenCount 应为 14，实际 %v", usage["totalTokenCount"])
	}
}

func TestGemini_EncodeResponse_长度截断映射(t *testing.T) {
	adapter := NewGeminiAdapter()
	upstream := []byte(`{
		"id":"x","model":"m",
		"choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"被截断"}}]
	}`)

	var out map[string]any
	_ = json.Unmarshal(adapter.EncodeResponse(upstream), &out)
	candidate := out["candidates"].([]any)[0].(map[string]any)
	if candidate["finishReason"] != "MAX_TOKENS" {
		t.Fatalf("length 应映射为 MAX_TOKENS，实际 %v", candidate["finishReason"])
	}
}

func TestGemini_EncodeError_格式(t *testing.T) {
	adapter := NewGeminiAdapter()
	raw := []byte(`{"error":{"message":"限流","type":"rate_limit_error"}}`)

	var out map[string]any
	if err := json.Unmarshal(adapter.EncodeError(429, raw), &out); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	inner := out["error"].(map[string]any)
	if inner["code"] != float64(429) {
		t.Fatalf("错误 code 应为 429，实际 %v", inner["code"])
	}
	if inner["status"] != "RESOURCE_EXHAUSTED" {
		t.Fatalf("429 应映射为 RESOURCE_EXHAUSTED，实际 %v", inner["status"])
	}
	if inner["message"] != "限流" {
		t.Fatalf("错误信息未透传: %v", inner["message"])
	}
}

func TestGemini_StreamEncoder_文本分片(t *testing.T) {
	adapter := NewGeminiAdapter()
	encoder := adapter.NewStreamEncoder()
	if encoder == nil {
		t.Fatal("Gemini 适配器应支持流式")
	}

	out := string(encoder.Chunk([]byte(`{"id":"1","model":"m","choices":[{"index":0,"delta":{"content":"你"}}]}`)))
	if !strings.HasPrefix(out, "data: ") {
		t.Fatalf("Gemini SSE 分片应以 data: 开头，实际 %q", out)
	}
	if !strings.Contains(out, `"text":"你"`) {
		t.Fatalf("文本未转换: %s", out)
	}
	if !strings.Contains(out, `"role":"model"`) {
		t.Fatalf("角色应为 model: %s", out)
	}
	// 纯工具参数增量不应下发（客户端无法消费半个 JSON）
	if out := encoder.Chunk([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{\"a\":"}}]}}]}`)); len(out) != 0 {
		t.Fatalf("工具参数增量不应立即下发，实际 %s", out)
	}
}

/* ────────────────────── 辅助 ────────────────────── */

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
