// 模型实体 / 渠道模型映射接口与映射解析纯函数的单元测试。
//
// 意图（Why）：
//
//	模型映射是"对外名 ↔ 上游名"双向解析的依据，一旦匹配规则出错，用户请求的
//	模型会被发到错误的上游，或在回包时被回写成错误的名字——两类问题都极难排查。
//	本组用例锁死三件事：
//	  1) 后台模型增删改查与筛选链路可用；
//	  2) 映射整组替换的语义正确（旧行被清空、新行落库）；
//	  3) ResolveMapping / ResolvePublicModel 的精确、通配、优先级与确定性行为。
//
// 流转（Flow）：
//
//	go test ./internal/server/ -run Model → httptest 直接调用 Handler（带管理员会话）
//
// 扩展（Extend）：
//
//	新增匹配规则时，在 TestResolveMapping_* 中补充"最优先者"的期望值。
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

// modelMetaFixture 汇总模型相关接口测试所需的仓储与管理员会话。
type modelMetaFixture struct {
	srv      *Server
	models   model.ModelRepository
	mappings model.ChannelModelMappingRepository
	channels model.ChannelRepository
	tokens   model.TokenRepository
	prices   model.ModelPriceRepository
	adminTok string
}

// newModelMetaFixture 构造含模型/映射/渠道/令牌/计价仓储、带管理员会话的最小服务。
func newModelMetaFixture(t *testing.T) *modelMetaFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "model_meta_test.db")
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

	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())
	admin := &model.User{
		Username: "model-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled,
		Quota: model.QuotaUnlimited,
	}
	if err := users.Create(context.Background(), admin); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}

	fx := &modelMetaFixture{
		models:   store.NewModelMetaRepository(st.DB()),
		mappings: store.NewChannelModelMappingRepository(st.DB()),
		channels: store.NewChannelRepository(st.DB(), cipher),
		tokens:   store.NewTokenRepository(st.DB(), cipher),
		prices:   store.NewModelPriceRepository(st.DB()),
	}
	fx.srv = New(Deps{
		Config:               cfg,
		Store:                st,
		Channels:             fx.channels,
		Users:                users,
		Sessions:             sessions,
		Settings:             store.NewSettingRepository(st.DB(), st.Dialect()),
		Models:               fx.models,
		ChannelModelMappings: fx.mappings,
		ModelPrices:          fx.prices,
		Tokens:               fx.tokens,
	})
	fx.adminTok = createModelMetaSession(t, sessions, admin.ID)
	return fx
}

// createModelMetaSession 为管理员建立一条有效会话并返回明文令牌。
func createModelMetaSession(t *testing.T, sessions model.SessionRepository, userID uint64) string {
	t.Helper()
	token := "session-" + strconv.FormatUint(userID, 10) + "-model-meta-token"
	if err := sessions.Create(context.Background(), &model.Session{
		UserID:    userID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	return token
}

// modelItems 把列表响应里的 items 转成切片，便于断言。
func modelItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应缺少 items 数组：%v", body)
	}
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		card, ok := item.(map[string]any)
		if ok {
			result = append(result, card)
		}
	}
	return result
}

// TestModelMetaAdmin_未登录被拒 确认管理接口统一守卫。
func TestModelMetaAdmin_未登录被拒(t *testing.T) {
	fx := newModelMetaFixture(t)

	rec, _ := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/models", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带会话应返回 401，实际 %d", rec.Code)
	}
}

