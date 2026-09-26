// 充值订单仓储的单元测试。
//
// 测试重点（都是"错了会直接资损"的场景）：
//   - 入账幂等：重复回调只能入账一次（第一次 true，第二次 false 且额度不变）；
//   - 未支付订单不允许入账；
//   - 入账与加额度在同一事务内完成（订单标记与用户额度必须同时变化）；
//   - 不限额度账户（quota = -1）入账时不修改额度。
package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestOrderRepo 构造订单仓储并返回用户仓储（入账需要真实用户）。
func newTestOrderRepo(t *testing.T) (model.PaymentOrderRepository, model.UserRepository, uint64) {
	t.Helper()
	st := newTestStore(t)
	orderRepo := NewPaymentOrderRepository(st.DB())
	userRepo := NewUserRepository(st.DB())

	user := newActiveUser(t, "支付测试用户")
	if err := userRepo.Create(context.Background(), user); err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	return orderRepo, userRepo, user.ID
}

// newActiveUser 构造一个额度为 0 的普通用户。
func newActiveUser(t *testing.T, username string) *model.User {
	t.Helper()
	return &model.User{
		Username:     username,
		PasswordHash: "test-hash",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusEnabled,
		Quota:        0,
	}
}

// newPendingOrder 构造一笔待支付订单。
func newPendingOrder(userID uint64, tradeNo string, amount, quota int64) *model.PaymentOrder {
	return &model.PaymentOrder{
		TradeNo:   tradeNo,
		UserID:    userID,
		Amount:    amount,
		Currency:  "CNY",
		Quota:     quota,
		Method:    model.PaymentMethodManual,
		Status:    model.PaymentStatusPending,
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
}

func TestPaymentOrderRepository_创建与查询(t *testing.T) {
	repo, _, userID := newTestOrderRepo(t)
	ctx := context.Background()

	order := newPendingOrder(userID, "pay20260101000000abcdef", 1000, 1000)
	if err := repo.Create(ctx, order); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	if order.ID == 0 {
		t.Fatal("创建后应回填 ID")
	}

	got, err := repo.GetByTradeNo(ctx, order.TradeNo)
	if err != nil {
		t.Fatalf("按订单号查询失败: %v", err)
	}
	if got.Amount != 1000 || got.Quota != 1000 {
		t.Fatalf("读回的数据不一致: %+v", got)
	}
	if got.Status != model.PaymentStatusPending {
		t.Fatalf("新订单应为待支付，实际 %v", got.Status)
	}
	if got.IsCredited() {
		t.Fatal("新订单不应处于已入账状态")
	}
}

func TestPaymentOrderRepository_查询不存在_应返回ErrOrderNotFound(t *testing.T) {
	repo, _, _ := newTestOrderRepo(t)

	_, err := repo.GetByTradeNo(context.Background(), "pay00000000000000000000")
	if !errors.Is(err, model.ErrPaymentOrderNotFound) {
		t.Fatalf("应返回 ErrPaymentOrderNotFound，实际 %v", err)
	}
}

func TestPaymentOrderRepository_CreditOrder_必须幂等(t *testing.T) {
	repo, users, userID := newTestOrderRepo(t)
	ctx := context.Background()

	order := newPendingOrder(userID, "pay20260101000001aaaaaa", 2000, 2000)
	if err := repo.Create(ctx, order); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	if paid, err := repo.MarkPaid(ctx, order.TradeNo, "third-1", "{}", time.Now()); err != nil || !paid {
		t.Fatalf("标记支付应成功，paid=%v err=%v", paid, err)
	}

	credited, err := repo.CreditOrder(ctx, order.TradeNo, time.Now())
	if err != nil {
		t.Fatalf("首次入账失败: %v", err)
	}
	if !credited {
		t.Fatal("首次入账应返回 true")
	}

	// 第二次入账（模拟回调重复到达）必须返回 false 且额度不再增加
	credited, err = repo.CreditOrder(ctx, order.TradeNo, time.Now())
	if err != nil {
		t.Fatalf("重复入账不应报错: %v", err)
	}
	if credited {
		t.Fatal("重复入账必须返回 false（否则会重复给钱）")
	}

	user, err := users.GetByID(ctx, userID)
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if user.Quota != 2000 {
		t.Fatalf("额度应只被加一次（2000），实际 %d", user.Quota)
	}

	got, _ := repo.GetByTradeNo(ctx, order.TradeNo)
	if !got.IsCredited() {
		t.Fatal("订单应处于已入账状态")
	}
}

func TestPaymentOrderRepository_CreditOrder_未支付不入账(t *testing.T) {
	repo, users, userID := newTestOrderRepo(t)
	ctx := context.Background()

	order := newPendingOrder(userID, "pay20260101000002bbbbbb", 500, 500)
	if err := repo.Create(ctx, order); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	credited, err := repo.CreditOrder(ctx, order.TradeNo, time.Now())
	if err != nil {
		t.Fatalf("未支付订单入账不应报错: %v", err)
	}
	if credited {
		t.Fatal("未支付订单不得入账")
	}

	user, _ := users.GetByID(ctx, userID)
	if user.Quota != 0 {
		t.Fatalf("未支付订单不应改变额度，实际 %d", user.Quota)
	}
}

func TestPaymentOrderRepository_CreditOrder_不限额度账户只标记(t *testing.T) {
	repo, users, userID := newTestOrderRepo(t)
	ctx := context.Background()

	// 把用户改成不限额度
	user, err := users.GetByID(ctx, userID)
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	user.Quota = model.QuotaUnlimited
	if err := users.Update(ctx, user); err != nil {
		t.Fatalf("更新用户失败: %v", err)
	}

	order := newPendingOrder(userID, "pay20260101000003cccccc", 100, 100)
	if err := repo.Create(ctx, order); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	if _, err := repo.MarkPaid(ctx, order.TradeNo, "", "", time.Now()); err != nil {
		t.Fatalf("标记支付失败: %v", err)
	}
	if _, err := repo.CreditOrder(ctx, order.TradeNo, time.Now()); err != nil {
		t.Fatalf("入账失败: %v", err)
	}

	updated, _ := users.GetByID(ctx, userID)
	if updated.Quota != model.QuotaUnlimited {
		t.Fatalf("不限额度账户的额度不应被改成数字，实际 %d", updated.Quota)
	}
}

func TestPaymentOrderRepository_MarkPaid_只跃迁一次(t *testing.T) {
	repo, _, userID := newTestOrderRepo(t)
	ctx := context.Background()

	order := newPendingOrder(userID, "pay20260101000004dddddd", 100, 100)
	if err := repo.Create(ctx, order); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	first, err := repo.MarkPaid(ctx, order.TradeNo, "t-1", "第一次", time.Now())
	if err != nil || !first {
		t.Fatalf("首次标记支付应成功: paid=%v err=%v", first, err)
	}
	second, err := repo.MarkPaid(ctx, order.TradeNo, "t-2", "重复回调", time.Now())
	if err != nil {
		t.Fatalf("重复标记不应报错: %v", err)
	}
	if second {
		t.Fatal("重复标记必须返回 false（状态跃迁只允许发生一次）")
	}
}

func TestPaymentOrderRepository_ListPaidUncredited_只返回已支付未入账(t *testing.T) {
	repo, _, userID := newTestOrderRepo(t)
	ctx := context.Background()

	pending := newPendingOrder(userID, "pay20260101000005eeeeee", 100, 100)
	uncredited := newPendingOrder(userID, "pay20260101000006ffffff", 100, 100)
	done := newPendingOrder(userID, "pay20260101000007gggggg", 100, 100)

	for _, order := range []*model.PaymentOrder{pending, uncredited, done} {
		if err := repo.Create(ctx, order); err != nil {
			t.Fatalf("创建订单失败: %v", err)
		}
	}

	for _, tradeNo := range []string{uncredited.TradeNo, done.TradeNo} {
		if _, err := repo.MarkPaid(ctx, tradeNo, "", "", time.Now()); err != nil {
			t.Fatalf("标记支付失败: %v", err)
		}
	}
	if _, err := repo.CreditOrder(ctx, done.TradeNo, time.Now()); err != nil {
		t.Fatalf("入账失败: %v", err)
	}

	items, err := repo.ListPaidUncredited(ctx, 10)
	if err != nil {
		t.Fatalf("查询未入账订单失败: %v", err)
	}
	if len(items) != 1 || items[0].TradeNo != uncredited.TradeNo {
		t.Fatalf("应只返回 1 笔已支付未入账订单，实际 %+v", items)
	}
}

func TestPaymentOrderRepository_CloseExpired_只关闭待支付(t *testing.T) {
	repo, _, userID := newTestOrderRepo(t)
	ctx := context.Background()

	expired := newPendingOrder(userID, "pay20260101000008hhhhhh", 100, 100)
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	paid := newPendingOrder(userID, "pay20260101000009iiiiii", 100, 100)
	paid.ExpiresAt = time.Now().Add(-time.Minute)

	for _, order := range []*model.PaymentOrder{expired, paid} {
		if err := repo.Create(ctx, order); err != nil {
			t.Fatalf("创建订单失败: %v", err)
		}
	}
	if _, err := repo.MarkPaid(ctx, paid.TradeNo, "", "", time.Now()); err != nil {
		t.Fatalf("标记支付失败: %v", err)
	}

	closed, err := repo.CloseExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("关闭超时订单失败: %v", err)
	}
	if closed != 1 {
		t.Fatalf("应只关闭 1 笔超时订单，实际 %d", closed)
	}

	got, _ := repo.GetByTradeNo(ctx, expired.TradeNo)
	if got.Status != model.PaymentStatusClosed {
		t.Fatalf("超时订单应被关闭，实际 %v", got.Status)
	}
	gotPaid, _ := repo.GetByTradeNo(ctx, paid.TradeNo)
	if gotPaid.Status != model.PaymentStatusPaid {
		t.Fatalf("已支付订单不应被关闭，实际 %v", gotPaid.Status)
	}
}

func TestPaymentOrderRepository_SumPaidQuota(t *testing.T) {
	repo, _, userID := newTestOrderRepo(t)
	ctx := context.Background()

	first := newPendingOrder(userID, "pay2026010100000ajjjjjj", 100, 150)
	second := newPendingOrder(userID, "pay2026010100000bkkkkkk", 100, 250)
	for _, order := range []*model.PaymentOrder{first, second} {
		if err := repo.Create(ctx, order); err != nil {
			t.Fatalf("创建订单失败: %v", err)
		}
		if _, err := repo.MarkPaid(ctx, order.TradeNo, "", "", time.Now()); err != nil {
			t.Fatalf("标记支付失败: %v", err)
		}
	}

	total, err := repo.SumPaidQuota(ctx, userID)
	if err != nil {
		t.Fatalf("统计充值额度失败: %v", err)
	}
	if total != 400 {
		t.Fatalf("已支付额度合计应为 400，实际 %d", total)
	}
}
