// 计价规则仓储与额度扣减的单元测试。
//
// 测试重点：
//   - 规则唯一性：同分组同模型不允许两条，否则"哪条生效"会变成玄学；
//   - 列表顺序：必须"具体 → 笼统"，让管理员一眼看出优先级；
//   - 额度扣减的原子性：并发扣费不能丢计数（这是计费系统最典型的漏账缺陷）。
package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestPriceRepo 构造基于临时数据库的计价规则仓储。
func newTestPriceRepo(t *testing.T) model.ModelPriceRepository {
	t.Helper()
	st := newTestStore(t)
	return NewModelPriceRepository(st.DB())
}

func newPrice(modelName string, prompt, completion int64) *model.ModelPrice {
	return &model.ModelPrice{
		Model:           modelName,
		PromptPrice:     prompt,
		CompletionPrice: completion,
		Group:           "default",
		Enabled:         true,
	}
}

func TestModelPriceRepository_CreateAndGet(t *testing.T) {
	repo := newTestPriceRepo(t)
	ctx := context.Background()

	price := newPrice("gpt-4o", 3_000_000, 15_000_000)
	if err := repo.Create(ctx, price); err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	if price.ID == 0 {
		t.Fatal("新增后应回填 ID")
	}

	got, err := repo.GetByID(ctx, price.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.Model != "gpt-4o" || got.PromptPrice != 3_000_000 || got.CompletionPrice != 15_000_000 {
		t.Fatalf("读回的数据不一致: %+v", got)
	}
	if !got.Enabled {
		t.Fatal("启用状态应被保留")
	}
}

func TestModelPriceRepository_同分组同名_应被拒绝(t *testing.T) {
	repo := newTestPriceRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, newPrice("gpt-4o", 1, 1)); err != nil {
		t.Fatalf("首次新增失败: %v", err)
	}
	err := repo.Create(ctx, newPrice("gpt-4o", 2, 2))
	if !errors.Is(err, model.ErrModelPriceDuplicated) {
		t.Fatalf("重复规则应返回 ErrModelPriceDuplicated，实际 %v", err)
	}

	// 不同分组可以同名
	other := newPrice("gpt-4o", 9, 9)
	other.Group = "vip"
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("不同分组应允许同名: %v", err)
	}
}

func TestModelPriceRepository_List_具体优先排序(t *testing.T) {
	repo := newTestPriceRepo(t)
	ctx := context.Background()

	for _, name := range []string{"*", "gpt-*", "gpt-4o"} {
		if err := repo.Create(ctx, newPrice(name, 1, 1)); err != nil {
			t.Fatalf("新增 %s 失败: %v", name, err)
		}
	}
	// 另一分组的规则不应出现在 default 的列表里
	other := newPrice("claude-*", 1, 1)
	other.Group = "other"
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("新增分组规则失败: %v", err)
	}

	list, err := repo.List(ctx, "default", false)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应返回 3 条 default 规则，实际 %d", len(list))
	}
	if list[0].Model != "gpt-4o" {
		t.Fatalf("首条应为精确规则，实际 %q", list[0].Model)
	}
	if list[len(list)-1].Model != "*" {
		t.Fatalf("末条应为全局通配，实际 %q", list[len(list)-1].Model)
	}
}

func TestModelPriceRepository_List_仅启用(t *testing.T) {
	repo := newTestPriceRepo(t)
	ctx := context.Background()

	enabled := newPrice("gpt-4o", 1, 1)
	disabled := newPrice("gpt-3.5", 1, 1)
	disabled.Enabled = false
	if err := repo.Create(ctx, enabled); err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	if err := repo.Create(ctx, disabled); err != nil {
		t.Fatalf("新增失败: %v", err)
	}

	all, err := repo.List(ctx, "", false)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("不过滤时应返回 2 条，实际 %d", len(all))
	}

	onlyEnabled, err := repo.List(ctx, "", true)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(onlyEnabled) != 1 || onlyEnabled[0].Model != "gpt-4o" {
		t.Fatalf("仅启用应返回 1 条 gpt-4o，实际 %+v", onlyEnabled)
	}
}

