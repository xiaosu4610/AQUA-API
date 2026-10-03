// 本文件实现「限时试用额度」的后台发放接口与门户查询接口。
//
// 意图（Why）：
//
//	试用额是一次**给全体用户加钱**的批量写操作，比普通 CRUD 更需要克制：
//	  1) 发出去就进了用户余额，只能等 24 小时到期才收得回；
//	  2) 误点两次 = 全站发双份。因此接口要求显式 confirm，
//	     并且必须带一个**批次标识**——同一批次只允许发放一次（由唯一索引兜底）。
//
//	金额口径刻意用「分」而不是额度：站长按"1 毛钱"思考，而不是按"100000 额度"。
//	分 → 额度的换算复用充值那套比例（payment_exchange_rate），
//	保证"发放 0.1 元"与"充值 0.1 元"在站内是同一个数值。
//
// 流转（Flow）：
//
//	后台界面 / 运维脚本 → POST /api/admin/trial-grants
//	  → 校验（确认、批次、金额、时长）→ TrialGrants.GrantAll
//	  → 台账落行 + users.quota 增加；到期由后台协程回收
//
//	用户门户 → GET /api/user/trial → TrialGrants.ActiveFor
//	  → 返回"还剩多少额度、几时过期"，由前端折算成人民币展示
//
// 扩展（Extend）：
//
//	新增发放对象（如只发给新用户）时，在仓储的发放 SQL 上加条件即可，
//	本文件的校验与返回结构无需改动。
package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
	"github.com/LTZY-ACU/ltzy-api/internal/server/middleware"
)

// trialGrantMaxHours 是有效的可配置时长上限（30 天）。
//
// 为什么要设上限：这本质是"限时"额度，时长填错一个数量级（比如 2400 小时）
// 会让这笔钱事实上变成永久额度，且要等很久才收得回。宁可让调用方显式改代码。
const trialGrantMaxHours = 24 * 30

// trialGrantRequest 是「给全站用户发放试用额」的请求体。
type trialGrantRequest struct {
	// AmountCents 是每位用户获得的金额（单位：分）。0.1 元填 10。
	AmountCents int64 `json:"amount_cents"`
	// Hours 是自发放时刻起的有效小时数（1..720）。
	Hours int `json:"hours"`
	// Batch 是批次标识，不可为空；同一批次只允许发放一次（防重复发放）。
	Batch string `json:"batch"`
	// Confirm 必须显式为 true：发放即入用户余额，且只能等到期才收回。
	Confirm bool `json:"confirm"`
}

// trialGrantResultDTO 是发放结果的对外表示。
type trialGrantResultDTO struct {
	Batch      string `json:"batch"`
	Recipients int64  `json:"recipients"`
	// AmountQuota 是每人获得的额度（内部单位）。
	AmountQuota int64 `json:"amount_quota"`
	// AmountCents 是每人获得的金额（分），由请求原样回显便于核对。
	AmountCents int64 `json:"amount_cents"`
	Hours       int   `json:"hours"`
	ExpiresAt   int64 `json:"expires_at"`
}

// trialActiveDTO 是门户展示的试用额状态。
//
// 恒为非 nil：前端可直接读 active 判断要不要显示横幅，不必判空。
type trialActiveDTO struct {
	Active bool `json:"active"`
	// Remaining 是仍可用的额度（内部单位）；由前端按兑换比例折算成人民币展示。
	Remaining int64 `json:"remaining"`
	// ExpiresAt 是到期时间（unix 秒）；active=false 时为 0。
	ExpiresAt int64 `json:"expires_at"`
	// ExpiresInSeconds 是距到期的剩余秒数，便于前端直接渲染倒计时。
	ExpiresInSeconds int64 `json:"expires_in_seconds"`
}

