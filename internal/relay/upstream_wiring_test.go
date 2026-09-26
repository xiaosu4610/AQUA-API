// 上游"接线"的端到端单元测试：验证渠道的 type_key / extra_config 落库后，
// 转发层确实据此选到正确的协议规格并组装出正确的上游请求。
//
// 意图（Why）：
//
//	上一轮虽然实现了各协议的转换函数，但渠道表没有承载类型标识的地方，
//	转发层永远解析成 OpenAI 兼容，导致 Azure / Anthropic / Gemini 端到端打不通。
//	本测试钉住"接线"这一关键路径，且专门用一条 type_key 为空的渠道验证
//	历史渠道的 URL / 鉴权头 / 请求体与改造前【逐字节一致】——向后兼容是硬要求。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  └─ prepareChannelUpstream（forwardChat 实际调用的组装入口）
//	       ├─ 依 type_key 选规格（upstreamSpecForChannel）
//	       ├─ 转换请求体（encodeUpstreamRequestBody）
//	       └─ 组装 URL / 头（buildUpstreamRequest，含 extra_config 占位符与查询）
//
// 扩展（Extend）：
//
//	新增上游类型时，在此追加一条"该类型的关键差异"断言（路径 / 头 / 鉴权）。
package relay

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// TestPrepareChannelUpstream_Azure 验证 type_key=azure_openai 时：
// 部署名进路径、api-version 进查询、鉴权走 api-key 头。
func TestPrepareChannelUpstream_Azure(t *testing.T) {
	ch := &model.Channel{
		TypeKey: "azure_openai",
		BaseURL: "https://myres.openai.azure.com",
		ExtraConfig: map[string]string{
			"deployment":  "prod-deploy",
			"api_version": "2024-10-21",
		},
	}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	spec, outBody, built, err := prepareChannelUpstream(
		ch, "azure-secret", "gpt-4o", oai.ChatCompletionsPath, body, http.Header{}, false)
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	if spec.Protocol != channeltype.ProtocolAzure {
		t.Fatalf("spec.Protocol = %q，期望 azure", spec.Protocol)
	}
	if !strings.Contains(built.URL, "/openai/deployments/prod-deploy/chat/completions") {
		t.Errorf("URL = %q，期望部署名进路径", built.URL)
	}
	if !strings.HasSuffix(built.URL, "?api-version=2024-10-21") {
		t.Errorf("URL = %q，期望带 api-version 查询参数", built.URL)
	}
	if got := built.Header.Get("api-key"); got != "azure-secret" {
		t.Errorf("api-key = %q，期望 azure-secret", got)
	}
	// Azure 不改写请求体
	if !bytes.Equal(outBody, body) {
		t.Errorf("Azure 请求体不应被改写，实际 = %s", outBody)
	}
}

// TestPrepareChannelUpstream_Anthropic 验证 type_key=anthropic 时：
// 路径改写为 /messages、带 anthropic-version 头、鉴权走 x-api-key、请求体被转换。
func TestPrepareChannelUpstream_Anthropic(t *testing.T) {
	ch := &model.Channel{
		TypeKey: "anthropic",
		BaseURL: "https://api.anthropic.com/v1",
	}
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[
		{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)

	spec, outBody, built, err := prepareChannelUpstream(
		ch, "sk-anthropic", "claude-3-5-sonnet", oai.ChatCompletionsPath, body, http.Header{}, false)
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	if spec.Protocol != channeltype.ProtocolAnthropic {
		t.Fatalf("spec.Protocol = %q，期望 anthropic", spec.Protocol)
	}
	if built.URL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("URL = %q，期望 /v1/messages（不应出现 /v1/v1）", built.URL)
	}
	if got := built.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q，期望 2023-06-01", got)
	}
	if got := built.Header.Get("x-api-key"); got != "sk-anthropic" {
		t.Errorf("x-api-key = %q，期望 sk-anthropic", got)
	}
	// 请求体应被转换为 Anthropic Messages（system 是顶层字段）
	converted := decodeMap(t, outBody)
	if converted["system"] != "sys" {
		t.Errorf("转换后的请求体 system = %v，期望 sys", converted["system"])
	}
}