func TestModelPriceRepository_Update与Delete(t *testing.T) {
	repo := newTestPriceRepo(t)
	ctx := context.Background()

	price := newPrice("gpt-4o", 1, 1)
	if err := repo.Create(ctx, price); err != nil {
		t.Fatalf("新增失败: %v", err)
	}

	price.PromptPrice = 12345
	price.Enabled = false
	if err := repo.Update(ctx, price); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	got, _ := repo.GetByID(ctx, price.ID)
	if got.PromptPrice != 12345 || got.Enabled {
		t.Fatalf("更新未生效: %+v", got)
	}

	if err := repo.Delete(ctx, price.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := repo.GetByID(ctx, price.ID); !errors.Is(err, model.ErrModelPriceNotFound) {
		t.Fatalf("删除后应返回 ErrModelPriceNotFound，实际 %v", err)
	}
	// 重复删除应明确报"不存在"，而不是静默成功
	if err := repo.Delete(ctx, price.ID); !errors.Is(err, model.ErrModelPriceNotFound) {
		t.Fatalf("重复删除应返回 ErrModelPriceNotFound，实际 %v", err)
	}
}

// newQuotaToken 造一个指定额度的令牌。
//
// 复用 token_repo_test.go 中的 newTestTokenRepo（签名一致），避免重复定义。
func newQuotaToken(t *testing.T, repo model.TokenRepository, unlimited bool, remain int64) *model.Token {
	t.Helper()
	key, err := model.GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	token := &model.Token{
		OwnerID:        1,
		Name:           "额度测试",
		Key:            key,
		Status:         model.TokenStatusEnabled,
		RemainQuota:    remain,
		UnlimitedQuota: unlimited,
	}
	if err := repo.Create(context.Background(), token); err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}
	return token
}

func TestTokenRepository_ConsumeQuota_扣减剩余并累加已用(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	token := newQuotaToken(t, repo, false, 1000)

	if err := repo.ConsumeQuota(ctx, token.ID, 300, time.Now()); err != nil {
		t.Fatalf("扣费失败: %v", err)
	}

	got, err := repo.GetByID(ctx, token.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.RemainQuota != 700 {
		t.Fatalf("剩余额度应为 700，实际 %d", got.RemainQuota)
	}
	if got.UsedQuota != 300 {
		t.Fatalf("已用额度应为 300，实际 %d", got.UsedQuota)
	}
}

func TestTokenRepository_ConsumeQuota_不限额度只累加已用(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	token := newQuotaToken(t, repo, true, 0)
	if err := repo.ConsumeQuota(ctx, token.ID, 500, time.Now()); err != nil {
		t.Fatalf("扣费失败: %v", err)
	}

	got, _ := repo.GetByID(ctx, token.ID)
	if got.UsedQuota != 500 {
		t.Fatalf("已用额度应累加，实际 %d", got.UsedQuota)
	}
	if got.RemainQuota != 0 {
		t.Fatalf("不限额度令牌的剩余额度不应被改动，实际 %d", got.RemainQuota)
	}
}

func TestTokenRepository_ConsumeQuota_扣到零不为负(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	token := newQuotaToken(t, repo, false, 100)
	if err := repo.ConsumeQuota(ctx, token.ID, 999, time.Now()); err != nil {
		t.Fatalf("扣费失败: %v", err)
	}

	got, _ := repo.GetByID(ctx, token.ID)
	if got.RemainQuota != 0 {
		t.Fatalf("剩余额度应停在 0，实际 %d（负数额度会导致后续判定混乱）", got.RemainQuota)
	}
	if got.UsedQuota != 999 {
		t.Fatalf("已用额度应如实累加，实际 %d", got.UsedQuota)
	}
}

func TestTokenRepository_ConsumeQuota_零额度不写库(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	token := newQuotaToken(t, repo, false, 100)
	before, _ := repo.GetByID(ctx, token.ID)

	// amount <= 0 应直接返回，不产生写操作（未定价模型的每次调用都会走到这里）
	if err := repo.ConsumeQuota(ctx, token.ID, 0, time.Now()); err != nil {
		t.Fatalf("零额度扣费不应报错: %v", err)
	}
	after, _ := repo.GetByID(ctx, token.ID)
	if after.UsedQuota != before.UsedQuota {
		t.Fatalf("零额度不应改变已用额度")
	}
}

func TestTokenRepository_ConsumeQuota_并发扣费不丢计数(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	token := newQuotaToken(t, repo, false, 100_000)

	const goroutines = 50
	const perCall = 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := repo.ConsumeQuota(ctx, token.ID, perCall, time.Now()); err != nil {
				t.Errorf("并发扣费失败: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := repo.GetByID(ctx, token.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	wantUsed := int64(goroutines * perCall)
	if got.UsedQuota != wantUsed {
		t.Fatalf("并发扣费应累计 %d，实际 %d（说明存在计数丢失，属于计费系统严重缺陷）",
			wantUsed, got.UsedQuota)
	}
	if got.RemainQuota != 100_000-wantUsed {
		t.Fatalf("剩余额度应为 %d，实际 %d", 100_000-wantUsed, got.RemainQuota)
	}
}

func TestModelPriceRepository_价格校验拦截负数(t *testing.T) {
	repo := newTestPriceRepo(t)
	ctx := context.Background()

	bad := newPrice("bad-model", -1, 0)
	err := repo.Create(ctx, bad)
	if err == nil {
		t.Fatal("负数价格应被拒绝（否则调用会反过来增加额度）")
	}
	if !strings.Contains(err.Error(), "负数") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
}
