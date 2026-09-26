// 邀请返利 / 每日签到 HTTP 接口的单元测试。
//
// 说明：被测路由（/api/user/referral、/api/user/checkin）由 router.go 统一注册，
// 本测试只负责准备数据、直接调用处理器链路，不重复挂载路由。
//
// 测试重点：
//   - 注册带邀请码能建立邀请关系并（按配置）给邀请人发注册奖；
//   - 非法邀请码不影响注册成功（注册接口必须照常返回会话）；
//   - 同一用户同一天重复签到被拒绝，跨天可再次签到；
//   - 充值返利幂等：同一订单号调用两次只发一次额度。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/relay"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// newReferralTestServer 装配一个含邀请仓储的测试服务，并挂上被测路由。
func newReferralTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	gin.DefaultWriter = io.Discard

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "referral_test.db"))
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
	channels := store.NewChannelRepository(st.DB(), cipher)

	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	srv := New(Deps{
		Config:    cfg,
		Store:     st,
		Channels:  channels,
		Tokens:    store.NewTokenRepository(st.DB(), cipher),
		Users:     store.NewUserRepository(st.DB()),
		Sessions:  store.NewSessionRepository(st.DB()),
		UsageLogs: store.NewUsageLogRepository(st.DB(), st.Dialect()),
		Settings:  store.NewSettingRepository(st.DB(), st.Dialect()),
		Orders:    store.NewPaymentOrderRepository(st.DB()),
		Referrals: store.NewReferralRepository(st.DB()),
		Relay:     relay.New(channels, relay.Options{}),
	})

	// 被测路由（/api/user/referral 与 /api/user/checkin）已由 router.go 注册，
	// 这里不再重复挂载——重复注册会让 gin 直接 panic。

	return srv, st
}

// applySettings 直接写设置库（测试用）。
func applySettings(t *testing.T, st *store.Store, values map[string]string) {
	t.Helper()
	repo := store.NewSettingRepository(st.DB(), st.Dialect())
	if err := repo.SetMany(context.Background(), values); err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}
}

// callJSON 发送一次 JSON 请求并解析响应。
func callJSON(t *testing.T, srv *Server, method, path string, payload any, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, body)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	result := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &result)
	}
	return rec, result
}

// registerUser 通过注册接口创建账号并返回会话令牌与用户信息。
func registerUser(t *testing.T, srv *Server, username, inviteCode string) (string, map[string]any) {
	t.Helper()
	payload := map[string]any{"username": username, "password": "pass-" + username}
	if inviteCode != "" {
		payload["invite_code"] = inviteCode
	}
	rec, body := callJSON(t, srv, http.MethodPost, "/api/auth/register", payload, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("注册失败：状态 %d，响应 %v", rec.Code, body)
	}
	token, _ := body["session_token"].(string)
	if token == "" {
		t.Fatalf("注册响应缺少会话令牌：%v", body)
	}
	user, _ := body["user"].(map[string]any)
	return token, user
}

func TestHandlerRegister_带邀请码建立关系并发注册奖(t *testing.T) {
	srv, st := newReferralTestServer(t)
	applySettings(t, st, map[string]string{
		model.SettingKeyRegistrationRequireEmailCode: "false",
		// 新用户默认额度设为 0（而非默认的不限），否则"不限额度"账户加数字会被跳过，
		// 无法观察注册奖励是否发放。
		model.SettingKeyDefaultUserQuota:      "0",
		model.SettingKeyReferralEnabled:       "true",
		model.SettingKeyReferralRegisterBonus: "100",
	})

	// 先建邀请人
	_, inviter := registerUser(t, srv, "inviter", "")
	inviterID := uint64(inviter["id"].(float64))

	users := srv.deps.Users
	inviterRow, err := users.GetByID(context.Background(), inviterID)
	if err != nil {
		t.Fatalf("读回邀请人失败: %v", err)
	}
	if inviterRow.InviteCode == "" {
		t.Fatal("注册应自动生成邀请码")
	}

	// 用邀请码注册被邀请人：注册奖应发给邀请人
	_, invitee := registerUser(t, srv, "invitee", inviterRow.InviteCode)
	inviteeID := uint64(invitee["id"].(float64))

	newcomer, err := users.GetByID(context.Background(), inviteeID)
	if err != nil {
		t.Fatalf("读回被邀请人失败: %v", err)
	}
	if newcomer.InviterID != inviterID {
		t.Fatalf("被邀请人的 inviter_id = %d，期望 %d", newcomer.InviterID, inviterID)
	}

	reloadedInviter, err := users.GetByID(context.Background(), inviterID)
	if err != nil {
		t.Fatalf("读回邀请人失败: %v", err)
	}
	if reloadedInviter.Quota != 100 {
		t.Fatalf("邀请人应获得 100 注册奖励，实际 %d", reloadedInviter.Quota)
	}
}

