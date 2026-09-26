// 模型分组与模型广场接口的单元测试。
//
// 意图（Why）：
//
//	模型广场是本项目数据组装最复杂的一处：它要把「渠道声明的模型」
//	与「计价规则」两份数据合并成模型卡片，并算出每个分组的价格与倍率。
//	组装规则一旦写错，用户就会看到与实际扣费不符的价格——
//	这是最能直接引发投诉的一类缺陷，必须用测试锁死。
//
// 流转（Flow）：
//
//	go test ./internal/server/ → httptest 直接调用 Handler
//
// 扩展（Extend）：
//
//	新增广场字段（如上下文长度）时，在断言中一并补充校验。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/payment"
	"gitee.com/xiaosu4610/aqua-api/internal/relay"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// plazaFixture 汇总测试所需的仓储与已创建的访问令牌。
//
// 为什么要带上访问令牌：/v1/models 挂在令牌鉴权之后，
// 没有可用令牌就无法验证它的真实行为（而它正是生产环境里暴露出来的缺口）。
type plazaFixture struct {
	server       *Server
	channels     model.ChannelRepository
	groups       model.ModelGroupRepository
	prices       model.ModelPriceRepository
	tokenRepo    model.TokenRepository
	userRepo     model.UserRepository
	plainToken   string
	tokenOwnerID uint64
}

// newPlazaFixture 构造包含分组 / 计价规则 / 渠道 / 令牌的最小可用服务。
func newPlazaFixture(t *testing.T) *plazaFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "plaza_test.db")
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
	cfg.Server.Mode = "test"          // gin 测试模式，抑制调试输出
	cfg.Server.Listen = "127.0.0.1:0" // 端口 0：测试不会真正监听

	channels := store.NewChannelRepository(st.DB(), cipher)
	groups := store.NewModelGroupRepository(st.DB())
	priceRepo := store.NewModelPriceRepository(st.DB())
	tokenRepo := store.NewTokenRepository(st.DB(), cipher)
	userRepo := store.NewUserRepository(st.DB())

	ctx := context.Background()
	owner := &model.User{
		Username:     "plaza-owner",
		PasswordHash: "test-hash",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusEnabled,
		Quota:        model.QuotaUnlimited,
	}
	if err := userRepo.Create(ctx, owner); err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}

	plainKey, err := model.GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	if err := tokenRepo.Create(ctx, &model.Token{
		Name:           "plaza-token",
		Key:            plainKey,
		OwnerID:        owner.ID,
		Status:         model.TokenStatusEnabled,
		UnlimitedQuota: true,
	}); err != nil {
		t.Fatalf("创建访问令牌失败: %v", err)
	}

	srv := New(Deps{
		Config:      cfg,
		Store:       st,
		Channels:    channels,
		Groups:      groups,
		ModelPrices: priceRepo,
		Settings:    store.NewSettingRepository(st.DB(), st.Dialect()),
		Tokens:      tokenRepo,
		Users:       userRepo,
		Relay:       relay.New(channels, relay.Options{}),
		Payment:     payment.NewRegistry(payment.Options{}),
	})

	return &plazaFixture{
		server:       srv,
		channels:     channels,
		groups:       groups,
		prices:       priceRepo,
		tokenRepo:    tokenRepo,
		userRepo:     userRepo,
		plainToken:   plainKey,
		tokenOwnerID: owner.ID,
	}
}

// doAuthRequest 带访问令牌发起一次请求（用于 /v1/* 接口）。
func doAuthRequest(t *testing.T, srv *Server, method, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec, body
}

// createTestChannel 写入一个启用的渠道（测试数据固定，便于断言）。
func createTestChannel(t *testing.T, channels model.ChannelRepository, models []string) {
	t.Helper()
	if err := channels.Create(context.Background(), &model.Channel{
		Name:     "测试渠道",
		Type:     1,
		BaseURL:  "https://api.example.com",
		APIKey:   "sk-test-key",
		Models:   models,
		Group:    model.DefaultGroupName,
		Priority: 1,
		Weight:   1,
		Status:   model.ChannelStatusEnabled,
	}); err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}
}

