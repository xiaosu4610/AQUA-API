// Gemini 上游适配（出站方向）的单元测试。
//
// 意图（Why）：
//
//	Gemini 与 OpenAI 有多处硬性差异（system 是顶层 systemInstruction、角色叫
//	model 而非 assistant、模型名与动作在 URL 路径里、密钥走查询参数、流式是
//	整段 JSON 而非 delta 分片）。转换写错会直接导致上游 400/404 或下游解析失败，
//	因此这里对"请求体转换 / 实际发出的请求 / 非流式响应 / 流式事件序列 /
//	工具映射报错"逐项断言。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ encodeGeminiRequest：请求体差异（systemInstruction / 角色 / 生成参数）
//	  ├─ httptest：把转换后的请求真正发往假上游，断言路径 / 查询 / body
//	  ├─ decodeGeminiUpstreamResponse：非流式字段映射（含 usage）
//	  └─ wrapGeminiUpstreamStream：Gemini SSE → OpenAI 分片
//
// 扩展（Extend）：
//
//	Gemini 新增 parts 类型或事件时，在此补一条断言；映射不了的情形必须断言
//	"明确报错"，避免实现退化为静默丢弃。
package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
)

// geminiSpec 是测试用的 Gemini 类型规格。
func geminiSpec(t *testing.T) channeltype.Type {
	t.Helper()
	return mustType(t, "gemini")
}

// TestEncodeGeminiRequest_请求体转换 覆盖 system 拆分、角色映射与生成参数映射。
func TestEncodeGeminiRequest_请求体转换(t *testing.T) {
	body := []byte(`{
		"model":"gemini-1.5-pro",
		"messages":[
			{"role":"system","content":"你是助手"},
			{"role":"user","content":"你好"},
			{"role":"assistant","content":"在的"}
		],
		"temperature":0.3,
		"top_p":0.9,
		"max_tokens":256,
		"stop":["END"]
	}`)

	encoded, err := encodeGeminiRequest(body)
	if err != nil {
		t.Fatalf("请求转换失败: %v", err)
	}
	out := decodeMap(t, encoded)

	// system 被拎成顶层 systemInstruction
	system, _ := out["systemInstruction"].(map[string]any)
	if system == nil {
		t.Fatalf("缺少 systemInstruction: %v", out)
	}
	systemParts, _ := system["parts"].([]any)
	if len(systemParts) != 1 {
		t.Fatalf("systemInstruction.parts 长度 = %d，期望 1", len(systemParts))
	}
	if first, _ := systemParts[0].(map[string]any); first["text"] != "你是助手" {
		t.Errorf("systemInstruction.parts[0].text = %v，期望 你是助手", first["text"])
	}

	contents, _ := out["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents 长度 = %d，期望 2（system 已被拎出）", len(contents))
	}
	userContent, _ := contents[0].(map[string]any)
	if userContent["role"] != "user" {
		t.Errorf("contents[0].role = %v，期望 user", userContent["role"])
	}
	// assistant 必须映射为 model
	assistantContent, _ := contents[1].(map[string]any)
	if assistantContent["role"] != "model" {
		t.Errorf("contents[1].role = %v，期望 model（Gemini 用 model 表示助手）", assistantContent["role"])
	}
	assistantParts, _ := assistantContent["parts"].([]any)
	if first, _ := assistantParts[0].(map[string]any); first["text"] != "在的" {
		t.Errorf("contents[1].parts[0].text = %v，期望 在的", first["text"])
	}

	gen, _ := out["generationConfig"].(map[string]any)
	if gen == nil {
		t.Fatalf("缺少 generationConfig: %v", out)
	}
	if gen["maxOutputTokens"] != float64(256) {
		t.Errorf("generationConfig.maxOutputTokens = %v，期望 256", gen["maxOutputTokens"])
	}
	if gen["temperature"] != 0.3 || gen["topP"] != 0.9 {
		t.Errorf("generationConfig 温度/采样参数 = %v", gen)
	}
	stops, _ := gen["stopSequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("generationConfig.stopSequences = %v，期望 [END]", gen["stopSequences"])
	}
}

