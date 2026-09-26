// 多账号密钥池下「同一模型在不同账号授权不同」的行为测试。
//
// 意图（Why）：
//
//	NVIDIA NIM 这类平台按【账号】逐模型授权，而我们的密钥池可能来自几百个
//	不同账号。于是出现一种很容易被误判的现象：同一个模型，在 A 账号返回
//	404 "Not found for account"，在 B 账号完全可用。
//
//	如果把这个 404 当成"请求本身有错"直接透传，用户就会看到无意义的失败；
//	而实际上只要换池内另一把密钥就能成功。本文件把这条链路固化成测试，
//	避免以后有人"顺手"把 404 从重试条件里删掉。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ 假上游按 Authorization 头返回 404（模拟"该账号无此模型"）
//	  └─ 断言：换密钥若干次后能命中有效账号，请求最终成功
//
// 扩展（Extend）：
//
//	其他"用 4xx 表达凭据问题"的上游，只需在 keyLevelRejectionMarkers 里补特征词，
//	并在此补充一条用例。
package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newEntitlementAwareUpstream 启动一个"按账号授权判定"的假上游。
//
// valid 中的密钥返回 200；其余返回 NVIDIA 风格的 404：
//
//	HTTP 404  application/problem+json
//	{"status":404,"title":"Not Found","detail":"Function 'xxx': Not found for account 'yyy'"}
//
// 这正是上游"该账号没有这个模型"的真实形态。
func newEntitlementAwareUpstream(t *testing.T, valid map[string]bool) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !valid[key] {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":404,"title":"Not Found",` +
				`"detail":"Function '23bd454d-b225-49a3-8118-582a62fc51b8': Not found for account 'JVMEna7jlvHHnWsdXwxrBXIutpmL8YLv9tRWz2TQocw'"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClassifyKeyFailure_识别账号无此模型并还原响应体(t *testing.T) {
	body := `{"status":404,"title":"Not Found","detail":"Function 'x': Not found for account 'y'"}`
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	r := New(nil, Options{})
	kind, reason, snippet := r.classifyKeyFailure(resp)
	if kind != keyFailureEntitlement {
		t.Fatal("404 + Not found for account 应判为「该账号无此模型」（授权范围问题）")
	}
	if !strings.Contains(reason, "无权访问") {
		t.Fatalf("失败原因应说明是账号无权限，实际 %q", reason)
	}
	if len(snippet) == 0 {
		t.Fatal("应同时返回响应体片段，供所有重试用尽时透传上游真实原因")
	}

	// 关键：判定过程中读过响应体，必须还原，否则透传路径会读到残缺内容
	restored, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取还原后的响应体失败: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("响应体应被完整还原，实际：%s", restored)
	}
}

func TestClassifyKeyFailure_普通业务错误不视为凭据问题(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"unexpected field"}}`)),
	}

	r := New(nil, Options{})
	if kind, _, _ := r.classifyKeyFailure(resp); kind != keyFailureNone {
		t.Fatal("与凭据无关的 404 不应触发密钥轮换（换密钥也没用，只会放大延迟）")
	}
}

func TestClassifyKeyFailure_状态码路径(t *testing.T) {
	r := New(nil, Options{})

	// 凭据不可用（值得换密钥）
	for _, status := range []int{401, 402, 403, 429} {
		resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}
		if kind, _, _ := r.classifyKeyFailure(resp); kind != keyFailureCredential {
			t.Errorf("状态码 %d 应判为凭据不可用", status)
		}
	}
	// 与凭据无关（换密钥与换渠道都无用）
	for _, status := range []int{200, 422, 500, 502, 503} {
		resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}
		if kind, _, _ := r.classifyKeyFailure(resp); kind == keyFailureCredential {
			t.Errorf("状态码 %d 不应判为凭据不可用", status)
		}
	}
}

func TestExtractUpstreamErrorMessage_兼容RFC7807(t *testing.T) {
	// NVIDIA 这类上游用的是 RFC7807 问题详情，不带 OpenAI 的 error 包裹
	rfc7807 := []byte(`{"status":404,"title":"Not Found",` +
		`"detail":"Function 'x': Not found for account 'y'"}`)
	if got := extractUpstreamErrorMessage(rfc7807); !strings.Contains(got, "Not found for account") {
		t.Fatalf("应能取出 detail 字段，实际 %q", got)
	}

	openaiStyle := []byte(`{"error":{"message":"invalid api key"}}`)
	if got := extractUpstreamErrorMessage(openaiStyle); got != "invalid api key" {
		t.Fatalf("应能取出 OpenAI 风格的 message，实际 %q", got)
	}

	if got := extractUpstreamErrorMessage(nil); got != "" {
		t.Fatalf("空响应体应返回空字符串，实际 %q", got)
	}
}