// plazaItems 把响应里的 items 转成「模型名 → 卡片」映射，便于断言。
func plazaItems(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应缺少 items 数组：%v", body)
	}
	result := make(map[string]map[string]any, len(raw))
	for _, item := range raw {
		card, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := card["model"].(string); ok {
			result[name] = card
		}
	}
	return result
}

func TestModelPlaza_合并渠道与价格并算出生效价格(t *testing.T) {
	fx := newPlazaFixture(t)
	srv, channels, groups, priceRepo := fx.server, fx.channels, fx.groups, fx.prices
	ctx := context.Background()

	if err := groups.Create(ctx, &model.ModelGroup{
		Name: "vip", DisplayName: "VIP 用户", Ratio: 150, Enabled: true,
	}); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}

	// 默认分组下为 test-model 定价（精确匹配）
	if err := priceRepo.Create(ctx, &model.ModelPrice{
		Model:           "test-model",
		PromptPrice:     1_000_000,
		CompletionPrice: 2_000_000,
		Group:           model.DefaultGroupName,
		Enabled:         true,
	}); err != nil {
		t.Fatalf("创建计价规则失败: %v", err)
	}
	// 只定价、未接渠道的模型：应出现在清单里但标记为不可用
	if err := priceRepo.Create(ctx, &model.ModelPrice{
		Model:        "unwired-model",
		PerCallPrice: 500,
		Group:        model.DefaultGroupName,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("创建计价规则失败: %v", err)
	}
	// 通配规则不应被当成一张"模型卡片"
	if err := priceRepo.Create(ctx, &model.ModelPrice{
		Model:        "*",
		PerCallPrice: 10,
		Group:        model.DefaultGroupName,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("创建通配规则失败: %v", err)
	}

	createTestChannel(t, channels, []string{"test-model"})

	// 再挂一个 vip 分组的渠道（用于验证分组视图与倍率透出）。
	// 注意：广场只展示「有模型的分组」——空分组不出现是有意为之，
	// 否则访客会看到一批点进去什么都没有的分组标签。
	if err := channels.Create(ctx, &model.Channel{
		Name: "VIP 渠道", Type: 1, BaseURL: "https://vip.example.com", APIKey: "sk-vip",
		Models: []string{"vip-model"}, Group: "vip",
		Priority: 1, Weight: 1, Status: model.ChannelStatusEnabled,
	}); err != nil {
		t.Fatalf("创建 vip 渠道失败: %v", err)
	}

	rec, body := doRequest(t, srv, "GET", "/api/models")
	if rec.Code != 200 {
		t.Fatalf("状态码应为 200，实际 %d，响应：%s", rec.Code, rec.Body.String())
	}

	byName := plazaItems(t, body)

	// 通配规则不构成模型卡片
	if _, exists := byName["*"]; exists {
		t.Fatal("通配规则不应作为模型卡片出现在广场中")
	}

	// 有渠道的模型：可用，并带出默认分组的价格与倍率
	ready, exists := byName["test-model"]
	if !exists {
		t.Fatalf("test-model 应出现在广场中，实际：%v", byName)
	}
	if available, _ := ready["available"].(bool); !available {
		t.Fatal("有启用渠道支持的模型应标记为可用")
	}
	modelGroups, _ := ready["groups"].([]any)
	if len(modelGroups) != 1 || modelGroups[0] != model.DefaultGroupName {
		t.Fatalf("模型应归属于 default 分组，实际 %v", modelGroups)
	}
	prices, _ := ready["prices"].([]any)
	if len(prices) != 1 {
		t.Fatalf("应带出 1 条分组价格，实际 %v", prices)
	}
	price, _ := prices[0].(map[string]any)
	if promptPrice, _ := price["prompt_price"].(float64); promptPrice != 1_000_000 {
		t.Fatalf("输入价格应为 1000000，实际 %v", price["prompt_price"])
	}
	// 默认分组倍率为 100（1.0 倍）
	if ratio, _ := price["ratio"].(float64); ratio != 100 {
		t.Fatalf("default 分组倍率应为 100，实际 %v", price["ratio"])
	}

	// 只定价未接渠道的模型：出现在清单里，但标记为不可用
	unwired, exists := byName["unwired-model"]
	if !exists {
		t.Fatalf("已定价但未接渠道的模型也应列出，实际：%v", byName)
	}
	if available, _ := unwired["available"].(bool); available {
		t.Fatal("没有启用渠道支持的模型不应标记为可用")
	}

	// 分组视图：vip 与 default 都应出现，且各自统计到 1 个模型
	groupsView, ok := body["groups"].([]any)
	if !ok {
		t.Fatalf("响应缺少 groups 数组：%s", rec.Body.String())
	}
	counts := make(map[string]float64, len(groupsView))
	ratios := make(map[string]float64, len(groupsView))
	for _, raw := range groupsView {
		view, _ := raw.(map[string]any)
		name, _ := view["name"].(string)
		count, _ := view["model_count"].(float64)
		ratio, _ := view["ratio"].(float64)
		counts[name] = count
		ratios[name] = ratio
	}
	if _, exists := counts["vip"]; !exists {
		t.Fatalf("分组视图应包含 vip（它已有渠道），实际 %v", counts)
	}
	if ratios["vip"] != 150 {
		t.Fatalf("vip 分组倍率应为 150，实际 %v", ratios["vip"])
	}
	if counts[model.DefaultGroupName] != 1 {
		t.Fatalf("default 分组应有 1 个模型，实际 %v", counts[model.DefaultGroupName])
	}
}

func TestModelPlaza_按分组与关键词过滤(t *testing.T) {
	fx := newPlazaFixture(t)
	srv, channels, priceRepo := fx.server, fx.channels, fx.prices
	ctx := context.Background()

	for _, name := range []string{"alpha-model", "beta-model"} {
		if err := priceRepo.Create(ctx, &model.ModelPrice{
			Model: name, PerCallPrice: 100, Group: model.DefaultGroupName, Enabled: true,
		}); err != nil {
			t.Fatalf("创建计价规则失败: %v", err)
		}
	}
	createTestChannel(t, channels, []string{"alpha-model", "beta-model"})

	// 关键词过滤
	_, body := doRequest(t, srv, "GET", "/api/models?keyword=alpha")
	if len(plazaItems(t, body)) != 1 {
		t.Fatalf("关键词过滤后应剩 1 个模型，实际 %v", plazaItems(t, body))
	}

	// 分组过滤：default 分组下两个模型都在
	_, body = doRequest(t, srv, "GET", "/api/models?group=default")
	if len(plazaItems(t, body)) != 2 {
		t.Fatalf("default 分组下应有 2 个模型")
	}

	// 不存在的分组：应为空（而不是把所有模型都返回）
	_, body = doRequest(t, srv, "GET", "/api/models?group=not-exist")
	if len(plazaItems(t, body)) != 0 {
		t.Fatal("不存在的分组不应返回模型")
	}
}

func TestPublicPaymentInfo_默认关闭充值且只启用人工通道(t *testing.T) {
	fx := newPlazaFixture(t)
	srv := fx.server

	rec, body := doRequest(t, srv, "GET", "/api/payment/public")
	if rec.Code != 200 {
		t.Fatalf("状态码应为 200，实际 %d", rec.Code)
	}
	if enabled, _ := body["enabled"].(bool); enabled {
		t.Fatal("默认不应开放充值（需站长显式配置并开启）")
	}
	if rate, _ := body["exchange_rate"].(float64); rate <= 0 {
		t.Fatalf("默认兑换比例应为正数，实际 %v", body["exchange_rate"])
	}

	methods, _ := body["methods"].([]any)
	if len(methods) != 1 {
		t.Fatalf("默认应只启用 1 个通道，实际 %v", methods)
	}
	first, _ := methods[0].(map[string]any)
	if name, _ := first["name"].(string); name != model.PaymentMethodManual {
		t.Fatalf("默认通道应为人工确认，实际 %v", first["name"])
	}
	// 密钥状态字段必须存在且不包含任何密钥内容（只有布尔值）
	if _, ok := first["ready"].(bool); !ok {
		t.Fatal("通道应带 ready 字段（密钥是否就绪）")
	}
}

func TestListModels_OpenAI兼容清单(t *testing.T) {
	fx := newPlazaFixture(t)
	ctx := context.Background()

	// 渠道声明两个模型 + 计价规则里有一个"只定价未接渠道"的模型
	if err := fx.prices.Create(ctx, &model.ModelPrice{
		Model: "priced-only", PerCallPrice: 100, Group: model.DefaultGroupName, Enabled: true,
	}); err != nil {
		t.Fatalf("创建计价规则失败: %v", err)
	}
	if err := fx.prices.Create(ctx, &model.ModelPrice{
		Model: "gpt-4*", PerCallPrice: 100, Group: model.DefaultGroupName, Enabled: true,
	}); err != nil {
		t.Fatalf("创建计价规则失败: %v", err)
	}
	createTestChannel(t, fx.channels, []string{"alpha-model", "beta-model"})

	// 1) 未携带令牌 → 必须 401（不能因为它是只读接口就放开）
	rec, _ := doRequest(t, fx.server, "GET", "/v1/models")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带令牌应返回 401，实际 %d", rec.Code)
	}

	// 2) 带令牌 → 200，且返回 OpenAI 兼容结构
	rec, body := doAuthRequest(t, fx.server, "GET", "/v1/models", fx.plainToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d，响应：%s", rec.Code, rec.Body.String())
	}
	if object, _ := body["object"].(string); object != "list" {
		t.Fatalf("object 应为 list，实际 %v", body["object"])
	}

	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("响应缺少 data 数组：%s", rec.Body.String())
	}
	ids := make([]string, 0, len(data))
	for _, raw := range data {
		item, _ := raw.(map[string]any)
		if id, _ := item["id"].(string); id != "" {
			ids = append(ids, id)
		}
		if obj, _ := item["object"].(string); obj != "model" {
			t.Fatalf("每个条目的 object 应为 model，实际 %v", item["object"])
		}
	}

	// 渠道声明的两个模型必须在列表里，且按名称升序
	if len(ids) != 2 || ids[0] != "alpha-model" || ids[1] != "beta-model" {
		t.Fatalf("应返回渠道声明的 2 个模型且按名称升序，实际 %v", ids)
	}
	// 只定价未接渠道的模型不应出现（渠道有声明时不走兜底）
	for _, id := range ids {
		if id == "priced-only" || id == "gpt-4*" {
			t.Fatalf("不应出现仅存在于计价规则中的模型，实际 %v", ids)
		}
	}
}

func TestListModels_渠道未声明模型时回退到已定价模型(t *testing.T) {
	fx := newPlazaFixture(t)
	ctx := context.Background()

	// 渠道的模型清单为空（过渡约定：支持全部模型），此时无法枚举，只能用已定价模型兜底
	if err := fx.prices.Create(ctx, &model.ModelPrice{
		Model: "priced-model", PerCallPrice: 100, Group: model.DefaultGroupName, Enabled: true,
	}); err != nil {
		t.Fatalf("创建计价规则失败: %v", err)
	}
	if err := fx.prices.Create(ctx, &model.ModelPrice{
		Model: "*", PerCallPrice: 10, Group: model.DefaultGroupName, Enabled: true,
	}); err != nil {
		t.Fatalf("创建通配规则失败: %v", err)
	}
	createTestChannel(t, fx.channels, nil)

	_, body := doAuthRequest(t, fx.server, "GET", "/v1/models", fx.plainToken)
	data, _ := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("应回退到 1 个已定价模型，实际 %d", len(data))
	}
	item, _ := data[0].(map[string]any)
	if id, _ := item["id"].(string); id != "priced-model" {
		t.Fatalf("兜底结果应为 priced-model，实际 %v", item["id"])
	}
}