// TestEncodeGeminiRequest_工具映射 覆盖正常映射与"映射不了就报错"。
func TestEncodeGeminiRequest_工具映射(t *testing.T) {
	t.Run("function工具与tool_choice正常映射", func(t *testing.T) {
		body := []byte(`{
			"model":"gemini-1.5-pro",
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"get_weather","description":"查天气",
				"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
			"tool_choice":"auto"
		}`)
		encoded, err := encodeGeminiRequest(body)
		if err != nil {
			t.Fatalf("工具映射失败: %v", err)
		}
		out := decodeMap(t, encoded)

		tools, _ := out["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools 长度 = %d，期望 1", len(tools))
		}
		tool, _ := tools[0].(map[string]any)
		decls, _ := tool["functionDeclarations"].([]any)
		if len(decls) != 1 {
			t.Fatalf("functionDeclarations 长度 = %d，期望 1", len(decls))
		}
		decl, _ := decls[0].(map[string]any)
		if decl["name"] != "get_weather" {
			t.Errorf("functionDeclaration.name = %v，期望 get_weather", decl["name"])
		}
		config, _ := out["toolConfig"].(map[string]any)
		fcc, _ := config["functionCallingConfig"].(map[string]any)
		if fcc["mode"] != "AUTO" {
			t.Errorf("toolConfig.mode = %v，期望 AUTO", fcc["mode"])
		}
	})

	t.Run("无法映射的工具类型必须报错", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"code_interpreter"}]}`)
		if _, err := encodeGeminiRequest(body); err == nil {
			t.Fatal("无法映射的工具类型应报错，而不是静默丢弃")
		}
	})

	t.Run("未知内容块类型必须报错", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","text":"x"}]}]}`)
		if _, err := encodeGeminiRequest(body); err == nil {
			t.Fatal("未知内容块类型应报错，而不是静默丢弃")
		}
	})

	t.Run("工具结果缺少函数名必须报错", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[
			{"role":"tool","tool_call_id":"unknown","content":"结果"}]}`)
		if _, err := encodeGeminiRequest(body); err == nil {
			t.Fatal("工具结果映射不到函数名时应报错，而不是静默丢弃")
		}
	})
}

// TestEncodeGeminiRequest_实际发送 用 httptest 假上游断言"真正发出的请求"。
//
// 重点验证三件事：路径含 :generateContent、密钥落在查询参数、body 的
// contents/role/systemInstruction 映射正确。流式另验路径与 alt=sse。
func TestEncodeGeminiRequest_实际发送(t *testing.T) {
	var (
		gotPath  string
		gotQuery string
		gotBody  []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	spec := geminiSpec(t)
	openAIBody := []byte(`{"model":"gemini-1.5-pro","messages":[
		{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)

	outBody, err := encodeUpstreamRequestBody(spec, openAIBody)
	if err != nil {
		t.Fatalf("请求转换失败: %v", err)
	}

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: upstream.URL,
		APIKey:  "gemini-secret",
		Model:   "gemini-1.5-pro",
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

	if gotPath != "/v1beta/models/gemini-1.5-pro:generateContent" {
		t.Errorf("上游收到的路径 = %q，期望含 :generateContent", gotPath)
	}
	if !strings.Contains(gotQuery, "key=gemini-secret") {
		t.Errorf("上游收到的查询 = %q，期望密钥走查询参数 key=", gotQuery)
	}

	sent := decodeMap(t, gotBody)
	contents, _ := sent["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("上游收到的 contents 长度 = %d，期望 1", len(contents))
	}
	content, _ := contents[0].(map[string]any)
	if content["role"] != "user" {
		t.Errorf("上游收到的 role = %v，期望 user", content["role"])
	}
	system, _ := sent["systemInstruction"].(map[string]any)
	if system == nil {
		t.Errorf("上游收到的 body 缺少 systemInstruction: %v", sent)
	}

	// 流式请求应改写为 streamGenerateContent 并带 alt=sse
	streamBuilt, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: upstream.URL,
		APIKey:  "gemini-secret",
		Model:   "gemini-1.5-pro",
		Path:    "/v1/chat/completions",
		Stream:  true,
	})
	if err != nil {
		t.Fatalf("组装流式请求失败: %v", err)
	}
	if !strings.Contains(streamBuilt.URL, ":streamGenerateContent") {
		t.Errorf("流式 URL = %q，期望含 :streamGenerateContent", streamBuilt.URL)
	}
	if !strings.Contains(streamBuilt.URL, "alt=sse") {
		t.Errorf("流式 URL = %q，期望带 alt=sse", streamBuilt.URL)
	}
}

