// 渠道凭据调度策略与凭据调度参数的接口测试。
//
// 意图（Why）：
//
//	调度策略与凭据参数此前只能改数据库，站长无从配置。本组用例锁死三件事：
//	  1) 非法策略必须 400 且给出可选值，不能被静默兜底成默认值；
//	  2) 合法策略与凭据的 weight / priority / rpm_limit 必须能保存并原样读回；
//	  3) key-strategies 目录必须完整返回五种策略（且密钥字段不下发硬编码文案）。
//
// 流转（Flow）：
//
//	go test ./internal/server/ -run Key → httptest 直接调用 Handler（带管理员会话）
//
// 扩展（Extend）：
//
//	新增策略时，在 TestKeyStrategies_返回五种且字段完整 的期望集合里补充标识。
package server

import (
	"context"
	"io"
	"net/http"
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

// channelStrategyFixture 汇总调度相关接口测试所需的仓储与管理员会话。
type channelStrategyFixture struct {
	srv      *Server
	channels model.ChannelRepository
	keys     model.ChannelKeyRepository
	adminTok string
}

// newChannelStrategyFixture 构造含渠道与密钥池仓储、带管理员会话的最小服务。
func newChannelStrategyFixture(t *testing.T) *channelStrategyFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "channel_strategy_test.db")
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

	channels := store.NewChannelRepository(st.DB(), cipher)
	keys := store.NewChannelKeyRepository(st.DB(), cipher)
	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())

	admin := &model.User{
		Username: "strategy-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled,
		Quota: model.QuotaUnlimited,
	}
	if err := users.Create(context.Background(), admin); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}

	srv := New(Deps{
		Config:      cfg,
		Store:       st,
		Channels:    channels,
		ChannelKeys: keys,
		Users:       users,
		Sessions:    sessions,
		Settings:    store.NewSettingRepository(st.DB(), st.Dialect()),
	})

	return &channelStrategyFixture{
		srv:      srv,
		channels: channels,
		keys:     keys,
		adminTok: createChannelStrategySession(t, sessions, admin.ID),
	}
}

// createChannelStrategySession 为管理员建立一条有效会话并返回明文令牌。
func createChannelStrategySession(t *testing.T, sessions model.SessionRepository, userID uint64) string {
	t.Helper()
	token := "session-" + strconv.FormatUint(userID, 10) + "-strategy-test-token"
	if err := sessions.Create(context.Background(), &model.Session{
		UserID:    userID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	return token
}

// createChannelViaAPI 通过管理接口创建一个渠道，返回响应体。
func createChannelViaAPI(t *testing.T, fx *channelStrategyFixture, body string) map[string]any {
	t.Helper()
	rec, resp := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/channels", fx.adminTok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建渠道失败：%d %s", rec.Code, rec.Body.String())
	}
	return resp
}

// channelIDOf 从渠道响应体里取出 id。
func channelIDOf(t *testing.T, body map[string]any) string {
	t.Helper()
	id, ok := body["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("响应缺少有效的渠道 id：%v", body)
	}
	return strconv.FormatUint(uint64(id), 10)
}

