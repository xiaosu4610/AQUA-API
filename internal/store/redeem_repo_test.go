// 兑换码仓储的单元测试。
//
// 测试重点（全部围绕资损风险）：
//   - 一码一用：兑换成功后再兑换同一码必须失败；
//   - 并发安全：同一张码被并发兑换时只能成功一次（其余返回"已使用"）；
//   - 状态语义：过期码、作废码必须被拒绝，且各自返回精确的哨兵错误；
//   - 额度正确：兑换后用户额度按码面额增加；不限额度账户保持不限。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 真实 SQLite（临时文件）上执行迁移并验证仓储行为
//
// 扩展（Extend）：
//
//	新增兑换规则（如"单用户限领""限定分组"）时，在下方补充对应用例。
package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestRedeemFixture 构造基于临时数据库的兑换码仓储与用户仓储。
func newTestRedeemFixture(t *testing.T) (model.RedeemCodeRepository, model.UserRepository) {
	t.Helper()
	st := newTestStore(t)
	return NewRedeemCodeRepository(st.DB()), NewUserRepository(st.DB())
}

// testUserSeq 为测试用户提供唯一名字后缀（用户名有唯一索引，避免撞名）。
var testUserSeq int64

// createTestUser 写入一个测试用户并返回其 ID。
func createTestUser(t *testing.T, users model.UserRepository, quota int64) uint64 {
	t.Helper()
	seq := atomic.AddInt64(&testUserSeq, 1)
	user := &model.User{
		Username:     "redeem-user-" + strconv.FormatInt(seq, 10),
		PasswordHash: "test-hash",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusEnabled,
		Quota:        quota,
	}
	if err := users.Create(context.Background(), user); err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	return user.ID
}

// newTestRedeemCode 构造一张未使用的兑换码。
func newTestRedeemCode(t *testing.T, quota int64) *model.RedeemCode {
	t.Helper()
	code, err := model.GenerateRedeemCode()
	if err != nil {
		t.Fatalf("生成兑换码失败: %v", err)
	}
	return &model.RedeemCode{
		Code:    code,
		Quota:   quota,
		Status:  model.RedeemStatusUnused,
		BatchNo: "B-TEST",
	}
}

func TestGenerateRedeemCode_不含易混字符且长度稳定(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		code, err := model.GenerateRedeemCode()
		if err != nil {
			t.Fatalf("生成兑换码失败: %v", err)
		}
		if len(code) != 16 {
			t.Fatalf("兑换码长度应为 16，实际 %d（%q）", len(code), code)
		}
		if code != strings.ToUpper(code) {
			t.Fatalf("兑换码应为大写，实际 %q", code)
		}
		// 易混字符（0/O/1/I/L）必须不出现，否则用户手抄时几乎必然出错
		if strings.ContainsAny(code, "0O1IL") {
			t.Fatalf("兑换码含易混字符: %q", code)
		}
		seen[code] = struct{}{}
	}
	if len(seen) != 200 {
		t.Fatalf("200 次生成出现重复，实际唯一值 %d（随机性不足）", len(seen))
	}
}

func TestRedeemCodeRepository_批量生成并可按码查询(t *testing.T) {
	repo, _ := newTestRedeemFixture(t)
	ctx := context.Background()

	batch := make([]*model.RedeemCode, 0, 5)
	for i := 0; i < 5; i++ {
		batch = append(batch, newTestRedeemCode(t, 100))
	}
	if err := repo.CreateBatch(ctx, batch); err != nil {
		t.Fatalf("批量写入兑换码失败: %v", err)
	}
	for _, code := range batch {
		if code.ID == 0 {
			t.Fatal("批量写入后应回填 ID")
		}
	}

	got, err := repo.GetByCode(ctx, batch[0].Code)
	if err != nil {
		t.Fatalf("按码查询失败: %v", err)
	}
	if got.Quota != 100 || got.Status != model.RedeemStatusUnused {
		t.Fatalf("读回的数据不一致: %+v", got)
	}
	if got.BatchNo != "B-TEST" {
		t.Fatalf("批次号应被保存，实际 %q", got.BatchNo)
	}

	// 查询条件：状态 + 批次
	status := model.RedeemStatusUnused
	items, total, err := repo.List(ctx, model.RedeemCodeQuery{Status: &status, BatchNo: "B-TEST", Limit: 50})
	if err != nil {
		t.Fatalf("查询列表失败: %v", err)
	}
	if total != 5 || len(items) != 5 {
		t.Fatalf("应有 5 张未使用码，实际 total=%d len=%d", total, len(items))
	}

	// 不存在的码
	if _, err := repo.GetByCode(ctx, "NOT-EXIST-CODE"); !errors.Is(err, model.ErrRedeemCodeNotFound) {
		t.Fatalf("不存在的码应返回 ErrRedeemCodeNotFound，实际 %v", err)
	}
}