// TestDecodeGeminiUpstreamResponse_字段映射 验证非流式响应与 usage 转换。
func TestDecodeGeminiUpstreamResponse_字段映射(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":7,"totalTokenCount":19},
		"modelVersion":"gemini-1.5-pro"
	}`)

	out := decodeMap(t, decodeGeminiUpstreamResponse(body))

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
		t.Errorf("usage = %v，期望 promptTokenCount/candidatesTokenCount → prompt/completion", usage)
	}
}

// TestDecodeGeminiUpstreamResponse_工具调用 验证 functionCall 映射为 tool_calls。
func TestDecodeGeminiUpstreamResponse_工具调用(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"北京"}}}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4}
	}`)

	out := decodeMap(t, decodeGeminiUpstreamResponse(body))
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

// TestDecodeGeminiUpstreamError_错误改写 验证错误体被改写为 OpenAI 错误体。
func TestDecodeGeminiUpstreamError_错误改写(t *testing.T) {
	body := []byte(`{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}`)
	out := decodeMap(t, decodeGeminiUpstreamError(body, http.StatusUnauthorized))

	errObj, _ := out["error"].(map[string]any)
	if errObj["type"] != "authentication_error" {
		t.Errorf("error.type = %v，期望 authentication_error", errObj["type"])
	}
	if errObj["message"] != "API key not valid" {
		t.Errorf("error.message = %v，期望保留上游原文", errObj["message"])
	}
}

// TestWrapGeminiUpstreamStream_事件序列 验证 Gemini SSE 被转换为 OpenAI 分片。
func TestWrapGeminiUpstreamStream_事件序列(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}],"index":0},"finishReason":""}],"modelVersion":"gemini-1.5-pro"}`,
		"",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"，世界"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`,
		"",
	}, "\n")

	raw, err := io.ReadAll(wrapGeminiUpstreamStream(io.NopCloser(strings.NewReader(sse))))
	if err != nil {
		t.Fatalf("读取转换后的流失败: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, `"role":"assistant"`) {
		t.Errorf("缺少 role=assistant 的首个分片:\n%s", text)
	}
	if !strings.Contains(text, `"content":"你好"`) || !strings.Contains(text, `"content":"，世界"`) {
		t.Errorf("缺少文本增量分片:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason=stop 的结束分片:\n%s", text)
	}
	if !strings.Contains(text, `"prompt_tokens":10`) || !strings.Contains(text, `"completion_tokens":5`) {
		t.Errorf("结束分片缺少用量映射:\n%s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Errorf("流未以 data: [DONE] 收尾:\n%s", text)
	}

	// 顺序正确性：role 分片必须早于文本分片，[DONE] 在最后
	rolePos := strings.Index(text, `"role":"assistant"`)
	textPos := strings.Index(text, `"content":"你好"`)
	donePos := strings.Index(text, "data: [DONE]")
	if !(rolePos < textPos && textPos < donePos) {
		t.Errorf("分片顺序错误: role=%d text=%d done=%d", rolePos, textPos, donePos)
	}
}

// TestWrapGeminiUpstreamStream_工具调用 验证 functionCall 被转换为 tool_calls 分片。
func TestWrapGeminiUpstreamStream_工具调用(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"北京"}}}]},"finishReason":"STOP"}]}`,
		"",
	}, "\n")

	raw, err := io.ReadAll(wrapGeminiUpstreamStream(io.NopCloser(strings.NewReader(sse))))
	if err != nil {
		t.Fatalf("读取转换后的流失败: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, `"name":"get_weather"`) {
		t.Errorf("缺少工具调用分片:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Errorf("结束分片 finish_reason 应为 tool_calls:\n%s", text)
	}
}
