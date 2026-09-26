// 转发引擎的单元测试。
//
// 意图（Why）：
//
//	透传是网关最基本也最容易出错的能力（鉴权头是否错用、流式是否被缓冲、
//	错误是否原样透传都会直接影响用户）。这里用真实的 httptest 上游做端到端验证，
//	而不是 mock——真实的 HTTP 往返才能暴露头处理与流式行为的问题。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ 启动假上游（httptest.Server）
//	  ├─ 把渠道指向假上游（临时 SQLite + 加密仓储）
//	  └─ 调用 Relay 并断言客户端收到什么、上游收到什么
//
// 扩展（Extend）：
//
//	新增协议时，仿照本文件的结构补充对应测试（正常路径 + 鉴权隔离 + 流式 + 错误透传）。
package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// testEncryptionKey 是测试用密钥材料（非真实密钥）。
const testEncryptionKey = "relay-test-key-material-0123456789abcdef0123456789"

// newTestRepo 构造一个基于临时数据库的渠道仓储。
func newTestRepo(t *testing.T) model.ChannelRepository {
	t.Helper()

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "relay_test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	cipher, err := crypto.New(testEncryptionKey)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}
	return store.NewChannelRepository(st.DB(), cipher)
}

// addChannel 向仓储写入一个启用状态的渠道，返回其 ID。
func addChannel(t *testing.T, repo model.ChannelRepository, baseURL, apiKey string, models []string, priority int) *model.Channel {
	t.Helper()

	ch := &model.Channel{
		Name:     "测试渠道",
		Type:     1,
		BaseURL:  baseURL,
		APIKey:   apiKey,
		Models:   models,
		Group:    "default",
		Priority: priority,
		Weight:   1,
		Status:   model.ChannelStatusEnabled,
	}
	if err := repo.Create(context.Background(), ch); err != nil {
		t.Fatalf("写入测试渠道失败: %v", err)
	}
	return ch
}

// newRelay 构造一个指向给定仓储的转发引擎。
func newRelay(repo model.ChannelRepository) *Relay {
	return New(repo, Options{})
}

// TestServeChatCompletions_Passthrough 验证请求被原样转发、响应被原样回写。
func TestServeChatCompletions_Passthrough(t *testing.T) {
	const upstreamKey = "sk-upstream-secret-key"

	var (
		gotAuth string
		gotBody []byte
		gotPath string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Custom", "preserved")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"content":"你好"}}]}`)
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, upstream.URL, upstreamKey, []string{"gpt-4o"}, 10)
	rl := newRelay(repo)

	gateway := httptest.NewServer(http.HandlerFunc(rl.ServeChatCompletions))
	defer gateway.Close()

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 1) 客户端拿到上游的状态码与响应体
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "chatcmpl-1") {
		t.Errorf("响应体未透传上游内容: %s", respBody)
	}

	// 2) 上游收到了渠道密钥（而不是客户端令牌）
	if gotAuth != "Bearer "+upstreamKey {
		t.Errorf("上游收到的 Authorization = %q，期望 %q", gotAuth, "Bearer "+upstreamKey)
	}

	// 3) 请求体被原样转发
	var forwarded map[string]any
	if err := json.Unmarshal(gotBody, &forwarded); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v", err)
	}
	if forwarded["model"] != "gpt-4o" {
		t.Errorf("转发后 model = %v，期望 gpt-4o", forwarded["model"])
	}
	if _, ok := forwarded["messages"]; !ok {
		t.Error("转发时丢失了 messages 字段")
	}

	// 4) 上游路径正确拼接（base_url 末尾不应出现双斜杠）
	if gotPath != "/v1/chat/completions" {
		t.Errorf("上游收到路径 = %q，期望 /v1/chat/completions", gotPath)
	}

	// 5) 自定义响应头被保留
	if resp.Header.Get("X-Upstream-Custom") != "preserved" {
		t.Error("上游自定义响应头未被透传")
	}
}

