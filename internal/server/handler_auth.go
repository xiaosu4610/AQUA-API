// 本文件实现站点公开接口与认证接口：站点信息、注册、登录、退出、当前用户。
//
// 意图（Why）：
//
//	这是网站的大门：未登录者只能看到站点信息并完成注册/登录；
//	登录后拿到「会话令牌」，后续所有管理与门户接口都凭它鉴权。
//
// 安全设计（几条容易被忽略但很关键的点）：
//  1. 登录失败时不区分"用户不存在"与"口令错误"，且两条路径都执行口令哈希校验，
//     避免攻击者通过报错文案或响应时间差异枚举出有效用户名；
//  2. 注册是否开放由系统设置控制，关闭时明确拒绝并给出可操作提示；
//  3. 新用户额度取站点设置中的默认值（默认 0，即需管理员分配），
//     避免任何人注册后即可无偿使用上游额度；
//  4. 退出登录只吊销当前会话，不影响该用户在其他设备的登录。
//
// 流转（Flow）：
//
//	POST /api/auth/register → 校验开关 → 校验口令强度 → HashPassword → 建用户 → 建会话
//	POST /api/auth/login    → 查用户 → VerifyPassword → 建会话 → 返回会话令牌
//	POST /api/auth/logout   → 删除当前会话（按摘要）
//	GET  /api/auth/me       → 返回当前登录用户
//
// 扩展（Extend）：
//
//	新增登录方式（OAuth、二次验证）时：在 handleLogin 之前插入额外校验步骤，
//	不要改动会话签发逻辑（会话模型与吊销语义保持不变）。
package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
	"gitee.com/xiaosu4610/aqua-api/internal/version"
)

// sessionTTL 是网站登录会话的有效期。
//
// 取值 7 天的考虑：太短会让使用者频繁重新登录（尤其后台管理场景）；
// 太长则一旦会话泄露影响面大。配合"禁用/改密即吊销全部会话"来兜底。
const sessionTTL = 7 * 24 * time.Hour

// writeUserError 输出「面向最终用户」的错误响应，按请求语言本地化。
//
// 与 oai.WriteError 的分工：
//   - 本函数用于会被展示给用户的错误（注册/登录/令牌/兑换/额度等），
//     文案以语义化键从 i18n 目录取用，locale 来自 locale 中间件写入的请求 context；
//   - oai.WriteError 继续用于「运维/内部错误」（如"查询失败""解析失败"），
//     这些消息出现在日志与管理后台，翻译反而影响按关键词检索排障，故保持中文。
//
// args 为可选格式化参数（词条含 %d 等占位符时使用）。
func writeUserError(c *gin.Context, status int, key, errType, code string, args ...any) {
	oai.WriteErrorKey(c.Writer, status, key, errType, code, reqctx.Locale(c.Request.Context()), args...)
}

// dummyPasswordHash 是一个固定的口令哈希，用于登录失败时消耗等量时间。
//
// 背景：若"用户不存在"直接返回，而"口令错误"要跑一次 bcrypt，
// 攻击者可通过响应时间差判断用户名是否存在（典型的时间侧信道）。
// 因此在用户不存在时也执行一次哈希比对来抹平差异。
//
// 它在包初始化时计算一次（约百毫秒），之后每次登录失败只是复用同一字符串做比对。
var dummyPasswordHash string

func init() {
	// 忽略错误：这只是一段用于对齐耗时的常量哈希，生成失败也不影响正确性
	dummyPasswordHash, _ = crypto.HashPassword("aqua-dummy-password-for-timing-equalization")
}

// ---------------------------------------------------------------------------
// GET /api/status
// ---------------------------------------------------------------------------

// siteStatusResponse 是站点信息响应。
type siteStatusResponse struct {
	Name                string   `json:"name"`
	Version             string   `json:"version"`
	RegistrationEnabled bool     `json:"registration_enabled"`
	SiteDescription     string   `json:"site_description"`
	Models              []string `json:"models"`
	// EmailCodeRequired 注册是否必须填写邮箱验证码。
	//
	// 前端据此决定"邮箱与验证码"是必填还是可选，避免把校验规则复制到前端后失配。
	EmailCodeRequired bool `json:"email_code_required"`
	// EmailServiceReady 邮件发送通道是否已就绪（SMTP 配置完整）。
	//
	// 暴露这个布尔值不泄露任何凭据，但能让前端在通道未就绪时提前提示
	// "请联系管理员"，而不是让用户点半天按钮都收不到邮件。
	EmailServiceReady bool `json:"email_service_ready"`
}

