// 上游凭据池调度算法的单元测试。
//
// 意图（Why）：
//
//	调度策略的 bug 极难通过人工测试发现：池里往往有几百把凭据，
//	"随机选中一把坏的"看起来只是偶发失败，而"轮询游标没推进""冷却不生效"
//	"粘性失效后不清绑定"这类问题会在高并发下被放大成事故。
//	本文件把五种策略的选择行为、冷却与摘除的分界、粘性与在途计数固化成测试。
//
// 流转（Flow）：
//
//	go test ./internal/relay/ -run KeyPicker
//	  ├─ 临时 SQLite（真实迁移 + 真实仓储）
//	  ├─ 直接调用 keyPicker.pick（可控时钟 + 独立粘性表）
//	  └─ 断言选中的凭据、冷却/摘除状态、游标持久化
//
// 扩展（Extend）：
//
//	新增策略时，仿照"策略_预期行为"的命名补充用例，并在 selectByStrategy 加分支。
package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestPicker 构造一个使用可控时钟与独立粘性表的 keyPicker。
//
// 独立粘性表（而非全局表）保证用例之间互不串味；可控时钟让"冷却到期"可被精确验证。
func newTestPicker(repo model.ChannelKeyRepository, now *time.Time) *keyPicker {
	return &keyPicker{
		repo:   repo,
		sticky: newStickyBook(time.Hour, 8),
		now:    func() time.Time { return *now },
	}
}

// loadPool 读取渠道的全部凭据（按 id 升序），用于喂给 keyPicker。
func loadPool(t *testing.T, repo model.ChannelKeyRepository, ctx context.Context, channelID uint64) []*model.ChannelKey {
	t.Helper()
	pool, err := repo.ListByChannel(ctx, channelID)
	if err != nil {
		t.Fatalf("读取凭据池失败: %v", err)
	}
	return pool
}

// addTestChannel 建渠道并导入一组凭据，返回渠道与初始凭据列表（按 id 升序）。
func addTestChannel(t *testing.T, keys []string) (model.ChannelRepository, model.ChannelKeyRepository, *model.Channel, []*model.ChannelKey) {
	t.Helper()
	channels, keyRepo := newTestRepos(t)
	ctx := context.Background()

	ch := addChannel(t, channels, "https://strategy.example.com", "", []string{"test-model"}, 10)
	if _, _, err := keyRepo.ReplaceAll(ctx, ch.ID, keys, nil); err != nil {
		t.Fatalf("导入凭据失败: %v", err)
	}
	return channels, keyRepo, ch, loadPool(t, keyRepo, ctx, ch.ID)
}

func TestKeyPicker_顺序策略_始终取优先级最高ID最小(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"k-a", "k-b", "k-c"})
	ctx := context.Background()
	if err := keys.SetChannelStrategy(ctx, ch.ID, model.KeyStrategySequential); err != nil {
		t.Fatalf("设置策略失败: %v", err)
	}
	// 让第二把优先级最高、第三把次之
	if err := keys.UpdateScheduling(ctx, pool[1].ID, 1, 5, 0); err != nil {
		t.Fatalf("设置优先级失败: %v", err)
	}
	if err := keys.UpdateScheduling(ctx, pool[2].ID, 1, 3, 0); err != nil {
		t.Fatalf("设置优先级失败: %v", err)
	}

	now := time.Now()
	p := newTestPicker(keys, &now)
	for i := 0; i < 5; i++ {
		got, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
		if err != nil {
			t.Fatalf("第 %d 次选择失败: %v", i+1, err)
		}
		if got.ID != pool[1].ID {
			t.Fatalf("顺序策略应始终选优先级最高者（第二把），第 %d 次选中 %d", i+1, got.ID)
		}
	}

	// 优先级相同 → 取 id 最小者
	if err := keys.UpdateScheduling(ctx, pool[1].ID, 1, 0, 0); err != nil {
		t.Fatalf("重置优先级失败: %v", err)
	}
	if err := keys.UpdateScheduling(ctx, pool[2].ID, 1, 0, 0); err != nil {
		t.Fatalf("重置优先级失败: %v", err)
	}
	got, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if got.ID != pool[0].ID {
		t.Fatalf("同优先级应取 id 最小者，实际选中 %d", got.ID)
	}
}

