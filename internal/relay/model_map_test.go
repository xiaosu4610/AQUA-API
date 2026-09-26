// 渠道级模型 ID 映射在转发链路上的端到端测试。
//
// 意图（Why）：
//
//	映射最容易出的错不是"算错了"，而是"根本没生效"——存储与后台接口都正常，
//	但转发链路没用它，于是用户请求 B、上游仍收到 B（这正是改造前的半成品状态）。
//	这里用真实 httptest 上游做端到端验证，钉住四件事：
//	  1) 请求方向：命中映射时上游收到的 model 是上游名，且路径模板用上游名；
//	  2) 响应方向：非流式整体回写、流式只回写首个含 model 的事件，且分帧仍合法；
//	  3) 无映射时逐字节零影响（请求体与原始请求完全一致）；
//	  4) 映射仓储查询失败时退化为"无映射"，转发不受影响。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ 假上游（httptest.Server）记录收到的请求体
//	  ├─ 假映射仓储（按渠道 ID 返回映射，可注入错误）
//	  └─ 断言"上游收到什么 / 客户端收到什么"
//
// 扩展（Extend）：
//
//	新增映射语义（如按分组映射）：在 fakeMappingRepo 里补数据源并加用例，
//	其余断言结构可复用。
package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// fakeMappingRepo 是 model.ChannelModelMappingRepository 的内存实现。
//
// 只实现转发链路用到的 ListByChannel（ReplaceForChannel 在转发路径上不会被调用）。
type fakeMappingRepo struct {
	byChannel map[uint64][]*model.ChannelModelMapping
	// err 非 nil 时 ListByChannel 恒失败，用于验证"查询失败仍能转发"。
	err error
	// calls 记录 ListByChannel 的调用次数，用于验证缓存生效。
	calls int
}

// ListByChannel 返回指定渠道的映射列表。
func (f *fakeMappingRepo) ListByChannel(_ context.Context, channelID uint64) ([]*model.ChannelModelMapping, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byChannel[channelID], nil
}

// ReplaceForChannel 转发路径不会调用，实现以满足接口。
func (f *fakeMappingRepo) ReplaceForChannel(context.Context, uint64, []*model.ChannelModelMapping) error {
	return nil
}

// newRelayWithMappings 构造一个注入了映射仓储的转发引擎。
func newRelayWithMappings(repo model.ChannelRepository, mappings model.ChannelModelMappingRepository) *Relay {
	return New(repo, Options{ChannelModelMappings: mappings})
}

// mappingFor 构造一条启用的映射，便于用例简洁。
func mappingFor(channelID uint64, publicModel, upstreamModel string) []*model.ChannelModelMapping {
	return []*model.ChannelModelMapping{
		{
			ChannelID:     channelID,
			PublicModel:   publicModel,
			UpstreamModel: upstreamModel,
			Enabled:       true,
		},
	}
}

// TestModelMapping_请求改写与响应回写_非流式 验证请求方向改写、响应方向回写。
func TestModelMapping_请求改写与响应回写_非流式(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"upstream-model","choices":[]}`)
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	ch := addChannel(t, repo, upstream.URL, "sk-key", []string{"public-model"}, 10)

	mappings := &fakeMappingRepo{byChannel: map[uint64][]*model.ChannelModelMapping{
		ch.ID: mappingFor(ch.ID, "public-model", "upstream-model"),
	}}
	gateway := newGateway(t, newRelayWithMappings(repo, mappings))

	resp := postChat(t, gateway.URL, `{"model":"public-model","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}

	// ① 上游收到的 model 必须是上游名
	var forwarded map[string]any
	if err := json.Unmarshal(gotBody, &forwarded); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v（%s）", err, gotBody)
	}
	if forwarded["model"] != "upstream-model" {
		t.Errorf("上游收到 model = %v，期望上游名 upstream-model", forwarded["model"])
	}

	// ② 客户端收到的 model 必须回写为对外名
	body, _ := io.ReadAll(resp.Body)
	var echoed map[string]any
	if err := json.Unmarshal(body, &echoed); err != nil {
		t.Fatalf("客户端收到的响应体不是合法 JSON: %v（%s）", err, body)
	}
	if echoed["model"] != "public-model" {
		t.Errorf("客户端收到 model = %v，期望对外名 public-model", echoed["model"])
	}
}

