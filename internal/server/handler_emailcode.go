// 本文件实现「注册邮箱验证码」的发送与校验。
//
// 意图（Why）：
//
//	开放注册的站点最大的运营风险是脚本批量注册（垃圾账号、刷光免费额度、
//	发信通道被投诉）。邮箱验证码把"拥有一个真实可收信的邮箱"变成注册门槛，
//	使批量注册的成本显著高于收益。
//
//	另一个不放在 relay / model 层的原因是：本流程涉及"发信"这一外部副作用，
//	需要冷却、限流、失败回滚等编排逻辑，属于典型的应用层职责。
//
// 流转（Flow）：
//
//	POST /api/auth/email-code  申请验证码
//	  → 校验注册开关与邮件配置 → 冷却/小时上限检查 → 生成码
//	  → 落库（只存摘要）→ 发信 → 失败则回滚记录 → 返回剩余有效期
//
//	POST /api/auth/register    注册（见 handler_auth.go）
//	  → verifyAndConsumeRegisterEmailCode 校验并一次性消费 → 建用户
//
// 扩展（Extend）：
//
//	新增用途（如找回密码）：复用 sendEmailCode 的核心逻辑，
//	只是把 model.EmailCodePurposeRegister 换成新用途常量，
//	并在校验侧使用同一用途值——务必保证"发送"与"校验"两侧的用途字符串一致。
package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/mailer"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
)

// emailCodeRequest 是申请验证码的请求体。
type emailCodeRequest struct {
	Email string `json:"email"`
}

// emailCodeResponse 是申请验证码的响应体。
//
// 把 expires_in 与 cooldown 一并返回，前端即可显示"剩余有效时间"与
// "多少秒后可重发"的倒计时，而不必把这两个数值硬编码在前端。
type emailCodeResponse struct {
	OK        bool   `json:"ok"`
	ExpiresIn int    `json:"expires_in"` // 验证码有效期（秒）
	Cooldown  int    `json:"cooldown"`   // 重发冷却时间（秒）
	Message   string `json:"message"`    // 给用户看的提示文案
}

// handleSendEmailCode 处理验证码申请。
//
// 挂载在登录限流中间件之后：即使没有邮箱维度限流，也能挡住高频刷接口。
func (s *Server) handleSendEmailCode(c *gin.Context) {
	var req emailCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"request.invalid_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()

	settings, err := model.LoadSiteSettings(ctx, s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取站点设置失败")
		return
	}
	if !settings.RegistrationEnabled {
		writeUserError(c, http.StatusForbidden,
			"auth.registration_closed", oai.TypePermission, "registration_disabled")
		return
	}
	if !settings.RegistrationRequireEmailCode {
		// 管理员已关闭"验证码校验"，此时不应再消耗邮件配额
		writeUserError(c, http.StatusBadRequest,
			"email.code_not_required", oai.TypeInvalidRequest, "email_code_not_required")
		return
	}

	email := model.NormalizeEmail(req.Email)
	if err := model.ValidateEmailFormat(email); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"auth.invalid_email", oai.TypeInvalidRequest, "invalid_email")
		return
	}

	// 邮件通道未就绪时给出可操作的提示，而不是让用户以为"验证码已发出但没收到"
	if s.deps.Mailer == nil || !s.deps.Mailer.Configured() {
		writeUserError(c, http.StatusServiceUnavailable,
			"email.service_unavailable", oai.TypeServer, "email_service_unavailable")
		return
	}

	now := time.Now()
	purpose := model.EmailCodePurposeRegister

	// ── 冷却检查：同一邮箱两次申请之间必须间隔 60 秒 ──────────────
	// 目的：防止把本站当作"邮件轰炸机"（反复给同一个人发信）。
	// 注意这里用最新一条记录（含已过期但未消费的），因为"刚发过"这个事实
	// 不应因验证码过期而消失。
	if latest, err := s.deps.EmailCodes.LatestActive(ctx, email, purpose); err == nil {
		if elapsed := now.Sub(latest.CreatedAt); elapsed < model.EmailCodeResendCooldown {
			wait := int((model.EmailCodeResendCooldown - elapsed).Seconds()) + 1
			c.Header("Retry-After", strconv.Itoa(wait))
			writeUserError(c, http.StatusTooManyRequests,
				"email.cooldown", oai.TypeRateLimit, "email_code_cooldown", wait)
			return
		}
	} else if !errors.Is(err, model.ErrEmailCodeNotFound) {
		s.respondInternalError(c, "查询验证码记录失败")
		return
	}

	// ── 邮箱维度小时上限 ─────────────────────────────────────────
	since := now.Add(-time.Hour)
	emailCount, err := s.deps.EmailCodes.CountByEmailSince(ctx, email, since)
	if err != nil {
		s.respondInternalError(c, "统计验证码申请次数失败")
		return
	}
	if emailCount >= model.EmailCodeMaxPerEmailPerHour {
		writeUserError(c, http.StatusTooManyRequests,
			"email.rate_email", oai.TypeRateLimit, "email_code_email_limit")
		return
	}

	// ── 来源 IP 维度小时上限 ─────────────────────────────────────
	// 这一层是为了防止"换邮箱绕过邮箱维度限流"。
	clientIP := middleware.ClientIP(c)
	ipCount, err := s.deps.EmailCodes.CountByIPSince(ctx, clientIP, since)
	if err != nil {
		s.respondInternalError(c, "统计验证码申请次数失败")
		return
	}
	if ipCount >= model.EmailCodeMaxPerIPPerHour {
		writeUserError(c, http.StatusTooManyRequests,
			"email.rate_ip", oai.TypeRateLimit, "email_code_ip_limit")
		return
	}

	// ── 生成并落库（只存摘要，明文不落库）───────────────────────
	code, err := model.GenerateEmailCode()
	if err != nil {
		s.respondInternalError(c, "生成验证码失败")
		return
	}

	record := &model.EmailCode{
		Email:     email,
		Purpose:   purpose,
		CodeHash:  model.HashEmailCode(email, purpose, code),
		ExpiresAt: now.Add(model.EmailCodeTTL),
		RequestIP: clientIP,
	}
	if err := s.deps.EmailCodes.Create(ctx, record); err != nil {
		s.respondInternalError(c, "保存验证码失败")
		return
	}

	// ── 发信 ────────────────────────────────────────────────────
	subject, body := mailer.RegisterCodeEmail(settings.SiteName, code, model.EmailCodeTTL)
	if err := s.deps.Mailer.Send(ctx, email, subject, body); err != nil {
		// 关键：发信失败必须回滚记录，否则用户会因这条"从未收到"的记录
		// 被冷却 60 秒，表现为"点了重发却一直提示过于频繁"。
		// 回滚失败也不向用户暴露细节，仅记录（此处无结构化日志，交给启动日志覆盖）。
		_ = s.deps.EmailCodes.DeleteByID(ctx, record.ID)
		writeUserError(c, http.StatusBadGateway,
			"email.send_failed", oai.TypeServer, "email_send_failed")
		return
	}

	c.JSON(http.StatusOK, emailCodeResponse{
		OK:        true,
		ExpiresIn: int(model.EmailCodeTTL.Seconds()),
		Cooldown:  int(model.EmailCodeResendCooldown.Seconds()),
		Message:   "验证码已发送，请查收邮件（含垃圾箱）",
	})
}

