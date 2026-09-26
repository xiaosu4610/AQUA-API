// 本文件实现 OAuth 凭据的自动刷新。
//
// 意图（Why）：
//
//	订阅账号（如 Claude / Gemini 的网页版账号）用 OAuth 令牌调用上游，
//	access_token 通常 1 小时就过期。若不自动刷新，管理员每小时都要手工更新，
//	否则整条渠道会静默失效——这是"账号池化"能否真正可用的分水岭。
//
// 实现要点：
//
//  1. 提前刷新（见 model.refreshAheadSeconds）：不等真正过期就换新，
//     避免"过期瞬间的请求必然失败一次"；
//  2. 刷新请求用标准 OAuth2 的 refresh_token 授权（form-urlencoded），
//     端点与 client 凭据来自 oauth_providers 配置表，不写死；
//  3. 刷新成功后回写数据库：否则下一个请求还要再刷，等于每次调用都多一次往返；
//  4. 刷新失败按"该凭据不可用"处理：计入连续失败，达到阈值后自动摘除，
//     并让本次请求改用池内其他凭据（见 resolveChatKey）。
//
// 流转（Flow）：
//
//	relay.resolveChatKey → OAuthRefresher.EnsureFresh
//	  ├─ 未到期：直接返回现有 access_token
//	  └─ 需要刷新：POST {token_url} → 回写 channel_keys → 返回新令牌
//
// 扩展（Extend）：
//
//	若某平台不是标准 OAuth2（例如需要 PKCE 或额外签名），
//	在 buildRefreshRequest 中按 provider.Name 增加分支。
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// oauthRefreshTimeout 是刷新请求的超时时间。
//
// 取 15 秒：刷新是轻量请求（一次 HTTP 往返），正常在 1 秒内完成；
// 超过 15 秒基本可判定平台异常，快速失败让请求改走其他凭据更好。
const oauthRefreshTimeout = 15 * time.Second

// OAuthRefresher 负责把 refresh_token 换成可用的 access_token。
//
// 并发安全：内部只有一个串行锁。选择"全局串行"而不是"每凭据一把锁"，
// 是因为刷新是低频事件（每凭据每小时一次），串行带来的排队成本可忽略，
// 而实现简单意味着更少的并发缺陷。若将来凭据规模导致排队明显，
// 再改为按凭据 ID 分片加锁即可。
type OAuthRefresher struct {
	providers model.OAuthProviderRepository
	keys      model.ChannelKeyRepository
	client    *http.Client

	mu sync.Mutex
}

// NewOAuthRefresher 创建刷新器。
func NewOAuthRefresher(providers model.OAuthProviderRepository, keys model.ChannelKeyRepository) *OAuthRefresher {
	return &OAuthRefresher{
		providers: providers,
		keys:      keys,
		client: &http.Client{
			Timeout: oauthRefreshTimeout,
			// 刷新请求不应经代理绕行：它是网关自身的控制面流量
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
}

// oauthTokenResponse 是刷新端点的响应。
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// EnsureFresh 返回该凭据当前可用的令牌值；必要时先刷新。
//
// 返回错误表示该凭据本次不可用（调用方应改用池内其他凭据，而不是直接失败）。
func (o *OAuthRefresher) EnsureFresh(ctx context.Context, key *model.ChannelKey) (string, error) {
	if key == nil {
		return "", fmt.Errorf("relay: 凭据为空")
	}
	if !key.IsOAuth() {
		return key.CredentialValue(), nil
	}
	if !key.NeedsRefresh(time.Now()) {
		return key.AccessToken, nil
	}
	if strings.TrimSpace(key.RefreshToken) == "" {
		return "", fmt.Errorf("relay: OAuth 凭据缺少 refresh_token，无法刷新")
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	// 重新读取一次：可能在等锁期间已被其他请求刷新过
	// （多个并发请求同时发现过期时，只有第一个真正去刷新）
	latest, err := o.reload(ctx, key)
	if err == nil && latest != nil && !latest.NeedsRefresh(time.Now()) {
		key.AccessToken = latest.AccessToken
		key.ExpiresAt = latest.ExpiresAt
		return latest.AccessToken, nil
	}

	return o.refresh(ctx, key)
}

// reload 重新读取凭据的当前状态。
//
// 读不到时返回 nil（调用方据此走正常刷新流程）：
// 例如凭据刚被管理员删除，此时刷新也会失败，但错误信息由 refresh 给出更准确。
func (o *OAuthRefresher) reload(ctx context.Context, key *model.ChannelKey) (*model.ChannelKey, error) {
	if o.keys == nil {
		return nil, nil
	}
	keys, err := o.keys.ListByChannel(ctx, key.ChannelID)
	if err != nil {
		return nil, err
	}
	for _, item := range keys {
		if item.ID == key.ID {
			return item, nil
		}
	}
	return nil, nil
}

// refresh 执行一次刷新并回写数据库。
func (o *OAuthRefresher) refresh(ctx context.Context, key *model.ChannelKey) (string, error) {
	if o.providers == nil {
		return "", fmt.Errorf("relay: 未配置 OAuth 提供方仓储")
	}
	if strings.TrimSpace(key.Provider) == "" {
		return "", fmt.Errorf("relay: 该凭据未指定 OAuth 提供方（provider 为空），无法刷新")
	}

	provider, err := o.providers.GetByName(ctx, key.Provider)
	if err != nil {
		return "", fmt.Errorf("relay: 读取 OAuth 提供方 %q 失败: %w", key.Provider, err)
	}
	if !provider.Enabled {
		return "", fmt.Errorf("relay: OAuth 提供方 %q 已停用", key.Provider)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", key.RefreshToken)
	if strings.TrimSpace(provider.ClientID) != "" {
		form.Set("client_id", provider.ClientID)
	}
	if strings.TrimSpace(provider.ClientSecret) != "" {
		form.Set("client_secret", provider.ClientSecret)
	}
	if strings.TrimSpace(provider.Scope) != "" {
		form.Set("scope", provider.Scope)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("relay: 构造刷新请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("relay: 刷新令牌请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := readAllLimited(resp.Body, maxAdaptedBodyBytes)

	var payload oauthTokenResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("relay: 刷新响应不是合法 JSON（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode >= 300 || payload.AccessToken == "" {
		reason := payload.ErrorDesc
		if reason == "" {
			reason = payload.Error
		}
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("relay: 刷新令牌被拒绝：%s", reason)
	}

	expiresAt := time.Time{}
	if payload.ExpiresIn > 0 {
		// 预留 30 秒余量：避免"回写的时间刚好已过期"，导致下个请求立刻又刷一次
		expiresAt = time.Now().Add(time.Duration(payload.ExpiresIn)*time.Second - 30*time.Second)
	}

	if o.keys != nil {
		if err := o.keys.UpdateTokens(ctx, key.ID, payload.AccessToken, expiresAt, payload.RefreshToken); err != nil {
			// 回写失败不影响本次使用：新令牌已在内存里，至少本次调用能成功
			return payload.AccessToken, nil
		}
	}

	key.AccessToken = payload.AccessToken
	key.ExpiresAt = expiresAt
	return payload.AccessToken, nil
}
