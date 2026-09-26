// 本文件覆盖「按请求分组」的路由与计费：分组解析优先级、分组隔离与缓存隔离。
//
// 意图（Why）：
//
//	把"分组"从进程级固定值改为按请求解析，最大的风险是"默认分组行为被改坏"
//	（存量令牌全部失效）与"不同分组互相串价/串缓存"（直接错账）。
//	因此这里用最贴近真实链路的方式（真实渠道仓储 + httptest 上游）钉住这两点。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ 建渠道（不同分组）→ 按请求分组选渠道 / 计费
//	  └─ 断言：默认回退不变、令牌分组生效、分组与缓存互不串
//
// 扩展（Extend）：
//
//	新增按分组生效的维度（如按分组的限流、按分组的模型清单）时，
//	在本文件补一条"分组 A 与分组 B 行为不同且互不影响"的用例。
package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
)

// addChannelInGroup 向仓储写入一个指定分组的启用渠道。
//
// 与 openai_test.go 的 addChannel 的区别：分组可变（那边固定 default）。
func addChannelInGroup(t *testing.T, repo model.ChannelRepository, baseURL, group string, models []string, priority int) *model.Channel {
	t.Helper()

	ch := &model.Channel{
		Name:     "测试渠道",
		Type:     1,
		BaseURL:  baseURL,
		APIKey:   "sk-test",
		Models:   models,
		Group:    group,
		Priority: priority,
		Weight:   1,
		Status:   model.ChannelStatusEnabled,
	}
	if err := repo.Create(context.Background(), ch); err != nil {
		t.Fatalf("写入测试渠道失败: %v", err)
	}
	return ch
}

// countingUpstream 启动一个 httptest 上游，记录被请求次数。
func countingUpstream(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	t.Cleanup(server.Close)
	return server
}

// serveWithGroup 以"携带令牌分组"的请求上下文调用转发入口，返回响应记录器。
func serveWithGroup(rl *Relay, group, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(reqctx.WithGroup(req.Context(), group))
	rec := httptest.NewRecorder()
	rl.ServeChatCompletions(rec, req)
	return rec
}