// TestChannelKeyStrategy_非法值返回400 覆盖新增与更新两条路径上的策略校验。
func TestChannelKeyStrategy_非法值返回400(t *testing.T) {
	fx := newChannelStrategyFixture(t)

	// 1) 新建时提交非法策略
	rec, body := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/channels", fx.adminTok,
		`{"name":"非法策略渠道","type":1,"base_url":"https://api.example.com","group":"default","priority":1,"weight":1,"status":1,"api_key":"sk-x","key_strategy":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法策略应返回 400，实际 %d，响应：%s", rec.Code, rec.Body.String())
	}
	if code := redeemErrorCode(body); code != "invalid_key_strategy" {
		t.Fatalf("错误码应为 invalid_key_strategy，实际 %q", code)
	}
	// 错误信息必须列出可选值，管理员才能自助修正
	if msg := redeemErrorMessage(body); !strings.Contains(msg, "round_robin") {
		t.Fatalf("错误信息应列出可选策略，实际 %q", msg)
	}

	// 2) 更新时提交非法策略
	created := createChannelViaAPI(t, fx,
		`{"name":"正常渠道","type":1,"base_url":"https://api.example.com","group":"default","priority":1,"weight":1,"status":1,"api_key":"sk-x"}`)
	path := "/api/admin/channels/" + channelIDOf(t, created)
	rec, body = doBearerJSON(t, fx.srv, http.MethodPut, path, fx.adminTok,
		`{"name":"正常渠道","type":1,"base_url":"https://api.example.com","group":"default","priority":1,"weight":1,"status":1,"key_strategy":"nope"}`)
	if rec.Code != http.StatusBadRequest || redeemErrorCode(body) != "invalid_key_strategy" {
		t.Fatalf("更新时非法策略应返回 400/invalid_key_strategy，实际 %d %v", rec.Code, body)
	}
}

// TestChannelKeyStrategy_合法值保存并读回 覆盖策略的往返与"部分更新不重置"。
func TestChannelKeyStrategy_合法值保存并读回(t *testing.T) {
	fx := newChannelStrategyFixture(t)

	created := createChannelViaAPI(t, fx,
		`{"name":"轮询渠道","type":1,"base_url":"https://api.example.com","group":"default","priority":1,"weight":1,"status":1,"api_key":"sk-x","key_strategy":"round_robin"}`)
	if got, _ := created["key_strategy"].(string); got != "round_robin" {
		t.Fatalf("创建响应策略应为 round_robin，实际 %q", got)
	}
	path := "/api/admin/channels/" + channelIDOf(t, created)

	// 详情读回
	rec, detail := doBearerJSON(t, fx.srv, http.MethodGet, path, fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取渠道详情失败：%d %s", rec.Code, rec.Body.String())
	}
	if got, _ := detail["key_strategy"].(string); got != "round_robin" {
		t.Fatalf("详情读回策略应为 round_robin，实际 %q", got)
	}

	// 更新为另一合法策略
	rec, updated := doBearerJSON(t, fx.srv, http.MethodPut, path, fx.adminTok,
		`{"name":"轮询渠道","type":1,"base_url":"https://api.example.com","group":"default","priority":1,"weight":1,"status":1,"key_strategy":"weighted_random"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新策略失败：%d %s", rec.Code, rec.Body.String())
	}
	if got, _ := updated["key_strategy"].(string); got != "weighted_random" {
		t.Fatalf("更新响应策略应为 weighted_random，实际 %q", got)
	}

	// 只提交部分字段（不带 key_strategy）时，策略必须保持不变——
	// 否则前端"仅切换状态"就会把策略重置回默认值。
	rec, partial := doBearerJSON(t, fx.srv, http.MethodPut, path, fx.adminTok,
		`{"name":"轮询渠道","type":1,"base_url":"https://api.example.com","group":"default","priority":5,"weight":2,"status":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("部分更新失败：%d %s", rec.Code, rec.Body.String())
	}
	if got, _ := partial["key_strategy"].(string); got != "weighted_random" {
		t.Fatalf("部分更新不应重置策略，实际 %q", got)
	}
}

// TestChannelKeys_调度参数保存并读回 覆盖凭据的 weight/priority/rpm_limit 与运行态字段透出。
func TestChannelKeys_调度参数保存并读回(t *testing.T) {
	fx := newChannelStrategyFixture(t)

	created := createChannelViaAPI(t, fx,
		`{"name":"池渠道","type":1,"base_url":"https://api.example.com","group":"default","priority":1,"weight":1,"status":1,"keys_text":"nvapi-aaaaaa 池1\nnvapi-bbbbbb 池2"}`)
	base := "/api/admin/channels/" + channelIDOf(t, created) + "/keys"

	rec, list := doBearerJSON(t, fx.srv, http.MethodGet, base, fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取密钥池失败：%d %s", rec.Code, rec.Body.String())
	}
	items, _ := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("应导入 2 把凭据，实际 %d", len(items))
	}
	first, _ := items[0].(map[string]any)
	// 默认调度参数：weight=1（迁移默认值），priority=0，rpm_limit=0（不限速）
	if w, _ := first["weight"].(float64); w != 1 {
		t.Fatalf("凭据默认权重应为 1，实际 %v", first["weight"])
	}
	if p, _ := first["priority"].(float64); p != 0 {
		t.Fatalf("凭据默认优先级应为 0，实际 %v", first["priority"])
	}
	if r, _ := first["rpm_limit"].(float64); r != 0 {
		t.Fatalf("凭据默认每分钟上限应为 0（不限），实际 %v", first["rpm_limit"])
	}
	// 运行态字段必须一并透出（前端只读展示在途数与冷却）
	if _, ok := first["in_flight"]; !ok {
		t.Fatal("密钥 DTO 应包含 in_flight")
	}
	if _, ok := first["cooldown_until"]; !ok {
		t.Fatal("密钥 DTO 应包含 cooldown_until")
	}

	keyID, _ := first["id"].(float64)
	keyPath := "/api/admin/keys/" + strconv.FormatUint(uint64(keyID), 10)

	// 保存调度参数
	rec, _ = doBearerJSON(t, fx.srv, http.MethodPut, keyPath, fx.adminTok,
		`{"weight":5,"priority":3,"rpm_limit":60}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存调度参数失败：%d %s", rec.Code, rec.Body.String())
	}

	// 读回校验
	_, list = doBearerJSON(t, fx.srv, http.MethodGet, base, fx.adminTok, "")
	items, _ = list["items"].([]any)
	first, _ = items[0].(map[string]any)
	if w, _ := first["weight"].(float64); w != 5 {
		t.Fatalf("权重应为 5，实际 %v", first["weight"])
	}
	if p, _ := first["priority"].(float64); p != 3 {
		t.Fatalf("优先级应为 3，实际 %v", first["priority"])
	}
	if r, _ := first["rpm_limit"].(float64); r != 60 {
		t.Fatalf("每分钟上限应为 60，实际 %v", first["rpm_limit"])
	}

	// 原有状态更新能力保持不变（向后兼容）
	rec, statusResp := doBearerJSON(t, fx.srv, http.MethodPut, keyPath, fx.adminTok, `{"status":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新密钥状态失败：%d %s", rec.Code, rec.Body.String())
	}
	if status, _ := statusResp["status"].(float64); status != 2 {
		t.Fatalf("状态应更新为 2，实际 %v", statusResp["status"])
	}

	// 只给部分调度参数应被拒绝（避免未给项被误写 0）
	rec, body := doBearerJSON(t, fx.srv, http.MethodPut, keyPath, fx.adminTok, `{"weight":1}`)
	if rec.Code != http.StatusBadRequest || redeemErrorCode(body) != "incomplete_scheduling" {
		t.Fatalf("部分调度参数应返回 400/incomplete_scheduling，实际 %d %v", rec.Code, body)
	}
}

// TestKeyStrategies_返回五种且字段完整 覆盖策略目录接口。
func TestKeyStrategies_返回五种且字段完整(t *testing.T) {
	fx := newChannelStrategyFixture(t)

	// 未登录访问必须被拒绝（管理接口统一守卫）
	rec, _ := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/key-strategies", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带会话应返回 401，实际 %d", rec.Code)
	}

	rec, body := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/key-strategies", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取策略目录失败：%d %s", rec.Code, rec.Body.String())
	}

	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应缺少 items 数组：%s", rec.Body.String())
	}
	if len(items) != 5 {
		t.Fatalf("应返回 5 种策略，实际 %d", len(items))
	}

	want := map[string]bool{
		"sequential":      true,
		"round_robin":     true,
		"weighted_random": true,
		"least_recent":    true,
		"least_in_flight": true,
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		key, _ := item["key"].(string)
		if !want[key] {
			t.Fatalf("出现未预期的策略标识：%q", key)
		}
		if label, _ := item["label"].(string); label == "" {
			t.Fatalf("策略 %q 缺少中文名", key)
		}
		if desc, _ := item["description"].(string); desc == "" {
			t.Fatalf("策略 %q 缺少一句话说明", key)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("策略目录缺少：%v", want)
	}
}
