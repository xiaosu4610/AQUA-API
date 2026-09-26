// 邀请返利 / 每日签到仓储的单元测试。
//
// 测试重点（都是"错了会导致资损或数据不一致"的场景）：
//   - 邀请码唯一且规范（生成的所有邀请码互不相同、长度与字符集正确）；
//   - 懒生成的并发安全（多个请求同时为新用户生成邀请码，必须只写一次、结果一致）；
//   - 按天唯一（同一用户同一天只能签到一次，换一天可以再签）；
//   - 发奖幂等（同一订单/同一注册事由只发一次额度）。
package store

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestReferralRepo 构造邀请仓储与该库的用户仓储。
func newTestReferralRepo(t *testing.T) (*storeHandle, model.ReferralRepository, model.UserRepository) {
	t.Helper()
	st := newTestStore(t)
	return &storeHandle{st: st}, NewReferralRepository(st.DB()), NewUserRepository(st.DB())
}

// storeHandle 只是把 *Store 包一层，便于测试里直接跑 SQL 造数据。
type storeHandle struct {
	st *Store
}

// exec 在测试库上执行一条 SQL（用于模拟历史数据/直接断言）。
func (h *storeHandle) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.st.DB().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("执行 SQL 失败: %v", err)
	}
}

func TestReferralRepository_邀请码唯一且规范(t *testing.T) {
	h, repo, users := newTestReferralRepo(t)
	ctx := context.Background()

	seen := make(map[string]struct{}, 32)
	for i := 0; i < 24; i++ {
		user := newActiveUser(t, "邀请码用户"+string(rune('A'+i)))
		if err := users.Create(ctx, user); err != nil {
			t.Fatalf("创建用户失败: %v", err)
		}
		if len(user.InviteCode) != 8 {
			t.Fatalf("邀请码应为 8 位，实际 %q", user.InviteCode)
		}
		if user.InviteCode != strings.ToUpper(user.InviteCode) {
			t.Fatalf("邀请码应为大写，实际 %q", user.InviteCode)
		}
		// 易混字符必须被剔除
		for _, ch := range []string{"0", "O", "1", "I"} {
			if strings.Contains(user.InviteCode, ch) {
				t.Fatalf("邀请码不应包含易混字符 %q，实际 %q", ch, user.InviteCode)
			}
		}
		if _, dup := seen[user.InviteCode]; dup {
			t.Fatalf("邀请码重复: %s", user.InviteCode)
		}
		seen[user.InviteCode] = struct{}{}

		// 按邀请码应能反查到本人
		got, err := repo.UserIDByInviteCode(ctx, user.InviteCode)
		if err != nil {
			t.Fatalf("按邀请码查询失败: %v", err)
		}
		if got != user.ID {
			t.Fatalf("邀请码 %s 反查得到 %d，期望 %d", user.InviteCode, got, user.ID)
		}
	}

	// 空邀请码不得命中任何用户
	if _, err := repo.UserIDByInviteCode(ctx, "   "); err != model.ErrInviteCodeNotFound {
		t.Fatalf("空邀请码应返回 ErrInviteCodeNotFound，实际 %v", err)
	}
	_ = h
}

func TestReferralRepository_懒生成_并发安全(t *testing.T) {
	h, repo, users := newTestReferralRepo(t)
	ctx := context.Background()

	user := newActiveUser(t, "老用户")
	if err := users.Create(ctx, user); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	// 模拟历史用户：清空邀请码（迁移落地的老数据即为此形态）
	h.exec(t, "UPDATE users SET invite_code = '' WHERE id = ?", user.ID)

	const concurrency = 6
	results := make([]string, concurrency)
	errs := make([]error, concurrency)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // 同时起跑，制造真实并发
			results[idx], errs[idx] = repo.EnsureInviteCode(ctx, user.ID)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < concurrency; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发懒生成失败: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("并发懒生成返回了不同邀请码：%q vs %q", results[i], results[0])
		}
	}
	if results[0] == "" {
		t.Fatal("懒生成不应返回空邀请码")
	}

	// 落库值应与返回值一致，且再次调用返回同一码（幂等）
	var stored string
	if err := h.st.DB().QueryRowContext(ctx, "SELECT invite_code FROM users WHERE id = ?", user.ID).Scan(&stored); err != nil {
		t.Fatalf("读取落库邀请码失败: %v", err)
	}
	if stored != results[0] {
		t.Fatalf("落库邀请码 %q 与返回值 %q 不一致", stored, results[0])
	}
	again, err := repo.EnsureInviteCode(ctx, user.ID)
	if err != nil {
		t.Fatalf("重复调用 EnsureInviteCode 失败: %v", err)
	}
	if again != results[0] {
		t.Fatalf("重复调用应返回同一邀请码，实际 %q vs %q", again, results[0])
	}

	// 不存在的用户应返回 ErrUserNotFound
	if _, err := repo.EnsureInviteCode(ctx, 999999); err != model.ErrUserNotFound {
		t.Fatalf("不存在的用户应返回 ErrUserNotFound，实际 %v", err)
	}
}

