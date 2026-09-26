// 本文件实现 Google Vertex AI 的服务账号鉴权：本地签 RS256 JWT → 换取 access_token。
//
// 意图（Why）：
//
//	Vertex AI 不使用静态 API Key，而是要求用「服务账号」（一段 JSON，含 client_email 与
//	私钥）自证身份：先本地签一个短期 JWT，再拿它去 OAuth2 端点换 access_token，最后以
//	Authorization: Bearer <token> 调用。若每个请求都去换一次令牌，不但多一次往返，
//	还会迅速触发端点限流——因此必须缓存，并在过期前提前刷新。
//
//	实现要点：
//	  1. 签名算法固定 RS256（服务账号私钥是 RSA），JWT 头/载荷/签名均用 base64url 无填充；
//	  2. aud 指向将要换取令牌的端点，iss 是服务账号邮箱，scope 取 cloud-platform；
//	  3. 令牌缓存以「凭据指纹」为键：换了一把服务账号就是另一条缓存，互不串味；
//	  4. 提前 5 分钟刷新，避免"刚好过期的那次请求必然失败一次"（与 oauth.go 同一取舍）。
//
//	凭据表达方式（约定）：
//	  渠道密钥字段放**服务账号 JSON 原文**（client_email / private_key / token_uri）；
//	  项目、区域等非敏感参数放渠道 extra_config（project_id / region / publisher）。
//	  理由与 Bedrock 一致：私钥是敏感信息，集中在加密的密钥字段；其余是普通配置。
//
// 流转（Flow）：
//
//	buildUpstreamRequest（鉴权方式 = service_account 时）
//	  └─ signUpstreamRequest → applyVertexServiceAccountAuth
//	       └─ vertexAccessToken
//	            ├─ 命中缓存 → 直接返回
//	            └─ 未命中/将过期 → buildVertexJWT → POST token_uri → 写入缓存
//
// 扩展（Extend）：
//
//	换用其它 Google 端点（如地域化 token 端点）时，只需在服务账号 JSON 里改 token_uri，
//	无需改代码；若要支持非服务账号的 OAuth 流程（如用户授权码），另建文件，勿塞进这里。
package relay

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Vertex 服务账号鉴权相关常量。
const (
	// vertexDefaultTokenURI 是 Google 的服务账号换令牌端点（服务账号 JSON 未给 token_uri 时的兜底）。
	vertexDefaultTokenURI = "https://oauth2.googleapis.com/token"
	// vertexOAuthScope 是访问 Vertex AI 所需的 OAuth2 作用域。
	vertexOAuthScope = "https://www.googleapis.com/auth/cloud-platform"
	// vertexJWTAssertionGrant 是"用 JWT 断言换令牌"的授权类型（RFC 7523）。
	vertexJWTAssertionGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// vertexTokenTTL 是令牌有效期兜底值（端点未返回 expires_in 时使用）。
	vertexTokenTTL = time.Hour
	// vertexTokenRefreshAhead 是提前刷新余量：过期前这么久就换新令牌。
	vertexTokenRefreshAhead = 5 * time.Minute
	// vertexTokenHTTPTimeout 是换取令牌请求的超时时间。
	vertexTokenHTTPTimeout = 15 * time.Second
)

// vertexTokenHTTPClient 是换取令牌使用的 HTTP 客户端。
//
// 单独定义（而非复用转发客户端）的原因：这是网关自身的控制面流量，
// 超时与连接池策略都应与转发流量解耦；测试也可替换它注入 httptest 端点。
var vertexTokenHTTPClient = &http.Client{Timeout: vertexTokenHTTPTimeout}

// vertexServiceAccount 是服务账号 JSON 中我们用到的字段。
type vertexServiceAccount struct {
	// ClientEmail 是服务账号邮箱，作为 JWT 的 iss。
	ClientEmail string `json:"client_email"`
	// PrivateKey 是 PEM 编码的 RSA 私钥，用于给 JWT 签名（敏感）。
	PrivateKey string `json:"private_key"`
	// TokenURI 是换令牌端点；缺省用 vertexDefaultTokenURI。
	TokenURI string `json:"token_uri"`
}

