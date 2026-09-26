// 本文件实现访问令牌的增删改查，供管理后台与用户门户共用。
//
// 意图（Why）：
//
//	管理后台与用户门户操作的是同一类资源（访问令牌），差别只在"能操作谁"：
//	  管理员：可为任意用户创建、可改任意令牌；
//	  普通用户：只能操作自己名下的令牌。
//	若把两套逻辑各写一份，极易出现"门户侧忘了校验归属"这类越权漏洞。
//	因此这里只实现一份，用 ownerID 参数控制可见范围：
//	  ownerID > 0  表示限定归属（门户调用）
//	  ownerID == 0 表示不限定（管理员调用）
//
// 流转（Flow）：
//
//	/api/user/tokens/*  → 门户（ownerID = 当前用户）
//	/api/admin/tokens/* → 后台（ownerID = 0）
//	  └─ 均落到 createTokenAndRespond / updateToken / deleteToken
//
// 扩展（Extend）：
//
//	新增令牌属性（如 IP 白名单）时：在请求结构体加字段，并同步更新
//	createTokenAndRespond 与 updateToken（两处都要改，否则创建与编辑行为不一致）。
package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
)

// tokenUpdateRequest 是更新令牌的请求体。
//
// 字段全部用指针：未提交的字段保持原值。
// 例如前端"仅切换启用状态"时不会带上额度字段，若用值类型就会把额度清零。
type tokenUpdateRequest struct {
	Name           *string   `json:"name"`
	Status         *int      `json:"status"`
	UnlimitedQuota *bool     `json:"unlimited_quota"`
	RemainQuota    *int64    `json:"remain_quota"`
	ExpiresInDays  *int      `json:"expires_in_days"`
	Models         *[]string `json:"models"`
	// GroupName 是令牌所属分组标识。
	// 语义与渠道的 key_strategy 一致：留空（字段缺失或空串）表示"不修改"，
	// 避免前端只提交部分字段（如仅启停）时把已配置的分组意外清空。
	GroupName *string `json:"group_name"`
}

// handleMyListTokens 返回当前用户的令牌列表。
func (s *Server) handleMyListTokens(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}

	page, size, offset := parsePagination(c)
	ownerID := user.ID

	ctx := c.Request.Context()
	query := model.TokenQuery{OwnerID: &ownerID, Limit: size, Offset: offset}

	tokens, err := s.deps.Tokens.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询令牌列表失败")
		return
	}
	total, err := s.deps.Tokens.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计令牌总数失败")
		return
	}

	now := time.Now()
	items := make([]tokenDTO, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, toTokenDTO(token, token.EffectiveStatus(now)))
	}
	c.JSON(http.StatusOK, newPagedResponse(items, total, page, size))
}

// handleMyCreateToken 为当前用户创建令牌。
func (s *Server) handleMyCreateToken(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}

	var req adminTokenCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest, "request.invalid_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	// 安全要点：归属强制为当前用户，忽略请求体中的 user_id。
	// 否则普通用户可通过伪造 user_id 把令牌挂到别人名下（或反之）。
	s.createTokenAndRespond(c, user.ID, req.Name, req.ExpiresInDays, req.Models, req.UnlimitedQuota, req.RemainQuota, req.GroupName)
}

// handleMyUpdateToken 更新当前用户的令牌。
func (s *Server) handleMyUpdateToken(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	s.updateToken(c, user.ID)
}

// handleMyDeleteToken 删除当前用户的令牌。
func (s *Server) handleMyDeleteToken(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	s.deleteToken(c, user.ID)
}

// createTokenAndRespond 创建令牌并返回（含一次性明文）。
//
// 参数 ownerID 为令牌归属；expiresInDays 为 0 表示永不过期；
// groupName 为空表示使用网关默认分组，非空时必须指向一个已存在的分组。
func (s *Server) createTokenAndRespond(c *gin.Context, ownerID uint64, name string, expiresInDays int, models []string, unlimitedQuota bool, remainQuota int64, groupName string) {
	name = strings.TrimSpace(name)
	if name == "" {
		writeUserError(c, http.StatusBadRequest, "token.name_required", oai.TypeInvalidRequest, "invalid_name")
		return
	}

	// 分组先校验再落库：若令牌指向一个不存在的分组，调用时会出现"无渠道可用"
	// 或全量 404，且从令牌列表上看不出原因，属于极难定位的运营故障。
	group, ok := s.resolveTokenGroupName(c, groupName)
	if !ok {
		return
	}

	key, err := model.GenerateTokenKey()
	if err != nil {
		s.respondInternalError(c, "生成令牌失败")
		return
	}

	token := &model.Token{
		OwnerID:        ownerID,
		Name:           name,
		Key:            key,
		Status:         model.TokenStatusEnabled,
		UnlimitedQuota: unlimitedQuota,
		Models:         models,
		GroupName:      group,
	}
	if expiresInDays > 0 {
		token.ExpiresAt = time.Now().AddDate(0, 0, expiresInDays)
	}
	if !unlimitedQuota {
		token.RemainQuota = remainQuota
	}

	if err := s.deps.Tokens.Create(c.Request.Context(), token); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "创建令牌失败："+err.Error(), oai.TypeInvalidRequest, "invalid_token")
		return
	}

	// 一次性返回明文：数据库只保存摘要与密文，此后再也无法取回明文。
	// 前端必须显著提示使用者立即保存。
	dto := toTokenDTO(token, token.EffectiveStatus(time.Now()))
	dto.Key = key
	c.JSON(http.StatusOK, dto)
}