// TestServeChatCompletions_ClientTokenNotLeakedToUpstream 验证客户端令牌不会被当作上游密钥。
//
// 这是最危险的一类实现错误：若把客户端 Authorization 直接转发给上游，
// 既会导致上游鉴权失败，也会把网关令牌泄露给第三方上游。
func TestServeChatCompletions_ClientTokenNotLeakedToUpstream(t *testing.T) {
	const (
		upstreamKey = "sk-real-upstream-key"
		clientToken = "sk-gateway-client-token-should-not-leak"
	)

	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, upstream.URL, upstreamKey, nil, 10)
	rl := newRelay(repo)

	gateway := httptest.NewServer(http.HandlerFunc(rl.ServeChatCompletions))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"any-model"}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if strings.Contains(gotAuth, clientToken) {
		t.Fatalf("客户端令牌被转发到上游（严重安全问题）: %q", gotAuth)
	}
	if gotAuth != "Bearer "+upstreamKey {
		t.Errorf("上游收到的 Authorization = %q，期望渠道密钥", gotAuth)
	}
}

// TestServeChatCompletions_StreamingFlushesIncrementally 验证流式响应是"边到边转发"。
//
// 测试手法：
//  1. 假上游发出第一个分片后【阻塞】，等待测试放行；
//  2. 若网关正确 Flush，客户端应能在放行前就收到第一个分片；
//  3. 若网关做了缓冲，客户端会一直等到上游结束——此时测试超时失败。
func TestServeChatCompletions_StreamingFlushesIncrementally(t *testing.T) {
	release := make(chan struct{})
	// 防御：无论测试因何提前结束，都要放行上游，否则 httptest.Server.Close() 会一直等待
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		_, _ = io.WriteString(w, "data: {\"chunk\":1}\n\n")
		if flusher != nil {
			flusher.Flush()
		}

		<-release // 等待测试确认已收到第一个分片

		_, _ = io.WriteString(w, "data: {\"chunk\":2}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, upstream.URL, "sk-stream-key", nil, 10)
	rl := newRelay(repo)

	gateway := httptest.NewServer(http.HandlerFunc(rl.ServeChatCompletions))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"any-model","stream":true}`))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	reader := bufio.NewReader(resp.Body)

	// 在独立 goroutine 中读取第一行，避免上游阻塞时测试主协程被卡死
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()

	select {
	case line := <-lineCh:
		if !strings.Contains(line, "chunk") {
			t.Errorf("第一个分片内容异常: %q", line)
		}
	case err := <-errCh:
		t.Fatalf("读取第一个分片失败: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("3 秒内未收到第一个分片：网关可能缓冲了响应，破坏了流式体验")
	}

	// 放行上游，验证剩余分片完整到达
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取剩余分片失败: %v", err)
	}
	if !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("剩余分片不完整，期望包含 [DONE]: %q", rest)
	}
}

// TestServeChatCompletions_UpstreamErrorPassedThrough 验证上游错误原样透传。
//
// 为什么重要：客户端需要区分"我的密钥无效(401)"与"上游限流(429)"，
// 若网关把上游错误统一改写为 500，调用方将完全无法自助排查。
func TestServeChatCompletions_UpstreamErrorPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`)
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, upstream.URL, "sk-key", nil, 10)
	rl := newRelay(repo)

	gateway := httptest.NewServer(http.HandlerFunc(rl.ServeChatCompletions))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"any-model"}`))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d，期望原样透传 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "rate limit exceeded") {
		t.Errorf("上游错误体未被透传: %s", body)
	}
}

// TestServeChatCompletions_NoAvailableChannel 验证无可用渠道时返回 503 与 OpenAI 风格错误。
func TestServeChatCompletions_NoAvailableChannel(t *testing.T) {
	repo := newTestRepo(t) // 空库，没有任何渠道
	rl := newRelay(repo)

	gateway := httptest.NewServer(http.HandlerFunc(rl.ServeChatCompletions))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4o"}`))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", resp.StatusCode)
	}

	// 错误体必须符合 OpenAI 格式，客户端 SDK 才能正确解析
	var body openAIErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("错误体不是 OpenAI 格式: %v", err)
	}
	if body.Error.Code != "no_available_channel" {
		t.Errorf("错误码 = %q，期望 no_available_channel", body.Error.Code)
	}
}