// parseVertexServiceAccount 解析渠道密钥字段里的服务账号 JSON。
//
// 未配置或关键字段缺失时返回明确的中文错误，绝不静默不带鉴权头。
func parseVertexServiceAccount(apiKey string) (vertexServiceAccount, error) {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return vertexServiceAccount{}, fmt.Errorf("Vertex 渠道需要配置服务账号 JSON（client_email / private_key）")
	}

	var account vertexServiceAccount
	if err := json.Unmarshal([]byte(trimmed), &account); err != nil {
		return vertexServiceAccount{}, fmt.Errorf("Vertex 渠道密钥必须是服务账号 JSON: %w", err)
	}
	if strings.TrimSpace(account.ClientEmail) == "" {
		return vertexServiceAccount{}, fmt.Errorf("Vertex 渠道的服务账号 JSON 缺少 client_email（服务账号邮箱）")
	}
	if strings.TrimSpace(account.PrivateKey) == "" {
		return vertexServiceAccount{}, fmt.Errorf("Vertex 渠道的服务账号 JSON 缺少 private_key（RSA 私钥）")
	}
	if strings.TrimSpace(account.TokenURI) == "" {
		account.TokenURI = vertexDefaultTokenURI
	}
	return account, nil
}

// vertexTokenResponse 是换令牌端点的响应。
type vertexTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// vertexCachedToken 是缓存里的一条令牌。
type vertexCachedToken struct {
	token     string
	expiresAt time.Time
}

// vertexTokenStore 是按凭据指纹缓存 access_token 的进程内存储。
//
// 选择进程内缓存而非共享存储：令牌是短周期、可重算的派生数据，多实例各存一份
// 只是多换几次令牌，不值得为此引入外部依赖（与 key_select.go 的粘性表同一取舍）。
// 换令牌时持锁串行：这是低频事件（每凭据每小时一次），串行实现简单，
// 还能天然避免并发请求同时去换令牌（惊群）。
type vertexTokenStore struct {
	mu      sync.Mutex
	entries map[string]vertexCachedToken
}

// vertexTokenCache 是全局令牌缓存。
var vertexTokenCache = &vertexTokenStore{entries: make(map[string]vertexCachedToken)}

// get 返回未过期且留有刷新余量的令牌。
func (s *vertexTokenStore) get(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(key, now)
}

// getLocked 在已持锁的前提下查询令牌。
//
// 单独拆分的原因：sync.Mutex 不可重入，refresh 路径已持锁，不能再调用 get，
// 否则会自锁死（本文件早期版本正是栽在这里）。
func (s *vertexTokenStore) getLocked(key string, now time.Time) (string, bool) {
	entry, ok := s.entries[key]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt.Add(-vertexTokenRefreshAhead)) {
		// 已过期或进入提前刷新窗口：视为未命中，走刷新流程。
		delete(s.entries, key)
		return "", false
	}
	return entry.token, true
}

// putLocked 在已持锁的前提下写入令牌（同样是为了避免自锁）。
func (s *vertexTokenStore) putLocked(key, token string, expiresAt time.Time) {
	s.entries[key] = vertexCachedToken{token: token, expiresAt: expiresAt}
}

// applyVertexServiceAccountAuth 为 Vertex 请求换取（或复用）access_token 并写入鉴权头。
func applyVertexServiceAccountAuth(in upstreamRequestInput, header http.Header) error {
	token, err := vertexAccessToken(in)
	if err != nil {
		return err
	}
	header.Set("Authorization", "Bearer "+token)
	return nil
}

// vertexAccessToken 返回当前可用的 access_token，必要时先换取并缓存。
func vertexAccessToken(in upstreamRequestInput) (string, error) {
	account, err := parseVertexServiceAccount(in.APIKey)
	if err != nil {
		return "", err
	}

	at := in.clock()
	key := vertexCredentialFingerprint(account)
	if token, ok := vertexTokenCache.get(key, at); ok {
		return token, nil
	}

	// 持锁换取：并发请求只有一个真正去换令牌，其余拿到同一结果。
	vertexTokenCache.mu.Lock()
	defer vertexTokenCache.mu.Unlock()

	// 等锁期间可能已被别的请求刷新过，再查一次（持锁状态下用 getLocked）。
	if token, ok := vertexTokenCache.getLocked(key, at); ok {
		return token, nil
	}

	token, expiresAt, err := fetchVertexAccessToken(account, at)
	if err != nil {
		return "", err
	}
	vertexTokenCache.putLocked(key, token, expiresAt)
	return token, nil
}