// verifyAndConsumeRegisterEmailCode 校验注册验证码并一次性消费。
//
// 返回值：true 表示校验通过（调用方可继续注册）；false 表示已写出错误响应，
// 调用方必须立即 return —— 这样把"错误响应只写一次"的责任收敛在本函数内，
// 避免调用方重复写响应导致响应体拼接错乱。
func (s *Server) verifyAndConsumeRegisterEmailCode(c *gin.Context, email, code string) bool {
	ctx := c.Request.Context()

	if email == "" {
		writeUserError(c, http.StatusBadRequest,
			"email.required", oai.TypeInvalidRequest, "email_required")
		return false
	}
	if code == "" {
		writeUserError(c, http.StatusBadRequest,
			"email.code_required", oai.TypeInvalidRequest, "email_code_required")
		return false
	}

	record, err := s.deps.EmailCodes.LatestActive(ctx, email, model.EmailCodePurposeRegister)
	if err != nil {
		if errors.Is(err, model.ErrEmailCodeNotFound) {
			writeUserError(c, http.StatusBadRequest,
				"email.code_invalid", oai.TypeInvalidRequest, "email_code_invalid")
			return false
		}
		s.respondInternalError(c, "查询验证码失败")
		return false
	}

	now := time.Now()
	if record.IsExpired(now) {
		writeUserError(c, http.StatusBadRequest,
			"email.code_expired", oai.TypeInvalidRequest, "email_code_expired")
		return false
	}

	// 失败次数上限：6 位数字共 100 万种组合，限次后在线穷举不可行
	if record.Attempts >= model.EmailCodeMaxAttempts {
		writeUserError(c, http.StatusTooManyRequests,
			"email.code_attempts_exceeded", oai.TypeRateLimit, "email_code_attempts_exceeded")
		return false
	}

	if !model.VerifyEmailCode(record, code) {
		// 累加失败次数；累加失败不阻断本次错误提示（用户看到的仍是"验证码错误"）
		_ = s.deps.EmailCodes.IncreaseAttempts(ctx, record.ID)
		remaining := model.EmailCodeMaxAttempts - record.Attempts - 1
		if remaining < 0 {
			remaining = 0
		}
		writeUserError(c, http.StatusBadRequest,
			"email.code_mismatch", oai.TypeInvalidRequest, "email_code_mismatch", remaining)
		return false
	}

	// 一次性消费：并发提交同一验证码时，只有一条 UPDATE 会生效
	if err := s.deps.EmailCodes.Consume(ctx, record.ID, now); err != nil {
		if errors.Is(err, model.ErrEmailCodeNotFound) {
			writeUserError(c, http.StatusBadRequest,
				"email.code_used", oai.TypeInvalidRequest, "email_code_used")
			return false
		}
		s.respondInternalError(c, "消费验证码失败")
		return false
	}

	return true
}
