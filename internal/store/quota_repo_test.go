// 额度预扣台账（quota_reservations）仓储的单元测试。
//
// 意图（Why）：
//
//	预扣台账是"堵住并发超支"的核心。这里把四条不变量钉死：
//	  1) 并发预留不会超支（受影响行数判定必须真实生效）；
//	  2) 结算多退少补，补扣不足时按可用量扣且可识别；
//	  3) 预留/结算/释放三者幂等，重复调用不重复加减；
//	  4) 超时在途会被回收并退还额度（进程崩溃后的兜底）。
//
// 流转（Flow）：
//
//	go test ./internal/store/ -run Quota
//	  └─ 在真实 SQLite 上执行事务，验证并发与原子性（mock 无法证明这些性质）
//
// 扩展（Extend）：
//
//	新增额度维度（如按渠道限额）时，在 TestQuotaRepository_* 中补充对应断言。
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestQuotaRepos 在同一临时库上构造额度台账、用户与令牌仓储。
func newTestQuotaRepos(t *testing.T) (model.QuotaRepository, model.UserRepository, model.TokenRepository) {
	t.Helper()

	st := newTestStore(t)
	cipher, err := crypto.New(testEncryptionKey)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}
	return NewQuotaRepository(st.DB()), NewUserRepository(st.DB()), NewTokenRepository(st.DB(), cipher)
}

// newQuotaUser 创建一个指定总额度的用户（用户名唯一）。
func newQuotaUser(t *testing.T, users model.UserRepository, quota int64, suffix string) *model.User {
	t.Helper()

	u := &model.User{
		Username:     "quota-user-" + suffix,
		PasswordHash: "test-hash-placeholder",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusEnabled,
		Quota:        quota,
	}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return u
}

// newOwnedQuotaToken 创建一个归属于 ownerID 的令牌。
func newOwnedQuotaToken(t *testing.T, tokens model.TokenRepository, ownerID uint64, unlimited bool, remain int64) *model.Token {
	t.Helper()

	key, err := model.GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	token := &model.Token{
		OwnerID:        ownerID,
		Name:           "quota-test-token",
		Key:            key,
		Status:         model.TokenStatusEnabled,
		RemainQuota:    remain,
		UnlimitedQuota: unlimited,
	}
	if err := tokens.Create(context.Background(), token); err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}
	return token
}

// reserve 以给定 requestID / 金额发起一次预留。
func reserve(ctx context.Context, repo model.QuotaRepository, requestID string, userID, tokenID uint64, amount int64) (*model.QuotaReservation, error) {
	return repo.Reserve(ctx, model.ReserveRequest{
		RequestID: requestID,
		UserID:    userID,
		TokenID:   tokenID,
		Amount:    amount,
		TTL:       time.Minute,
	})
}

// mustUsedQuota 读取用户当前已用额度。
func mustUsedQuota(t *testing.T, users model.UserRepository, userID uint64) int64 {
	t.Helper()
	u, err := users.GetByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	return u.UsedQuota
}