func TestKeyPicker_轮询策略_游标推进并持久化(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"k1", "k2", "k3"})
	ctx := context.Background()
	if err := keys.SetChannelStrategy(ctx, ch.ID, model.KeyStrategyRoundRobin); err != nil {
		t.Fatalf("设置策略失败: %v", err)
	}

	now := time.Now()
	p := newTestPicker(keys, &now)

	// 依次应命中 id 升序的第 0/1/2/0 把
	want := []uint64{pool[0].ID, pool[1].ID, pool[2].ID, pool[0].ID}
	for i, wantID := range want {
		got, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
		if err != nil {
			t.Fatalf("第 %d 次选择失败: %v", i+1, err)
		}
		if got.ID != wantID {
			t.Fatalf("第 %d 次轮询应选 %d，实际 %d", i+1, wantID, got.ID)
		}
	}

	// 游标应已持久化：每次选中后自增，共 4 次
	_, cursor, err := keys.ChannelKeyStrategy(ctx, ch.ID)
	if err != nil {
		t.Fatalf("读取游标失败: %v", err)
	}
	if cursor != 4 {
		t.Fatalf("轮询游标应持久化为 4，实际 %d", cursor)
	}
}

func TestKeyPicker_加权随机_权重全零退化等概率(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"k1", "k2", "k3"})
	ctx := context.Background()
	if err := keys.SetChannelStrategy(ctx, ch.ID, model.KeyStrategyWeightedRandom); err != nil {
		t.Fatalf("设置策略失败: %v", err)
	}
	// 权重全设为 0
	for _, k := range pool {
		if err := keys.UpdateScheduling(ctx, k.ID, 0, 0, 0); err != nil {
			t.Fatalf("清零权重失败: %v", err)
		}
	}

	now := time.Now()
	p := newTestPicker(keys, &now)
	seen := make(map[uint64]int)
	for i := 0; i < 300; i++ {
		got, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
		if err != nil {
			t.Fatalf("选择失败: %v", err)
		}
		seen[got.ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("权重全为 0 时应退化为等概率（三把都应出现过），实际命中 %d 把", len(seen))
	}
}

func TestKeyPicker_加权随机_零权重不参与(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"k1", "k2"})
	ctx := context.Background()
	if err := keys.SetChannelStrategy(ctx, ch.ID, model.KeyStrategyWeightedRandom); err != nil {
		t.Fatalf("设置策略失败: %v", err)
	}
	// 第一把权重 0（不应被选中），第二把权重 100
	if err := keys.UpdateScheduling(ctx, pool[0].ID, 0, 0, 0); err != nil {
		t.Fatalf("设置权重失败: %v", err)
	}
	if err := keys.UpdateScheduling(ctx, pool[1].ID, 100, 0, 0); err != nil {
		t.Fatalf("设置权重失败: %v", err)
	}

	now := time.Now()
	p := newTestPicker(keys, &now)
	for i := 0; i < 200; i++ {
		got, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
		if err != nil {
			t.Fatalf("选择失败: %v", err)
		}
		if got.ID != pool[1].ID {
			t.Fatalf("权重为 0 的凭据不应被选中，第 %d 次选中 %d", i+1, got.ID)
		}
	}
}

func TestKeyPicker_最久未用_优先从未使用(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"k1", "k2"})
	ctx := context.Background()
	if err := keys.SetChannelStrategy(ctx, ch.ID, model.KeyStrategyLeastRecent); err != nil {
		t.Fatalf("设置策略失败: %v", err)
	}

	now := time.Now()
	// 第一把刚用过，第二把从未使用
	if err := keys.MarkUsed(ctx, pool[0].ID, now); err != nil {
		t.Fatalf("记录使用失败: %v", err)
	}
	p := newTestPicker(keys, &now)
	got, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if got.ID != pool[1].ID {
		t.Fatalf("应优先选中从未使用的凭据 %d，实际 %d", pool[1].ID, got.ID)
	}

	// 第二把也用了（更晚），则第一把更久未用 → 应选第一把
	if err := keys.MarkUsed(ctx, pool[1].ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("记录使用失败: %v", err)
	}
	got, err = p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if got.ID != pool[0].ID {
		t.Fatalf("应优先选中更久未用者 %d，实际 %d", pool[0].ID, got.ID)
	}
}

func TestKeyPicker_最少在途_优先在途最少(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"k1", "k2"})
	ctx := context.Background()
	if err := keys.SetChannelStrategy(ctx, ch.ID, model.KeyStrategyLeastInFlight); err != nil {
		t.Fatalf("设置策略失败: %v", err)
	}

	now := time.Now()
	p := newTestPicker(keys, &now)

	// 第二把有 1 个在途 → 应选第一把
	if err := keys.Acquire(ctx, pool[1].ID); err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	got, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if got.ID != pool[0].ID {
		t.Fatalf("应选中在途更少的凭据 %d，实际 %d", pool[0].ID, got.ID)
	}

	// 第一把加到 2 个在途 → 应改选第二把
	_ = keys.Acquire(ctx, pool[0].ID)
	_ = keys.Acquire(ctx, pool[0].ID)
	got, err = p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if got.ID != pool[1].ID {
		t.Fatalf("应选中在途更少的凭据 %d，实际 %d", pool[1].ID, got.ID)
	}
}

