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

func TestClassifyKeyLevelFailure_识别账号无此模型并还原响应体(t *testing.T) {
	body := `{"status":404,"title":"Not Found","detail":"Function 'x': Not found for account 'y'"}`
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	r := New(nil, Options{})
	violation, reason := r.classifyKeyLevelFailure(resp)
	if !violation {
		t.Fatal("404 + Not found for account 应判为密钥级失败（换把密钥可能就成功）")
	}
	if !strings.Contains(reason, "无权访问") {
		t.Fatalf("失败原因应说明是凭据无权限，实际 %q", reason)
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

func TestClassifyKeyLevelFailure_普通业务错误不视为凭据问题(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"unexpected field"}}`)),
	}

	r := New(nil, Options{})
	if violation, _ := r.classifyKeyLevelFailure(resp); violation {
		t.Fatal("与凭据无关的 404 不应触发密钥轮换（换密钥也没用，只会放大延迟）")
	}
}

func TestClassifyKeyLevelFailure_状态码路径(t *testing.T) {
	r := New(nil, Options{})

	for _, status := range []int{401, 402, 403, 429} {
		resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}
		if violation, _ := r.classifyKeyLevelFailure(resp); !violation {
			t.Errorf("状态码 %d 应判为凭据级失败", status)
		}
	}
	for _, status := range []int{200, 400, 422, 500, 502, 503} {
		resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}
		if violation, _ := r.classifyKeyLevelFailure(resp); violation {
			t.Errorf("状态码 %d 不应判为凭据级失败", status)
		}
	}
}

// TestForward_多账号池_前几把无此模型_换密钥后成功 是本文件的核心用例。
//
// 场景：池里 8 把密钥来自不同账号，只有最后一把所在账号拥有该模型。
// 期望：请求仍然成功——因为"该账号无此模型"属于密钥级失败，
// 不应消耗渠道预算，也不该直接把 404 甩给用户。
func TestForward_多账号池_前几把无此模型_换密钥后成功(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	owner := map[string]bool{"nvapi-entitled": true}
	upstream := newEntitlementAwareUpstream(t, owner)

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	pool := []string{"nvapi-a", "nvapi-b", "nvapi-c", "nvapi-d", "nvapi-e", "nvapi-f", "nvapi-g", "nvapi-entitled"}
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, pool, nil); err != nil {
		t.Fatalf("导入密钥池失败: %v", err)
	}

	// MaxAttempts=1：渠道级预算只给 1 次，用来证明"换密钥"不依赖渠道预算。
	r := New(channels, Options{Keys: keys, MaxAttempts: 1})
	gateway := newGateway(t, r)

	// 反复请求：随机挑选密钥，必然多次落在没有授权的账号上；
	// 只要有一次命中 nvapi-entitled，请求就应当成功。
	const rounds = 15
	for i := 0; i < rounds; i++ {
		resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求应成功（池内存在拥有该模型的账号），实际 %d：%s",
				i+1, resp.StatusCode, body)
		}
	}
}

// TestForward_全池均无该模型_透传上游404 兜底用例。
//
// 场景：池里所有账号都没有该模型的授权。
// 期望：试完密钥后把上游的 404 原样透传（让用户看到"该模型在当前账号池不可用"），
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