func TestRedeemCodeRepository_兑换成功后不可重复兑换(t *testing.T) {
	repo, users := newTestRedeemFixture(t)
	ctx := context.Background()

	userID := createTestUser(t, users, 1000)
	code := newTestRedeemCode(t, 500)
	if err := repo.CreateBatch(ctx, []*model.RedeemCode{code}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}

	quota, err := repo.Redeem(ctx, code.Code, int64(userID))
	if err != nil {
		t.Fatalf("首次兑换应成功，实际: %v", err)
	}
	if quota != 500 {
		t.Fatalf("本次获得额度应为 500，实际 %d", quota)
	}

	// 用户额度应增加 500
	user, err := users.GetByID(ctx, userID)
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if user.Quota != 1500 {
		t.Fatalf("兑换后总额度应为 1500，实际 %d", user.Quota)
	}

	// 再次兑换同一码必须失败，且原因是"已使用"
	if _, err := repo.Redeem(ctx, code.Code, int64(userID)); !errors.Is(err, model.ErrRedeemCodeUsed) {
		t.Fatalf("重复兑换应返回 ErrRedeemCodeUsed，实际 %v", err)
	}

	// 兑换记录应落到码上
	got, err := repo.GetByCode(ctx, code.Code)
	if err != nil {
		t.Fatalf("查询兑换码失败: %v", err)
	}
	if got.Status != model.RedeemStatusUsed || got.UsedBy != userID || got.UsedAt.IsZero() {
		t.Fatalf("兑换后码状态/领取人不正确: %+v", got)
	}
}

func TestRedeemCodeRepository_并发兑换同一码只成功一次(t *testing.T) {
	repo, users := newTestRedeemFixture(t)
	ctx := context.Background()

	userID := createTestUser(t, users, 0)
	code := newTestRedeemCode(t, 300)
	if err := repo.CreateBatch(ctx, []*model.RedeemCode{code}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}

	const goroutines = 10
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		success   int
		usedCount int
		otherErrs []error
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := repo.Redeem(ctx, code.Code, int64(userID))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, model.ErrRedeemCodeUsed):
				usedCount++
			default:
				otherErrs = append(otherErrs, err)
			}
		}()
	}
	wg.Wait()

	if len(otherErrs) > 0 {
		t.Fatalf("并发兑换出现非预期错误: %v", otherErrs)
	}
	if success != 1 {
		t.Fatalf("并发兑换只应成功 1 次，实际 %d", success)
	}
	if usedCount != goroutines-1 {
		t.Fatalf("其余 %d 次应返回已使用错误，实际 %d", goroutines-1, usedCount)
	}

	// 额度只应增加一次
	user, err := users.GetByID(ctx, userID)
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if user.Quota != 300 {
		t.Fatalf("并发兑换后额度应只增加一次（300），实际 %d", user.Quota)
	}
}