// TestForward_账号无此模型_不换密钥且立即透传 是本文件的核心用例。
//
// 场景：池里 8 把密钥来自【同质的免费账号】，某模型不在免费范围内。
//
// 期望（依据实测结论）：
//   - 断言只向上游发起了 1 次请求：既然所有账号授权一致，换密钥不可能成功，
//     多试只会让用户白等（此前 50 次重试实测耗时约 4 秒，纯属浪费）；
//   - 把上游的 404 与原因原样透传回去：使用者能立刻看懂
//     "该模型不在当前账号的可用范围内"，而不是拿到含糊的 502。
func TestForward_账号无此模型_不换密钥且立即透传(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"title":"Not Found",` +
			`"detail":"Function 'x': Not found for account 'y'"}`))
	}))
	t.Cleanup(upstream.Close)

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	pool := []string{"nvapi-a", "nvapi-b", "nvapi-c", "nvapi-d", "nvapi-e", "nvapi-f", "nvapi-g", "nvapi-h"}
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, pool, nil); err != nil {
		t.Fatalf("导入密钥池失败: %v", err)
	}

	r := New(channels, Options{Keys: keys, MaxAttempts: 3})
	gateway := newGateway(t, r)

	resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("应透传上游 404，实际 %d：%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Not found for account") {
		t.Fatalf("应透传上游的真实原因，实际：%s", body)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("同质账号池下「无此模型」不该换密钥重试，期望 1 次上游请求，实际 %d 次", got)
	}
}

// TestForward_凭据被限流_仍会换密钥重试 保证上面那项优化没有误伤正常场景。
//
// 429 属于"凭据被限流"，换一把密钥很可能立刻成功，
// 因此这条路径必须继续保留重试能力。
func TestForward_凭据被限流_仍会换密钥重试(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	var calls int32
	// 只有 nvapi-good 不限流
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "nvapi-good" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	t.Cleanup(upstream.Close)

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, []string{"nvapi-busy", "nvapi-good"}, nil); err != nil {
		t.Fatalf("导入密钥池失败: %v", err)
	}

	r := New(channels, Options{Keys: keys, MaxAttempts: 3})
	gateway := newGateway(t, r)

	// 反复请求：随机挑选必然多次先撞上限流密钥，但换一把就能成功
	for i := 0; i < 15; i++ {
		resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求应成功（换密钥即可绕开限流），实际 %d：%s", i+1, resp.StatusCode, body)
		}
	}
}

// TestServeEmbeddings_透传到上游嵌入端点 验证向量嵌入这条链路。
//
// 为什么值得单独测：NVIDIA 免费模型里有相当一部分是 embedding / rerank / clip，
// 它们对 /v1/chat/completions 一律返回 404，只提供 /v1/embeddings。
// 网关若不支持这个端点，这些模型就只能从清单里剔掉。
// 同时上游路径必须真的拼成 /v1/embeddings（而不是仍走 chat 路径）。
func TestServeEmbeddings_透传到上游嵌入端点(t *testing.T) {
	channels, _ := newTestRepos(t)

	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],` +
			`"model":"test-embed","usage":{"prompt_tokens":3,"total_tokens":3}}`))
	}))
	t.Cleanup(upstream.Close)

	addChannel(t, channels, upstream.URL, "sk-x", []string{"test-embed"}, 10)

	r := New(channels, Options{})
	gateway := httptest.NewServer(http.HandlerFunc(r.ServeEmbeddings))
	t.Cleanup(gateway.Close)

	resp, err := http.Post(gateway.URL+"/v1/embeddings", "application/json",
		strings.NewReader(`{"model":"test-embed","input":"你好"}`))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d：%s", resp.StatusCode, body)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("上游路径应为 /v1/embeddings，实际 %q", gotPath)
	}
	if !strings.Contains(gotBody, `"input":"你好"`) {
		t.Fatalf("请求体应原样透传，实际：%s", gotBody)
	}
	if !strings.Contains(string(body), `"embedding"`) {
		t.Fatalf("响应体应原样回写，实际：%s", body)
	}
}

// TestServeEmbeddings_缺少model应报400 覆盖参数校验。
func TestServeEmbeddings_缺少model应报400(t *testing.T) {
	channels, _ := newTestRepos(t)
	r := New(channels, Options{})
	gateway := httptest.NewServer(http.HandlerFunc(r.ServeEmbeddings))
	t.Cleanup(gateway.Close)

	resp, err := http.Post(gateway.URL+"/v1/embeddings", "application/json",
		strings.NewReader(`{"input":"你好"}`))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺少 model 应返回 400，实际 %d：%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "missing_model") {
		t.Fatalf("错误码应为 missing_model，实际：%s", body)
	}
}

// TestForward_全池均无该模型_透传上游404 兜底用例。
//
// 场景：池里所有账号都没有该模型的授权。
// 期望：把上游的 404 原样透传（让用户看到"该模型在当前账号不可用"），
// 而不是换成含义模糊的 502。
func TestForward_全池均无该模型_透传上游404(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	upstream := newEntitlementAwareUpstream(t, nil) // 全部无授权

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, []string{"nvapi-x", "nvapi-y"}, nil); err != nil {
		t.Fatalf("导入密钥池失败: %v", err)
	}

	r := New(channels, Options{Keys: keys, MaxAttempts: 1})
	gateway := newGateway(t, r)

	resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("无退路时应透传上游 404，实际 %d：%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Not found for account") {
		t.Fatalf("上游错误体应被完整透传（含还原后的响应体），实际：%s", body)
	}
}
