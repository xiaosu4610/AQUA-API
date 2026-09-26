// 用量嗅探（usageSniffer）的单元测试。
//
// 意图（Why）：
//
//	usage 决定每次调用的计费。此前实现只保留响应前 256KB，而 OpenAI 兼容协议
//	把 usage 放在流的最后一个事件里，长回答（>256KB）末尾的 usage 会被丢弃，
//	导致该次调用被按 0 token 计费——直接造成收入流失。本文件用测试把这条
//	行为钉死：无论响应多长、usage 在第几个事件、接收被怎样切分，都必须能取到。
//
// 流转（Flow）：
//
//	go test ./internal/relay/ -run Usage
//	  ├─ 直接向 usageSniffer 写入（可控制分片边界）
//	  └─ 断言 Usage() 的结果与"内存有界"的内部状态
//
// 扩展（Extend）：
//
//	上游出现新的 usage 形态时，在 TestUsageSniffer_* 中补充一个用例；
//	若新增请求体改写逻辑，仿照 TestWithStreamUsageOption_InjectionRules 补测。
package relay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mustWrite 向嗅探器写入内容，写入失败即终止测试。
func mustWrite(t *testing.T, sniffer *usageSniffer, chunk string) {
	t.Helper()
	if _, err := sniffer.Write([]byte(chunk)); err != nil {
		t.Fatalf("写入嗅探器失败: %v", err)
	}
}

// TestUsageSniffer_LongStreamingResponse_LastUsageExtracted 是本 bug 的复现与回归。
//
// 构造一个总长度超过 1MiB 的 SSE 流，把含 usage 的事件放在【最后】，
// 按 32KiB 分片写入（模拟真实转发路径的拷贝缓冲），断言能取到末端的 usage。
// 旧实现（仅保留前 256KB）会在此用例中返回零值。
func TestUsageSniffer_LongStreamingResponse_LastUsageExtracted(t *testing.T) {
	// 每行正文约 200 字节，不断追加直到流长度超过 1MiB
	bodyChunk := `data: {"id":"c","choices":[{"delta":{"content":"` + strings.Repeat("x", 200) + `"}}]}` + "\n\n"

	var stream strings.Builder
	for stream.Len() <= 1<<20 {
		stream.WriteString(bodyChunk)
	}
	// usage 出现在最后一个数据事件（OpenAI 在流末尾才回报用量）
	stream.WriteString(`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":321,"completion_tokens":654,"total_tokens":975}}` + "\n\n")
	stream.WriteString("data: [DONE]\n\n")

	full := stream.String()
	if len(full) <= 1<<20 {
		t.Fatalf("用例构造的流长度 = %d 字节，期望 > 1MiB", len(full))
	}

	sniffer := newUsageSniffer()
	const chunkSize = 32 * 1024 // 与 flushCopy 的拷贝缓冲一致
	for start := 0; start < len(full); start += chunkSize {
		end := start + chunkSize
		if end > len(full) {
			end = len(full)
		}
		mustWrite(t, sniffer, full[start:end])
	}

	usage, ok := sniffer.Usage()
	if !ok {
		t.Fatal("未能从超过 1MiB 的流式响应中提取 usage：长回答末尾的用量被丢弃（计费会记 0）")
	}
	if usage.PromptTokens != 321 || usage.CompletionTokens != 654 || usage.TotalTokens != 975 {
		t.Errorf("usage = %+v，期望 prompt=321 completion=654 total=975", usage)
	}

	// 内存有界：解析器只应保留未解析完的尾部，不随响应总长度增长
	if len(sniffer.tail) > 64<<10 {
		t.Errorf("尾部缓冲 = %d 字节，期望保持在很小规模（内存未受控）", len(sniffer.tail))
	}
}

// TestUsageSniffer_MultipleUsages_KeepsLastNonEmpty 验证流中出现多次 usage 时取最后一次非空值。
func TestUsageSniffer_MultipleUsages_KeepsLastNonEmpty(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}` + "\n\n",
		// 中间的 null 占位与全零对象都应被忽略
		`data: {"choices":[{"delta":{}}],"usage":null}` + "\n\n",
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}` + "\n\n",
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")

	sniffer := newUsageSniffer()
	mustWrite(t, sniffer, stream)

	usage, ok := sniffer.Usage()
	if !ok {
		t.Fatal("应从多次 usage 中取到最后一个非空值")
	}
	if usage.PromptTokens != 20 || usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v，期望取最后一次非空值 prompt=20 completion=5", usage)
	}
}

