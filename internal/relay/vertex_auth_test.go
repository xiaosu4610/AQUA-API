// Google Vertex 服务账号鉴权（JWT → access_token）的单元测试。
//
// 意图（Why）：
//
//	Vertex 的鉴权链路有两段容易出错：① 本地签的 RS256 JWT 必须结构正确且可被公钥验证；
//	② 换取令牌必须真正带上断言、把结果缓存起来、未配置凭据时明确报错。
//	这些都无需真实网络：JWT 用现场生成的 RSA 密钥自验，换令牌用 httptest 假端点。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ buildVertexJWT：解码头/载荷 + 用公钥验签
//	  ├─ buildUpstreamRequest：httptest 假 token 端点 → 断言 Bearer 头与缓存命中
//	  └─ prepareChannelUpstream：确认 Vertex 走 Gemini 请求体转换 + 服务账号鉴权
//
// 扩展（Extend）：
//
//	若支持非服务账号的 OAuth 流程，新增对应用例，不要改本文件的断言。
package relay

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// newTestServiceAccount 现场生成一对 RSA 密钥并拼出服务账号 JSON。
//
// 传 tokenURI 可指向 httptest 假端点；返回的私钥供测试用公钥验证 JWT 签名。
func newTestServiceAccount(t *testing.T, tokenURI string) (string, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 私钥失败: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	payload, err := json.Marshal(map[string]string{
		"client_email": "aqua-test@project.iam.gserviceaccount.com",
		"private_key":  string(pemText),
		"token_uri":    tokenURI,
	})
	if err != nil {
		t.Fatalf("编码服务账号 JSON 失败: %v", err)
	}
	return string(payload), key
}

