// 计费组件（含分组倍率）的单元测试。
//
// 测试重点：
//   - 倍率换算必须用整数、向下取整（浮点或四舍五入会让账目长期对不上）；
//   - 倍率为 100 时数值完全不变（默认分组不得引入任何误差）；
//   - 分组不存在时按 1.0 倍处理（历史数据的兼容行为）；
//   - 价格与倍率来自同一份缓存，避免"新价格 × 旧倍率"的中间态。
package relay

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
)

// fakePriceRepo 是内存版计价规则仓储。
type fakePriceRepo struct {
	prices []*model.ModelPrice
}

func (f *fakePriceRepo) Create(context.Context, *model.ModelPrice) error { return nil }

func (f *fakePriceRepo) GetByID(context.Context, uint64) (*model.ModelPrice, error) {
	return nil, model.ErrModelPriceNotFound
}

func (f *fakePriceRepo) List(_ context.Context, group string, enabledOnly bool) ([]*model.ModelPrice, error) {
	result := make([]*model.ModelPrice, 0, len(f.prices))
	for _, price := range f.prices {
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

func (f *fakePriceRepo) Update(context.Context, *model.ModelPrice) error { return nil }
func (f *fakePriceRepo) Delete(context.Context, uint64) error            { return nil }

// fakeGroupRepo 是内存版分组仓储。
type fakeGroupRepo struct {
	groups map[string]*model.ModelGroup
}

func newFakeGroupRepo(ratio int64) *fakeGroupRepo {
	return &fakeGroupRepo{groups: map[string]*model.ModelGroup{
		"default": {ID: 1, Name: "default", DisplayName: "默认分组", Ratio: ratio, Enabled: true},
	}}
}

func (f *fakeGroupRepo) Create(context.Context, *model.ModelGroup) error { return nil }

func (f *fakeGroupRepo) GetByName(_ context.Context, name string) (*model.ModelGroup, error) {
	group, ok := f.groups[name]
	if !ok {
		return nil, model.ErrModelGroupNotFound
	}
	return group, nil
}

func (f *fakeGroupRepo) List(context.Context, model.ModelGroupQuery) ([]*model.ModelGroup, error) {
	result := make([]*model.ModelGroup, 0, len(f.groups))
	for _, group := range f.groups {
		result = append(result, group)
	}
	return result, nil
}

func (f *fakeGroupRepo) Count(context.Context, model.ModelGroupQuery) (int64, error) {
	return int64(len(f.groups)), nil
}

func (f *fakeGroupRepo) Update(context.Context, *model.ModelGroup) error { return nil }
func (f *fakeGroupRepo) Delete(context.Context, uint64) error            { return nil }

// newTestBilling 构造带固定价格的计费组件。
func newTestBilling(ratio int64, promptPrice, completionPrice, perCallPrice int64) *Billing {
	prices := &fakePriceRepo{prices: []*model.ModelPrice{{
		ID:              1,
		Model:           "test-model",
		PromptPrice:     promptPrice,
		CompletionPrice: completionPrice,
		PerCallPrice:    perCallPrice,
		Group:           "default",
		Enabled:         true,
	}}}
	return NewBilling(prices, newFakeGroupRepo(ratio), nil, nil, "default")
}

func TestApplyRatio_整数换算与向下取整(t *testing.T) {
	cases := []struct {
		base, ratio, want int64
	}{
		{1000, 100, 1000}, // 1.0 倍不变
		{1000, 150, 1500}, // 1.5 倍
		{1000, 50, 500},   // 0.5 倍
		{101, 150, 151},   // 151.5 → 151（向下取整，不多收）
		{1, 50, 0},        // 0.5 → 0（不足 1 个额度不计费）
		{1000, 0, 1000},   // 非法倍率不改变数值（不做静默放大）
		{0, 150, 0},       // 基础额为 0 时保持 0
	}
	for _, item := range cases {
		if got := applyRatio(item.base, item.ratio); got != item.want {
			t.Errorf("applyRatio(%d, %d) = %d，期望 %d", item.base, item.ratio, got, item.want)
		}
	}
}

func TestBilling_QuoteOnce_按倍率计价(t *testing.T) {
	ctx := context.Background()

	// 倍率 200（2.0 倍），每次 500 额度
	billing := newTestBilling(200, 0, 0, 500)
	if got := billing.QuoteOnce(ctx, "test-model", 1); got != 1000 {
		t.Fatalf("2.0 倍下每次应扣 1000，实际 %d", got)
	}
	if got := billing.QuoteOnce(ctx, "test-model", 3); got != 3000 {
		t.Fatalf("2.0 倍下三次应扣 3000，实际 %d", got)
	}

	// 倍率 100 时与未配置倍率完全一致
	plain := newTestBilling(100, 0, 0, 500)
	if got := plain.QuoteOnce(ctx, "test-model", 1); got != 500 {
		t.Fatalf("1.0 倍下每次应扣 500，实际 %d", got)
	}
}

func TestBilling_Quote_按倍率计价(t *testing.T) {
	ctx := context.Background()

	// 每 1M token 输入 1_000_000 额度、输出 2_000_000 额度
	billing := newTestBilling(150, 1_000_000, 2_000_000, 0)

	// 1000 输入 + 500 输出 = 1000 + 1000 = 2000 基础额度；×1.5 = 3000
	if got := billing.Quote(ctx, "test-model", 1000, 500); got != 3000 {
		t.Fatalf("1.5 倍下应扣 3000，实际 %d", got)
	}
}

func TestBilling_未定价模型不扣费(t *testing.T) {
	ctx := context.Background()
	billing := newTestBilling(150, 1_000_000, 2_000_000, 0)

	if got := billing.Quote(ctx, "不存在的模型", 1000, 1000); got != 0 {
		t.Fatalf("未定价模型不应扣费，实际 %d", got)
	}
	if got := billing.QuoteOnce(ctx, "不存在的模型", 1); got != 0 {
		t.Fatalf("未定价模型按次也不应扣费，实际 %d", got)
	}
}

func TestBilling_分组不存在时按一倍处理(t *testing.T) {
	ctx := context.Background()

	prices := &fakePriceRepo{prices: []*model.ModelPrice{{
		ID: 1, Model: "test-model", PromptPrice: 1_000_000, Group: "default", Enabled: true,
	}}}
	// 分组仓储里没有 default（模拟历史数据里未登记的分组）
	billing := NewBilling(prices, &fakeGroupRepo{groups: map[string]*model.ModelGroup{}}, nil, nil, "default")

	if got := billing.Quote(ctx, "test-model", 1000, 0); got != 1000 {
		t.Fatalf("分组不存在时应按 1.0 倍处理（1000），实际 %d", got)
	}
}

func TestBilling_Invalidate后重新读取倍率(t *testing.T) {
	ctx := context.Background()

	prices := &fakePriceRepo{prices: []*model.ModelPrice{{
		ID: 1, Model: "test-model", PerCallPrice: 100, Group: "default", Enabled: true,
	}}}
	groups := newFakeGroupRepo(100)
	billing := NewBilling(prices, groups, nil, nil, "default")

	if got := billing.QuoteOnce(ctx, "test-model", 1); got != 100 {
		t.Fatalf("初始应扣 100，实际 %d", got)
	}

	// 管理员把倍率改成 300 并清缓存：应立即按新倍率计价
	groups.groups["default"].Ratio = 300
	billing.Invalidate()

	if got := billing.QuoteOnce(ctx, "test-model", 1); got != 300 {
		t.Fatalf("改倍率并清缓存后应扣 300，实际 %d", got)
	}
}

func TestBilling_无分组仓储时倍率恒为一倍(t *testing.T) {
	ctx := context.Background()

	prices := &fakePriceRepo{prices: []*model.ModelPrice{{
		ID: 1, Model: "test-model", PerCallPrice: 100, Group: "default", Enabled: true,
	}}}
	// groups 传 nil：用于单元测试与"仅统计不限制"的部署形态
	billing := NewBilling(prices, nil, nil, nil, "default")

	if got := billing.QuoteOnce(ctx, "test-model", 1); got != 100 {
		t.Fatalf("无分组仓储时应扣 100，实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 额度预扣 / 结算 / 退还
// ---------------------------------------------------------------------------

// fakeQuotaRepo 是内存版额度预留台账，用于验证计费组件是否正确委托。
type fakeQuotaRepo struct {
	mu           sync.Mutex
	nextID       uint64
	byRequestID  map[string]*model.QuotaReservation
	reserveCalls int
	settleCalls  int
	releaseCalls int
}

func newFakeQuotaRepo() *fakeQuotaRepo {
	return &fakeQuotaRepo{byRequestID: map[string]*model.QuotaReservation{}}
}

func (f *fakeQuotaRepo) Reserve(_ context.Context, req model.ReserveRequest) (*model.QuotaReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	if existing, ok := f.byRequestID[req.RequestID]; ok {
		return existing, nil
	}
	f.nextID++
	item := &model.QuotaReservation{
		ID: f.nextID, RequestID: req.RequestID, UserID: req.UserID, TokenID: req.TokenID,
		Reserved: req.Amount, Status: model.ReservationInFlight,
	}
	f.byRequestID[req.RequestID] = item
	return item, nil
}

func (f *fakeQuotaRepo) Settle(_ context.Context, requestID string, actualQuota int64) (*model.QuotaReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settleCalls++
	item, ok := f.byRequestID[requestID]
	if !ok {
		return nil, model.ErrReservationNotFound
	}
	if item.Status != model.ReservationInFlight {
		return item, nil
	}
	item.Status = model.ReservationSettled
	if actualQuota < 0 {
		item.Settled = item.Reserved
	} else {
		item.Settled = actualQuota
	}
	return item, nil
}

func (f *fakeQuotaRepo) Release(_ context.Context, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	if item, ok := f.byRequestID[requestID]; ok && item.Status == model.ReservationInFlight {
		item.Status = model.ReservationReleased
	}
	return nil
}

func (f *fakeQuotaRepo) GetByRequestID(_ context.Context, requestID string) (*model.QuotaReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.byRequestID[requestID]
	if !ok {
		return nil, model.ErrReservationNotFound
	}
	return item, nil
}

func (f *fakeQuotaRepo) PendingAmount(_ context.Context, userID uint64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, item := range f.byRequestID {
		if item.UserID == userID && item.Status == model.ReservationInFlight {
			total += item.Reserved
		}
	}
	return total, nil
}

func (f *fakeQuotaRepo) CleanupExpired(context.Context, time.Time) (int, error) { return 0, nil }

// TestBilling_EstimateReserve_未定价模型不预留 验证不计费模型返回 priced=false。
//
// 这是"不计费模型跳过预留"的计费侧依据：估算口以价格规则为前提，
// 未命中价格时绝不产生预留，避免免费模型被额度墙挡住。
func TestBilling_EstimateReserve_未定价模型不预留(t *testing.T) {
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0)

	if amount, priced := billing.EstimateReserve(context.Background(), "未定价模型", 300); priced || amount != 0 {
		t.Fatalf("未定价模型应返回 (0, false)，实际 (%d, %v)", amount, priced)
	}
}

// TestBilling_EstimateReserve_定价模型预留为正 验证估算按价格与倍率给出正数预留。
func TestBilling_EstimateReserve_定价模型预留为正(t *testing.T) {
	// 输入 1 额度/1M token、输出 2 额度/1M token、倍率 1.0。
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0)

	// 请求体 300 字节 → 估算 100 token；假设输入输出同量级：
	// (100×1_000_000 + 100×2_000_000) / 1_000_000 = 300。
	amount, priced := billing.EstimateReserve(context.Background(), "test-model", 300)
	if !priced {
		t.Fatal("已定价模型应返回 priced=true")
	}
	if amount != 300 {
		t.Fatalf("估算预留 = %d，期望 300", amount)
	}
}

// TestBilling_预留结算释放_委托台账 验证三个方法正确委托给台账。
func TestBilling_预留结算释放_委托台账(t *testing.T) {
	ctx := context.Background()
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0)
	fake := newFakeQuotaRepo()
	if billing.WithQuotaRepository(fake) != billing {
		t.Fatal("WithQuotaRepository 应返回自身以支持链式装配")
	}

	res, err := billing.Reserve(ctx, model.ReserveRequest{
		RequestID: "r1", UserID: 1, TokenID: 2, Amount: 10,
	})
	if err != nil {
		t.Fatalf("Reserve 失败: %v", err)
	}
	if res == nil || res.Reserved != 10 || fake.reserveCalls != 1 {
		t.Fatalf("Reserve 未正确委托：res=%+v calls=%d", res, fake.reserveCalls)
	}

	settled, err := billing.Settle(ctx, "r1", 4)
	if err != nil {
		t.Fatalf("Settle 失败: %v", err)
	}
	if settled == nil || settled.Settled != 4 || fake.settleCalls != 1 {
		t.Fatalf("Settle 未正确委托：settled=%+v calls=%d", settled, fake.settleCalls)
	}

	// 另一条预留用于验证释放。
	if _, err := billing.Reserve(ctx, model.ReserveRequest{RequestID: "r2", UserID: 1, Amount: 7}); err != nil {
		t.Fatalf("Reserve(r2) 失败: %v", err)
	}
	if err := billing.Release(ctx, "r2"); err != nil {
		t.Fatalf("Release 失败: %v", err)
	}
	if fake.releaseCalls != 1 {
		t.Fatalf("Release 未正确委托：calls=%d", fake.releaseCalls)
	}
}

// TestBilling_PendingReserved_委托台账 验证在途预留统计被正确委托。
func TestBilling_PendingReserved_委托台账(t *testing.T) {
	ctx := context.Background()
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0).WithQuotaRepository(newFakeQuotaRepo())

	if _, err := billing.Reserve(ctx, model.ReserveRequest{RequestID: "p1", UserID: 9, Amount: 10}); err != nil {
		t.Fatalf("Reserve 失败: %v", err)
	}
	if _, err := billing.Reserve(ctx, model.ReserveRequest{RequestID: "p2", UserID: 9, Amount: 20}); err != nil {
		t.Fatalf("Reserve 失败: %v", err)
	}

	pending, err := billing.PendingReserved(ctx, 9)
	if err != nil {
		t.Fatalf("PendingReserved 失败: %v", err)
	}
	if pending != 30 {
		t.Fatalf("在途预留 = %d，期望 30", pending)
	}
}

// TestBilling_未注入台账时均为无操作 验证未配置预留台账时三方法安全降级。
func TestBilling_未注入台账时均为无操作(t *testing.T) {
	ctx := context.Background()
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0)

	if res, err := billing.Reserve(ctx, model.ReserveRequest{RequestID: "x"}); res != nil || err != nil {
		t.Fatalf("未注入台账时 Reserve 应返回 (nil, nil)，实际 (%v, %v)", res, err)
	}
	if res, err := billing.Settle(ctx, "x", 1); res != nil || err != nil {
		t.Fatalf("未注入台账时 Settle 应返回 (nil, nil)，实际 (%v, %v)", res, err)
	}
	if err := billing.Release(ctx, "x"); err != nil {
		t.Fatalf("未注入台账时 Release 应返回 nil，实际 %v", err)
	}
	if pending, err := billing.PendingReserved(ctx, 1); pending != 0 || err != nil {
		t.Fatalf("未注入台账时 PendingReserved 应返回 (0, nil)，实际 (%d, %v)", pending, err)
	}
}

