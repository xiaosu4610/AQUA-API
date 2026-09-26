// 兑换码接口的单元测试。
//
// 意图（Why）：
//
//	兑换码是"用户可自助领取额度"的入口，涉及两处必须锁死的行为：
//	  1) 后台生成后必须能把码取回来（否则管理员无从分发）；
//	  2) 兑换失败必须返回【精确】的原因与状态码（不存在/已使用/已过期/已作废），
//	     含糊的提示会让用户反复重试并引发无效客服沟通。
//
// 流转（Flow）：
//
//	go test ./internal/server/ → httptest 直接调用 Handler（带会话鉴权头）
//
// 扩展（Extend）：
//
//	新增兑换相关接口时，按"正常路径 + 精确错误路径"两类补充用例。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// redeemFixture 汇总兑换码接口测试所需的仓储与已登录凭据。
type redeemFixture struct {
	srv      *Server
	redeem   model.RedeemCodeRepository
	users    model.UserRepository
	adminTok string
	userTok  string
	userID   uint64
}

// newRedeemFixture 构造一个装配完整（含会话鉴权）但不监听端口的服务。
func newRedeemFixture(t *testing.T) *redeemFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "redeem_server_test.db")
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())
	redeem := store.NewRedeemCodeRepository(st.DB())

	ctx := context.Background()
	admin := &model.User{
		Username: "redeem-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled,
		Quota: model.QuotaUnlimited,
	}
	if err := users.Create(ctx, admin); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	user := &model.User{
		Username: "redeem-alice", PasswordHash: "test-hash",
		Role: model.UserRoleUser, Status: model.UserStatusEnabled,
		Quota: 1000,
	}
	if err := users.Create(ctx, user); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	srv := New(Deps{
		Config:      cfg,
		Store:       st,
		Users:       users,
		Sessions:    sessions,
		RedeemCodes: redeem,
		Settings:    store.NewSettingRepository(st.DB(), st.Dialect()),
	})

	return &redeemFixture{
		srv:      srv,
		redeem:   redeem,
		users:    users,
		adminTok: createRedeemTestSession(t, sessions, admin.ID),
		userTok:  createRedeemTestSession(t, sessions, user.ID),
		userID:   user.ID,
	}
}