func TestKeyPicker_冷却过滤_到期自动恢复(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"only"})
	ctx := context.Background()
	now := time.Now()
	p := newTestPicker(keys, &now)

	// 初始可用
	if _, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), ""); err != nil {
		t.Fatalf("初始应可选: %v", err)
	}

	// 设置 10 分钟冷却
	if err := keys.SetCooldown(ctx, pool[0].ID, now.Add(10*time.Minute), "429"); err != nil {
		t.Fatalf("设置冷却失败: %v", err)
	}
	_, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if !errors.Is(err, ErrNoUsableCredential) {
		t.Fatalf("冷却中应无可用凭据，实际 err=%v", err)
	}

	// 时间推进到冷却之后 → 自动恢复，无需任何额外动作
	now = now.Add(11 * time.Minute)
	if _, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), ""); err != nil {
		t.Fatalf("冷却到期后应自动恢复可用: %v", err)
	}
}

func TestKeyPicker_可用集为空_返回哨兵错误(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"only"})
	ctx := context.Background()

	// 手动禁用该凭据
	if err := keys.UpdateStatus(ctx, pool[0].ID, model.ChannelKeyStatusDisabled); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}

	now := time.Now()
	p := newTestPicker(keys, &now)
	_, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if !errors.Is(err, ErrNoUsableCredential) {
		t.Fatalf("无可用凭据应返回哨兵错误，实际 err=%v", err)
	}
}

func TestKeyPicker_RPM限速_窗口内过滤(t *testing.T) {
	_, keys, ch, pool := addTestChannel(t, []string{"only"})
	ctx := context.Background()

	// 限速 2 次/分钟
	if err := keys.UpdateScheduling(ctx, pool[0].ID, 1, 0, 2); err != nil {
		t.Fatalf("设置限速失败: %v", err)
	}

	now := time.Now()
	p := newTestPicker(keys, &now)
	_ = keys.RecordRequest(ctx, pool[0].ID, now, rpmWindow)
	_ = keys.RecordRequest(ctx, pool[0].ID, now, rpmWindow)

	_, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "")
	if !errors.Is(err, ErrNoUsableCredential) {
		t.Fatalf("窗口内用满限速应无可用凭据，实际 err=%v", err)
	}

	// 窗口过期 → 计数失效，恢复可用
	now = now.Add(61 * time.Second)
	if _, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), ""); err != nil {
		t.Fatalf("限速窗口过期后应恢复可用: %v", err)
	}
}

func TestKeyPicker_粘性命中_与目标失效后清除绑定(t *testing.T) {
	_, keys, ch, _ := addTestChannel(t, []string{"k1", "k2", "k3"})
	ctx := context.Background()

	now := time.Now()
	p := newTestPicker(keys, &now)

	first, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "sess-1")
	if err != nil {
		t.Fatalf("首次选择失败: %v", err)
	}
	second, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "sess-1")
	if err != nil {
		t.Fatalf("第二次选择失败: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("同一会话应命中粘性、选同一把凭据，实际 %d vs %d", second.ID, first.ID)
	}
	if _, ok := p.sticky.get("sess-1", now); !ok {
		t.Fatal("命中后应存在粘性绑定")
	}

	// 让粘性目标不可用（冷却）→ 应放弃粘性改选其他凭据，而不是报错
	if err := keys.SetCooldown(ctx, first.ID, now.Add(10*time.Minute), "429"); err != nil {
		t.Fatalf("设置冷却失败: %v", err)
	}
	third, err := p.pick(ctx, ch.ID, loadPool(t, keys, ctx, ch.ID), "sess-1")
	if err != nil {
		t.Fatalf("粘性目标失效后仍应能选出其他凭据: %v", err)
	}
	if third.ID == first.ID {
		t.Fatal("粘性目标已冷却，不应再返回它")
	}
	// 绑定应已更新为新目标（旧的失效绑定被清除）
	if id, ok := p.sticky.get("sess-1", now); !ok || id != third.ID {
		t.Fatalf("粘性绑定应更新为新选中的凭据 %d，实际 id=%d ok=%v", third.ID, id, ok)
	}
}

