// 渠道密钥池在转发链路上的行为测试。
//
// 意图（Why）：
//
//	密钥池的价值全在"某把密钥坏掉时能自动换下一把"。若这段逻辑出错，
//	表现是"池里有几百把密钥，用户却频繁收到 401"——而这恰恰是最难通过
//	人工测试发现的问题（因为随机挑选，手工点几次可能恰好都命中好密钥）。
//	因此这里用可重复的批量请求把它固化成自动化测试。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ 假上游按 Authorization 头判定该密钥是否有效
//	  ├─ 渠道挂入密钥池（临时 SQLite + 真实加解密）
//	  └─ 反复请求，断言成功率与密钥状态变化
//
// 扩展（Extend）：
//
//	新增密钥策略（如按模型区分密钥）时，在此补充对应场景即可。
package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// newTestRepos 在同一临时数据库上同时构造渠道仓储与密钥池仓储。
//
// 必须共用一个数据库：密钥池表通过 channel_id 关联渠道，
// 用两个独立库会让"渠道存在但密钥池查不到"这种集成问题测不出来。
func newTestRepos(t *testing.T) (model.ChannelRepository, model.ChannelKeyRepository) {
	t.Helper()

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "keypool_test.db"))
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
	return store.NewChannelRepository(st.DB(), cipher), store.NewChannelKeyRepository(st.DB(), cipher)
}

// newKeyAwareUpstream 启动一个"按密钥判定有效性"的假上游。
//
// valid 中的密钥返回 200；其余返回 401（模拟密钥失效）。
// 这样我们就能精确控制"池里哪几把能用"，从而验证换密钥行为。
func newKeyAwareUpstream(t *testing.T, valid map[string]bool) (*httptest.Server, *int32) {
	t.Helper()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !valid[key] {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestIsKeyLevelFailure(t *testing.T) {
	keyLevel := []int{401, 402, 403, 429}
	for _, s := range keyLevel {
		if !isKeyLevelFailure(s) {
			t.Errorf("状态码 %d 应判为密钥级失败", s)
		}
	}

	notKeyLevel := []int{400, 404, 422, 500, 502, 503}
	for _, s := range notKeyLevel {
		if isKeyLevelFailure(s) {
			t.Errorf("状态码 %d 不应判为密钥级失败", s)
		}
	}
}

func TestResolveChatKey_密钥池优先于渠道单密钥(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	ch := addChannel(t, channels, "https://upstream.example.com", "sk-single-key", []string{"m"}, 10)
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, []string{"nvapi-pool-key"}, nil); err != nil {
		t.Fatalf("导入密钥池失败: %v", err)
	}

	r := New(channels, Options{Keys: keys})

	// 渠道同时存在"单密钥"与"池中密钥"时，应优先用池里的
	// （这是配置者的显式意图：把密钥放进池子就是要走池化轮询）
	key, keyID, ok, _ := r.resolveChatKey(ctx, ch, map[uint64]struct{}{})
	if !ok {
		t.Fatal("应能解析出密钥")
	}
	if key != "nvapi-pool-key" {
		t.Fatalf("应使用密钥池中的密钥，实际使用了 %q", key)
	}
	if keyID == 0 {
		t.Fatal("池化模式下应返回密钥池记录 ID（用于失败统计）")
	}
}

func TestResolveChatKey_无池时回退单密钥(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	// 渠道只有单密钥，从未导入密钥池（历史数据形态）
	ch := addChannel(t, channels, "https://upstream.example.com", "sk-legacy-key", []string{"m"}, 10)

	r := New(channels, Options{Keys: keys})

	key, keyID, ok, _ := r.resolveChatKey(ctx, ch, map[uint64]struct{}{})
	if !ok || key != "sk-legacy-key" || keyID != 0 {
		t.Fatalf("应回退为单密钥模式，实际 key=%q keyID=%d ok=%v", key, keyID, ok)
	}
}

func TestResolveChatKey_本次已用过的密钥不再返回(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	ch := addChannel(t, channels, "https://upstream.example.com", "", []string{"m"}, 10)
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, []string{"k1", "k2"}, nil); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	r := New(channels, Options{Keys: keys})
	used := make(map[uint64]struct{})

	// 连续两次应拿到不同的密钥（池内只有两把）
	_, id1, ok1, spare1 := r.resolveChatKey(ctx, ch, used)
	if !ok1 {
		t.Fatal("第一次应能解析出密钥")
	}
	if !spare1 {
		t.Fatal("第一次解析后池内还有一把未用，应标记为有备用密钥")
	}
	used[id1] = struct{}{}

	_, id2, ok2, spare2 := r.resolveChatKey(ctx, ch, used)
	if !ok2 {
		t.Fatal("第二次应能解析出密钥")
	}
	if id2 == id1 {
		t.Fatal("第二次不应返回已用过的同一把密钥")
	}
	if spare2 {
		t.Fatal("两把都用过之后不应再标记有备用密钥")
	}
	used[id2] = struct{}{}

	// 第三次：池内密钥已全部用过，应返回 ok=false（让上层换渠道）
	_, _, ok3, _ := r.resolveChatKey(ctx, ch, used)
	if ok3 {
		t.Fatal("池内密钥全部用过后应返回 ok=false")
	}
}