func TestRedeemCodeRepository_过期码与作废码兑换失败(t *testing.T) {
	repo, users := newTestRedeemFixture(t)
	ctx := context.Background()

	userID := createTestUser(t, users, 0)

	expired := newTestRedeemCode(t, 100)
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	voided := newTestRedeemCode(t, 100)
	voided.Status = model.RedeemStatusVoid
	if err := repo.CreateBatch(ctx, []*model.RedeemCode{expired, voided}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}

	if _, err := repo.Redeem(ctx, expired.Code, int64(userID)); !errors.Is(err, model.ErrRedeemCodeExpired) {
		t.Fatalf("过期码应返回 ErrRedeemCodeExpired，实际 %v", err)
	}
	if _, err := repo.Redeem(ctx, voided.Code, int64(userID)); !errors.Is(err, model.ErrRedeemCodeVoid) {
		t.Fatalf("作废码应返回 ErrRedeemCodeVoid，实际 %v", err)
	}
	if _, err := repo.Redeem(ctx, "NO-SUCH-CODE", int64(userID)); !errors.Is(err, model.ErrRedeemCodeNotFound) {
		t.Fatalf("不存在的码应返回 ErrRedeemCodeNotFound，实际 %v", err)
	}
}

func TestRedeemCodeRepository_不限额度用户兑换后仍不限(t *testing.T) {
	repo, users := newTestRedeemFixture(t)
	ctx := context.Background()

	userID := createTestUser(t, users, model.QuotaUnlimited)
	code := newTestRedeemCode(t, 888)
	if err := repo.CreateBatch(ctx, []*model.RedeemCode{code}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}

	if _, err := repo.Redeem(ctx, code.Code, int64(userID)); err != nil {
		t.Fatalf("不限额度用户兑换应成功，实际: %v", err)
	}

	user, err := users.GetByID(ctx, userID)
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if user.Quota != model.QuotaUnlimited {
		t.Fatalf("不限额度账户兑换后仍应为 -1，实际 %d", user.Quota)
	}
}

func TestRedeemCodeRepository_DeleteInvalid清理已使用与已过期(t *testing.T) {
	repo, users := newTestRedeemFixture(t)
	ctx := context.Background()

	userID := createTestUser(t, users, 0)

	used := newTestRedeemCode(t, 10)
	expired := newTestRedeemCode(t, 10)
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	voided := newTestRedeemCode(t, 10)
	voided.Status = model.RedeemStatusVoid
	valid := newTestRedeemCode(t, 10)

	if err := repo.CreateBatch(ctx, []*model.RedeemCode{used, expired, voided, valid}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}
	if _, err := repo.Redeem(ctx, used.Code, int64(userID)); err != nil {
		t.Fatalf("兑换失败: %v", err)
	}

	deleted, err := repo.DeleteInvalid(ctx)
	if err != nil {
		t.Fatalf("清理失效码失败: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("应清理 2 张（已使用 1 + 已过期 1），实际 %d", deleted)
	}

	items, total, err := repo.List(ctx, model.RedeemCodeQuery{Limit: 50})
	if err != nil {
		t.Fatalf("查询列表失败: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("清理后应剩 2 张（作废 1 + 有效 1），实际 total=%d len=%d", total, len(items))
	}
}

func TestRedeemCodeRepository_UpdateStatus与Delete(t *testing.T) {
	repo, _ := newTestRedeemFixture(t)
	ctx := context.Background()

	code := newTestRedeemCode(t, 10)
	if err := repo.CreateBatch(ctx, []*model.RedeemCode{code}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}

	if err := repo.UpdateStatus(ctx, code.ID, model.RedeemStatusVoid); err != nil {
		t.Fatalf("更新状态失败: %v", err)
	}
	got, err := repo.GetByCode(ctx, code.Code)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.Status != model.RedeemStatusVoid {
		t.Fatalf("状态应已改为作废，实际 %v", got.Status)
	}

	if err := repo.UpdateRemark(ctx, code.ID, "双十一活动"); err != nil {
		t.Fatalf("更新备注失败: %v", err)
	}
	got, _ = repo.GetByCode(ctx, code.Code)
	if got.Remark != "双十一活动" {
		t.Fatalf("备注应已更新，实际 %q", got.Remark)
	}

	if err := repo.Delete(ctx, code.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if err := repo.Delete(ctx, code.ID); !errors.Is(err, model.ErrRedeemCodeNotFound) {
		t.Fatalf("重复删除应返回 ErrRedeemCodeNotFound，实际 %v", err)
	}
}