// handleGrantTrial 处理 POST /api/admin/trial-grants。
func (s *Server) handleGrantTrial(c *gin.Context) {
	var req trialGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	if !req.Confirm {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"发放试用额会直接加到全站用户余额且只能等到期收回，请确认金额与批次后显式确认",
			oai.TypeInvalidRequest, "trial_grant_confirm_required")
		return
	}
	if s.deps.TrialGrants == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"试用额模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	ctx := c.Request.Context()

	// 分 → 额度：复用充值口径，保证"发放 0.1 元"与"充值 0.1 元"是同一个数值。
	settings, err := model.LoadSiteSettings(ctx, s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取站点设置失败")
		return
	}
	amountQuota := settings.Payment.AmountToQuota(req.AmountCents)
	if amountQuota <= 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"发放金额过小或站点未配置兑换比例（需先在「系统设置 → 支付」填写 1 元等于多少额度）",
			oai.TypeInvalidRequest, "trial_grant_amount_too_small")
		return
	}
	// 时长上限可在后台「运行上限」页调整（默认 720 小时 = 30 天）。
	maxHours := s.limitSettingsCached(ctx).TrialGrantMaxHours
	if req.Hours <= 0 || req.Hours > maxHours {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			fmt.Sprintf("有效时长需在 1 到 %d 小时之间", maxHours),
			oai.TypeInvalidRequest, "trial_grant_invalid_hours")
		return
	}
	if strings.TrimSpace(req.Batch) == "" {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"请填写批次标识（同一批次只会发放一次，用于防止误发双份）",
			oai.TypeInvalidRequest, "trial_grant_batch_required")
		return
	}

	result, err := s.deps.TrialGrants.GrantAll(ctx, model.TrialGrantRequest{
		Batch:  req.Batch,
		Amount: amountQuota,
		TTL:    time.Duration(req.Hours) * time.Hour,
	})
	if err != nil {
		if errors.Is(err, model.ErrTrialBatchExists) {
			oai.WriteError(c.Writer, http.StatusConflict,
				"该批次已经发放过了，未重复发放",
				oai.TypeInvalidRequest, "trial_grant_batch_exists")
			return
		}
		if errors.Is(err, model.ErrTrialGrantInvalid) {
			oai.WriteError(c.Writer, http.StatusBadRequest,
				"发放参数不合法：批次标识不能为空、金额与时长必须为正",
				oai.TypeInvalidRequest, "trial_grant_invalid")
			return
		}
		s.respondInternalError(c, "发放试用额失败")
		return
	}
	if result.Recipients == 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"没有任何符合条件的用户（仅对「启用且额度非不限」的用户发放），本次未发放",
			oai.TypeInvalidRequest, "trial_grant_no_recipients")
		return
	}

	c.JSON(http.StatusOK, trialGrantResultDTO{
		Batch:       result.Batch,
		Recipients:  result.Recipients,
		AmountQuota: result.Amount,
		AmountCents: req.AmountCents,
		Hours:       req.Hours,
		ExpiresAt:   unixOrZero(result.ExpiresAt),
	})
}

// handleMyTrialGrant 处理 GET /api/user/trial。
//
// 只回答"我现在还有没有试用额、还剩多少、几时过期"，
// 不返回发放历史（那是后台的事，用户不需要知道发了几批）。
func (s *Server) handleMyTrialGrant(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		writeUserError(c, http.StatusUnauthorized,
			"auth.not_logged_in", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}
	if s.deps.TrialGrants == nil {
		// 模块未启用时返回"没有试用额"而不是报错：前端横幅据此不展示即可。
		c.JSON(http.StatusOK, trialActiveDTO{})
		return
	}

	now := time.Now()
	active, err := s.deps.TrialGrants.ActiveFor(c.Request.Context(), user.ID, now)
	if err != nil {
		s.respondInternalError(c, "查询试用额失败")
		return
	}
	if !active.IsActive() {
		c.JSON(http.StatusOK, trialActiveDTO{})
		return
	}
	c.JSON(http.StatusOK, trialActiveDTO{
		Active:           true,
		Remaining:        active.Remaining,
		ExpiresAt:        active.ExpiresAt.Unix(),
		ExpiresInSeconds: int64(active.ExpiresAt.Sub(now).Seconds()),
	})
}
