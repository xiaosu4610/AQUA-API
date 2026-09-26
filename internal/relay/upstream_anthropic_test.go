// Anthropic 上游适配（出站方向）的单元测试。
//
// 意图（Why）：
//
//	Anthropic 的 Messages 协议与 OpenAI 有多处硬性差异（system 是顶层字段、
//	max_tokens 必填、工具结果是 user 消息里的内容块…）。转换写错会直接导致
//	上游 400 或下游解析失败，因此这里对"请求体转换 / 非流式响应转换 /
//	流式事件序列 / 工具映射报错"逐项断言。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ encodeAnthropicRequest：请求体差异
//	  ├─ httptest：把转换后的请求真正发往假上游，断言路径 / 头 / body
//	  ├─ decodeAnthropicUpstreamResponse：非流式字段映射
//	  └─ wrapAnthropicUpstreamStream：SSE 事件 → OpenAI 分片
//
// 扩展（Extend）：
//
//	Anthropic 新增事件或内容块类型时，在此补一条断言；映射不了的情形
//	必须断言"明确报错"，避免实现退化为静默丢弃。
package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
)

// decodeMap 把 JSON 字节解析为 map，便于按字段断言。
func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("JSON 解析失败: %v，原文 = %s", err, string(raw))
	}
	return out
}

// anthropicSpec 是测试用的 Anthropic 类型规格。
func anthropicSpec(t *testing.T) channeltype.Type {
	t.Helper()
	return mustType(t, "anthropic")
}

// TestEncodeAnthropicRequest_请求体转换 覆盖 system 拆分、多段内容、参数映射与默认值。
func TestEncodeAnthropicRequest_请求体转换(t *testing.T) {
	body := []byte(`{
		"model":"claude-3-5-sonnet",
		"messages":[
			{"role":"system","content":"你是助手"},
			{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
			]}
		],
		"temperature":0.3,
		"top_p":0.9,
		"stop":["END"],
		"stream":true
	}`)

	encoded, err := encodeAnthropicRequest(body)
	if err != nil {
		t.Fatalf("请求转换失败: %v", err)
	}
	out := decodeMap(t, encoded)

	if out["system"] != "你是助手" {
		t.Errorf("system = %v，期望单独拎出为 %q", out["system"], "你是助手")
	}
	if got := out["max_tokens"]; got != float64(anthropicDefaultMaxTokens) {
		t.Errorf("max_tokens = %v，期望默认值 %d（Anthropic 必填）", got, anthropicDefaultMaxTokens)
	}
	if out["stream"] != true {
		t.Errorf("stream = %v，期望 true", out["stream"])
	}
	if out["temperature"] != 0.3 {
		t.Errorf("temperature = %v，期望 0.3", out["temperature"])
	}
	if out["top_p"] != 0.9 {
		t.Errorf("top_p = %v，期望 0.9", out["top_p"])
	}
	stops, _ := out["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop_sequences = %v，期望 [END]", out["stop_sequences"])
	}

	messages, _ := out["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages 长度 = %d，期望 1（system 已被拎出）", len(messages))
	}
	msg, _ := messages[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role = %v，期望 user", msg["role"])
	}
	blocks, _ := msg["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content 块数 = %d，期望 2（text + image）", len(blocks))
	}
	imageBlock, _ := blocks[1].(map[string]any)
	if imageBlock["type"] != "image" {
		t.Errorf("第二个内容块类型 = %v，期望 image", imageBlock["type"])
	}
	source, _ := imageBlock["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "AAAA" {
		t.Errorf("image.source = %v，期望 base64/image.png/AAAA", source)
	}
}