// TestBilling_结算接入_成功多退_失败退还_未知用量按预留收 覆盖 usage.go 的结算分流。
//
// 这是 requirement「结算接入」的回归：成功走 Settle（多退少补），
// 失败走 Release（全额退还），拿不到 usage 时按预留量收（不退成 0）。
func TestBilling_结算接入_成功多退_失败退还_未知用量按预留收(t *testing.T) {
	ctx := context.Background()
	fake := newFakeQuotaRepo()
	// 输入 1 额度/1M token、输出 2 额度/1M token、倍率 1.0。
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0).WithQuotaRepository(fake)
	r := &Relay{billing: billing}

	// 场景一：成功且取得 usage → 结算（按实际用量多退）。
	if _, err := billing.Reserve(ctx, model.ReserveRequest{RequestID: "ok", UserID: 1, TokenID: 2, Amount: 1000}); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	ctxOK := reqctx.WithIdentity(ctx, reqctx.Identity{UserID: 1, TokenID: 2, RequestID: "ok"})
	// (100×1_000_000 + 100×2_000_000) / 1_000_000 = 300。
	got := r.settleQuota(ctxOK, usageEntry{
		Model:      "test-model",
		StatusCode: 200,
		Usage:      openAIUsage{PromptTokens: 100, CompletionTokens: 100, TotalTokens: 200},
	})
	if got != 300 {
		t.Fatalf("成功结算额度 = %d，期望 300（多退）", got)
	}
	if item := fake.byRequestID["ok"]; item.Status != model.ReservationSettled || item.Settled != 300 {
		t.Fatalf("台账未结算：status=%v settled=%d", item.Status, item.Settled)
	}

	// 场景二：请求失败 → 释放（全额退还，日志额度为 0）。
	if _, err := billing.Reserve(ctx, model.ReserveRequest{RequestID: "fail", UserID: 1, TokenID: 2, Amount: 1000}); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	ctxFail := reqctx.WithIdentity(ctx, reqctx.Identity{UserID: 1, TokenID: 2, RequestID: "fail"})
	got = r.settleQuota(ctxFail, usageEntry{Model: "test-model", StatusCode: 502, ErrorText: "上游失败"})
	if got != 0 {
		t.Fatalf("失败请求计入额度 = %d，期望 0", got)
	}
	if item := fake.byRequestID["fail"]; item.Status != model.ReservationReleased {
		t.Fatalf("失败请求应释放预留，实际 status=%v", item.Status)
	}

	// 场景三：成功但未取得 usage → 按预留量收（不退成 0）。
	if _, err := billing.Reserve(ctx, model.ReserveRequest{RequestID: "nousage", UserID: 1, TokenID: 2, Amount: 77}); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	ctxNo := reqctx.WithIdentity(ctx, reqctx.Identity{UserID: 1, TokenID: 2, RequestID: "nousage"})
	got = r.settleQuota(ctxNo, usageEntry{Model: "test-model", StatusCode: 200})
	if got != 77 {
		t.Fatalf("未取得 usage 时应按预留量收 77，实际 %d", got)
	}

	// 场景四：未做预留（无 requestID）→ 走响应后扣费，不触碰台账。
	before := fake.reserveCalls
	got = r.settleQuota(ctx, usageEntry{
		Model:      "test-model",
		StatusCode: 200,
		Usage:      openAIUsage{PromptTokens: 100, CompletionTokens: 100, TotalTokens: 200},
	})
	if got != 300 {
		t.Fatalf("未预留应走响应后扣费 300，实际 %d", got)
	}
	if fake.reserveCalls != before {
		t.Fatal("未预留的请求不应触碰预留台账")
	}
}