func TestForward_密钥池自动换密钥_请求最终成功(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	// 假上游：只有 nvapi-good 有效
	upstream, _ := newKeyAwareUpstream(t, map[string]bool{"nvapi-good": true})

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, []string{"nvapi-bad", "nvapi-good"}, nil); err != nil {
		t.Fatalf("导入密钥池失败: %v", err)
	}

	r := New(channels, Options{Keys: keys, MaxAttempts: 3})
	gateway := newGateway(t, r)

	// 反复请求：随机挑选意味着必然会撞上失效密钥，
	// 若没有"换密钥重试"能力，这些请求就会返回 401。
	const rounds = 25
	for i := 0; i < rounds; i++ {
		resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求应成功（池内存在可用密钥），实际 %d：%s", i+1, resp.StatusCode, body)
		}
	}
}

func TestForward_密钥池全部失效_透传上游错误并累计失败(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	// 假上游：没有任何有效密钥，全部返回 401
	upstream, _ := newKeyAwareUpstream(t, nil)

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, []string{"nvapi-bad1", "nvapi-bad2"}, nil); err != nil {
		t.Fatalf("导入密钥池失败: %v", err)
	}

	r := New(channels, Options{Keys: keys, MaxAttempts: 2})
	gateway := newGateway(t, r)

	resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// 池内密钥全部失效且没有其他渠道：应把 401 原样透传，
	// 而不是丢掉上游错误再返回含义模糊的 502
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无退路时应透传 401，实际 %d：%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "invalid api key") {
		t.Fatalf("错误体应原样透传，实际：%s", body)
	}

	// 两把密钥都应被记上一次失败（连续失败达阈值后会被自动摘除）
	pool, err := keys.ListByChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("查询密钥池失败: %v", err)
	}
	failed := 0
	for _, k := range pool {
		if k.FailCount > 0 {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("失效密钥应被记录失败次数（否则永远无法自动摘除）")
	}
}

func TestForward_密钥连续失败达阈值_自动摘除不再使用(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	upstream, _ := newKeyAwareUpstream(t, nil) // 全部实效

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	// 池里只放一把：它会被反复选中，从而快速达到摘除阈值
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, []string{"nvapi-doomed"}, nil); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	r := New(channels, Options{Keys: keys, MaxAttempts: 1})
	gateway := newGateway(t, r)

	// 反复请求直到该密钥被自动摘除（阈值 3 次连续失败）
	for i := 0; i < model.KeyAutoRemoveThreshold+1; i++ {
		resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
		_ = resp.Body.Close()
	}

	pool, err := keys.ListByChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("查询密钥池失败: %v", err)
	}
	if len(pool) != 1 {
		t.Fatalf("池内应有 1 把密钥，实际 %d", len(pool))
	}
	if pool[0].Status != model.ChannelKeyStatusAutoRemoved {
		t.Fatalf("连续失败达 %d 次后应自动摘除，实际状态 %s",
			model.KeyAutoRemoveThreshold, pool[0].Status)
	}

	// 摘除后该渠道不再有可用密钥：请求应返回 503（无可用渠道），
	// 而不是继续拿失效密钥去撞上游
	usable, err := keys.ListUsable(ctx, ch.ID)
	if err != nil {
		t.Fatalf("查询可用密钥失败: %v", err)
	}
	if len(usable) != 0 {
		t.Fatalf("被摘除的密钥不应仍可用，实际 %d 把", len(usable))
	}
}

func TestForward_单渠道多密钥_分布到不同密钥(t *testing.T) {
	channels, keys := newTestRepos(t)
	ctx := context.Background()

	// 5 把密钥全部有效；用于验证"请求确实在池内分散"，而不是永远用第一把
	valid := map[string]bool{}
	keyList := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("nvapi-multi-%d", i)
		valid[k] = true
		keyList = append(keyList, k)
	}
	upstream, _ := newKeyAwareUpstream(t, valid)

	ch := addChannel(t, channels, upstream.URL, "", []string{"test-model"}, 10)
	if _, _, err := keys.ReplaceAll(ctx, ch.ID, keyList, nil); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	r := New(channels, Options{Keys: keys, MaxAttempts: 1})
	gateway := newGateway(t, r)

	const rounds = 40
	for i := 0; i < rounds; i++ {
		resp := postChat(t, gateway.URL, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求失败：%d", i+1, resp.StatusCode)
		}
	}

	// 40 次请求分散到 5 把密钥：每把都应被使用过（随机会有波动，用"至少 3 把被用过"作为判据）
	pool, err := keys.ListByChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	used := 0
	for _, k := range pool {
		if !k.LastUsedAt.IsZero() {
			used++
		}
	}
	if used < 3 {
		t.Fatalf("40 次请求应分散到多把密钥，实际只用到 %d 把（可能退化为固定选第一把）", used)
	}
}