// TestBuildVertexJWT_结构与签名可被公钥验证 验证 JWT 三段结构与 RS256 签名。
func TestBuildVertexJWT_结构与签名可被公钥验证(t *testing.T) {
	saJSON, key := newTestServiceAccount(t, "https://oauth2.example.com/token")
	account, err := parseVertexServiceAccount(saJSON)
	if err != nil {
		t.Fatalf("解析服务账号失败: %v", err)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	assertion, err := buildVertexJWT(account, at)
	if err != nil {
		t.Fatalf("签发 JWT 失败: %v", err)
	}

	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 应有 3 段，实际 %d 段: %q", len(parts), assertion)
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("解码 JWT 头部失败: %v", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		t.Fatalf("解析 JWT 头部失败: %v", err)
	}
	if header.Alg != "RS256" || header.Typ != "JWT" {
		t.Errorf("JWT 头部 = %+v，期望 alg=RS256 typ=JWT", header)
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解码 JWT 载荷失败: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		t.Fatalf("解析 JWT 载荷失败: %v", err)
	}
	if claims["iss"] != account.ClientEmail {
		t.Errorf("iss = %v，期望 %s", claims["iss"], account.ClientEmail)
	}
	if claims["aud"] != "https://oauth2.example.com/token" {
		t.Errorf("aud = %v，期望 token_uri", claims["aud"])
	}
	if claims["scope"] != vertexOAuthScope {
		t.Errorf("scope = %v，期望 %s", claims["scope"], vertexOAuthScope)
	}
	if int64(claims["iat"].(float64)) != at.Unix() {
		t.Errorf("iat = %v，期望 %d", claims["iat"], at.Unix())
	}
	if int64(claims["exp"].(float64)) != at.Add(time.Hour).Unix() {
		t.Errorf("exp = %v，期望 %d", claims["exp"], at.Add(time.Hour).Unix())
	}

	// 用对应公钥验证签名：能验过说明确实是这段私钥对这段内容做的 RS256 签名。
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("解码 JWT 签名失败: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Errorf("JWT 签名无法用对应公钥验证: %v", err)
	}
}

// TestVertex_换取令牌并缓存 用 httptest 假端点验证换令牌与缓存行为。
func TestVertex_换取令牌并缓存(t *testing.T) {
	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)

		if err := r.ParseForm(); err != nil {
			t.Errorf("解析换令牌表单失败: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != vertexJWTAssertionGrant {
			t.Errorf("grant_type = %q，期望 %q", got, vertexJWTAssertionGrant)
		}
		if strings.Count(r.Form.Get("assertion"), ".") != 2 {
			t.Errorf("assertion 不是三段式 JWT: %q", r.Form.Get("assertion"))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.test-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer server.Close()

	saJSON, _ := newTestServiceAccount(t, server.URL)
	spec := mustType(t, "vertex_ai")
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

	input := upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://us-central1-aiplatform.googleapis.com",
		APIKey:  saJSON,
		Model:   "gemini-1.5-pro",
		Path:    "/v1/chat/completions",
		Extra:   map[string]string{"project_id": "my-proj", "region": "us-central1"},
		Body:    []byte(`{}`),
		now:     at,
	}

	built, err := buildUpstreamRequest(input)
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	if got := built.Header.Get("Authorization"); got != "Bearer ya29.test-token" {
		t.Errorf("Authorization = %q，期望 Bearer ya29.test-token", got)
	}
	wantPath := "/v1/projects/my-proj/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent"
	if !strings.HasSuffix(built.URL, wantPath) {
		t.Errorf("URL = %q，期望以 %q 结尾", built.URL, wantPath)
	}
	if !built.authInjected {
		t.Error("Vertex 请求应标记为已注入凭据")
	}

	// 第二次组装：令牌应命中缓存，不再请求假端点。
	if _, err := buildUpstreamRequest(input); err != nil {
		t.Fatalf("第二次组装请求失败: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("换令牌端点被请求 %d 次，期望 1 次（第二次应命中缓存）", got)
	}
}

// TestVertex_流式带alt查询 验证流式请求的路径与 alt=sse 查询参数。
func TestVertex_流式带alt查询(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	defer server.Close()

	saJSON, _ := newTestServiceAccount(t, server.URL)
	spec := mustType(t, "vertex_ai")

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://us-central1-aiplatform.googleapis.com",
		APIKey:  saJSON,
		Model:   "gemini-1.5-pro",
		Path:    "/v1/chat/completions",
		Extra:   map[string]string{"project_id": "p", "region": "us-central1", "publisher": "acme"},
		Stream:  true,
		Body:    []byte(`{}`),
		now:     time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	if !strings.Contains(built.URL, ":streamGenerateContent") {
		t.Errorf("流式 URL = %q，期望含 :streamGenerateContent", built.URL)
	}
	if !strings.Contains(built.URL, "/publishers/acme/") {
		t.Errorf("URL = %q，期望使用自定义 publisher=acme", built.URL)
	}
	if !strings.HasSuffix(built.URL, "?alt=sse") {
		t.Errorf("流式 URL = %q，期望带 alt=sse 查询参数", built.URL)
	}
}

// TestVertex_凭据缺失给出明确中文错误 验证未配置服务账号时不静默放行。
func TestVertex_凭据缺失给出明确中文错误(t *testing.T) {
	spec := mustType(t, "vertex_ai")

	cases := []struct {
		name    string
		apiKey  string
		wantMsg string
	}{
		{name: "空密钥", apiKey: "", wantMsg: "服务账号 JSON"},
		{name: "非合法JSON", apiKey: "not-json", wantMsg: "服务账号 JSON"},
		{name: "缺client_email", apiKey: `{"private_key":"x"}`, wantMsg: "client_email"},
		{name: "缺private_key", apiKey: `{"client_email":"a@b.c"}`, wantMsg: "private_key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildUpstreamRequest(upstreamRequestInput{
				Type:    spec,
				BaseURL: "https://us-central1-aiplatform.googleapis.com",
				APIKey:  tc.apiKey,
				Model:   "gemini-1.5-pro",
				Path:    "/v1/chat/completions",
				Extra:   map[string]string{"project_id": "p", "region": "us-central1"},
				Body:    []byte(`{}`),
			})
			if err == nil {
				t.Fatal("应报错，实际未报错")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestPrepareChannelUpstream_Vertex 验证 Vertex 渠道的端到端"接线"：
// 走 Gemini 的请求体转换、路径含项目/区域/发布方、鉴权用服务账号令牌。
func TestPrepareChannelUpstream_Vertex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok-vertex","expires_in":3600}`))
	}))
	defer server.Close()

	saJSON, _ := newTestServiceAccount(t, server.URL)
	ch := &model.Channel{
		TypeKey: "vertex_ai",
		BaseURL: "https://us-central1-aiplatform.googleapis.com",
		ExtraConfig: map[string]string{
			"project_id": "my-proj",
			"region":     "us-central1",
		},
	}
	body := []byte(`{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"hi"}]}`)

	spec, outBody, built, err := prepareChannelUpstream(
		ch, saJSON, "gemini-1.5-pro", oai.ChatCompletionsPath, body, http.Header{}, false)
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	if spec.Protocol != channeltype.ProtocolVertex {
		t.Fatalf("spec.Protocol = %q，期望 vertex", spec.Protocol)
	}
	if !strings.Contains(built.URL, "/v1/projects/my-proj/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent") {
		t.Errorf("URL = %q，期望含 Vertex 生成路径", built.URL)
	}
	if got := built.Header.Get("Authorization"); got != "Bearer tok-vertex" {
		t.Errorf("Authorization = %q，期望 Bearer tok-vertex", got)
	}
	// 请求体应被转换为 Gemini 的 contents 形态（Vertex 复用 Gemini 转换）。
	var converted map[string]any
	if err := json.Unmarshal(outBody, &converted); err != nil {
		t.Fatalf("转换后的请求体不是合法 JSON: %v", err)
	}
	if _, ok := converted["contents"]; !ok {
		t.Errorf("转换后的请求体缺少 contents: %v", converted)
	}
	if _, ok := converted["model"]; ok {
		t.Errorf("Gemini/Vertex 请求体不应带 model 字段: %v", converted)
	}
}