// TestModelMapping_无映射时请求体逐字节不变 验证零影响保证。
//
// 关键：请求体刻意带有多余空白与键顺序，若转发层做了任何 JSON 重排，
// 这里就会失败——无映射时必须逐字节原样透传。
func TestModelMapping_无映射时请求体逐字节不变(t *testing.T) {
	const requestBody = `{ "model" : "public-model" ,  "messages" : [ ] }`

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"public-model","choices":[]}`)
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	// 渠道没有任何映射
	addChannel(t, repo, upstream.URL, "sk-key", []string{"public-model"}, 10)

	mappings := &fakeMappingRepo{byChannel: map[uint64][]*model.ChannelModelMapping{}}
	gateway := newGateway(t, newRelayWithMappings(repo, mappings))

	resp := postChat(t, gateway.URL, requestBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}

	if string(gotBody) != requestBody {
		t.Errorf("无映射时请求体被改动：\n收到 = %q\n期望 = %q", gotBody, requestBody)
	}
}

// TestModelMapping_流式只改写首个事件 验证流式响应回写且不破坏分帧。
//
// 断言：
//   - 首个含 model 的数据事件被改为对外名；
//   - 其余事件保持上游原名（不做无谓重建）；
//   - 每个数据帧仍是合法 JSON、仍以 data: 前缀 + 空行分帧；
//   - 结束标记 [DONE] 未被破坏。
func TestModelMapping_流式只改写首个事件(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"model\":\"upstream-model\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"model\":\"upstream-model\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	ch := addChannel(t, repo, upstream.URL, "sk-key", []string{"public-model"}, 10)

	mappings := &fakeMappingRepo{byChannel: map[uint64][]*model.ChannelModelMapping{
		ch.ID: mappingFor(ch.ID, "public-model", "upstream-model"),
	}}
	gateway := newGateway(t, newRelayWithMappings(repo, mappings))

	resp := postChat(t, gateway.URL, `{"model":"public-model","stream":true,"messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if strings.Count(got, "public-model") != 1 {
		t.Errorf("对外名应恰好出现 1 次（只改写首个事件），实际 %d 次：%q",
			strings.Count(got, "public-model"), got)
	}
	if strings.Count(got, "upstream-model") != 1 {
		t.Errorf("上游名应恰好保留 1 次（其余事件不改写），实际 %d 次：%q",
			strings.Count(got, "upstream-model"), got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Errorf("结束标记 [DONE] 丢失：%q", got)
	}
	assertValidSSEFrames(t, got)
}

// TestModelMapping_仓储查询失败时仍正常转发 验证映射的降级行为。
func TestModelMapping_仓储查询失败时仍正常转发(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"public-model","choices":[]}`)
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	addChannel(t, repo, upstream.URL, "sk-key", []string{"public-model"}, 10)

	mappings := &fakeMappingRepo{err: context.DeadlineExceeded}
	gateway := newGateway(t, newRelayWithMappings(repo, mappings))

	resp := postChat(t, gateway.URL, `{"model":"public-model","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("映射仓储失败不应影响转发，状态码 = %d，期望 200", resp.StatusCode)
	}
	// 降级为无映射：上游应收到原始对外名
	var forwarded map[string]any
	if err := json.Unmarshal(gotBody, &forwarded); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v", err)
	}
	if forwarded["model"] != "public-model" {
		t.Errorf("降级后上游收到 model = %v，期望原样 public-model", forwarded["model"])
	}
}