func TestHandlerRegister_非法邀请码不影响注册(t *testing.T) {
	srv, st := newReferralTestServer(t)
	applySettings(t, st, map[string]string{
		model.SettingKeyRegistrationRequireEmailCode: "false",
		model.SettingKeyReferralEnabled:              "true",
		model.SettingKeyReferralRegisterBonus:        "100",
	})

	// 不存在的邀请码：注册仍应成功
	_, user := registerUser(t, srv, "with-bad-code", "NOTEXIST")
	id := uint64(user["id"].(float64))

	row, err := srv.deps.Users.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("读回用户失败: %v", err)
	}
	if row.InviterID != 0 {
		t.Fatalf("非法邀请码不应建立邀请关系，实际 inviter_id = %d", row.InviterID)
	}
}

func TestHandlerCheckin_重复签到被拒_跨天可再签(t *testing.T) {
	srv, st := newReferralTestServer(t)
	applySettings(t, st, map[string]string{
		model.SettingKeyRegistrationRequireEmailCode: "false",
		model.SettingKeyCheckinEnabled:               "true",
		model.SettingKeyCheckinDailyQuota:            "5",
	})

	token, user := registerUser(t, srv, "checkin-user", "")
	userID := uint64(user["id"].(float64))

	// 先造一条"昨天已签到"的记录：随后在"今天"签到应能成功（跨天可再签）。
	yesterday := time.Now().In(beijingZone).AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO checkin_records (user_id, checkin_date, quota, created_at) VALUES (?, ?, ?, ?)`,
		userID, yesterday, 5, time.Now().Unix()); err != nil {
		t.Fatalf("写入昨日签到记录失败: %v", err)
	}

	// 今天首次签到成功
	rec, body := callJSON(t, srv, http.MethodPost, "/api/user/checkin", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("今日签到应成功：状态 %d 响应 %v", rec.Code, body)
	}
	if checked, _ := body["checked_today"].(bool); !checked {
		t.Fatalf("签到后 checked_today 应为 true：%v", body)
	}

	// 同一天重复签到 → 409
	rec, _ = callJSON(t, srv, http.MethodPost, "/api/user/checkin", nil, token)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复签到应返回 409，实际 %d", rec.Code)
	}

	// 查询：累计 2 天、连续 2 天、今日已签
	rec, body = callJSON(t, srv, http.MethodGet, "/api/user/checkin", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询签到状态失败：状态 %d 响应 %v", rec.Code, body)
	}
	totalDays, _ := body["total_days"].(float64)
	streak, _ := body["streak_days"].(float64)
	if int(totalDays) != 2 || int(streak) != 2 {
		t.Fatalf("跨天累计应为 2 天、连续 2 天，实际 total=%v streak=%v", totalDays, streak)
	}
}

func TestRewardReferralOnRecharge_同一订单只返一次(t *testing.T) {
	srv, st := newReferralTestServer(t)
	ctx := context.Background()
	applySettings(t, st, map[string]string{
		model.SettingKeyReferralEnabled:       "true",
		model.SettingKeyReferralRechargeRatio: "10",
	})

	users := srv.deps.Users
	inviter := &model.User{Username: "inviter2", PasswordHash: "h", Role: model.UserRoleUser, Status: model.UserStatusEnabled, Quota: 0}
	invitee := &model.User{Username: "invitee2", PasswordHash: "h", Role: model.UserRoleUser, Status: model.UserStatusEnabled, Quota: 0}
	for _, u := range []*model.User{inviter, invitee} {
		if err := users.Create(ctx, u); err != nil {
			t.Fatalf("创建用户失败: %v", err)
		}
	}
	if err := srv.deps.Referrals.BindInviter(ctx, invitee.ID, inviter.ID); err != nil {
		t.Fatalf("绑定邀请关系失败: %v", err)
	}

	order := &model.PaymentOrder{TradeNo: "pay-referral-test-1", UserID: invitee.ID, Quota: 1000}

	// 第一次：应发 1000 × 10% = 100
	if err := srv.rewardReferralOnRecharge(ctx, order); err != nil {
		t.Fatalf("首次返利失败: %v", err)
	}
	// 第二次：同一订单号，应幂等跳过
	if err := srv.rewardReferralOnRecharge(ctx, order); err != nil {
		t.Fatalf("重复返利不应报错: %v", err)
	}

	reloaded, err := users.GetByID(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("读回邀请人失败: %v", err)
	}
	if reloaded.Quota != 100 {
		t.Fatalf("邀请人应只获得一次返利 100，实际 %d", reloaded.Quota)
	}
	total, err := srv.deps.Referrals.TotalRewardQuota(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("统计返利失败: %v", err)
	}
	if total != 100 {
		t.Fatalf("累计返利应为 100，实际 %d", total)
	}
}