// TestPrepareChannelUpstream_Gemini 验证 type_key=gemini 时：
// 模型名与动作进路径、密钥进查询参数、请求体被转换为 contents/systemInstruction。
func TestPrepareChannelUpstream_Gemini(t *testing.T) {
	ch := &model.Channel{
		TypeKey: "gemini",
		BaseURL: "https://generativelanguage.googleapis.com",
	}
	body := []byte(`{"model":"gemini-1.5-pro","messages":[
		{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)

	spec, outBody, built, err := prepareChannelUpstream(
		ch, "gemini-secret", "gemini-1.5-pro", oai.ChatCompletionsPath, body, http.Header{}, false)
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	if spec.Protocol != channeltype.ProtocolGemini {
		t.Fatalf("spec.Protocol = %q，期望 gemini", spec.Protocol)
	}
	if !strings.Contains(built.URL, "/v1beta/models/gemini-1.5-pro:generateContent") {
		t.Errorf("URL = %q，期望模型名与动作进路径", built.URL)
	}
	if !strings.Contains(built.URL, "key=gemini-secret") {
		t.Errorf("URL = %q，期望密钥进查询参数", built.URL)
	}
	converted := decodeMap(t, outBody)
	if _, ok := converted["contents"]; !ok {
		t.Errorf("转换后的请求体缺少 contents: %v", converted)
	}
	if _, ok := converted["systemInstruction"]; !ok {
		t.Errorf("转换后的请求体缺少 systemInstruction: %v", converted)
	}
}

// TestPrepareChannelUpstream_空类型逐字节兼容 是本轮改造最重要的回归护栏。
//
// type_key 为空（历史渠道）时，最终 URL、Authorization 头与请求体必须与
// "改造前的朴素实现（base_url + path、Bearer 鉴权、原样转发）"完全一致；
// 同时验证指向未登记类型（理论不该出现）也会回退为 OpenAI 兼容而非报错。
func TestPrepareChannelUpstream_空类型逐字节兼容(t *testing.T) {
	const (
		apiKey = "sk-legacy-credential"
		base   = "https://api.legacy.example.com"
	)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	headers := http.Header{}
	headers.Set("Accept", "application/json")

	for _, typeKey := range []string{"", "not_a_registered_type"} {
		t.Run("type_key="+typeKey, func(t *testing.T) {
			ch := &model.Channel{TypeKey: typeKey, BaseURL: base}

			spec, outBody, built, err := prepareChannelUpstream(
				ch, apiKey, "gpt-4o", oai.ChatCompletionsPath, body, headers, oai.PeekStream(body))
			if err != nil {
				t.Fatalf("组装请求失败: %v", err)
			}

			if spec.Protocol != channeltype.ProtocolOpenAI {
				t.Errorf("spec.Protocol = %q，期望回退为 openai 兼容", spec.Protocol)
			}
			// 改造前的朴素实现就是 base_url + path
			if want := base + oai.ChatCompletionsPath; built.URL != want {
				t.Errorf("URL = %q，期望 %q（与改造前一致）", built.URL, want)
			}
			if got := built.Header.Get("Authorization"); got != "Bearer "+apiKey {
				t.Errorf("Authorization = %q，期望 Bearer 鉴权（与改造前一致）", got)
			}
			if got := built.Header.Get("Accept"); got != "application/json" {
				t.Errorf("Accept = %q，期望透传", got)
			}
			// 请求体逐字节不变：不做任何协议转换
			if !bytes.Equal(outBody, body) {
				t.Errorf("请求体被改动：\n实际 = %s\n期望 = %s", outBody, body)
			}
		})
	}
}
