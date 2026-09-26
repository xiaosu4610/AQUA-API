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
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
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