// updateToken 更新令牌；ownerID > 0 时校验归属。
func (s *Server) updateToken(c *gin.Context, ownerID uint64) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req tokenUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest, "request.invalid_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	token, ok := s.loadOwnedToken(c, id, ownerID)
	if !ok {
		return
	}

	if req.Name != nil {
		if name := strings.TrimSpace(*req.Name); name != "" {
			token.Name = name
		}
	}
	if req.Status != nil {
		if !model.TokenStatus(*req.Status).IsValid() {
			writeUserError(c, http.StatusBadRequest, "token.invalid_status", oai.TypeInvalidRequest, "invalid_status")
			return
		}
		token.Status = model.TokenStatus(*req.Status)
	}
	if req.UnlimitedQuota != nil {
		token.UnlimitedQuota = *req.UnlimitedQuota
	}
	if req.RemainQuota != nil {
		token.RemainQuota = *req.RemainQuota
	}
	if req.ExpiresInDays != nil {
		if *req.ExpiresInDays <= 0 {
			token.ExpiresAt = time.Time{} // 0 天 = 永不过期
		} else {
			token.ExpiresAt = time.Now().AddDate(0, 0, *req.ExpiresInDays)
		}
	}
	if req.Models != nil {
		token.Models = *req.Models
	}
	// 分组：留空表示"不修改"（与渠道的 key_strategy 保护方式一致），
	// 非空时校验"格式合法 + 分组存在"后再覆盖，避免令牌指向不存在的分组。
	if req.GroupName != nil {
		if group, ok := s.resolveTokenGroupName(c, *req.GroupName); ok {
			if group != "" {
				token.GroupName = group
			}
		} else {
			return
		}
	}

	if err := s.deps.Tokens.Update(ctx, token); err != nil {
		s.respondInternalError(c, "更新令牌失败")
		return
	}
	c.JSON(http.StatusOK, toTokenDTO(token, token.EffectiveStatus(time.Now())))
}

// deleteToken 删除令牌；ownerID > 0 时校验归属。
func (s *Server) deleteToken(c *gin.Context, ownerID uint64) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	if _, ok := s.loadOwnedToken(c, id, ownerID); !ok {
		return
	}

	if err := s.deps.Tokens.Delete(ctx, id); err != nil {
		if errors.Is(err, model.ErrTokenNotFound) {
			writeUserError(c, http.StatusNotFound, "token.not_found", oai.TypeInvalidRequest, "token_not_found")
			return
		}
		s.respondInternalError(c, "删除令牌失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// loadOwnedToken 载入令牌并校验归属。
//
// 权限与信息泄露的取舍：当令牌存在但归属不符时，返回 404（而不是 403）。
// 若返回 403，攻击者可据此推断"该 ID 确实存在"，从而枚举系统中的令牌数量；
// 返回 404 则与"不存在"无法区分，不泄露额外信息。
func (s *Server) loadOwnedToken(c *gin.Context, id, ownerID uint64) (*model.Token, bool) {
	token, err := s.deps.Tokens.GetByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, model.ErrTokenNotFound) {
			writeUserError(c, http.StatusNotFound, "token.not_found", oai.TypeInvalidRequest, "token_not_found")
			return nil, false
		}
		s.respondInternalError(c, "查询令牌失败")
		return nil, false
	}

	if ownerID > 0 && token.OwnerID != ownerID {
		writeUserError(c, http.StatusNotFound, "token.not_found", oai.TypeInvalidRequest, "token_not_found")
		return nil, false
	}
	return token, true
}

// resolveTokenGroupName 校验并归一化令牌的分组标识，失败时已写出响应。
//
// 返回 (归一化后的分组名, 是否通过)：
//   - 入参为空 → ("", true)：表示"使用网关默认分组"，交由转发层回退；
//   - 格式非法（大写、含空格/逗号/斜杠、超长）→ 400 并原样回传领域错误；
//   - 分组不存在 → 400 提示"分组不存在"（避免令牌指向空分组导致调用全量失败）。
//
// 为什么存在性校验放在 server 层而不是 model 层：领域模型不应依赖仓储，
// 由接口层组合"格式校验（model）+ 存在性校验（仓储）"才是正确的分层。
func (s *Server) resolveTokenGroupName(c *gin.Context, raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", true
	}
	if err := model.ValidateGroupName(name); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(), oai.TypeInvalidRequest, "invalid_group_name")
		return "", false
	}
	if s.deps.Groups == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "分组模块未启用", oai.TypeServer, oai.CodeInternal)
		return "", false
	}
	if _, err := s.deps.Groups.GetByName(c.Request.Context(), name); err != nil {
		if errors.Is(err, model.ErrModelGroupNotFound) {
			writeUserError(c, http.StatusBadRequest, "group.not_found", oai.TypeInvalidRequest, "group_not_found")
			return "", false
		}
		s.respondInternalError(c, "查询分组失败")
		return "", false
	}
	return name, true
}

// requireCurrentUser 取出当前登录用户；未登录时已写出 401 响应，返回 false。
func (s *Server) requireCurrentUser(c *gin.Context) (*model.User, bool) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		writeUserError(c, http.StatusUnauthorized,
			"auth.not_logged_in", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return nil, false
	}
	return user, true
}
