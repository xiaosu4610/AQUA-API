// 令牌「所属分组」接口的单元测试。
//
// 意图（Why）：
//
//	给令牌加分组，是为了让"用哪把密钥就走哪个分组"成为可能。但这套能力一旦
//	校验不严就会埋下两类难查的故障：
//	  1) 令牌指向一个不存在的分组 → 调用时无渠道可用（全量 404），且从列表上看不出原因；
//	  2) 更新部分字段时把已配置的分组意外清空 → 令牌静默回退到默认分组，价格随之改变。
//	本组用例把创建/更新的分组语义与校验边界锁死。
//
// 流转（Flow）：
//
//	go test ./internal/server/ -run TokenGroup → httptest 直接调用 Handler（带管理员/用户会话）
//
// 扩展（Extend）：
//
//	新增令牌字段（如限速）时，参照本文件的 fixture 一并补齐断言。
package server

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// tokenGroupFixture 汇总令牌分组测试所需的仓储、会话与归属用户。
type tokenGroupFixture struct {
	srv      *Server
	tokens   model.TokenRepository
	groups   model.ModelGroupRepository
	adminTok string
	userTok  string
	userID   uint64
}

// newTokenGroupFixture 构造含分组/令牌仓储、带管理员与普通用户会话的最小服务。
func newTokenGroupFixture(t *testing.T) *tokenGroupFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "token_group_test.db")
	st, err := store.Open("sqlite", dsn)
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

	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	ctx := context.Background()
	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())

	admin := &model.User{
		Username: "token-group-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled, Quota: model.QuotaUnlimited,
	}
	if err := users.Create(ctx, admin); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	owner := &model.User{
		Username: "token-group-user", PasswordHash: "test-hash",
		Role: model.UserRoleUser, Status: model.UserStatusEnabled, Quota: model.QuotaUnlimited,
	}
	if err := users.Create(ctx, owner); err != nil {
		t.Fatalf("创建普通用户失败: %v", err)
	}

	fx := &tokenGroupFixture{
		tokens:   store.NewTokenRepository(st.DB(), cipher),
		groups:   store.NewModelGroupRepository(st.DB()),
		userID:   owner.ID,
		adminTok: createTokenGroupSession(t, sessions, admin.ID),
		userTok:  createTokenGroupSession(t, sessions, owner.ID),
	}
	fx.srv = New(Deps{
		Config:      cfg,
		Store:       st,
		Channels:    store.NewChannelRepository(st.DB(), cipher),
		Groups:      fx.groups,
		Tokens:      fx.tokens,
		Users:       users,
		Sessions:    sessions,
		Settings:    store.NewSettingRepository(st.DB(), st.Dialect()),
		ModelPrices: store.NewModelPriceRepository(st.DB()),
	})
	return fx
}

// createTokenGroupSession 为用户建立一条有效会话并返回明文令牌。
func createTokenGroupSession(t *testing.T, sessions model.SessionRepository, userID uint64) string {
	t.Helper()
	token := "session-" + strconv.FormatUint(userID, 10) + "-token-group-test"
	if err := sessions.Create(context.Background(), &model.Session{
		UserID:    userID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	return token
}

// findTokenItemByID 在列表响应里按 ID 找到令牌卡片。
func findTokenItemByID(t *testing.T, body map[string]any, id uint64) map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应缺少 items 数组：%v", body)
	}
	for _, it := range raw {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if got, _ := item["id"].(float64); uint64(got) == id {
			return item
		}
	}
	return nil
}

// TestAdminTokenGroup_创建带合法分组 覆盖"带合法分组创建 → 读回一致"。
func TestAdminTokenGroup_创建带合法分组(t *testing.T) {
	fx := newTokenGroupFixture(t)
	ctx := context.Background()

	if err := fx.groups.Create(ctx, &model.ModelGroup{
		Name: "vip", DisplayName: "VIP", Ratio: 150, Enabled: true,
	}); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}

	reqBody := `{"user_id":` + strconv.FormatUint(fx.userID, 10) +
		`,"name":"分组令牌","unlimited_quota":true,"group_name":"vip"}`
	rec, created := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/tokens", fx.adminTok, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建令牌失败：%d %s", rec.Code, rec.Body.String())
	}
	if group, _ := created["group_name"].(string); group != "vip" {
		t.Fatalf("创建响应 group_name = %q，期望 vip", group)
	}
	id := uint64(created["id"].(float64))
	if id == 0 {
		t.Fatal("创建响应缺少有效 id")
	}

	// 落库校验：读回一致
	stored, err := fx.tokens.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if stored.GroupName != "vip" {
		t.Errorf("落库 GroupName = %q，期望 vip", stored.GroupName)
	}

	// 列表接口透出 group_name
	rec, list := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/tokens", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询令牌列表失败：%d %s", rec.Code, rec.Body.String())
	}
	item := findTokenItemByID(t, list, id)
	if item == nil {
		t.Fatal("列表未找到刚创建的令牌")
	}
	if group, _ := item["group_name"].(string); group != "vip" {
		t.Errorf("列表 group_name = %q，期望 vip", group)
	}
}