// handleSiteStatus 返回站点信息与可用模型列表。
//
// 无需登录：落地页与登录页都要靠它渲染站点名称、注册开关与模型清单。
// 注意本接口不返回任何敏感信息（无渠道地址、无密钥、无用户数据）。
func (s *Server) handleSiteStatus(c *gin.Context) {
	settings, err := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"读取站点设置失败", oai.TypeServer, oai.CodeInternal)
		return
	}

	// 可用模型 = 所有启用渠道声明模型的并集，去重后排序，便于前端稳定展示
	models, err := s.availableModels(c)
	if err != nil {
		// 模型列表失败不阻断整个接口：站点名称与注册开关仍然可用
		models = nil
	}

	c.JSON(http.StatusOK, siteStatusResponse{
		Name:                settings.SiteName,
		Version:             version.Get().Version,
		RegistrationEnabled: settings.RegistrationEnabled,
		SiteDescription:     settings.SiteDescription,
		Models:              models,
		EmailCodeRequired:   settings.RegistrationRequireEmailCode,
		EmailServiceReady:   s.deps.Mailer != nil && s.deps.Mailer.Configured(),
	})
}

// availableModels 汇总所有启用渠道声明的模型（去重并排序）。
func (s *Server) availableModels(c *gin.Context) ([]string, error) {
	enabled := model.ChannelStatusEnabled
	channels, err := s.deps.Channels.List(c.Request.Context(), model.ChannelQuery{Status: &enabled})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	models := make([]string, 0, 32)
	for _, ch := range channels {
		for _, name := range ch.Models {
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			models = append(models, name)
		}
	}
	// 排序保证同一份数据每次返回顺序一致（前端缓存与渲染更稳定）
	sortStrings(models)
	return models, nil
}

// ---------------------------------------------------------------------------
// POST /api/auth/register
// ---------------------------------------------------------------------------

// registerRequest 是注册请求体。
type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	// Code 是邮箱验证码；仅在站点开启"注册必须邮箱验证码"时必填。
	Code string `json:"code"`
	// InviteCode 是可选邀请码；由邀请链接 /register?invite=CODE 带出。
	//
	// 语义：非法邀请码一律【忽略】并照常注册成功（取舍理由见 applyInviteOnRegister）。
	InviteCode string `json:"invite_code"`
}

