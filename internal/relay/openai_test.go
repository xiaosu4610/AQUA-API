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
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
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
	var body oai.ErrorBody
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
			var body oai.ErrorBody
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
	ch, err := rl.SelectChannel(ctx, defaultGroup, "gpt-4o")
	if err != nil {
		t.Fatalf("选择渠道失败: %v", err)
	}
	if ch.BaseURL != "https://low.example.com" {
		t.Errorf("选中渠道 = %s，期望 low（高优先级渠道不支持该模型）", ch.BaseURL)
	}

	// 请求不存在的模型：应返回 ErrNoAvailableChannel
	if _, err := rl.SelectChannel(ctx, defaultGroup, "no-such-model"); err == nil {
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

	ch, err := rl.SelectChannel(context.Background(), defaultGroup, "whatever-model")
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
	if _, err := rl.SelectChannel(ctx, defaultGroup, "any-model"); err == nil {
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

// ───────────────────────── 多渠道路由与故障转移 ─────────────────────────

// addChannelWithWeight 写入一个指定权重的启用渠道，用于路由分布测试。
func addChannelWithWeight(t *testing.T, repo model.ChannelRepository, baseURL string, weight, priority int) {
	t.Helper()

	ch := &model.Channel{
		Name:     "加权渠道",
		Type:     1,
		BaseURL:  baseURL,
		APIKey:   "sk-weight-test",
		Group:    "default",
		Priority: priority,
		Weight:   weight,
		Status:   model.ChannelStatusEnabled,
	}
	if err := repo.Create(context.Background(), ch); err != nil {
		t.Fatalf("写入加权渠道失败: %v", err)
	}
}

// deadUpstreamURL 返回一个"必定连接失败"的上游地址。
//
// 做法：启动 httptest.Server 后立即关闭——地址仍有效但无人监听，
// 连接会被拒绝，从而稳定复现"连接层失败"这一可重试场景。
func deadUpstreamURL(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// newGateway 用给定转发引擎启动一个测试用网关。
func newGateway(t *testing.T, rl *Relay) *httptest.Server {
	t.Helper()
	gw := httptest.NewServer(http.HandlerFunc(rl.ServeChatCompletions))
	t.Cleanup(gw.Close)
	return gw
}

// postChat 向网关发起一次对话请求，返回响应。
func postChat(t *testing.T, gatewayURL, body string) *http.Response {
	t.Helper()

	resp, err := http.Post(gatewayURL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestForward_FailoverOnConnectionError 验证连接失败时自动换渠道并成功返回。
//
// 这是故障转移最核心的场景：高优先级渠道不可用时，请求应落到低优先级渠道，
// 而不是把错误直接抛给用户。
func TestForward_FailoverOnConnectionError(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"from-healthy-channel"}`)
	}))
	defer healthy.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, deadUpstreamURL(t), "sk-dead", nil, 100) // 高优先级但不可用
	addChannel(t, repo, healthy.URL, "sk-ok", nil, 10)           // 低优先级可用

	gateway := newGateway(t, newRelay(repo))
	resp := postChat(t, gateway.URL, `{"model":"any-model"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（应故障转移而非直接报错）", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "from-healthy-channel") {
		t.Errorf("响应体未来自可用渠道: %s", body)
	}
}

// TestForward_FailoverOnRetryableStatus 验证上游 5xx 时换渠道重试。
//
// 与"4xx 不重试"形成对照：5xx 是上游自身故障，换渠道很可能成功。
func TestForward_FailoverOnRetryableStatus(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502，可重试
	}))
	defer broken.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"from-healthy-channel"}`)
	}))
	defer healthy.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, broken.URL, "sk-broken", nil, 100)
	addChannel(t, repo, healthy.URL, "sk-ok", nil, 10)

	gateway := newGateway(t, newRelay(repo))
	resp := postChat(t, gateway.URL, `{"model":"any-model"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（502 应触发换渠道重试）", resp.StatusCode)
	}
}

