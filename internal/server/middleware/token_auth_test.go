// 令牌鉴权中间件的单元测试。
//
// 意图（Why）：
//
//	鉴权是网关的安全边界。这里逐条锁定行为：缺令牌/无效/禁用/过期/额度耗尽
//	分别返回什么状态码与错误码，以及一个容易被忽视但极其关键的契约——
//	鉴权读取请求体后必须还原，否则转发阶段会拿到空 body。
//
// 流转（Flow）：
//
//	go test ./internal/server/middleware/
//	  └─ 用真实仓储（临时 SQLite + 加密）构造令牌，经 httptest 走完整中间件链路
//
// 扩展（Extend）：
//
//	新增校验维度（如 IP 白名单）后，按同样风格补充"通过 / 拒绝"两类用例。
package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// testEncryptionKey 是测试用密钥材料（非真实密钥）。
const testEncryptionKey = "middleware-test-key-0123456789abcdef0123456789abcdef"

// newTestTokenRepo 构造一个基于临时数据库的令牌仓储。
func newTestTokenRepo(t *testing.T) model.TokenRepository {
	t.Helper()

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "middleware_test.db"))
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
	return store.NewTokenRepository(st.DB(), cipher)
}

// createToken 按给定配置创建令牌并返回明文。
//
// 说明：令牌明文需显式传入（而非由仓储生成），以便测试用固定值构造请求头。
func createToken(t *testing.T, repo model.TokenRepository, mutate func(*model.Token)) string {
	t.Helper()

	key, err := model.GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}

	token := &model.Token{
		Name:           "测试令牌",
		Key:            key,
		Status:         model.TokenStatusEnabled,
		UnlimitedQuota: true, // 默认不限额度，避免干扰状态类用例
	}
	if mutate != nil {
		mutate(token)
	}
	if err := repo.Create(context.Background(), token); err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}
	return key
}

// newTokenAndUserRepos 在同一临时库上同时构造令牌与用户仓储。
//
// 必须共用一个数据库：账号级额度校验会按 token.OwnerID 去查用户，
// 用两个独立库会让"令牌存在但用户查不到"这类集成问题测不出来。
func newTokenAndUserRepos(t *testing.T) (model.TokenRepository, model.UserRepository) {
	t.Helper()

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "auth_pair_test.db"))
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
	return store.NewTokenRepository(st.DB(), cipher), store.NewUserRepository(st.DB())
}

// TestTokenAuth_账号额度耗尽_应被拒绝 验证账号级限额确实生效。
//
// 这条用例守住的是"只校验令牌额度会被多建令牌绕过"这个漏洞：
// 即使令牌本身不限额度，只要账号额度耗尽，也必须拒绝。
func TestTokenAuth_账号额度耗尽_应被拒绝(t *testing.T) {
	tokens, users := newTokenAndUserRepos(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		quota    int64
		used     int64
		wantCode int
	}{
		{name: "额度已用尽", quota: 100, used: 100, wantCode: http.StatusTooManyRequests},
		{name: "额度为零", quota: 0, used: 0, wantCode: http.StatusTooManyRequests},
		// 已用超过总额度时剩余为负。这是真实存在过的漏洞：
		// 旧实现只判「剩余 == 0」，负剩余会被判为"还有额度"而放行。
		{name: "额度已超额", quota: 100, used: 150, wantCode: http.StatusTooManyRequests},
		{name: "额度充足", quota: 100, used: 50, wantCode: http.StatusOK},
		{name: "不限额度", quota: model.QuotaUnlimited, used: 999999, wantCode: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := &model.User{
				Username:     "owner-" + strings.ReplaceAll(tc.name, " ", ""),
				PasswordHash: "test-hash-placeholder",
				Role:         model.UserRoleUser,
				Status:       model.UserStatusEnabled,
				Quota:        tc.quota,
				UsedQuota:    tc.used,
			}
			if err := users.Create(ctx, owner); err != nil {
				t.Fatalf("创建用户失败: %v", err)
			}

			key, err := model.GenerateTokenKey()
			if err != nil {
				t.Fatalf("生成令牌失败: %v", err)
			}
			// 令牌自身不限额度：这样失败原因只可能来自账号级校验
			token := &model.Token{
				OwnerID:        owner.ID,
				Name:           "account-quota-probe",
				Key:            key,
				Status:         model.TokenStatusEnabled,
				UnlimitedQuota: true,
			}
			if err := tokens.Create(ctx, token); err != nil {
				t.Fatalf("创建令牌失败: %v", err)
			}

			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.Use(TokenAuth(tokens, users))
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"test-model"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("期望 HTTP %d，实际 %d（响应体：%s）", tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

// newAuthEngine 构造一个挂载了鉴权中间件的测试引擎。
//
// 探针处理器回显请求体，便于验证"鉴权读取后请求体仍可读"。
//
// 说明：这里传 nil 作为用户仓储，表示本组用例只关注令牌层校验；
// 账号级额度校验由 TestTokenAuth_RejectsExhaustedUser 单独覆盖。
func newAuthEngine(t *testing.T, repo model.TokenRepository) *gin.Engine {
	t.Helper()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(TokenAuth(repo, nil))
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusInternalServerError, "读取请求体失败")
			return
		}

		// 同时回报上下文中的令牌，验证中间件确实注入了身份信息
		injected := false
		if _, ok := TokenFromContext(c); ok {
			injected = true
		}
		c.JSON(http.StatusOK, gin.H{
			"body":          string(body),
			"tokenInjected": injected,
		})
	})
	return engine
}