// TestQuotaRepository_并发预留不超支 是本模块最重要的用例。
//
// 额度 100，起 20 个 goroutine 各预留 20：只有 5 个能成功，
// 其余必须拿到"额度不足"；且最终"已用"绝不超过"总额度"。
func TestQuotaRepository_并发预留不超支(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 100, "concurrent")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0) // 令牌不限额度，单独验证用户侧额度墙

	const (
		rounds   = 20
		perRound = 20
	)

	var (
		success      int32
		insufficient int32
		wg           sync.WaitGroup
	)
	errCh := make(chan error, rounds)

	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := reserve(ctx, repo, fmt.Sprintf("concurrent-%d", i), user.ID, token.ID, perRound)
			switch {
			case err == nil:
				atomic.AddInt32(&success, 1)
			case errors.Is(err, model.ErrQuotaInsufficient):
				atomic.AddInt32(&insufficient, 1)
			default:
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("预留出现非预期错误: %v", err)
	}

	if success != 5 {
		t.Fatalf("并发成功数 = %d，期望 5（额度 100，每次 20）", success)
	}
	if insufficient != rounds-5 {
		t.Fatalf("额度不足数 = %d，期望 %d", insufficient, rounds-5)
	}

	used := mustUsedQuota(t, users, user.ID)
	if used > 100 {
		t.Fatalf("已用额度 %d 超过总额度 100（超支漏洞未堵住）", used)
	}
	if used != 100 {
		t.Fatalf("已用额度 = %d，期望 100（5 次 × 20）", used)
	}

	// 在途预留合计应与已用一致（成功 5 条）
	pending, err := repo.PendingAmount(ctx, user.ID)
	if err != nil {
		t.Fatalf("查询在途预留失败: %v", err)
	}
	if pending != 100 {
		t.Fatalf("在途预留合计 = %d，期望 100", pending)
	}
}