// TestEncodeAnthropicRequest_工具映射 覆盖正常映射与"映射不了就报错"。
func TestEncodeAnthropicRequest_工具映射(t *testing.T) {
	t.Run("function工具正常映射", func(t *testing.T) {
		body := []byte(`{
			"model":"claude-3-5-sonnet",
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"get_weather","description":"查天气",
				"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
		}`)
		encoded, err := encodeAnthropicRequest(body)
		if err != nil {
			t.Fatalf("工具映射失败: %v", err)
		}
		out := decodeMap(t, encoded)
		tools, _ := out["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools 长度 = %d，期望 1", len(tools))
		}
		tool, _ := tools[0].(map[string]any)
		if tool["name"] != "get_weather" {
			t.Errorf("tool.name = %v，期望 get_weather", tool["name"])
		}
		if _, ok := tool["input_schema"].(map[string]any); !ok {
			t.Errorf("tool.input_schema 缺失或结构错误: %v", tool["input_schema"])
		}
	})

	t.Run("无法映射的工具类型必须报错", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"code_interpreter"}]}`)
		if _, err := encodeAnthropicRequest(body); err == nil {
			t.Fatal("无法映射的工具类型应报错，而不是静默丢弃")
		}
	})

	t.Run("tool_choice=none必须报错", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`)
		if _, err := encodeAnthropicRequest(body); err == nil {
			t.Fatal("Anthropic 不支持 tool_choice=none，应报错而不是静默丢弃")
		}
	})

	t.Run("未知内容块类型必须报错", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","text":"x"}]}]}`)
		if _, err := encodeAnthropicRequest(body); err == nil {
			t.Fatal("未知内容块类型应报错，而不是静默丢弃")
		}
	})
}