// doAuthRequest 发起一次带指定请求头的请求。
func doAuthRequest(t *testing.T, engine *gin.Engine, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// decodeError 解析 OpenAI 风格错误体。
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) oai.ErrorBody {
	t.Helper()

	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v（原文 %s）", err, rec.Body.String())
	}
	return body
}

// TestTokenAuth_MissingToken 验证未携带令牌时返回 401。
func TestTokenAuth_MissingToken(t *testing.T) {
	engine := newAuthEngine(t, newTestTokenRepo(t))

	rec := doAuthRequest(t, engine, nil, `{"model":"gpt-4o"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", rec.Code)
	}
	if body := decodeError(t, rec); body.Error.Code != oai.CodeMissingAPIKey {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, oai.CodeMissingAPIKey)
	}
}

// TestTokenAuth_InvalidToken 验证不存在的令牌返回 401。
func TestTokenAuth_InvalidToken(t *testing.T) {
	engine := newAuthEngine(t, newTestTokenRepo(t))

	rec := doAuthRequest(t, engine,
		map[string]string{"Authorization": "Bearer sk-000000000000000000000000000000000000000000000000"},
		`{"model":"gpt-4o"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", rec.Code)
	}
	if body := decodeError(t, rec); body.Error.Code != oai.CodeInvalidAPIKey {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, oai.CodeInvalidAPIKey)
	}
}

// TestTokenAuth_ValidToken_Passes 验证有效令牌被放行且身份注入上下文。
func TestTokenAuth_ValidToken_Passes(t *testing.T) {
	repo := newTestTokenRepo(t)
	key := createToken(t, repo, nil)
	engine := newAuthEngine(t, repo)

	rec := doAuthRequest(t, engine,
		map[string]string{"Authorization": "Bearer " + key},
		`{"model":"gpt-4o","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体：%s）", rec.Code, rec.Body.String())
	}

	var resp struct {
		TokenInjected bool `json:"tokenInjected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体解析失败: %v", err)
	}
	if !resp.TokenInjected {
		t.Error("中间件未把令牌注入上下文，后续处理器将无法获取调用者身份")
	}
}

// TestTokenAuth_BodyStillReadableAfterAuth 验证鉴权读取请求体后仍可被后续处理器读取。
//
// 这是最容易出错、也最难排查的一点：若中间件读完后不还原 body，
// 转发阶段会拿到空内容，表现为"上游提示缺少 messages"。
func TestTokenAuth_BodyStillReadableAfterAuth(t *testing.T) {
	repo := newTestTokenRepo(t)
	// 配置白名单，强制中间件读取请求体
	key := createToken(t, repo, func(tk *model.Token) {
		tk.Models = []string{"gpt-4o"}
		tk.UnlimitedQuota = true
	})
	engine := newAuthEngine(t, repo)

	const payload = `{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`
	rec := doAuthRequest(t, engine, map[string]string{"Authorization": "Bearer " + key}, payload)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}

	var resp struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体解析失败: %v", err)
	}
	if resp.Body != payload {
		t.Errorf("后续处理器读到的请求体 = %q，期望与原始一致（鉴权未还原 body？）", resp.Body)
	}
}