// TestExtractUsage_NonStreamingJSONParsed 验证非流式响应（一次性 JSON）能正确解析。
func TestExtractUsage_NonStreamingJSONParsed(t *testing.T) {
	body := `{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`

	usage, ok := extractUsage([]byte(body))
	if !ok {
		t.Fatal("非流式响应应能解析出 usage")
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 7 || usage.TotalTokens != 19 {
		t.Errorf("usage = %+v，期望 prompt=12 completion=7 total=19", usage)
	}
}

// TestUsageSniffer_NoUsage_ZerosWithoutError 验证完全没有 usage 时返回零值且不报错。
func TestUsageSniffer_NoUsage_ZerosWithoutError(t *testing.T) {
	sniffer := newUsageSniffer()
	mustWrite(t, sniffer, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
	mustWrite(t, sniffer, "data: [DONE]\n\n")

	usage, ok := sniffer.Usage()
	if ok {
		t.Error("响应中不存在 usage，不应报告找到")
	}
	if usage != (openAIUsage{}) {
		t.Errorf("期望零值 usage，实际 %+v", usage)
	}
}

// TestUsageSniffer_ChunkedWrites_SplitObjectParsed 验证逐字节写入（标记与对象被最大程度拆散）仍能解析。
//
// 意义：真实转发以任意边界切分数据，标记 `"usage"` 与对象都可能被劈成两半；
// 解析器必须靠"保留未完成尾部"跨写入拼接，而不是假设一次写入就是完整事件。
func TestUsageSniffer_ChunkedWrites_SplitObjectParsed(t *testing.T) {
	full := `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}` + "\n\n"

	sniffer := newUsageSniffer()
	for i := 0; i < len(full); i++ {
		mustWrite(t, sniffer, full[i:i+1])
	}

	usage, ok := sniffer.Usage()
	if !ok {
		t.Fatal("标记与对象被逐字节拆散时仍应解析出 usage")
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 9 {
		t.Errorf("usage = %+v，期望 prompt=3 completion=9", usage)
	}
}

// TestUsageSniffer_AnthropicStyleFields_Normalized 验证 Anthropic 口径字段（input/output_tokens）被归一化。
func TestUsageSniffer_AnthropicStyleFields_Normalized(t *testing.T) {
	// 形态一：嵌套在 message.usage 中的 Anthropic 口径
	body := `{"type":"message","message":{"usage":{"input_tokens":40,"output_tokens":60}}}`

	usage, ok := extractUsage([]byte(body))
	if !ok {
		t.Fatal("Anthropic 风格的 message.usage 应能解析")
	}
	if usage.PromptTokens != 40 || usage.CompletionTokens != 60 || usage.TotalTokens != 100 {
		t.Errorf("usage = %+v，期望归一化为 prompt=40 completion=60 total=100", usage)
	}
}

// TestUsageSniffer_OversizedUnterminatedObject_TailStaysBounded 验证内存兜底：
// 遇到一个永远不会闭合的 usage 对象时，尾部缓冲不会无限增长，且之后仍能解析后续 usage。
func TestUsageSniffer_OversizedUnterminatedObject_TailStaysBounded(t *testing.T) {
	sniffer := newUsageSniffer()
	mustWrite(t, sniffer, `{"usage":{`)

	// 约 1.5MiB 的合法 JSON 片段，但整体永不闭合——解码会一直被判定为"截断"
	filler := strings.Repeat(`"k":"vvvvvvvv",`, 100000)
	mustWrite(t, sniffer, filler)

	if len(sniffer.tail) > usageTailMaxBytes {
		t.Errorf("尾部缓冲 = %d 字节，超过兜底上限 %d（内存未受控）", len(sniffer.tail), usageTailMaxBytes)
	}

	// 放弃超长垃圾对象后，仍应能解析随后真正完整的 usage
	mustWrite(t, sniffer, `,"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)

	usage, ok := sniffer.Usage()
	if !ok || usage.PromptTokens != 1 || usage.CompletionTokens != 2 {
		t.Errorf("跳过超长垃圾对象后未能解析后续 usage: %+v ok=%v", usage, ok)
	}
}

// streamOptionsProbe 用于断言请求体中的 stream_options.include_usage。
type streamOptionsProbe struct {
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// TestWithStreamUsageOption_InjectionRules 验证 stream_options 注入的边界。
//
// 关键约束：默认不注入（上游是否认识该字段不可控）；即便开启，也只对
// "未指定 stream_options 的流式请求"注入，绝不覆盖客户端的显式设置。
func TestWithStreamUsageOption_InjectionRules(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		enabled      bool
		wantInjected bool
	}{
		{
			name:    "开关关闭时原样返回",
			body:    `{"model":"gpt-4o","stream":true}`,
			enabled: false,
		},
		{
			name:         "开关开启且为流式请求时注入",
			body:         `{"model":"gpt-4o","stream":true}`,
			enabled:      true,
			wantInjected: true,
		},
		{
			name:    "非流式请求不注入",
			body:    `{"model":"gpt-4o","stream":false}`,
			enabled: true,
		},
		{
			name:    "未声明 stream 时不注入",
			body:    `{"model":"gpt-4o"}`,
			enabled: true,
		},
		{
			name:    "客户端已指定 stream_options 时不覆盖",
			body:    `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":false}}`,
			enabled: true,
		},
		{
			name:    "请求体非法时原样返回",
			body:    `{not json`,
			enabled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.body)
			got := withStreamUsageOption(original, tc.enabled)

			var probe streamOptionsProbe
			if err := json.Unmarshal(got, &probe); err != nil {
				if tc.enabled {
					// 非法 JSON 用例：应原样返回，不要求可解析
					if !bytes.Equal(got, original) {
						t.Errorf("非法请求体不应被改写，实际 = %s", got)
					}
					return
				}
				t.Fatalf("返回的请求体不是合法 JSON: %v", err)
			}

			if probe.StreamOptions.IncludeUsage != tc.wantInjected {
				t.Errorf("include_usage = %v，期望 %v（body=%s）",
					probe.StreamOptions.IncludeUsage, tc.wantInjected, got)
			}
			if !tc.wantInjected && !bytes.Equal(got, original) {
				t.Errorf("不应改写请求体，实际 = %s，期望 = %s", got, original)
			}
			// 注入时其余字段必须保留
			if tc.wantInjected {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(got, &fields); err != nil {
					t.Fatalf("注入后请求体非法: %v", err)
				}
				if string(fields["model"]) != `"gpt-4o"` {
					t.Errorf("注入后 model 字段丢失: %s", got)
				}
			}
		})
	}
}