// TestGroupRouting_无令牌分组时沿用默认分组 验证默认行为不被改动破坏。
//
// 这是本次改动最大的风险点：存量令牌不带分组信息，必须与改动前完全一致
// （走 default 分组的渠道、按 default 分组计费）。
func TestGroupRouting_无令牌分组时沿用默认分组(t *testing.T) {
	var defaultHits, otherHits int32
	defaultUpstream := countingUpstream(t, &defaultHits)
	otherUpstream := countingUpstream(t, &otherHits)

	repo := newTestRepo(t)
	addChannelInGroup(t, repo, defaultUpstream.URL, defaultGroup, nil, 10)
	addChannelInGroup(t, repo, otherUpstream.URL, "other", nil, 10)
	rl := newRelay(repo)

	// 1) 分组解析：背景 ctx（无令牌）→ 默认分组
	if got := rl.groupFromContext(context.Background()); got != defaultGroup {
		t.Fatalf("无令牌分组时应回退默认分组，实际 %q", got)
	}

	// 2) 选渠道：只从默认分组里选
	ch, err := rl.SelectChannel(context.Background(), "", "any-model")
	if err != nil {
		t.Fatalf("默认分组选渠道失败: %v", err)
	}
	if ch.Group != defaultGroup {
		t.Fatalf("选中渠道分组 = %q，期望 %q", ch.Group, defaultGroup)
	}

	// 3) 端到端：无令牌分组的请求只打到默认分组渠道，其他分组渠道零命中
	rec := serveWithGroup(rl, "", `{"model":"any-model"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("转发状态码 = %d，期望 200", rec.Code)
	}
	if got := atomic.LoadInt32(&defaultHits); got != 1 {
		t.Fatalf("默认分组渠道命中 %d 次，期望 1", got)
	}
	if got := atomic.LoadInt32(&otherHits); got != 0 {
		t.Fatalf("其他分组渠道命中 %d 次，期望 0（默认行为被破坏）", got)
	}

	// 4) 计费：空分组与显式默认分组的计算结果完全一致
	billing := newTestBilling(150, 1_000_000, 2_000_000, 0)
	withEmpty := billing.Quote(context.Background(), "", "test-model", 1000, 500)
	withDefault := billing.Quote(context.Background(), defaultBillingGroup, "test-model", 1000, 500)
	if withEmpty != 3000 || withEmpty != withDefault {
		t.Fatalf("默认分组计费 = 空分组 %d / 显式默认 %d，期望均为 3000", withEmpty, withDefault)
	}
}

// TestGroupRouting_令牌分组决定候选渠道 验证"分组=X 只选 X 的渠道，Y 绝不入选"。
func TestGroupRouting_令牌分组决定候选渠道(t *testing.T) {
	var freeHits, paidHits int32
	freeUpstream := countingUpstream(t, &freeHits)
	paidUpstream := countingUpstream(t, &paidHits)

	repo := newTestRepo(t)
	addChannelInGroup(t, repo, freeUpstream.URL, "free", []string{"gpt-4o"}, 10)
	addChannelInGroup(t, repo, paidUpstream.URL, "paid", []string{"gpt-4o"}, 10)
	rl := newRelay(repo)

	ctx := reqctx.WithGroup(context.Background(), "free")
	group := rl.groupFromContext(ctx)
	if group != "free" {
		t.Fatalf("令牌分组应生效，实际解析为 %q", group)
	}

	// 候选集只含 free 分组的渠道
	candidates, err := rl.listCandidates(ctx, group, "gpt-4o")
	if err != nil {
		t.Fatalf("查询候选渠道失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("候选渠道数 = %d，期望 1（只应含 free 分组）", len(candidates))
	}
	for _, ch := range candidates {
		if ch.Group != "free" {
			t.Fatalf("候选里混入了分组 %q 的渠道（应为 free）", ch.Group)
		}
	}

	// 端到端：free 分组请求命中 free 渠道，paid 渠道零命中
	rec := serveWithGroup(rl, "free", `{"model":"gpt-4o"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("转发状态码 = %d，期望 200", rec.Code)
	}
	if got := atomic.LoadInt32(&freeHits); got != 1 {
		t.Fatalf("free 分组渠道命中 %d 次，期望 1", got)
	}
	if got := atomic.LoadInt32(&paidHits); got != 0 {
		t.Fatalf("paid 分组渠道命中 %d 次，期望 0（分组越界）", got)
	}
}

// TestGroupRouting_价格规则按分组隔离 验证同一模型在 A 分组计费、在 B 分组不计费。
//
// 直接对应第 3 点的实现风险：若价格缓存不隔离，B 分组会读到 A 分组的价格而误扣费。
func TestGroupRouting_价格规则按分组隔离(t *testing.T) {
	ctx := context.Background()

	// 只有 A 分组配了该模型的价格（每 1M 输入 token 收 1_000_000 额度）
	prices := &fakePriceRepo{prices: []*model.ModelPrice{{
		ID: 1, Model: "gpt-4o", PromptPrice: 1_000_000, Group: "A", Enabled: true,
	}}}
	billing := NewBilling(prices, newFakeGroupRepo(100), nil, nil, "default")

	// A 分组：命中价格，1000 token → 1000 额度
	if got := billing.Quote(ctx, "A", "gpt-4o", 1000, 0); got != 1000 {
		t.Fatalf("A 分组应扣 1000，实际 %d", got)
	}
	// B 分组：无价格规则 → 不计费（绝不串用 A 分组的价格）
	if got := billing.Quote(ctx, "B", "gpt-4o", 1000, 0); got != 0 {
		t.Fatalf("B 分组无价格规则应不计费，实际 %d（价格串组）", got)
	}
}

// recordingPriceRepo 记录每次 List 请求的分组，用于验证"缓存按分组各查一次库"。
type recordingPriceRepo struct {
	prices []*model.ModelPrice

	mu     sync.Mutex
	listed []string
}

func (r *recordingPriceRepo) Create(context.Context, *model.ModelPrice) error { return nil }

func (r *recordingPriceRepo) GetByID(context.Context, uint64) (*model.ModelPrice, error) {
	return nil, model.ErrModelPriceNotFound
}

func (r *recordingPriceRepo) List(_ context.Context, group string, enabledOnly bool) ([]*model.ModelPrice, error) {
	r.mu.Lock()
	r.listed = append(r.listed, group)
	r.mu.Unlock()

	result := make([]*model.ModelPrice, 0, len(r.prices))
	for _, price := range r.prices {
		if group != "" && price.Group != group {
			continue
		}
		if enabledOnly && !price.Enabled {
			continue
		}
		result = append(result, price)
	}
	return result, nil
}

func (r *recordingPriceRepo) Update(context.Context, *model.ModelPrice) error { return nil }
func (r *recordingPriceRepo) Delete(context.Context, uint64) error            { return nil }

// queriedGroups 返回按顺序记录到的 List 分组副本。
func (r *recordingPriceRepo) queriedGroups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.listed))
	copy(out, r.listed)
	return out
}