// TestQuotaRepository_结算多退少补 验证结算能退还差额、也能补扣差额。
func TestQuotaRepository_结算多退少补(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "settle")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	// 场景一：预留 100，实际 30 → 退还 70。
	if _, err := reserve(ctx, repo, "settle-refund", user.ID, token.ID, 100); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	got, err := repo.Settle(ctx, "settle-refund", 30)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if got.Settled != 30 {
		t.Fatalf("结算入账 = %d，期望 30（多退）", got.Settled)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 30 {
		t.Fatalf("已用额度 = %d，期望 30（预留 100 − 退还 70）", used)
	}

	// 场景二：预留 100，实际 150 → 补扣 50。
	if _, err := reserve(ctx, repo, "settle-charge", user.ID, token.ID, 100); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	got, err = repo.Settle(ctx, "settle-charge", 150)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if got.Settled != 150 {
		t.Fatalf("结算入账 = %d，期望 150（少补全额）", got.Settled)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 180 {
		t.Fatalf("已用额度 = %d，期望 180（30 + 150）", used)
	}
}

// TestQuotaRepository_补扣不足按可用量扣减 验证补扣受可用额度限制时，按可用量扣且可识别。
func TestQuotaRepository_补扣不足按可用量扣减(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	// 总额度 120：预留 100 后只剩 20 可用，实际 150 需补 50，只能补 20。
	user := newQuotaUser(t, users, 120, "charge-limit")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	if _, err := reserve(ctx, repo, "charge-limit", user.ID, token.ID, 100); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	got, err := repo.Settle(ctx, "charge-limit", 150)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if got.Settled != 120 {
		t.Fatalf("结算入账 = %d，期望 120（预留 100 + 可用 20）", got.Settled)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 120 {
		t.Fatalf("已用额度 = %d，期望 120（扣到总额度为止，不为负）", used)
	}
}

// TestQuotaRepository_幂等 验证同一 requestID 重复预留/结算/释放不重复加减。
func TestQuotaRepository_幂等(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "idempotent")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	// 预留两次：只扣一次。
	first, err := reserve(ctx, repo, "idem", user.ID, token.ID, 100)
	if err != nil {
		t.Fatalf("首次预留失败: %v", err)
	}
	second, err := reserve(ctx, repo, "idem", user.ID, token.ID, 100)
	if err != nil {
		t.Fatalf("重复预留失败（应幂等）: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("重复预留返回了不同记录：%d vs %d", first.ID, second.ID)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 100 {
		t.Fatalf("重复预留后已用 = %d，期望 100（只扣一次）", used)
	}

	// 结算两次：只结算一次。
	if _, err := repo.Settle(ctx, "idem", 40); err != nil {
		t.Fatalf("首次结算失败: %v", err)
	}
	if _, err := repo.Settle(ctx, "idem", 40); err != nil {
		t.Fatalf("重复结算失败（应幂等）: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 40 {
		t.Fatalf("重复结算后已用 = %d，期望 40（只退一次）", used)
	}

	// 释放两次：只退一次。
	if _, err := reserve(ctx, repo, "idem-release", user.ID, token.ID, 100); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if err := repo.Release(ctx, "idem-release"); err != nil {
		t.Fatalf("首次释放失败: %v", err)
	}
	if err := repo.Release(ctx, "idem-release"); err != nil {
		t.Fatalf("重复释放失败（应幂等）: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 40 {
		t.Fatalf("重复释放后已用 = %d，期望 40（只退一次）", used)
	}
}

// TestQuotaRepository_失败释放全额退还 验证释放后额度回到预留前。
func TestQuotaRepository_失败释放全额退还(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "release")
	token := newOwnedQuotaToken(t, tokens, user.ID, false, 1000)

	if _, err := reserve(ctx, repo, "release", user.ID, token.ID, 300); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 300 {
		t.Fatalf("预留后已用 = %d，期望 300", used)
	}

	if err := repo.Release(ctx, "release"); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 0 {
		t.Fatalf("释放后已用 = %d，期望 0（全额退还）", used)
	}

	got, err := tokens.GetByID(ctx, token.ID)
	if err != nil {
		t.Fatalf("查询令牌失败: %v", err)
	}
	if got.RemainQuota != 1000 || got.UsedQuota != 0 {
		t.Fatalf("令牌额度未回到预留前：remain=%d used=%d", got.RemainQuota, got.UsedQuota)
	}
}

// TestQuotaRepository_在途超时清理 验证过期在途被回收并退还额度。
func TestQuotaRepository_在途超时清理(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "cleanup")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	// TTL 极短：预留后立即"过期"。
	if _, err := repo.Reserve(ctx, model.ReserveRequest{
		RequestID: "stale", UserID: user.ID, TokenID: token.ID, Amount: 200, TTL: time.Millisecond,
	}); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 200 {
		t.Fatalf("预留后已用 = %d，期望 200", used)
	}

	// 用一个"未来时间"作为当前时刻，确保上面的预留已被判定为超时。
	cleaned, err := repo.CleanupExpired(ctx, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("清理条数 = %d，期望 1", cleaned)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 0 {
		t.Fatalf("清理后已用 = %d，期望 0（额度已退还）", used)
	}

	// 再次清理：已无在途记录，返回 0。
	cleaned, err = repo.CleanupExpired(ctx, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("重复清理失败: %v", err)
	}
	if cleaned != 0 {
		t.Fatalf("重复清理条数 = %d，期望 0", cleaned)
	}
}

// TestQuotaRepository_未知用量按预留量收 验证拿不到 usage 时结算不退成 0。
func TestQuotaRepository_未知用量按预留量收(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "unknown")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	if _, err := reserve(ctx, repo, "unknown", user.ID, token.ID, 100); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	got, err := repo.Settle(ctx, "unknown", model.QuotaUnknown)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if got.Settled != 100 {
		t.Fatalf("结算入账 = %d，期望 100（按预留量收取）", got.Settled)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 100 {
		t.Fatalf("已用额度 = %d，期望 100", used)
	}
}

// TestQuotaRepository_不限额度用户不受限 验证 quota = -1 的用户在预留/结算/释放全流程不受限。
func TestQuotaRepository_不限额度用户不受限(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, model.QuotaUnlimited, "unlimited")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	const huge = int64(1) << 40
	if _, err := reserve(ctx, repo, "unlimited", user.ID, token.ID, huge); err != nil {
		t.Fatalf("不限额度用户预留不应失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != huge {
		t.Fatalf("已用额度 = %d，期望 %d", used, huge)
	}

	got, err := repo.Settle(ctx, "unlimited", 1)
	if err != nil {
		t.Fatalf("不限额度用户结算不应失败: %v", err)
	}
	if got.Settled != 1 {
		t.Fatalf("结算入账 = %d，期望 1", got.Settled)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 1 {
		t.Fatalf("结算后已用 = %d，期望 1", used)
	}

	if _, err := reserve(ctx, repo, "unlimited-2", user.ID, token.ID, huge); err != nil {
		t.Fatalf("不限额度用户再次预留不应失败: %v", err)
	}
	if err := repo.Release(ctx, "unlimited-2"); err != nil {
		t.Fatalf("不限额度用户释放不应失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 1 {
		t.Fatalf("释放后已用 = %d，期望 1", used)
	}
}

// TestQuotaRepository_有限令牌限制预留 验证令牌剩余额度也会约束预留。
func TestQuotaRepository_有限令牌限制预留(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "token-limit")
	token := newOwnedQuotaToken(t, tokens, user.ID, false, 50)

	if _, err := reserve(ctx, repo, "token-1", user.ID, token.ID, 50); err != nil {
		t.Fatalf("首次预留应成功: %v", err)
	}
	if _, err := reserve(ctx, repo, "token-2", user.ID, token.ID, 10); !errors.Is(err, model.ErrQuotaInsufficient) {
		t.Fatalf("令牌额度耗尽后预留错误 = %v，期望 ErrQuotaInsufficient", err)
	}

	// 失败预留必须整体回滚：用户额度不应被扣。
	if used := mustUsedQuota(t, users, user.ID); used != 50 {
		t.Fatalf("已用额度 = %d，期望 50（失败预留不得残留扣减）", used)
	}
}

// TestQuotaRepository_GetByRequestID_NotFound 验证查询不存在的预留返回领域错误。
func TestQuotaRepository_GetByRequestID_NotFound(t *testing.T) {
	repo, _, _ := newTestQuotaRepos(t)

	if _, err := repo.GetByRequestID(context.Background(), "no-such"); !errors.Is(err, model.ErrReservationNotFound) {
		t.Fatalf("错误 = %v，期望 ErrReservationNotFound", err)
	}
}

// TestQuotaRepository_回收过期在途_退还并置终态 验证过期在途被回收：
// 用户与令牌额度都退回、记录状态落到终态（已释放）。
func TestQuotaRepository_回收过期在途_退还并置终态(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	// 令牌设有限额：一并验证回收会退还令牌剩余额度。
	user := newQuotaUser(t, users, 1000, "cleanup-expired")
	token := newOwnedQuotaToken(t, tokens, user.ID, false, 1000)

	if _, err := reserve(ctx, repo, "stale-expired", user.ID, token.ID, 300); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 300 {
		t.Fatalf("预留后已用 = %d，期望 300", used)
	}

	// 传入"过期之后"的时刻，确保该预留被判定为超时。
	cleaned, err := repo.CleanupExpired(ctx, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("回收条数 = %d，期望 1", cleaned)
	}

	if used := mustUsedQuota(t, users, user.ID); used != 0 {
		t.Fatalf("回收后用户已用 = %d，期望 0（额度已退还）", used)
	}
	gotToken, err := tokens.GetByID(ctx, token.ID)
	if err != nil {
		t.Fatalf("查询令牌失败: %v", err)
	}
	if gotToken.UsedQuota != 0 || gotToken.RemainQuota != 1000 {
		t.Fatalf("回收后令牌额度未退还：used=%d remain=%d，期望 used=0 remain=1000",
			gotToken.UsedQuota, gotToken.RemainQuota)
	}

	// 状态必须是终态（已释放），而不是仍留在"在途"。
	rec, err := repo.GetByRequestID(ctx, "stale-expired")
	if err != nil {
		t.Fatalf("查询预留失败: %v", err)
	}
	if rec.Status != model.ReservationReleased {
		t.Fatalf("回收后状态 = %v，期望 %v", rec.Status, model.ReservationReleased)
	}
	if !rec.Status.IsTerminal() {
		t.Fatalf("回收后状态 %v 不是终态", rec.Status)
	}
}

// TestQuotaRepository_未过期在途不被回收 验证尚未超时的在途预留不受回收影响。
func TestQuotaRepository_未过期在途不被回收(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "cleanup-fresh")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	// reserve 的 TTL 为 1 分钟；用"当前时刻"回收不应命中。
	if _, err := reserve(ctx, repo, "stale-fresh", user.ID, token.ID, 200); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	cleaned, err := repo.CleanupExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if cleaned != 0 {
		t.Fatalf("未过期预留被回收，条数 = %d，期望 0", cleaned)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 200 {
		t.Fatalf("未过期预留的额度被改动：已用 = %d，期望 200", used)
	}
	rec, err := repo.GetByRequestID(ctx, "stale-fresh")
	if err != nil {
		t.Fatalf("查询预留失败: %v", err)
	}
	if rec.Status != model.ReservationInFlight {
		t.Fatalf("未过期预留状态 = %v，期望仍在途", rec.Status)
	}
}

// TestQuotaRepository_已结算预留不重复退还 验证"已结算"记录即使 expires_at 已过也不会被回收，
// 否则会与正常结算叠加、把额度多退一次。
func TestQuotaRepository_已结算预留不重复退还(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "cleanup-settled")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	if _, err := reserve(ctx, repo, "stale-settled", user.ID, token.ID, 500); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	// 正常结算：预留 500、实际 120，退还 380，此后已用应为 120。
	if _, err := repo.Settle(ctx, "stale-settled", 120); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 120 {
		t.Fatalf("结算后已用 = %d，期望 120", used)
	}

	// 即便传入远晚于 expires_at 的时刻，已结算记录也不在回收范围内。
	cleaned, err := repo.CleanupExpired(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if cleaned != 0 {
		t.Fatalf("已结算记录被回收，条数 = %d，期望 0", cleaned)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 120 {
		t.Fatalf("回收后已用 = %d，期望 120（不得重复退还）", used)
	}
	rec, err := repo.GetByRequestID(ctx, "stale-settled")
	if err != nil {
		t.Fatalf("查询预留失败: %v", err)
	}
	if rec.Status != model.ReservationSettled {
		t.Fatalf("状态 = %v，期望仍为已结算", rec.Status)
	}
}

// TestQuotaRepository_回收幂等_重复调用不再退还 验证回收返回条数正确，且重复调用安全。
func TestQuotaRepository_回收幂等_重复调用不再退还(t *testing.T) {
	repo, users, tokens := newTestQuotaRepos(t)
	ctx := context.Background()

	user := newQuotaUser(t, users, 1000, "cleanup-idem")
	token := newOwnedQuotaToken(t, tokens, user.ID, true, 0)

	// 两条同时过期的在途预留，用于校验返回条数。
	if _, err := reserve(ctx, repo, "stale-idem-1", user.ID, token.ID, 100); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if _, err := reserve(ctx, repo, "stale-idem-2", user.ID, token.ID, 50); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 150 {
		t.Fatalf("预留后已用 = %d，期望 150", used)
	}

	future := time.Now().Add(2 * time.Minute)
	cleaned, err := repo.CleanupExpired(ctx, future)
	if err != nil {
		t.Fatalf("首次回收失败: %v", err)
	}
	if cleaned != 2 {
		t.Fatalf("首次回收条数 = %d，期望 2", cleaned)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 0 {
		t.Fatalf("首次回收后已用 = %d，期望 0", used)
	}

	// 第二次回收：已无在途记录，返回 0 且额度不再变化。
	cleaned, err = repo.CleanupExpired(ctx, future)
	if err != nil {
		t.Fatalf("重复回收失败: %v", err)
	}
	if cleaned != 0 {
		t.Fatalf("重复回收条数 = %d，期望 0", cleaned)
	}
	if used := mustUsedQuota(t, users, user.ID); used != 0 {
		t.Fatalf("重复回收后已用 = %d，期望 0（不得二次退还）", used)
	}
}