func TestStickyBook_TTL与LRU淘汰(t *testing.T) {
	book := newStickyBook(10*time.Minute, 2)
	now := time.Now()

	book.put("a", 1, now)
	book.put("b", 2, now)
	if id, ok := book.get("a", now); !ok || id != 1 {
		t.Fatalf("应命中绑定 a=1，实际 id=%d ok=%v", id, ok)
	}

	// 超容量：淘汰最久未使用的 b
	book.put("c", 3, now)
	if _, ok := book.get("b", now); ok {
		t.Fatal("超容量应淘汰最久未使用的绑定 b")
	}
	if book.len() != 2 {
		t.Fatalf("绑定数量应受容量限制为 2，实际 %d", book.len())
	}

	// TTL 过期
	if _, ok := book.get("c", now.Add(11*time.Minute)); ok {
		t.Fatal("超过 TTL 的绑定应失效")
	}
}

func TestClassifyCredentialFailure_冷却与摘除分界(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		failCount    int
		wantCooldown time.Duration
		wantRemove   bool
	}{
		{name: "429首次冷却", status: http.StatusTooManyRequests, failCount: 0, wantCooldown: 30 * time.Second},
		{name: "429指数退避", status: http.StatusTooManyRequests, failCount: 2, wantCooldown: 120 * time.Second},
		{name: "429触及上限", status: http.StatusTooManyRequests, failCount: 10, wantCooldown: 10 * time.Minute},
		{name: "5xx短冷却", status: http.StatusInternalServerError, failCount: 0, wantCooldown: 15 * time.Second},
		{name: "401长冷却", status: http.StatusUnauthorized, body: `{"error":{"message":"unauthorized"}}`, wantCooldown: 30 * time.Minute},
		{name: "403长冷却", status: http.StatusForbidden, wantCooldown: 30 * time.Minute},
		{name: "永久无效摘除", status: http.StatusUnauthorized, body: `{"error":{"message":"this api key has been revoked"}}`, wantRemove: true},
		{name: "其他状态不处置", status: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action := classifyCredentialFailure(tc.status, []byte(tc.body), tc.failCount)
			if action.Remove != tc.wantRemove {
				t.Errorf("Remove = %v，期望 %v", action.Remove, tc.wantRemove)
			}
			if action.Cooldown != tc.wantCooldown {
				t.Errorf("Cooldown = %v，期望 %v", action.Cooldown, tc.wantCooldown)
			}
		})
	}
}

func TestApplyCredentialFailure_冷却与摘除的分界(t *testing.T) {
	channels, keys, ch, pool := addTestChannel(t, []string{"k1", "k2", "k3"})
	ctx := context.Background()
	r := New(channels, Options{Keys: keys})

	// 429：冷却，但【不摘除】（这是本次改造的核心：临时失败不再永久移出池子）
	r.applyCredentialFailure(ctx, pool[0].ID, 0, http.StatusTooManyRequests, nil)
	got := loadPool(t, keys, ctx, ch.ID)
	if got[0].Status != model.ChannelKeyStatusEnabled {
		t.Fatalf("429 不应摘除凭据，实际状态 %s", got[0].Status)
	}
	if got[0].CooldownUntil.IsZero() {
		t.Fatal("429 应设置冷却截止时间")
	}

	// 401 但响应体无永久无效特征：长冷却，仍不摘除
	r.applyCredentialFailure(ctx, pool[1].ID, 0, http.StatusUnauthorized, []byte(`{"error":{"message":"unauthorized"}}`))
	got = loadPool(t, keys, ctx, ch.ID)
	if got[1].Status != model.ChannelKeyStatusEnabled {
		t.Fatalf("401 不应摘除凭据，实际状态 %s", got[1].Status)
	}
	if got[1].CooldownUntil.IsZero() {
		t.Fatal("401 应设置长冷却")
	}

	// 401 且响应体明确指出已吊销：摘除
	r.applyCredentialFailure(ctx, pool[2].ID, 0, http.StatusUnauthorized, []byte(`{"error":{"message":"api key revoked"}}`))
	got = loadPool(t, keys, ctx, ch.ID)
	if got[2].Status != model.ChannelKeyStatusAutoRemoved {
		t.Fatalf("明确永久无效应摘除凭据，实际状态 %s", got[2].Status)
	}
}

func TestStickySessionKey_无会话头时为空(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := stickySessionKey(req); got != "" {
		t.Fatalf("未携带会话头时应返回空串，实际 %q", got)
	}

	req.Header.Set(credentialSessionHeader, "conversation-abc")
	got := stickySessionKey(req)
	if got == "" {
		t.Fatal("携带会话头时应返回非空哈希")
	}
	// 同一会话应稳定哈希
	if again := stickySessionKey(req); again != got {
		t.Fatal("同一会话标识应得到稳定的哈希")
	}
}