// TestEncodeAnthropicRequest_tool结果合并 验证多条工具结果合并进一条 user 消息。
func TestEncodeAnthropicRequest_tool结果合并(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"结果1"},
			{"role":"tool","tool_call_id":"call_2","content":"结果2"}
		]
	}`)

	encoded, err := encodeAnthropicRequest(body)
	if err != nil {
		t.Fatalf("请求转换失败: %v", err)
	}
	messages, _ := decodeMap(t, encoded)["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages 长度 = %d，期望 2（assistant + 合并后的工具结果）", len(messages))
	}
	toolMsg, _ := messages[1].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Errorf("工具结果消息 role = %v，期望 user", toolMsg["role"])
	}
	blocks, _ := toolMsg["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("工具结果块数 = %d，期望 2", len(blocks))
	}
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "tool_result" || first["tool_use_id"] != "call_1" {
		t.Errorf("首个工具结果块 = %v，期望 tool_result/call_1", first)
	}
}

// TestEncodeAnthropicRequest_实际发送 用 httptest 假上游断言"真正发出的请求"。
func TestEncodeAnthropicRequest_实际发送(t *testing.T) {
	var (
		gotPath    string
		gotKey     string
		gotVersion string
		gotBody    []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	spec := anthropicSpec(t)
	openAIBody := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)

	outBody, err := encodeUpstreamRequestBody(spec, openAIBody)
	if err != nil {
		t.Fatalf("请求转换失败: %v", err)
	}
	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type: spec,
		// 模拟真实约定：Anthropic 的默认地址已含 /v1，故这里也带上 /v1，
		// 以验证最终路径为 /v1/messages 而不是重复的 /v1/v1/messages。
		BaseURL: upstream.URL + "/v1",
		APIKey:  "sk-anthropic",
		Model:   "claude-3-5-sonnet",
		Path:    "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, built.URL, strings.NewReader(string(outBody)))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header = built.Header

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	// 路径被改写为 /messages（BaseURL 已含 /v1，不应重复版本段）
	if gotPath != "/v1/messages" {
		t.Errorf("上游收到的路径 = %q，期望 /v1/messages", gotPath)
	}
	if gotKey != "sk-anthropic" {
		t.Errorf("上游收到的 x-api-key = %q", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("上游收到的 anthropic-version = %q，期望 2023-06-01", gotVersion)
	}

	sent := decodeMap(t, gotBody)
	if sent["system"] != "sys" {
		t.Errorf("上游收到的 system = %v，期望 sys", sent["system"])
	}
	if sent["max_tokens"] != float64(anthropicDefaultMaxTokens) {
		t.Errorf("上游收到的 max_tokens = %v，期望默认值 %d", sent["max_tokens"], anthropicDefaultMaxTokens)
	}
}

// TestDecodeAnthropicUpstreamResponse_字段映射 验证非流式响应转换为 OpenAI 响应。
func TestDecodeAnthropicUpstreamResponse_字段映射(t *testing.T) {
	body := []byte(`{
		"id":"msg_abc","type":"message","model":"claude-3-5-sonnet","stop_reason":"end_turn",
		"content":[{"type":"text","text":"你好"}],
		"usage":{"input_tokens":12,"output_tokens":7}
	}`)

	out := decodeMap(t, decodeAnthropicUpstreamResponse(body))

	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 长度 = %d，期望 1", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "你好" {
		t.Errorf("message.content = %v，期望 你好", message["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v，期望 stop", choice["finish_reason"])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(7) {
		t.Errorf("usage = %v，期望 input/output → prompt/completion", usage)
	}
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %q，期望 chatcmpl- 前缀", id)
	}
}

// TestDecodeAnthropicUpstreamResponse_工具调用 验证 tool_use 块映射为 tool_calls。
func TestDecodeAnthropicUpstreamResponse_工具调用(t *testing.T) {
	body := []byte(`{
		"id":"msg_t","model":"claude-3-5-sonnet","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}],
		"usage":{"input_tokens":3,"output_tokens":4}
	}`)

	out := decodeMap(t, decodeAnthropicUpstreamResponse(body))
	choice, _ := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v，期望 tool_calls", choice["finish_reason"])
	}
	message, _ := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 长度 = %d，期望 1", len(calls))
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v，期望 get_weather", fn["name"])
	}
	if args, _ := fn["arguments"].(string); !strings.Contains(args, "北京") {
		t.Errorf("function.arguments = %q，期望含 北京", args)
	}
}

// TestDecodeAnthropicUpstreamError_错误改写 验证错误体被改写为 OpenAI 错误体。
func TestDecodeAnthropicUpstreamError_错误改写(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	out := decodeMap(t, decodeAnthropicUpstreamError(body, http.StatusUnauthorized))

	errObj, _ := out["error"].(map[string]any)
	if errObj["type"] != "authentication_error" {
		t.Errorf("error.type = %v，期望 authentication_error", errObj["type"])
	}
	if errObj["message"] != "invalid x-api-key" {
		t.Errorf("error.message = %v，期望保留上游原文", errObj["message"])
	}
}

// TestWrapAnthropicUpstreamStream_事件序列 验证 SSE 事件被转换为 OpenAI 分片。
func TestWrapAnthropicUpstreamStream_事件序列(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-3-5-sonnet","usage":{"input_tokens":10}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	raw, err := io.ReadAll(wrapAnthropicUpstreamStream(io.NopCloser(strings.NewReader(sse))))
	if err != nil {
		t.Fatalf("读取转换后的流失败: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, `"role":"assistant"`) {
		t.Errorf("缺少 role=assistant 的首个分片:\n%s", text)
	}
	if !strings.Contains(text, `"content":"你好"`) {
		t.Errorf("缺少文本增量分片:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason=stop 的结束分片:\n%s", text)
	}
	if !strings.Contains(text, `"prompt_tokens":10`) || !strings.Contains(text, `"completion_tokens":5`) {
		t.Errorf("结束分片缺少用量映射（input/output → prompt/completion）:\n%s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Errorf("流未以 data: [DONE] 收尾:\n%s", text)
	}

	// 事件序列正确性：role 分片必须早于文本分片，结束分片在最后
	rolePos := strings.Index(text, `"role":"assistant"`)
	textPos := strings.Index(text, `"content":"你好"`)
	donePos := strings.Index(text, "data: [DONE]")
	if !(rolePos < textPos && textPos < donePos) {
		t.Errorf("分片顺序错误: role=%d text=%d done=%d", rolePos, textPos, donePos)
	}
}

// TestWrapAnthropicUpstreamStream_工具调用 验证工具增量被转换为 tool_calls 分片。
func TestWrapAnthropicUpstreamStream_工具调用(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_2","model":"claude-3-5-sonnet","usage":{"input_tokens":8}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":":\"北京\"}"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	raw, err := io.ReadAll(wrapAnthropicUpstreamStream(io.NopCloser(strings.NewReader(sse))))
	if err != nil {
		t.Fatalf("读取转换后的流失败: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, `"id":"toolu_1"`) || !strings.Contains(text, `"name":"get_weather"`) {
		t.Errorf("缺少工具调用起始分片:\n%s", text)
	}
	if !strings.Contains(text, `"arguments":"{\"city\""`) {
		t.Errorf("缺少首段工具参数增量:\n%s", text)
	}
	if !strings.Contains(text, `"arguments":":\"北京\"}"`) {
		t.Errorf("缺少后续工具参数增量:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Errorf("结束分片 finish_reason 应为 tool_calls:\n%s", text)
	}
}