// handleRegister 处理用户注册。
func (s *Server) handleRegister(c *gin.Context) {
	settings, err := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"读取站点设置失败", oai.TypeServer, oai.CodeInternal)
		return
	}
	if !settings.RegistrationEnabled {
		writeUserError(c, http.StatusForbidden,
			"auth.register_disabled", oai.TypePermission, "registration_disabled")
		return
	}

	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"request.invalid_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	username := strings.TrimSpace(req.Username)
	email := model.NormalizeEmail(req.Email)

	// 先做"不需要消耗外部资源"的校验（口令强度），
	// 再去校验验证码——顺序反了会导致"口令不合规却已浪费一个验证码"。
	if err := crypto.ValidatePasswordStrength(req.Password); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_password")
		return
	}

	ctx := c.Request.Context()

	if settings.RegistrationRequireEmailCode {
		// 用户名占用预检：放在消费验证码之前。
		// 理由：用户名被占用是最常见的失败原因之一，若先消费验证码再失败，
		// 用户必须重新获取验证码才能重试，体验明显变差。
		// 这里不是并发安全的"预留"，真正的唯一性仍由数据库唯一索引保证。
		if _, err := s.deps.Users.GetByUsername(ctx, username); err == nil {
			writeUserError(c, http.StatusConflict,
				"auth.username_taken", oai.TypeInvalidRequest, "username_taken")
			return
		} else if !errors.Is(err, model.ErrUserNotFound) {
			oai.WriteError(c.Writer, http.StatusInternalServerError,
				"网关内部错误", oai.TypeServer, oai.CodeInternal)
			return
		}

		if !s.verifyAndConsumeRegisterEmailCode(c, email, req.Code) {
			return
		}
	} else if email != "" {
		// 未开启验证码校验时邮箱仍为可选字段：填了就要合法，
		// 否则脏数据会进入用户表，后续做邮件通知时无从投递。
		if err := model.ValidateEmailFormat(email); err != nil {
			writeUserError(c, http.StatusBadRequest,
				"auth.invalid_email", oai.TypeInvalidRequest, "invalid_email")
			return
		}
	}

	hash, err := crypto.HashPassword(req.Password)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"网关内部错误", oai.TypeServer, oai.CodeInternal)
		return
	}

	user := &model.User{
		Username:     username,
		PasswordHash: hash,
		Email:        email,
		Role:         model.UserRoleUser,
		Status:       model.UserStatusEnabled,
		// 新用户额度取站点默认值：默认 0，即需要管理员分配后才可调用，
		// 避免任何人注册后即可无偿消耗上游额度。
		Quota: settings.DefaultUserQuota,
	}
	if err := s.deps.Users.Create(ctx, user); err != nil {
		if errors.Is(err, model.ErrUsernameTaken) {
			writeUserError(c, http.StatusConflict,
				"auth.username_taken", oai.TypeInvalidRequest, "username_taken")
			return
		}
		// 用户名/口令规则不满足时，领域校验的错误信息对使用者是有帮助的，
		// 但它可能包含内部描述，因此这里只回笼统提示，详细原因记录在服务端。
		// TODO(server): 接入结构化日志后记录 err
		writeUserError(c, http.StatusBadRequest,
			"auth.invalid_registration", oai.TypeInvalidRequest, "invalid_registration")
		return
	}

	// 邀请关系与注册奖：在账号创建成功之后处理（失败不影响注册，见方法注释）。
	s.applyInviteOnRegister(ctx, user, req.InviteCode, settings)

	s.issueSession(c, user, http.StatusOK)
}

// applyInviteOnRegister 在注册成功后建立邀请关系并（按配置）给邀请人发注册奖。
//
// 取舍（为什么"忽略非法邀请码"而不是报错拒绝注册）：
//
//	邀请码多由他人转述/手抄，填错是高频且低恶意的失误。若因此让注册整体失败，
//	用户会以为"网站坏了"而直接流失——为了一个可选的推广机制丢掉一个真实用户，
//	得不偿失。因此本方法只在能解析出合法、非自己的邀请人时才建立关系与发奖，
//	其余情况（空码、不存在、指向自己）一律静默忽略，账号照常创建。
//
// 失败处理：关系建立与发奖都是"锦上添花"，任何失败都不影响已成功创建的账号，
// 故各自显式吞掉错误并留注释，不向上抛（抛了会让用户看到莫名其妙的注册失败）。
func (s *Server) applyInviteOnRegister(ctx context.Context, user *model.User, rawCode string, settings model.SiteSettings) {
	code := model.NormalizeInviteCode(rawCode)
	if code == "" {
		return
	}

	inviterID, err := s.deps.Referrals.UserIDByInviteCode(ctx, code)
	if err != nil {
		// 邀请码不存在（或查询失败）：忽略，不阻断注册
		return
	}
	if inviterID == 0 || inviterID == user.ID {
		return
	}

	// 建立邀请关系（仅当被邀请人尚未绑定邀请人时才写入，幂等）。
	if err := s.deps.Referrals.BindInviter(ctx, user.ID, inviterID); err != nil {
		return
	}

	// 发放注册奖：仅在总开关开启且额度为正时。
	if !settings.Referral.Enabled || settings.Referral.RegisterBonus <= 0 {
		return
	}
	reward := &model.ReferralReward{
		InviterID: inviterID,
		InviteeID: user.ID,
		Kind:      model.ReferralKindRegister,
		Quota:     settings.Referral.RegisterBonus,
	}
	if _, err := s.deps.Referrals.GrantReward(ctx, reward); err != nil {
		// 发奖失败不回滚账号：关系已建立，站长可据台账人工补发。
		return
	}
}