// TestModelMetaAdmin_增删改查 覆盖创建 / 列表筛选 / 更新 / 删除主链路。
func TestModelMetaAdmin_增删改查(t *testing.T) {
	fx := newModelMetaFixture(t)

	// 创建
	rec, created := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/models", fx.adminTok,
		`{"name":"deepseek-chat","display_name":"DeepSeek 对话","vendor":"deepseek","context_length":65536,"capabilities":["chat","stream"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建模型失败：%d %s", rec.Code, rec.Body.String())
	}
	id, _ := created["id"].(float64)
	if id <= 0 {
		t.Fatalf("创建响应应带有效 id：%v", created)
	}
	path := "/api/admin/models/" + strconv.FormatUint(uint64(id), 10)

	// 同名再建必须 409
	rec, dup := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/models", fx.adminTok,
		`{"name":"deepseek-chat"}`)
	if rec.Code != http.StatusConflict || redeemErrorCode(dup) != "model_duplicated" {
		t.Fatalf("重名应返回 409/model_duplicated，实际 %d %v", rec.Code, dup)
	}

	// 列表 + 厂商筛选命中
	rec, list := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/models?vendor=deepseek", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询列表失败：%d %s", rec.Code, rec.Body.String())
	}
	items := modelItems(t, list)
	if len(items) != 1 {
		t.Fatalf("厂商筛选后应剩 1 个模型，实际 %d", len(items))
	}
	caps, _ := items[0]["capabilities"].([]any)
	if len(caps) != 2 {
		t.Fatalf("能力标签应原样返回 2 项，实际 %v", items[0]["capabilities"])
	}

	// 更新（不改名）
	rec, updated := doBearerJSON(t, fx.srv, http.MethodPut, path, fx.adminTok,
		`{"display_name":"对话改","context_length":131072,"enabled":false,"capabilities":["chat"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新模型失败：%d %s", rec.Code, rec.Body.String())
	}
	if ctx, _ := updated["context_length"].(float64); ctx != 131072 {
		t.Fatalf("上下文长度应更新为 131072，实际 %v", updated["context_length"])
	}
	if enabled, _ := updated["enabled"].(bool); enabled {
		t.Fatal("启用状态应更新为 false")
	}

	// 改名必须被拒绝
	rec, rename := doBearerJSON(t, fx.srv, http.MethodPut, path, fx.adminTok,
		`{"name":"renamed"}`)
	if rec.Code != http.StatusBadRequest || redeemErrorCode(rename) != "model_name_immutable" {
		t.Fatalf("改名应返回 400/model_name_immutable，实际 %d %v", rec.Code, rename)
	}

	// 删除后再删应 404
	rec, _ = doBearerJSON(t, fx.srv, http.MethodDelete, path, fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败：%d %s", rec.Code, rec.Body.String())
	}
	rec, _ = doBearerJSON(t, fx.srv, http.MethodDelete, path, fx.adminTok, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应返回 404，实际 %d", rec.Code)
	}
}

// TestChannelModelMappings_整组替换 覆盖映射整组提交与读回。
func TestChannelModelMappings_整组替换(t *testing.T) {
	fx := newModelMetaFixture(t)

	channel := &model.Channel{
		Name: "映射渠道", Type: 1, BaseURL: "https://api.example.com", APIKey: "sk-x",
		Models: []string{"deepseek-chat"}, Group: model.DefaultGroupName,
		Priority: 1, Weight: 1, Status: model.ChannelStatusEnabled,
	}
	if err := fx.channels.Create(context.Background(), channel); err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}
	base := "/api/admin/channels/" + strconv.FormatUint(channel.ID, 10) + "/mappings"

	// 首次整组写入 2 条
	rec, body := doBearerJSON(t, fx.srv, http.MethodPut, base, fx.adminTok,
		`{"items":[{"upstream_model":"deepseek-v3","public_model":"deepseek-chat","priority":10},{"upstream_model":"deepseek-r1","public_model":"deepseek-reasoner"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("写入映射失败：%d %s", rec.Code, rec.Body.String())
	}
	if total, _ := body["total"].(float64); total != 2 {
		t.Fatalf("应保存 2 条映射，实际 %v", body["total"])
	}
	// 默认优先级 0 且按优先级降序：priority=10 的排在前
	items := modelItems(t, body)
	first, _ := items[0]["public_model"].(string)
	if first != "deepseek-chat" {
		t.Fatalf("应按优先级降序，实际首项 %q", first)
	}

	// 读回
	rec, _ = doBearerJSON(t, fx.srv, http.MethodGet, base, fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取映射失败：%d %s", rec.Code, rec.Body.String())
	}

	// 整组替换为 1 条：旧的 2 条必须被清空
	rec, replaced := doBearerJSON(t, fx.srv, http.MethodPut, base, fx.adminTok,
		`{"items":[{"upstream_model":"gpt-4o","public_model":"gpt-4o"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("替换映射失败：%d %s", rec.Code, rec.Body.String())
	}
	if total, _ := replaced["total"].(float64); total != 1 {
		t.Fatalf("整组替换后应剩 1 条，实际 %v", replaced["total"])
	}

	// 同渠道内上游名重复必须被拒
	rec, dup := doBearerJSON(t, fx.srv, http.MethodPut, base, fx.adminTok,
		`{"items":[{"upstream_model":"dup","public_model":"a"},{"upstream_model":"dup","public_model":"b"}]}`)
	if rec.Code != http.StatusBadRequest || redeemErrorCode(dup) != "mapping_duplicated" {
		t.Fatalf("重复上游名应返回 400/mapping_duplicated，实际 %d %v", rec.Code, dup)
	}

	// 渠道不存在时 404
	rec, _ = doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/channels/999999/mappings", fx.adminTok, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的渠道应返回 404，实际 %d", rec.Code)
	}
}

// TestModelMetaReferences_统计引用 覆盖删除前引用统计的四个口径。
func TestModelMetaReferences_统计引用(t *testing.T) {
	fx := newModelMetaFixture(t)
	ctx := context.Background()

	// 渠道声明该模型
	channel := &model.Channel{
		Name: "引用渠道", Type: 1, BaseURL: "https://api.example.com", APIKey: "sk-x",
		Models: []string{"deepseek-chat", "other"}, Group: model.DefaultGroupName,
		Priority: 1, Weight: 1, Status: model.ChannelStatusEnabled,
	}
	if err := fx.channels.Create(ctx, channel); err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}
	// 令牌白名单包含该模型
	key, err := model.GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	if err := fx.tokens.Create(ctx, &model.Token{
		Name: "引用令牌", Key: key, Status: model.TokenStatusEnabled,
		UnlimitedQuota: true, Models: []string{"deepseek-chat"},
	}); err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}
	// 计价规则引用该模型（落在 default 分组）
	if err := fx.prices.Create(ctx, &model.ModelPrice{
		Model: "deepseek-chat", PerCallPrice: 100, Group: model.DefaultGroupName, Enabled: true,
	}); err != nil {
		t.Fatalf("创建计价规则失败: %v", err)
	}
	// 渠道映射引用该模型
	if err := fx.mappings.ReplaceForChannel(ctx, channel.ID, []*model.ChannelModelMapping{
		{UpstreamModel: "deepseek-v3", PublicModel: "deepseek-chat", Enabled: true},
	}); err != nil {
		t.Fatalf("写入映射失败: %v", err)
	}

	rec, body := doBearerJSON(t, fx.srv, http.MethodGet,
		"/api/admin/models/deepseek-chat/references", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询引用统计失败：%d %s", rec.Code, rec.Body.String())
	}
	if v, _ := body["token_count"].(float64); v != 1 {
		t.Fatalf("token_count 应为 1，实际 %v", body["token_count"])
	}
	if v, _ := body["channel_count"].(float64); v != 1 {
		t.Fatalf("channel_count 应为 1，实际 %v", body["channel_count"])
	}
	if v, _ := body["group_count"].(float64); v != 1 {
		t.Fatalf("group_count 应为 1，实际 %v", body["group_count"])
	}
	if v, _ := body["mapping_count"].(float64); v != 1 {
		t.Fatalf("mapping_count 应为 1，实际 %v", body["mapping_count"])
	}
}