// vertexCredentialFingerprint 计算服务账号的指纹，用作缓存键。
//
// 为什么包含私钥：同一 client_email 可能对应多把私钥（轮换场景），
// 用私钥一起哈希才能保证"换了私钥就是另一条缓存"，不会串用旧令牌。
// 只存哈希、不存原文，避免把私钥留在进程里多一份副本。
func vertexCredentialFingerprint(account vertexServiceAccount) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		account.ClientEmail, account.PrivateKey, account.TokenURI,
	}, "\n")))
	return hex.EncodeToString(sum[:])
}

// fetchVertexAccessToken 用服务账号换一次 access_token。
func fetchVertexAccessToken(account vertexServiceAccount, at time.Time) (string, time.Time, error) {
	assertion, err := buildVertexJWT(account, at)
	if err != nil {
		return "", time.Time{}, err
	}

	form := url.Values{}
	form.Set("grant_type", vertexJWTAssertionGrant)
	form.Set("assertion", assertion)

	req, err := http.NewRequest(http.MethodPost, account.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("relay: 构造 Vertex 换取令牌请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := vertexTokenHTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("relay: 请求 Vertex 换取令牌端点失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := readAllLimited(resp.Body, maxAdaptedBodyBytes)

	var payload vertexTokenResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", time.Time{}, fmt.Errorf("relay: Vertex 换取令牌响应不是合法 JSON（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode >= 300 || strings.TrimSpace(payload.AccessToken) == "" {
		reason := payload.ErrorDesc
		if reason == "" {
			reason = payload.Error
		}
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", time.Time{}, fmt.Errorf("relay: Vertex 换取令牌被拒绝：%s", reason)
	}

	ttl := vertexTokenTTL
	if payload.ExpiresIn > 0 {
		ttl = time.Duration(payload.ExpiresIn) * time.Second
	}
	return payload.AccessToken, at.Add(ttl), nil
}

// vertexJWTHeader 是 JWT 头部。
type vertexJWTHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// vertexJWTClaims 是 JWT 载荷（Google 服务账号断言所需的最小字段集）。
type vertexJWTClaims struct {
	Iss   string `json:"iss"`
	Scope string `json:"scope"`
	Aud   string `json:"aud"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

// buildVertexJWT 用服务账号私钥签一个 RS256 的 JWT 断言。
//
// 结构：base64url(header) + "." + base64url(claims) + "." + base64url(RS256 签名)。
// 有效期取 1 小时（Google 允许上限），作用域取 cloud-platform；aud 指向换令牌端点。
func buildVertexJWT(account vertexServiceAccount, at time.Time) (string, error) {
	key, err := parseRSAPrivateKey(account.PrivateKey)
	if err != nil {
		return "", err
	}

	headerJSON, err := json.Marshal(vertexJWTHeader{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("relay: 编码 Vertex JWT 头部失败: %w", err)
	}
	claimsJSON, err := json.Marshal(vertexJWTClaims{
		Iss:   account.ClientEmail,
		Scope: vertexOAuthScope,
		Aud:   account.TokenURI,
		Iat:   at.Unix(),
		Exp:   at.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("relay: 编码 Vertex JWT 载荷失败: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("relay: 签发 Vertex JWT 失败: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parseRSAPrivateKey 解析 PEM 编码的 RSA 私钥，兼容 PKCS#8 与 PKCS#1 两种编码。
//
// 为什么要兼容两种：Google 控制台导出的服务账号 JSON 历史上出现过两种私钥编码，
// 只认一种会让部分站点"配了却用不了"，因此两种都尝试解析。
func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("Vertex 服务账号的 private_key 不是合法的 PEM 私钥")
	}

	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("Vertex 服务账号的 private_key 不是 RSA 私钥")
		}
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("Vertex 服务账号的 private_key 无法解析（需为 PKCS#8 或 PKCS#1 的 RSA 私钥）")
}