// TestForward_NoRetryOnClientError 验证 4xx 不触发重试，避免无谓的上游调用。
//
// 意义：请求本身有问题（参数错、模型不存在）时换渠道结果相同，
// 重试只会放大上游压力与首字延迟。
func TestForward_NoRetryOnClientError(t *testing.T) {
	var secondChannelCalled bool

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400，不可重试
		_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	defer rejecting.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondChannelCalled = true
		_, _ = io.WriteString(w, `{"id":"second"}`)
	}))
	defer second.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, rejecting.URL, "sk-reject", nil, 100)
	addChannel(t, repo, second.URL, "sk-second", nil, 10)

	gateway := newGateway(t, newRelay(repo))
	resp := postChat(t, gateway.URL, `{"model":"any-model"}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望原样透传 400", resp.StatusCode)
	}
	if secondChannelCalled {
		t.Error("400 不应触发换渠道重试，但第二个渠道被调用了")
	}
}

// TestForward_AllChannelsFail 验证所有候选渠道失败时返回 502 且错误格式规范。
func TestForward_AllChannelsFail(t *testing.T) {
	repo := newTestRepo(t)
	addChannel(t, repo, deadUpstreamURL(t), "sk-dead-1", nil, 100)
	addChannel(t, repo, deadUpstreamURL(t), "sk-dead-2", nil, 10)

	gateway := newGateway(t, newRelay(repo))
	resp := postChat(t, gateway.URL, `{"model":"any-model"}`)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502", resp.StatusCode)
	}

	var body oai.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("错误体解析失败: %v", err)
	}
	if body.Error.Code != oai.CodeUpstreamRequestFailed {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, oai.CodeUpstreamRequestFailed)
	}
}

// TestForward_DoesNotRetrySameFailedChannel 验证重试不会重复选中同一渠道。
//
// 这是对参考实现常见缺陷的针对性验证：若只在同优先级内重新随机，
// 可能反复命中刚失败的渠道，表现为"重试了但毫无效果"。
func TestForward_DoesNotRetrySameFailedChannel(t *testing.T) {
	var firstChannelHits int

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstChannelHits++
		w.WriteHeader(http.StatusInternalServerError) // 500，可重试
	}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer healthy.Close()

	repo := newTestRepo(t)
	// 两个渠道同优先级，验证排除逻辑而非优先级回退
	addChannel(t, repo, failing.URL, "sk-fail", nil, 100)
	addChannel(t, repo, healthy.URL, "sk-ok", nil, 100)

	gateway := newGateway(t, newRelay(repo))
	resp := postChat(t, gateway.URL, `{"model":"any-model"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if firstChannelHits > 1 {
		t.Errorf("失败渠道被重复调用 %d 次，期望最多 1 次（应排除已失败渠道）", firstChannelHits)
	}
}

// TestSelectChannel_PrefersHigherPriorityTier 验证权重不会跨越优先级层。
//
// 设计意图：优先级表达"先用谁"的强意图（如先用便宜渠道），
// 因此即便低优先级渠道权重极高，也不应抢占高优先级层的流量。
func TestSelectChannel_PrefersHigherPriorityTier(t *testing.T) {
	repo := newTestRepo(t)
	addChannelWithWeight(t, repo, "https://low-priority.example.com", 1000, 1)
	addChannelWithWeight(t, repo, "https://high-priority.example.com", 1, 100)

	rl := newRelay(repo)
	for i := 0; i < 50; i++ {
		ch, err := rl.SelectChannel(context.Background(), defaultGroup, "any-model")
		if err != nil {
			t.Fatalf("第 %d 次选择渠道失败: %v", i+1, err)
		}
		if ch.BaseURL != "https://high-priority.example.com" {
			t.Fatalf("选中了低优先级渠道 %s（权重不应跨优先级层）", ch.BaseURL)
		}
	}
}

// TestSelectChannel_WeightedDistributionWithinTier 验证同层内按权重分流。
//
// 权重 1:9 时，期望权重小的一方约占 10%。取 2000 次采样以避免偶发波动误判。
func TestSelectChannel_WeightedDistributionWithinTier(t *testing.T) {
	const (
		samples    = 2000
		lowWeight  = 1
		highWeight = 9
	)

	repo := newTestRepo(t)
	addChannelWithWeight(t, repo, "https://light.example.com", lowWeight, 100)
	addChannelWithWeight(t, repo, "https://heavy.example.com", highWeight, 100)

	rl := newRelay(repo)

	lightHits := 0
	for i := 0; i < samples; i++ {
		ch, err := rl.SelectChannel(context.Background(), defaultGroup, "any-model")
		if err != nil {
			t.Fatalf("选择渠道失败: %v", err)
		}
		if ch.BaseURL == "https://light.example.com" {
			lightHits++
		}
	}

	// 期望命中率 10%，允许 ±5 个百分点（2000 次采样下极难越界）
	got := float64(lightHits) / float64(samples)
	if got < 0.05 || got > 0.15 {
		t.Errorf("低权重渠道命中率 = %.3f，期望约 %.2f（权重分流可能失效）",
			got, float64(lowWeight)/float64(lowWeight+highWeight))
	}
}

// TestWeightedPick_EdgeCases 验证权重选择的边界情况。
func TestWeightedPick_EdgeCases(t *testing.T) {
	t.Run("单元素直接返回", func(t *testing.T) {
		only := &model.Channel{BaseURL: "https://only.example.com", Weight: 5}
		if got := weightedPick([]*model.Channel{only}); got != only {
			t.Error("单元素列表应直接返回该元素")
		}
	})

	t.Run("权重之和为零时退化为等概率", func(t *testing.T) {
		// 正常流程下 Validate 会拦住权重 0，这里验证兜底逻辑不会死循环
		list := []*model.Channel{
			{BaseURL: "https://a.example.com", Weight: 0},
			{BaseURL: "https://b.example.com", Weight: 0},
		}
		seen := map[string]bool{}
		for i := 0; i < 100; i++ {
			seen[weightedPick(list).BaseURL] = true
		}
		if len(seen) != 2 {
			t.Errorf("权重全为 0 时应等概率返回，实际只命中 %d 个渠道", len(seen))
		}
	})

	t.Run("权重悬殊时几乎总选大权重", func(t *testing.T) {
		heavy := &model.Channel{BaseURL: "https://heavy.example.com", Weight: 1000}
		light := &model.Channel{BaseURL: "https://light.example.com", Weight: 1}
		heavyHits := 0
		for i := 0; i < 500; i++ {
			if weightedPick([]*model.Channel{light, heavy}) == heavy {
				heavyHits++
			}
		}
		if heavyHits < 490 {
			t.Errorf("大权重渠道命中 %d/500，期望绝大多数命中", heavyHits)
		}
	})
}

// TestIsRetryableStatus 验证重试状态码判定表。
func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504, 529}
	for _, code := range retryable {
		if !isRetryableStatus(code) {
			t.Errorf("状态码 %d 应可重试", code)
		}
	}

	notRetryable := []int{200, 201, 400, 401, 403, 404, 422}
	for _, code := range notRetryable {
		if isRetryableStatus(code) {
			t.Errorf("状态码 %d 不应重试（换渠道结果相同）", code)
		}
	}
}