// TestServeChatCompletions_BadRequests 验证各类非法请求都返回 400 且格式统一。
func TestServeChatCompletions_BadRequests(t *testing.T) {
	repo := newTestRepo(t)
	addChannel(t, repo, "https://example.com", "sk-key", nil, 10)
	rl := newRelay(repo)

	gateway := httptest.NewServer(http.HandlerFunc(rl.ServeChatCompletions))
	defer gateway.Close()

	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{name: "非法 JSON", body: `{ not json`, wantCode: "invalid_json"},
		{name: "缺少 model", body: `{"messages":[]}`, wantCode: "missing_model"},
		{name: "model 为空白", body: `{"model":"   "}`, wantCode: "missing_model"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("请求网关失败: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("状态码 = %d，期望 400", resp.StatusCode)
			}
			var body openAIErrorBody
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("错误体解析失败: %v", err)
			}
			if body.Error.Code != tc.wantCode {
				t.Errorf("错误码 = %q，期望 %q", body.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestSelectChannel_ModelMatching 验证渠道选择的模型匹配与优先级规则。
func TestSelectChannel_ModelMatching(t *testing.T) {
	repo := newTestRepo(t)

	// 低优先级：支持 gpt-4o
	addChannel(t, repo, "https://low.example.com", "k1", []string{"gpt-4o"}, 1)
	// 高优先级：仅支持 claude-3
	addChannel(t, repo, "https://high.example.com", "k2", []string{"claude-3"}, 100)

	rl := newRelay(repo)
	ctx := context.Background()

	// 请求 gpt-4o：高优先级渠道虽排在前，但不支持该模型，应落到低优先级渠道
	ch, err := rl.SelectChannel(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("选择渠道失败: %v", err)
	}
	if ch.BaseURL != "https://low.example.com" {
		t.Errorf("选中渠道 = %s，期望 low（高优先级渠道不支持该模型）", ch.BaseURL)
	}

	// 请求不存在的模型：应返回 ErrNoAvailableChannel
	if _, err := rl.SelectChannel(ctx, "no-such-model"); err == nil {
		t.Error("请求无人支持的模型应返回错误")
	} else if !strings.Contains(err.Error(), "没有可用") {
		t.Errorf("错误信息不明确: %v", err)
	}
}

// TestSelectChannel_EmptyModelsMeansWildcard 验证"模型列表为空 = 支持全部模型"的过渡约定。
func TestSelectChannel_EmptyModelsMeansWildcard(t *testing.T) {
	repo := newTestRepo(t)
	addChannel(t, repo, "https://wildcard.example.com", "k", nil, 1)

	rl := newRelay(repo)

	ch, err := rl.SelectChannel(context.Background(), "whatever-model")
	if err != nil {
		t.Fatalf("空模型列表应视为通配，实际报错: %v", err)
	}
	if ch.BaseURL != "https://wildcard.example.com" {
		t.Errorf("选中渠道 = %s，期望 wildcard", ch.BaseURL)
	}
}

// TestSelectChannel_IgnoresDisabledChannels 验证被禁用的渠道不参与路由。
func TestSelectChannel_IgnoresDisabledChannels(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	disabled := addChannel(t, repo, "https://disabled.example.com", "k", nil, 100)
	disabled.Status = model.ChannelStatusDisabled
	if err := repo.Update(ctx, disabled); err != nil {
		t.Fatalf("禁用渠道失败: %v", err)
	}

	rl := newRelay(repo)
	if _, err := rl.SelectChannel(ctx, "any-model"); err == nil {
		t.Error("仅存在禁用渠道时不应返回可用渠道")
	}
}

// TestIsHopByHopHeader 验证逐跳头过滤规则（大小写不敏感）。
func TestIsHopByHopHeader(t *testing.T) {
	hopByHop := []string{"Connection", "connection", "Keep-Alive", "Transfer-Encoding", "Content-Length", "Upgrade"}
	for _, h := range hopByHop {
		if !isHopByHopHeader(h) {
			t.Errorf("%s 应被判定为逐跳头", h)
		}
	}

	endToEnd := []string{"Content-Type", "X-Request-Id", "Cache-Control"}
	for _, h := range endToEnd {
		if isHopByHopHeader(h) {
			t.Errorf("%s 不应被判定为逐跳头", h)
		}
	}
}