// createRedeemTestSession 为用户建立一条有效会话并返回其明文令牌。
func createRedeemTestSession(t *testing.T, sessions model.SessionRepository, userID uint64) string {
	t.Helper()
	token := "session-" + strconv.FormatUint(userID, 10) + "-redeem-test-token"
	if err := sessions.Create(context.Background(), &model.Session{
		UserID:    userID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	return token
}

// doBearerJSON 带会话令牌发起一次请求（可选 JSON 请求体），并解析响应体。
func doBearerJSON(t *testing.T, srv *Server, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	parsed := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	}
	return rec, parsed
}

// TestRedeemCodes_后台生成与列表 覆盖"生成一批码 → 列表能查到"的主链路。
func TestRedeemCodes_后台生成与列表(t *testing.T) {
	fx := newRedeemFixture(t)

	// 未登录访问后台接口必须被拒绝
	rec, _ := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/redeem-codes", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带会话应返回 401，实际 %d", rec.Code)
	}

	rec, body := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/redeem-codes",
		fx.adminTok, `{"count":3,"quota":500,"expires_days":0,"remark":"测试活动"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("生成兑换码失败：%d %s", rec.Code, rec.Body.String())
	}
	batchNo, _ := body["batch_no"].(string)
	if batchNo == "" {
		t.Fatalf("应返回批次号：%v", body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("应生成 3 张码，实际 %d", len(items))
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		code, _ := item["code"].(string)
		if code == "" || code != strings.ToUpper(code) {
			t.Fatalf("兑换码应非空且为大写，实际 %q", code)
		}
		if strings.ContainsAny(code, "0O1IL") {
			t.Fatalf("兑换码不应包含易混字符，实际 %q", code)
		}
		if quota, _ := item["quota"].(float64); quota != 500 {
			t.Fatalf("额度应为 500，实际 %v", item["quota"])
		}
	}

	// 列表：默认分页能看到刚生成的 3 张
	rec, body = doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/redeem-codes?page=1&size=20", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询列表失败：%d %s", rec.Code, rec.Body.String())
	}
	if total, _ := body["total"].(float64); total != 3 {
		t.Fatalf("列表总数应为 3，实际 %v", body["total"])
	}

	// 按批次筛选同样命中
	_, body = doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/redeem-codes?batch_no="+batchNo, fx.adminTok, "")
	if total, _ := body["total"].(float64); total != 3 {
		t.Fatalf("按批次筛选应命中 3 张，实际 %v", body["total"])
	}
}

// TestUserRedeem_兑换成功与精确错误 覆盖"兑换成功 → 重复兑换 → 不存在"三条路径。
func TestUserRedeem_兑换成功与精确错误(t *testing.T) {
	fx := newRedeemFixture(t)
	ctx := context.Background()

	code, err := model.GenerateRedeemCode()
	if err != nil {
		t.Fatalf("生成兑换码失败: %v", err)
	}
	if err := fx.redeem.CreateBatch(ctx, []*model.RedeemCode{{
		Code: code, Quota: 500, Status: model.RedeemStatusUnused, BatchNo: "B-1",
	}}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}

	// 1) 兑换成功：本次额度 500，总额度 1000 + 500
	rec, body := doBearerJSON(t, fx.srv, http.MethodPost, "/api/user/redeem",
		fx.userTok, `{"code":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("兑换应成功：%d %s", rec.Code, rec.Body.String())
	}
	if quota, _ := body["quota"].(float64); quota != 500 {
		t.Fatalf("本次获得额度应为 500，实际 %v", body["quota"])
	}
	if total, _ := body["total_quota"].(float64); total != 1500 {
		t.Fatalf("兑换后总额度应为 1500，实际 %v", body["total_quota"])
	}

	// 2) 重复兑换：必须返回 409 且指明"已被使用"
	rec, body = doBearerJSON(t, fx.srv, http.MethodPost, "/api/user/redeem",
		fx.userTok, `{"code":"`+code+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复兑换应返回 409，实际 %d", rec.Code)
	}
	if code := redeemErrorCode(body); code != "redeem_code_used" {
		t.Fatalf("重复兑换错误码应为 redeem_code_used，实际 %q", code)
	}
	if message := redeemErrorMessage(body); !strings.Contains(message, "已被使用") {
		t.Fatalf("重复兑换提示应包含“已被使用”，实际 %q", message)
	}

	// 3) 不存在的码：404
	rec, body = doBearerJSON(t, fx.srv, http.MethodPost, "/api/user/redeem",
		fx.userTok, `{"code":"NO-SUCH-CODE-0000"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的码应返回 404，实际 %d", rec.Code)
	}
	if code := redeemErrorCode(body); code != "redeem_code_not_found" {
		t.Fatalf("错误码应为 redeem_code_not_found，实际 %q", code)
	}
}

// TestUserRedeem_过期与作废返回精确状态码 覆盖两种终态的差异提示。
func TestUserRedeem_过期与作废返回精确状态码(t *testing.T) {
	fx := newRedeemFixture(t)
	ctx := context.Background()

	expiredCode, _ := model.GenerateRedeemCode()
	voidCode, _ := model.GenerateRedeemCode()
	if err := fx.redeem.CreateBatch(ctx, []*model.RedeemCode{
		{Code: expiredCode, Quota: 100, Status: model.RedeemStatusUnused, ExpiresAt: time.Now().Add(-time.Hour)},
		{Code: voidCode, Quota: 100, Status: model.RedeemStatusVoid},
	}); err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}

	rec, body := doBearerJSON(t, fx.srv, http.MethodPost, "/api/user/redeem",
		fx.userTok, `{"code":"`+expiredCode+`"}`)
	if rec.Code != http.StatusGone || redeemErrorCode(body) != "redeem_code_expired" {
		t.Fatalf("过期码应返回 410 / redeem_code_expired，实际 %d %v", rec.Code, body)
	}

	rec, body = doBearerJSON(t, fx.srv, http.MethodPost, "/api/user/redeem",
		fx.userTok, `{"code":"`+voidCode+`"}`)
	if rec.Code != http.StatusConflict || redeemErrorCode(body) != "redeem_code_void" {
		t.Fatalf("作废码应返回 409 / redeem_code_void，实际 %d %v", rec.Code, body)
	}
}

// redeemErrorCode / redeemErrorMessage 从 OpenAI 风格错误体中取出 code / message。
func redeemErrorCode(body map[string]any) string {
	errObj, _ := body["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func redeemErrorMessage(body map[string]any) string {
	errObj, _ := body["error"].(map[string]any)
	message, _ := errObj["message"].(string)
	return message
}