// TestResolveMapping_精确优先于通配 验证精确匹配压过通配匹配。
func TestResolveMapping_精确优先于通配(t *testing.T) {
	mappings := []*model.ChannelModelMapping{
		{ID: 1, PublicModel: "deepseek-*", UpstreamModel: "wildcard-upstream", Enabled: true},
		{ID: 2, PublicModel: "deepseek-chat", UpstreamModel: "exact-upstream", Enabled: true},
	}
	got, ok := model.ResolveMapping(mappings, "deepseek-chat")
	if !ok || got != "exact-upstream" {
		t.Fatalf("精确匹配应优先，实际 (%q, %v)", got, ok)
	}
}

// TestResolveMapping_最长前缀优先级与确定性 验证通配竞争时的稳定胜出者。
func TestResolveMapping_最长前缀优先级与确定性(t *testing.T) {
	// 同长度前缀、同优先级：按 ID 升序，ID=3 胜
	sameLen := []*model.ChannelModelMapping{
		{ID: 5, PublicModel: "deepseek-*", UpstreamModel: "id5", Enabled: true},
		{ID: 3, PublicModel: "deepseek-*", UpstreamModel: "id3", Enabled: true},
	}
	if got, _ := model.ResolveMapping(sameLen, "deepseek-chat"); got != "id3" {
		t.Fatalf("同前缀同优先级应按 ID 升序取 id3，实际 %q", got)
	}

	// 最长前缀优先：更长的 deepseek-r1-* 压过 deepseek-*
	longest := []*model.ChannelModelMapping{
		{ID: 1, PublicModel: "deepseek-*", UpstreamModel: "short", Enabled: true},
		{ID: 2, PublicModel: "deepseek-r1-*", UpstreamModel: "long", Enabled: true},
	}
	if got, _ := model.ResolveMapping(longest, "deepseek-r1-preview"); got != "long" {
		t.Fatalf("最长前缀应优先，实际 %q", got)
	}

	// 优先级压过前缀长度：priority=100 的短前缀胜过长前缀
	priority := []*model.ChannelModelMapping{
		{ID: 1, PublicModel: "deepseek-r1-*", UpstreamModel: "long", Priority: 0, Enabled: true},
		{ID: 2, PublicModel: "deepseek-*", UpstreamModel: "prio", Priority: 100, Enabled: true},
	}
	if got, _ := model.ResolveMapping(priority, "deepseek-r1-preview"); got != "prio" {
		t.Fatalf("高优先级应优先，实际 %q", got)
	}
}