// TestAdminTokenGroup_创建指向不存在的分组被拒 覆盖"分组不存在 → 400 且信息可读"。
func TestAdminTokenGroup_创建指向不存在的分组被拒(t *testing.T) {
	fx := newTokenGroupFixture(t)

	reqBody := `{"user_id":` + strconv.FormatUint(fx.userID, 10) +
		`,"name":"坏令牌","unlimited_quota":true,"group_name":"not-exist"}`
	rec, body := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/tokens", fx.adminTok, reqBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("不存在的分组应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if msg := redeemErrorMessage(body); msg != "分组不存在" {
		t.Errorf("错误信息 = %q，期望 %q", msg, "分组不存在")
	}
}

// TestAdminTokenGroup_更新留空不修改分组 覆盖"更新时留空 / 字段缺失不改分组"。
//
// 这是最容易出事故的路径：前端只提交部分字段（如改名、启停）时，
// 若把未提交的分组当成空值写回，令牌会静默回退到默认分组，价格随之变化。
func TestAdminTokenGroup_更新留空不修改分组(t *testing.T) {
	fx := newTokenGroupFixture(t)
	ctx := context.Background()

	if err := fx.groups.Create(ctx, &model.ModelGroup{Name: "vip", Ratio: 150, Enabled: true}); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}

	// 1) 建一个 vip 分组的令牌
	createBody := `{"user_id":` + strconv.FormatUint(fx.userID, 10) +
		`,"name":"待更新令牌","unlimited_quota":true,"group_name":"vip"}`
	rec, created := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/tokens", fx.adminTok, createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建令牌失败：%d %s", rec.Code, rec.Body.String())
	}
	id := uint64(created["id"].(float64))
	path := "/api/admin/tokens/" + strconv.FormatUint(id, 10)

	// 2) 更新时【字段缺失】：只改名，不应动分组
	rec, _ = doBearerJSON(t, fx.srv, http.MethodPut, path, fx.adminTok, `{"name":"改名后的令牌"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新令牌失败：%d %s", rec.Code, rec.Body.String())
	}
	stored, err := fx.tokens.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if stored.Name != "改名后的令牌" {
		t.Errorf("名称应已更新，实际 %q", stored.Name)
	}
	if stored.GroupName != "vip" {
		t.Fatalf("字段缺失时分组应保持不变，实际 %q", stored.GroupName)
	}

	// 3) 更新时【显式传空串】：按约定同样视为"不修改"
	rec, _ = doBearerJSON(t, fx.srv, http.MethodPut, path, fx.adminTok, `{"name":"再改一次","group_name":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新令牌失败：%d %s", rec.Code, rec.Body.String())
	}
	stored, err = fx.tokens.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if stored.GroupName != "vip" {
		t.Fatalf("留空时分组应保持不变，实际 %q", stored.GroupName)
	}
}

// TestTokenGroup_非法分组名被拒 覆盖"大写 / 含空格"两类非法分组名。
func TestTokenGroup_非法分组名被拒(t *testing.T) {
	fx := newTokenGroupFixture(t)

	cases := []struct {
		name  string
		group string
	}{
		{name: "大写", group: "VIP"},
		{name: "含空格", group: "vip gold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqBody := `{"user_id":` + strconv.FormatUint(fx.userID, 10) +
				`,"name":"非法分组令牌","unlimited_quota":true,"group_name":"` + tc.group + `"}`
			rec, _ := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/tokens", fx.adminTok, reqBody)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("非法分组名应返回 400，实际 %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
