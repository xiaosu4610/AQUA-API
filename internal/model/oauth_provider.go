// 本文件定义「OAuth 提供方」领域模型与仓储接口。
//
// 意图（Why）：
//
//	订阅账号池（M3）需要定期用 refresh_token 换新的 access_token，
//	而"去哪换、用什么 client_id"因平台而异（Claude、Gemini、自建服务各不相同）。
//	把这些差异做成配置而不是代码：开源使用者接入小众平台时无需改代码，
//	平台变更端点时也无需重新发版。
//
// 安全约定：
//
//	ClientSecret 属于密钥类配置，落库由 store 层加密；对外输出必须脱敏。
//
// 流转（Flow）：
//
//	后台配置 provider → 导入 OAuth 凭据时选择 provider
//	  → 转发时发现令牌过期 → OAuthRefresher 用 provider 配置刷新
//	  → 新令牌回写 channel_keys
//
// 扩展（Extend）：
//
//	若某平台不是标准 OAuth2（如需要额外签名头），在 relay 的刷新实现里
//	按 provider.Name 增加分支，而不是往本结构体堆字段——
//	配置项应当表达"通用能力"，平台特有的怪癖放在代码里更容易被审查。
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrOAuthProviderNotFound 表示提供方配置不存在。
var ErrOAuthProviderNotFound = errors.New("model: OAuth 提供方不存在")

// OAuthProvider 描述一个可刷新令牌的平台配置。
type OAuthProvider struct {
	ID           uint64    // 主键
	Name         string    // 唯一标识（如 claude、gemini）
	TokenURL     string    // 刷新令牌的端点
	ClientID     string    // OAuth 客户端 ID
	ClientSecret string    // OAuth 客户端密钥【明文，仅内存】
	Scope        string    // 作用域（部分平台刷新时需要回传）
	Remark       string    // 备注
	Enabled      bool      // 是否启用
	CreatedAt    time.Time // 创建时间
	UpdatedAt    time.Time // 更新时间
}

// Validate 校验配置合法性。
func (p *OAuthProvider) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("提供方名称不能为空")
	}
	if strings.TrimSpace(p.Name) != strings.ToLower(strings.TrimSpace(p.Name)) {
		return errors.New("提供方名称请使用小写字母（它会作为配置键被引用，大小写不一致会导致查不到）")
	}
	if strings.TrimSpace(p.TokenURL) == "" {
		return errors.New("令牌端点（token_url）不能为空")
	}
	// 令牌端点是敏感信息（可能带临时签名参数），必须是 https
	if !strings.HasPrefix(p.TokenURL, "https://") {
		return fmt.Errorf("令牌端点必须以 https:// 开头，当前为 %q（明文传输令牌不可接受）", p.TokenURL)
	}
	return nil
}

// MaskedClientSecret 返回脱敏后的客户端密钥，供界面展示。
func (p *OAuthProvider) MaskedClientSecret() string {
	secret := p.ClientSecret
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}

// OAuthProviderRepository 定义提供方配置的持久化操作。
type OAuthProviderRepository interface {
	// Create 新增配置，名称重复时返回错误。
	Create(ctx context.Context, provider *OAuthProvider) error

	// GetByName 按名称查询，不存在时返回 ErrOAuthProviderNotFound。
	GetByName(ctx context.Context, name string) (*OAuthProvider, error)

	// List 返回全部配置（按名称升序），供后台管理与刷新时查找。
	List(ctx context.Context) ([]*OAuthProvider, error)

	// Update 按 ID 更新。
	Update(ctx context.Context, provider *OAuthProvider) error

	// Delete 按 ID 删除。
	Delete(ctx context.Context, id uint64) error
}