// TestModelMapping_类型专属路径使用上游模型名 验证 {model}/{deployment} 占位符用上游名。
//
// 用 Azure 渠道验证：未配置 deployment 时，路径里的部署名回退为模型名，
// 因此路径必须出现【上游模型名】而不是对外名（Gemini 的 /models/{model} 同理）。
func TestModelMapping_类型专属路径使用上游模型名(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"upstream-model","choices":[]}`)
	}))
	defer upstream.Close()

	repo := newTestRepo(t)
	ch := addTypedChannel(t, repo, upstream.URL, "azure_openai", nil, []string{"public-model"})

	mappings := &fakeMappingRepo{byChannel: map[uint64][]*model.ChannelModelMapping{
		ch.ID: mappingFor(ch.ID, "public-model", "upstream-model"),
	}}
	gateway := newGateway(t, newRelayWithMappings(repo, mappings))

	resp := postChat(t, gateway.URL, `{"model":"public-model","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}

	const want = "/openai/deployments/upstream-model/chat/completions"
	if gotPath != want {
		t.Errorf("上游路径 = %q，期望 %q（路径模板必须用上游模型名）", gotPath, want)
	}
}

// TestChannelMappingCache_缓存命中与失效 验证缓存避免重复查库、失效后重新查库。
func TestChannelMappingCache_缓存命中与失效(t *testing.T) {
	repo := &fakeMappingRepo{byChannel: map[uint64][]*model.ChannelModelMapping{
		7: mappingFor(7, "a", "b"),
	}}
	cache := newChannelMappingCache(time.Minute, 8)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := cache.get(ctx, repo, 7); err != nil {
			t.Fatalf("读取映射失败: %v", err)
		}
	}
	if repo.calls != 1 {
		t.Errorf("缓存未生效：查库 %d 次，期望 1 次", repo.calls)
	}

	cache.invalidate(7)
	if _, err := cache.get(ctx, repo, 7); err != nil {
		t.Fatalf("失效后读取映射失败: %v", err)
	}
	if repo.calls != 2 {
		t.Errorf("失效后未重新查库：查库 %d 次，期望 2 次", repo.calls)
	}
}

// TestRewriteRequestModel_边界 验证请求体改写的护栏。
func TestRewriteRequestModel_边界(t *testing.T) {
	t.Run("非 JSON 原样返回", func(t *testing.T) {
		body := []byte(`not json`)
		got, ok := rewriteRequestModel(body, "upstream")
		if ok || string(got) != string(body) {
			t.Errorf("非 JSON 应原样返回且 ok=false，实际 ok=%v got=%q", ok, got)
		}
	})
	t.Run("缺少 model 字段原样返回", func(t *testing.T) {
		body := []byte(`{"prompt":"hi"}`)
		if got, ok := rewriteRequestModel(body, "upstream"); ok || string(got) != string(body) {
			t.Errorf("缺少 model 应原样返回，实际 ok=%v got=%q", ok, got)
		}
	})
	t.Run("命中 model 字段时改写", func(t *testing.T) {
		got, ok := rewriteRequestModel([]byte(`{"model":"public","n":1}`), "upstream")
		if !ok {
			t.Fatal("应改写成功")
		}
		var fields map[string]any
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Fatalf("改写结果不是合法 JSON: %v", err)
		}
		if fields["model"] != "upstream" {
			t.Errorf("model = %v，期望 upstream", fields["model"])
		}
	})
}

// addTypedChannel 写入一个指定渠道类型与扩展配置的启用渠道。
func addTypedChannel(
	t *testing.T, repo model.ChannelRepository,
	baseURL, typeKey string, extra map[string]string, models []string,
) *model.Channel {
	t.Helper()

	ch := &model.Channel{
		Name:        "类型渠道",
		Type:        1,
		TypeKey:     typeKey,
		ExtraConfig: extra,
		BaseURL:     baseURL,
		APIKey:      "sk-typed",
		Models:      models,
		Group:       "default",
		Priority:    10,
		Weight:      1,
		Status:      model.ChannelStatusEnabled,
	}
	if err := repo.Create(context.Background(), ch); err != nil {
		t.Fatalf("写入类型渠道失败: %v", err)
	}
	return ch
}

// assertValidSSEFrames 校验 SSE 文本的每一帧都以 data: 前缀开头、数据体是合法 JSON。
func assertValidSSEFrames(t *testing.T, raw string) {
	t.Helper()

	for _, frame := range strings.Split(strings.TrimRight(raw, "\n"), "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data:") {
			t.Errorf("SSE 帧缺少 data: 前缀: %q", frame)
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(frame, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Errorf("SSE 数据帧不是合法 JSON: %q (%v)", payload, err)
		}
	}
}