// ---------------------------------------------------------------------------
// POST /api/auth/login
// ---------------------------------------------------------------------------

// loginRequest 是登录请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin 处理登录。
//
// 注意：本接口必须挂在登录限流中间件之后（见 router 注册处）。
func (s *Server) handleLogin(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"request.invalid_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	user, err := s.deps.Users.GetByUsername(c.Request.Context(), strings.TrimSpace(req.Username))
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			// 关键：即使用户不存在也执行一次哈希比对，抹平与"口令错误"的耗时差异，
			// 防止攻击者通过响应时间枚举有效用户名。
			crypto.VerifyPassword(req.Password, dummyPasswordHash)
			writeUserError(c, http.StatusUnauthorized,
				"auth.invalid_credentials", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
			return
		}
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"网关内部错误", oai.TypeServer, oai.CodeInternal)
		return
	}

	if !crypto.VerifyPassword(req.Password, user.PasswordHash) {
		// 与"用户不存在"返回完全相同的提示，不泄露用户名是否存在
		writeUserError(c, http.StatusUnauthorized,
			"auth.invalid_credentials", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
		return
	}

	if !user.IsActive() {
		writeUserError(c, http.StatusForbidden,
			"auth.account_disabled", oai.TypePermission, oai.CodeTokenDisabled)
		return
	}

	s.issueSession(c, user, http.StatusOK)
}

// ---------------------------------------------------------------------------
// POST /api/auth/logout
// ---------------------------------------------------------------------------

// handleLogout 吊销当前会话。
func (s *Server) handleLogout(c *gin.Context) {
	// 从请求头重新取一次明文令牌：会话表里只有摘要，必须由请求头提供原文才能算出摘要
	if rawToken := extractSessionToken(c); rawToken != "" {
		if err := s.deps.Sessions.DeleteByTokenHash(c.Request.Context(), crypto.SHA256Hex(rawToken)); err != nil {
			oai.WriteError(c.Writer, http.StatusInternalServerError,
				"退出登录失败", oai.TypeServer, oai.CodeInternal)
			return
		}
	}
	// 无论是否找到会话都返回成功：退出登录应当是幂等的
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------------------
// GET /api/auth/me
// ---------------------------------------------------------------------------

// handleMe 返回当前登录用户。
//
// 响应结构：{"user": {...}}。前端兼容解析，但契约以此为准。
func (s *Server) handleMe(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		writeUserError(c, http.StatusUnauthorized,
			"auth.not_logged_in", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": toUserDTO(user)})
}

// ---------------------------------------------------------------------------
// 内部辅助
// ---------------------------------------------------------------------------

// issueSession 为用户签发会话并返回响应体。
//
// 集中在此处的原因：注册与登录都需要"建会话 + 返回统一结构"，
// 若各写一份，很容易出现两处返回字段不一致（前端就得写兼容代码）。
func (s *Server) issueSession(c *gin.Context, user *model.User, status int) {
	token, err := model.GenerateSessionToken()
	if err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"网关内部错误", oai.TypeServer, oai.CodeInternal)
		return
	}

	expiresAt := time.Now().Add(sessionTTL)
	session := &model.Session{
		UserID:    user.ID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: expiresAt,
	}
	if err := s.deps.Sessions.Create(c.Request.Context(), session); err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"创建登录会话失败", oai.TypeServer, oai.CodeInternal)
		return
	}

	c.JSON(status, gin.H{
		"session_token": token,
		"expires_at":    expiresAt.Unix(),
		"user":          toUserDTO(user),
	})
}

// extractSessionToken 从请求头提取会话令牌明文。
//
// 说明：会话与访问令牌都走 Authorization: Bearer，提取规则一致，
// 因此直接复用中间件里那套规则，避免两处实现出现差异。
func extractSessionToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if key, found := strings.CutPrefix(auth, "Bearer "); found {
		return strings.TrimSpace(key)
	}
	return strings.TrimSpace(c.GetHeader("x-api-key"))
}

// sortStrings 对字符串切片做原地排序。
//
// 单独封装是为了避免在本文件引入 sort 包（本文件的职责是协议处理，
// 排序属于展示细节），同时便于将来替换为更符合中文语境的排序策略。
func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