// TestGroupRouting_价格缓存按分组隔离 验证"先请求 A 填充缓存，再请求 B 不会读到 A 的缓存"。
//
// 断言方式有两重：
//  1. 行为：B 返回 B 自己的价格（777），而不是 A 的价格（100）——缓存串了就会立刻暴露；
//  2. 调用：B 确实触发了一次对 B 分组的读库（若命中 A 的缓存则不会有这次查询）。
func TestGroupRouting_价格缓存按分组隔离(t *testing.T) {
	ctx := context.Background()

	repo := &recordingPriceRepo{prices: []*model.ModelPrice{
		{ID: 1, Model: "img", PerCallPrice: 100, Group: "A", Enabled: true},
		{ID: 2, Model: "img", PerCallPrice: 777, Group: "B", Enabled: true},
	}}
	billing := NewBilling(repo, newFakeGroupRepo(100), nil, nil, "default")

	// 先请求 A：填充 A 的缓存
	if got := billing.QuoteOnce(ctx, "A", "img", 1); got != 100 {
		t.Fatalf("A 分组应扣 100，实际 %d", got)
	}
	// 再请求 B：必须读到 B 自己的规则
	if got := billing.QuoteOnce(ctx, "B", "img", 1); got != 777 {
		t.Fatalf("B 分组应扣 777，实际 %d（读到了 A 的缓存）", got)
	}

	// A 的缓存未被 B 覆盖：再次请求 A 仍是 100
	if got := billing.QuoteOnce(ctx, "A", "img", 1); got != 100 {
		t.Fatalf("A 分组缓存被污染，实际 %d，期望 100", got)
	}

	// 两次不同分组都应各自读库一次（缓存键按分组区分）
	queried := repo.queriedGroups()
	var sawA, sawB bool
	for _, g := range queried {
		if g == "A" {
			sawA = true
		}
		if g == "B" {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Fatalf("缓存未按分组隔离：读库分组记录 = %v，期望同时出现 A 与 B", queried)
	}
}

// TestGroupRouting_未知分组安全降级 验证不存在的分组不 panic：
// 渠道侧返回"无可用渠道"，计费侧倍率回退默认并记录告警。
func TestGroupRouting_未知分组安全降级(t *testing.T) {
	// ── 渠道侧：不存在的分组 → 无可用渠道错误 ──
	repo := newTestRepo(t)
	addChannelInGroup(t, repo, "https://only.example.com", defaultGroup, nil, 10)
	rl := newRelay(repo)

	if _, err := rl.SelectChannel(context.Background(), "ghost", "any-model"); err == nil {
		t.Fatal("不存在的分组应返回错误，而不是选中其他分组的渠道")
	} else if !errors.Is(err, ErrNoAvailableChannel) {
		t.Fatalf("应为 ErrNoAvailableChannel，实际 %v", err)
	}
	// 未知分组不影响默认分组
	if _, err := rl.SelectChannel(context.Background(), defaultGroup, "any-model"); err != nil {
		t.Fatalf("默认分组应仍可用，实际报错: %v", err)
	}

	// ── 计费侧：分组不存在 → 倍率回退默认（100）并告警 ──
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(oldLogger)

	prices := &fakePriceRepo{prices: []*model.ModelPrice{{
		ID: 1, Model: "img", PerCallPrice: 100, Group: "ghost", Enabled: true,
	}}}
	// 分组仓储里没有 ghost
	billing := NewBilling(prices, &fakeGroupRepo{groups: map[string]*model.ModelGroup{}}, nil, nil, "default")

	if got := billing.QuoteOnce(context.Background(), "ghost", "img", 1); got != 100 {
		t.Fatalf("未知分组应按默认倍率计费（100），实际 %d", got)
	}
	if !strings.Contains(logs.String(), "计费分组不存在") || !strings.Contains(logs.String(), "ghost") {
		t.Fatalf("未知分组应记录告警，实际日志: %s", logs.String())
	}
}