// TestResolveMapping_通配捕获替换与未命中 验证结果侧通配替换与边界。
func TestResolveMapping_通配捕获替换与未命中(t *testing.T) {
	mappings := []*model.ChannelModelMapping{
		{ID: 1, PublicModel: "aqua-*", UpstreamModel: "deepseek-*", Enabled: true},
		{ID: 2, PublicModel: "disabled-*", UpstreamModel: "nope", Enabled: false},
	}
	// 对外 aqua-chat → 上游 deepseek-chat（捕获 chat 替换结果侧通配）
	if got, ok := model.ResolveMapping(mappings, "aqua-chat"); !ok || got != "deepseek-chat" {
		t.Fatalf("通配捕获应产出 deepseek-chat，实际 (%q, %v)", got, ok)
	}
	// 停用的映射不参与匹配
	if _, ok := model.ResolveMapping(mappings, "disabled-x"); ok {
		t.Fatal("停用的映射不应命中")
	}
	// 完全未命中
	if _, ok := model.ResolveMapping(mappings, "gpt-4o"); ok {
		t.Fatal("未匹配应返回 not ok")
	}
	// 空输入
	if _, ok := model.ResolveMapping(mappings, ""); ok {
		t.Fatal("空模型名不应命中")
	}
}

// TestResolvePublicModel_反向回写 验证上游名回写为对外名。
func TestResolvePublicModel_反向回写(t *testing.T) {
	mappings := []*model.ChannelModelMapping{
		{ID: 1, UpstreamModel: "deepseek-chat-*", PublicModel: "deepseek-chat", Enabled: true},
		{ID: 2, UpstreamModel: "deepseek-r1", PublicModel: "deepseek-reasoner", Enabled: true},
	}
	// 上游返回带版本后缀的名字 → 回写为稳定的对外别名
	if got, ok := model.ResolvePublicModel(mappings, "deepseek-chat-0731"); !ok || got != "deepseek-chat" {
		t.Fatalf("应回写为 deepseek-chat，实际 (%q, %v)", got, ok)
	}
	// 精确上游名 → 对外别名
	if got, ok := model.ResolvePublicModel(mappings, "deepseek-r1"); !ok || got != "deepseek-reasoner" {
		t.Fatalf("应回写为 deepseek-reasoner，实际 (%q, %v)", got, ok)
	}
	// 未命中
	if _, ok := model.ResolvePublicModel(mappings, "gpt-4o"); ok {
		t.Fatal("未命中应返回 not ok")
	}
}