func TestReferralRepository_按天唯一签到(t *testing.T) {
	_, repo, users := newTestReferralRepo(t)
	ctx := context.Background()

	user := newActiveUser(t, "签到用户")
	user.Quota = 100
	if err := users.Create(ctx, user); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	ok, err := repo.Checkin(ctx, user.ID, "2026-09-27", 10)
	if err != nil || !ok {
		t.Fatalf("首次签到应成功，ok=%v err=%v", ok, err)
	}
	// 同一天再签：应返回 false 而非错误
	ok, err = repo.Checkin(ctx, user.ID, "2026-09-27", 10)
	if err != nil {
		t.Fatalf("重复签到不应报错: %v", err)
	}
	if ok {
		t.Fatal("同一天重复签到应被拒绝（返回 false）")
	}

	// 换一天可以再签
	ok, err = repo.Checkin(ctx, user.ID, "2026-09-28", 10)
	if err != nil || !ok {
		t.Fatalf("次日签到应成功，ok=%v err=%v", ok, err)
	}

	// 额度只应增加两次（2 × 10）
	reloaded, err := users.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("读回用户失败: %v", err)
	}
	if reloaded.Quota != 120 {
		t.Fatalf("签到应只加两次额度（100 + 10 + 10 = 120），实际 %d", reloaded.Quota)
	}

	// 汇总：今天(9-28)已签、累计 2 天、连续 2 天、累计额度 20
	stats, err := repo.CheckinSummary(ctx, user.ID, "2026-09-28")
	if err != nil {
		t.Fatalf("统计签到失败: %v", err)
	}
	if !stats.CheckedToday || stats.TotalDays != 2 || stats.StreakDays != 2 || stats.TotalQuota != 20 {
		t.Fatalf("签到汇总不符：%+v", stats)
	}
}

func TestReferralRepository_发奖幂等(t *testing.T) {
	_, repo, users := newTestReferralRepo(t)
	ctx := context.Background()

	inviter := newActiveUser(t, "邀请人")
	inviter.Quota = 0
	invitee := newActiveUser(t, "被邀请人")
	for _, u := range []*model.User{inviter, invitee} {
		if err := users.Create(ctx, u); err != nil {
			t.Fatalf("创建用户失败: %v", err)
		}
	}

	// 建立邀请关系
	if err := repo.BindInviter(ctx, invitee.ID, inviter.ID); err != nil {
		t.Fatalf("绑定邀请关系失败: %v", err)
	}
	if got, err := repo.InviterID(ctx, invitee.ID); err != nil || got != inviter.ID {
		t.Fatalf("读回邀请人不符：%d err=%v", got, err)
	}
	// 重复绑定不应改变关系（幂等）
	if err := repo.BindInviter(ctx, invitee.ID, invitee.ID); err == nil {
		t.Fatal("不能邀请自己，应报错")
	}

	reward := &model.ReferralReward{
		InviterID:    inviter.ID,
		InviteeID:    invitee.ID,
		Kind:         model.ReferralKindRecharge,
		Quota:        50,
		OrderTradeNo: "pay20260927000000abcdef",
	}

	granted, err := repo.GrantReward(ctx, reward)
	if err != nil || !granted {
		t.Fatalf("首次发奖应成功，granted=%v err=%v", granted, err)
	}
	// 同一订单重复发奖：必须只发一次
	granted, err = repo.GrantReward(ctx, reward)
	if err != nil {
		t.Fatalf("重复发奖不应报错: %v", err)
	}
	if granted {
		t.Fatal("同一订单重复发奖应返回 false（幂等）")
	}

	reloaded, err := users.GetByID(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("读回邀请人失败: %v", err)
	}
	if reloaded.Quota != 50 {
		t.Fatalf("邀请人额度应为 50（只发一次），实际 %d", reloaded.Quota)
	}

	total, err := repo.TotalRewardQuota(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("统计返利失败: %v", err)
	}
	if total != 50 {
		t.Fatalf("累计返利应为 50，实际 %d", total)
	}
	count, err := repo.CountInvitees(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("统计邀请人数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("邀请人数应为 1，实际 %d", count)
	}

	// 不发零/负额度
	if granted, err := repo.GrantReward(ctx, &model.ReferralReward{
		InviterID: inviter.ID, InviteeID: invitee.ID, Kind: model.ReferralKindRegister, Quota: 0,
	}); err != nil || granted {
		t.Fatalf("0 额度不应发放，granted=%v err=%v", granted, err)
	}
}