// TestTokenAuth_XAPIKeyHeader 验证兼容 x-api-key 请求头（Anthropic SDK 的默认方式）。
func TestTokenAuth_XAPIKeyHeader(t *testing.T) {
	repo := newTestTokenRepo(t)
	key := createToken(t, repo, nil)
	engine := newAuthEngine(t, repo)

	rec := doAuthRequest(t, engine, map[string]string{"x-api-key": key}, `{"model":"gpt-4o"}`)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200（x-api-key 应被支持）", rec.Code)
	}
}

// TestTokenAuth_StatusChecks 表驱动覆盖各类令牌状态。
func TestTokenAuth_StatusChecks(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(*model.Token)
		wantStatus    int
		wantErrorCode string
	}{
		{
			name:          "手动禁用",
			mutate:        func(tk *model.Token) { tk.Status = model.TokenStatusDisabled },
			wantStatus:    http.StatusForbidden,
			wantErrorCode: oai.CodeTokenDisabled,
		},
		{
			name:          "已过期",
			mutate:        func(tk *model.Token) { tk.ExpiresAt = time.Now().Add(-time.Hour) },
			wantStatus:    http.StatusUnauthorized,
			wantErrorCode: oai.CodeTokenExpired,
		},
		{
			name: "额度耗尽",
			mutate: func(tk *model.Token) {
				tk.UnlimitedQuota = false
				tk.RemainQuota = 0
			},
			wantStatus:    http.StatusTooManyRequests,
			wantErrorCode: oai.CodeInsufficientQuota,
		},
		{
			name: "仍有剩余额度",
			mutate: func(tk *model.Token) {
				tk.UnlimitedQuota = false
				tk.RemainQuota = 100
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "未过期且不限额度",
			mutate:     func(tk *model.Token) { tk.ExpiresAt = time.Now().Add(time.Hour) },
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestTokenRepo(t)
			key := createToken(t, repo, tc.mutate)
			engine := newAuthEngine(t, repo)

			rec := doAuthRequest(t, engine,
				map[string]string{"Authorization": "Bearer " + key},
				`{"model":"gpt-4o"}`)

			if rec.Code != tc.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d（响应体：%s）", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantErrorCode != "" {
				if body := decodeError(t, rec); body.Error.Code != tc.wantErrorCode {
					t.Errorf("错误码 = %q，期望 %q", body.Error.Code, tc.wantErrorCode)
				}
			}
		})
	}
}

