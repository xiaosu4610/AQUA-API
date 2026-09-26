// 本文件实现 OAuth 提供方（订阅账号刷新配置）的后台管理接口。
//
// 意图（Why）：
//
//	订阅账号池要能自动续期，就必须知道"去哪刷新、用什么 client 凭据"。
//	把这部分做成后台可维护的配置，开源使用者接入任意 OAuth2 平台时无需改代码，
//	平台变更端点时也无需重新发版——这正是"凭据 DB 化"的落地方式。
//
// 安全约定：
//
//	ClientSecret 落库加密；接口只返回掩码（masked_client_secret），
//	更新时传空表示"不修改"，避免管理员只改备注却把密钥清空。
//
// 流转（Flow）：
//
//	后台「OAuth 提供方」→ GET/POST/PUT/DELETE /api/admin/oauth-providers
//	  → relay.OAuthRefresher 刷新令牌时按名称取用
package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// oauthProviderDTO 是 OAuth 提供方配置的对外表示。
type oauthProviderDTO struct {
	ID                 uint64 `json:"id"`
	Name               string `json:"name"`
	TokenURL           string `json:"token_url"`
	ClientID           string `json:"client_id"`
	MaskedClientSecret string `json:"masked_client_secret"`
	Scope              string `json:"scope"`
	Remark             string `json:"remark"`
	Enabled            bool   `json:"enabled"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

// toOAuthProviderDTO 把领域模型转为对外 DTO（脱敏客户端密钥）。
func toOAuthProviderDTO(provider *model.OAuthProvider) oauthProviderDTO {
	if provider == nil {
		return oauthProviderDTO{}
	}
	return oauthProviderDTO{
		ID:                 provider.ID,
		Name:               provider.Name,
		TokenURL:           provider.TokenURL,
		ClientID:           provider.ClientID,
		MaskedClientSecret: provider.MaskedClientSecret(),
		Scope:              provider.Scope,
		Remark:             provider.Remark,
		Enabled:            provider.Enabled,
		CreatedAt:          unixOrZero(provider.CreatedAt),
		UpdatedAt:          unixOrZero(provider.UpdatedAt),
	}
}

// handleListOAuthProviders 返回全部 OAuth 提供方配置。
func (s *Server) handleListOAuthProviders(c *gin.Context) {
	if s.deps.OAuthProviders == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"订阅账号功能未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	providers, err := s.deps.OAuthProviders.List(c.Request.Context())
	if err != nil {
		s.respondInternalError(c, "查询 OAuth 提供方失败")
		return
	}

	items := make([]oauthProviderDTO, 0, len(providers))
	for _, provider := range providers {
		items = append(items, toOAuthProviderDTO(provider))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// oauthProviderUpsertRequest 是新增/更新请求体。
type oauthProviderUpsertRequest struct {
	Name         string `json:"name"`
	TokenURL     string `json:"token_url"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Scope        string `json:"scope"`
	Remark       string `json:"remark"`
	Enabled      *bool  `json:"enabled"`
}

// handleCreateOAuthProvider 新增提供方配置。
func (s *Server) handleCreateOAuthProvider(c *gin.Context) {
	if s.deps.OAuthProviders == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"订阅账号功能未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req oauthProviderUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	provider := &model.OAuthProvider{
		Name:         strings.TrimSpace(req.Name),
		TokenURL:     strings.TrimSpace(req.TokenURL),
		ClientID:     strings.TrimSpace(req.ClientID),
		ClientSecret: strings.TrimSpace(req.ClientSecret),
		Scope:        strings.TrimSpace(req.Scope),
		Remark:       strings.TrimSpace(req.Remark),
		Enabled:      true,
	}
	if req.Enabled != nil {
		provider.Enabled = *req.Enabled
	}

	if err := s.deps.OAuthProviders.Create(c.Request.Context(), provider); err != nil {
		// 领域校验信息（如"令牌端点必须 https"）对使用者有直接帮助
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_provider")
		return
	}
	c.JSON(http.StatusOK, toOAuthProviderDTO(provider))
}

// handleUpdateOAuthProvider 更新提供方配置。
//
// 按 ID 定位：仓储以 name 为业务主键（刷新时按名称查找），
// 但配置数量很少，这里用 List 做一次线性查找即可，无需新增仓储方法。
func (s *Server) handleUpdateOAuthProvider(c *gin.Context) {
	if s.deps.OAuthProviders == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"订阅账号功能未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req oauthProviderUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	providers, err := s.deps.OAuthProviders.List(ctx)
	if err != nil {
		s.respondInternalError(c, "查询 OAuth 提供方失败")
		return
	}

	var target *model.OAuthProvider
	for _, provider := range providers {
		if provider.ID == id {
			target = provider
			break
		}
	}
	if target == nil {
		oai.WriteError(c.Writer, http.StatusNotFound, "OAuth 提供方不存在", oai.TypeInvalidRequest, "provider_not_found")
		return
	}

	if name := strings.TrimSpace(req.Name); name != "" {
		target.Name = name
	}
	if url := strings.TrimSpace(req.TokenURL); url != "" {
		target.TokenURL = url
	}
	if clientID := strings.TrimSpace(req.ClientID); clientID != "" {
		target.ClientID = clientID
	}
	// 空表示不修改：避免"只改备注却清空密钥"
	if secret := strings.TrimSpace(req.ClientSecret); secret != "" {
		target.ClientSecret = secret
	}
	if scope := strings.TrimSpace(req.Scope); scope != "" {
		target.Scope = scope
	}
	if remark := strings.TrimSpace(req.Remark); remark != "" {
		target.Remark = remark
	}
	if req.Enabled != nil {
		target.Enabled = *req.Enabled
	}

	if err := s.deps.OAuthProviders.Update(ctx, target); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_provider")
		return
	}
	c.JSON(http.StatusOK, toOAuthProviderDTO(target))
}

// handleDeleteOAuthProvider 删除提供方配置。
//
// 注意：删除配置不会删除已导入的凭据——那些凭据只是"暂时无法刷新"，
// 若一并删掉，管理员误删配置就等于丢了所有账号。凭据需要在渠道里单独移除。
func (s *Server) handleDeleteOAuthProvider(c *gin.Context) {
	if s.deps.OAuthProviders == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"订阅账号功能未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	if err := s.deps.OAuthProviders.Delete(c.Request.Context(), id); err != nil {
		if errors.Is(err, model.ErrOAuthProviderNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "OAuth 提供方不存在",
				oai.TypeInvalidRequest, "provider_not_found")
			return
		}
		s.respondInternalError(c, "删除 OAuth 提供方失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