// TestTokenAuth_ModelWhitelist 验证模型白名单的允许与拒绝。
func TestTokenAuth_ModelWhitelist(t *testing.T) {
	repo := newTestTokenRepo(t)
	key := createToken(t, repo, func(tk *model.Token) {
		tk.Models = []string{"gpt-4o", "claude-3"}
	})
	engine := newAuthEngine(t, repo)

	t.Run("白名单内模型放行", func(t *testing.T) {
		rec := doAuthRequest(t, engine,
			map[string]string{"Authorization": "Bearer " + key},
			`{"model":"gpt-4o"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("状态码 = %d，期望 200", rec.Code)
		}
	})

	t.Run("白名单外模型拒绝", func(t *testing.T) {
		rec := doAuthRequest(t, engine,
			map[string]string{"Authorization": "Bearer " + key},
			`{"model":"gpt-3.5-turbo"}`)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("状态码 = %d，期望 403", rec.Code)
		}
		if body := decodeError(t, rec); body.Error.Code != oai.CodeModelNotAllowed {
			t.Errorf("错误码 = %q，期望 %q", body.Error.Code, oai.CodeModelNotAllowed)
		}
	})
}

// TestTokenAuth_NoWhitelist_SkipsBodyParse 验证未配置白名单时跳过请求体解析。
//
// 设计意图（性能优化）：不限制模型的令牌是最常见配置，
// 此时无需读取与解析请求体，可省掉一次内存拷贝与 JSON 解析。
// 副作用是非法 JSON 会在转发阶段才被拒绝，这正是本用例要锁定的行为。
func TestTokenAuth_NoWhitelist_SkipsBodyParse(t *testing.T) {
	repo := newTestTokenRepo(t)
	key := createToken(t, repo, nil) // 未配置白名单
	engine := newAuthEngine(t, repo)

	rec := doAuthRequest(t, engine,
		map[string]string{"Authorization": "Bearer " + key},
		`{ 这不是合法 JSON `)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200（无白名单时不应解析请求体）", rec.Code)
	}
}

// TestTokenAuth_MissingModelWithWhitelist 验证配置白名单时缺少 model 字段会返回 400。
func TestTokenAuth_MissingModelWithWhitelist(t *testing.T) {
	repo := newTestTokenRepo(t)
	key := createToken(t, repo, func(tk *model.Token) { tk.Models = []string{"gpt-4o"} })
	engine := newAuthEngine(t, repo)

	rec := doAuthRequest(t, engine,
		map[string]string{"Authorization": "Bearer " + key},
		`{"messages":[]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	if body := decodeError(t, rec); body.Error.Code != oai.CodeMissingModel {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, oai.CodeMissingModel)
	}
}

// TestExtractAPIKey 验证令牌提取规则的边界行为。
func TestExtractAPIKey(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "标准 Bearer",
			headers: map[string]string{"Authorization": "Bearer sk-abc"},
			want:    "sk-abc",
		},
		{
			name:    "Bearer 后带多余空格",
			headers: map[string]string{"Authorization": "Bearer   sk-abc  "},
			want:    "sk-abc",
		},
		{
			name:    "x-api-key 形式",
			headers: map[string]string{"x-api-key": "sk-xyz"},
			want:    "sk-xyz",
		},
		{
			name:    "Authorization 非 Bearer 时回退到 x-api-key",
			headers: map[string]string{"Authorization": "Basic abc", "x-api-key": "sk-fallback"},
			want:    "sk-fallback",
		},
		{
			name:    "两者都无",
			headers: nil,
			want:    "",
		},
		{
			name:    "仅 Basic 认证",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			if got := extractAPIKey(req); got != tc.want {
				t.Errorf("extractAPIKey() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestTokenFromContext_WithoutAuth 验证无中间件时不会 panic 且返回 false。
//
// 意义：访问函数会被多种处理器调用，必须对"未经过鉴权"的情况安全降级。
func TestTokenFromContext_WithoutAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	if token, ok := TokenFromContext(c); ok || token != nil {
		t.Error("未注入令牌时应返回 (nil, false)")
	}
}

// ---------------------------------------------------------------------------
// 额度预扣接入
// ---------------------------------------------------------------------------

// fakeQuotaReserver 是内存版额度预留器，用于验证鉴权层的预留决策。
type fakeQuotaReserver struct {
	estimateAmount int64 // EstimateReserve 返回的预留额
	priced         bool  // 该模型是否命中计价规则
	reserveErr     error // Reserve 返回的错误
	pending        int64 // 在途预留合计

	reserveCalls  int
	lastRequestID string
}

func (f *fakeQuotaReserver) EstimateReserve(context.Context, string, int) (int64, bool) {
	return f.estimateAmount, f.priced
}

func (f *fakeQuotaReserver) Reserve(_ context.Context, req model.ReserveRequest) (*model.QuotaReservation, error) {
	f.reserveCalls++
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	f.lastRequestID = req.RequestID
	return &model.QuotaReservation{
		RequestID: req.RequestID,
		UserID:    req.UserID,
		TokenID:   req.TokenID,
		Reserved:  req.Amount,
	}, nil
}

func (f *fakeQuotaReserver) PendingReserved(context.Context, uint64) (int64, error) {
	return f.pending, nil
}

// newLimitedOwnerAndToken 创建一个有限额度用户与其名下的令牌，返回令牌明文。
func newLimitedOwnerAndToken(t *testing.T, tokens model.TokenRepository, users model.UserRepository,
	username string, quota, remainQuota int64) string {
	t.Helper()
	ctx := context.Background()

	owner := &model.User{
		Username:     username,
		PasswordHash: "test-hash-placeholder",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusEnabled,
		Quota:        quota,
	}
	if err := users.Create(ctx, owner); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	key, err := model.GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	token := &model.Token{
		OwnerID:     owner.ID,
		Name:        "reserve-probe",
		Key:         key,
		Status:      model.TokenStatusEnabled,
		RemainQuota: remainQuota,
	}
	if err := tokens.Create(ctx, token); err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}
	return key
}

// newQuotaAuthEngine 构造挂载了「令牌鉴权 + 额度预留器」的测试引擎。
func newQuotaAuthEngine(t *testing.T, tokens model.TokenRepository, users model.UserRepository,
	reserver QuotaReserver, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(TokenAuth(tokens, users, reserver))
	engine.POST("/v1/chat/completions", handler)
	return engine
}

// okHandler 是返回 200 的探针处理器。
func okHandler(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }

// TestTokenAuth_不计费模型跳过预留 验证未命中价格规则的模型不写预留、不扣额度。
//
// 这是"免费模型被额度墙挡住"这一线上事故的回归用例。
func TestTokenAuth_不计费模型跳过预留(t *testing.T) {
	tokens, users := newTokenAndUserRepos(t)
	// 账号额度紧张（100，低于信任阈值），确保"是否会预留"只取决于模型是否计费。
	key := newLimitedOwnerAndToken(t, tokens, users, "free-model-owner", 100, 1000)

	reserver := &fakeQuotaReserver{priced: false}
	engine := newQuotaAuthEngine(t, tokens, users, reserver, okHandler)

	rec := doAuthRequest(t, engine, map[string]string{"Authorization": "Bearer " + key},
		`{"model":"free-model","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("不计费模型应放行（200），实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	if reserver.reserveCalls != 0 {
		t.Fatalf("不计费模型不应写预留，实际调用 %d 次", reserver.reserveCalls)
	}
}

// TestTokenAuth_额度不足_返回429并带具体数字 验证预留失败时的状态码与文案。
func TestTokenAuth_额度不足_返回429并带具体数字(t *testing.T) {
	tokens, users := newTokenAndUserRepos(t)
	key := newLimitedOwnerAndToken(t, tokens, users, "insufficient-owner", 100, 1000)

	reserver := &fakeQuotaReserver{priced: true, estimateAmount: 500, reserveErr: model.ErrQuotaInsufficient}
	engine := newQuotaAuthEngine(t, tokens, users, reserver, okHandler)

	rec := doAuthRequest(t, engine, map[string]string{"Authorization": "Bearer " + key},
		`{"model":"priced-model","messages":[]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("额度不足应返回 429，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != oai.CodeInsufficientQuota {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, oai.CodeInsufficientQuota)
	}
	// 文案必须带具体数字，便于使用者自助定位
	if !strings.Contains(body.Error.Message, "100") || !strings.Contains(body.Error.Message, "500") {
		t.Errorf("错误文案应包含剩余/需要额度数字，实际 %q", body.Error.Message)
	}
}

// TestTokenAuth_预留成功_注入幂等键 验证预留成功后 requestID 被写入请求上下文。
func TestTokenAuth_预留成功_注入幂等键(t *testing.T) {
	tokens, users := newTokenAndUserRepos(t)
	key := newLimitedOwnerAndToken(t, tokens, users, "reserve-ok-owner", 100, 1000)

	reserver := &fakeQuotaReserver{priced: true, estimateAmount: 10}
	engine := newQuotaAuthEngine(t, tokens, users, reserver, func(c *gin.Context) {
		identity, _ := reqctx.IdentityFrom(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"requestId": identity.RequestID})
	})

	rec := doAuthRequest(t, engine, map[string]string{"Authorization": "Bearer " + key},
		`{"model":"priced-model","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体：%s）", rec.Code, rec.Body.String())
	}
	if reserver.reserveCalls != 1 {
		t.Fatalf("应预留一次，实际 %d 次", reserver.reserveCalls)
	}

	var resp struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体解析失败: %v", err)
	}
	if resp.RequestID == "" {
		t.Fatal("预留成功后应把 requestID 写入上下文（转发结束据此结算/退还）")
	}
	if resp.RequestID != reserver.lastRequestID {
		t.Errorf("上下文 requestID = %q，与预留时使用的不一致（%q）", resp.RequestID, reserver.lastRequestID)
	}
}

// TestTokenAuth_信任额度旁路_跳过预留 验证额度充足时跳过预留以减少写库。
func TestTokenAuth_信任额度旁路_跳过预留(t *testing.T) {
	tokens, users := newTokenAndUserRepos(t)
	// 额度高于信任阈值：应走旁路，不写预留。
	key := newLimitedOwnerAndToken(t, tokens, users, "trusted-owner", trustQuotaBypassThreshold+1, 1000)

	reserver := &fakeQuotaReserver{priced: true, estimateAmount: 500}
	engine := newQuotaAuthEngine(t, tokens, users, reserver, okHandler)

	rec := doAuthRequest(t, engine, map[string]string{"Authorization": "Bearer " + key},
		`{"model":"priced-model","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if reserver.reserveCalls != 0 {
		t.Fatalf("信任额度旁路不应写预留，实际 %d 次", reserver.reserveCalls)
	}
}
