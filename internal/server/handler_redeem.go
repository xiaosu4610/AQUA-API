// 本文件实现「兑换码」的后台管理接口与用户兑换接口。
//
// 意图（Why）：
//
//	兑换码是运营最常用的"低成本发额度"手段：管理员批量生成、分发，
//	用户在门户输入即可领取额度。本文件把这条链路完整暴露出来，并守住两个底线：
//	  1) 生成与清理对管理员透明——生成时返回本批的全部码（便于导出分发），
//	     清理时可一键移除已使用/已过期数据；
//	  2) 兑换失败必须给出【精确】原因（不存在 / 已使用 / 已过期 / 已作废），
//	     含糊的"无效码"会让用户反复重试并引发无效客服沟通。
//
// 流转（Flow）：
//
//	后台：RedeemCodesView → GET/POST/PUT/DELETE /api/admin/redeem-codes[...]
//	门户：兑换页 → POST /api/user/redeem → Repository.Redeem → 返回本次额度与最新额度
//
// 扩展（Extend）：
//
//	新增筛选维度或字段时：在 model.RedeemCodeQuery / DTO / 请求体三处同步补充。
//	新增失败语义时：在 model 层加哨兵错误，并在 writeRedeemError 中补一条映射。
package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
)

// 兑换码批量生成的约束。
const (
	// maxRedeemBatchSize 是单次生成的兑换码数量上限。
	//
	// 取 500：既能覆盖常见的活动批量发放，又能防止误填数量导致一次写出海量数据。
	// 需要更多时应分多批生成——批次号也正是为此而设。
	maxRedeemBatchSize = 500
	// maxRedeemExpiresDays 是有效期上限（约 10 年），用于拦下明显填错单位的输入。
	maxRedeemExpiresDays = 3650
)

// redeemCodeDTO 是兑换码的对外表示。
type redeemCodeDTO struct {
	ID         uint64 `json:"id"`
	Code       string `json:"code"`
	Quota      int64  `json:"quota"`
	Status     int    `json:"status"`
	StatusText string `json:"status_text"`
	ExpiresAt  int64  `json:"expires_at"`
	Expired    bool   `json:"expired"`
	UsedBy     uint64 `json:"used_by"`
	UsedAt     int64  `json:"used_at"`
	BatchNo    string `json:"batch_no"`
	Remark     string `json:"remark"`
	CreatedAt  int64  `json:"created_at"`
}

// toRedeemCodeDTO 把领域模型转为对外 DTO。
func toRedeemCodeDTO(code *model.RedeemCode) redeemCodeDTO {
	if code == nil {
		return redeemCodeDTO{}
	}
	return redeemCodeDTO{
		ID:         code.ID,
		Code:       code.Code,
		Quota:      code.Quota,
		Status:     int(code.Status),
		StatusText: code.Status.String(),
		ExpiresAt:  unixOrZero(code.ExpiresAt),
		Expired:    code.IsExpired(time.Now()),
		UsedBy:     code.UsedBy,
		UsedAt:     unixOrZero(code.UsedAt),
		BatchNo:    code.BatchNo,
		Remark:     code.Remark,
		CreatedAt:  unixOrZero(code.CreatedAt),
	}
}

// ---------------------------------------------------------------------------
// 后台：兑换码管理
// ---------------------------------------------------------------------------

// handleAdminListRedeemCodes 处理 GET /api/admin/redeem-codes（分页 + 筛选）。
func (s *Server) handleAdminListRedeemCodes(c *gin.Context) {
	if s.deps.RedeemCodes == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"兑换码模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	query := model.RedeemCodeQuery{
		Keyword: strings.TrimSpace(c.Query("keyword")),
		BatchNo: strings.TrimSpace(c.Query("batch_no")),
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			status := model.RedeemStatus(parsed)
			query.Status = &status
		}
	}

	page, size, offset := parsePagination(c)
	query.Limit = size
	query.Offset = offset

	items, total, err := s.deps.RedeemCodes.List(c.Request.Context(), query)
	if err != nil {
		s.respondInternalError(c, "查询兑换码列表失败")
		return
	}

	dtos := make([]redeemCodeDTO, 0, len(items))
	for _, item := range items {
		dtos = append(dtos, toRedeemCodeDTO(item))
	}
	c.JSON(http.StatusOK, newPagedResponse(dtos, total, page, size))
}

// createRedeemCodesRequest 是批量生成兑换码的请求体。
type createRedeemCodesRequest struct {
	Count int   `json:"count"`
	Quota int64 `json:"quota"`
	// ExpiresDays 为有效期（天）；0 表示永不过期。
	ExpiresDays int    `json:"expires_days"`
	Remark      string `json:"remark"`
	// BatchNo 可选；留空则服务端自动生成。
	BatchNo string `json:"batch_no"`
}

// handleAdminCreateRedeemCodes 处理 POST /api/admin/redeem-codes（批量生成）。
//
// 返回本批生成的全部兑换码：管理员的下一步操作必然是"把这些码导出分发给用户"，
// 因此不能只返回数量，必须把码本身带回来。
func (s *Server) handleAdminCreateRedeemCodes(c *gin.Context) {
	if s.deps.RedeemCodes == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"兑换码模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req createRedeemCodesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	if req.Count <= 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest, "生成数量必须大于 0",
			oai.TypeInvalidRequest, "invalid_count")
		return
	}
	if req.Count > maxRedeemBatchSize {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"单次最多生成 "+strconv.Itoa(maxRedeemBatchSize)+" 张兑换码，请分批生成",
			oai.TypeInvalidRequest, "invalid_count")
		return
	}
	if req.Quota <= 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest, "可兑换额度必须大于 0",
			oai.TypeInvalidRequest, "invalid_quota")
		return
	}
	if req.ExpiresDays < 0 || req.ExpiresDays > maxRedeemExpiresDays {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"有效期天数应在 0（永不过期）到 "+strconv.Itoa(maxRedeemExpiresDays)+" 之间",
			oai.TypeInvalidRequest, "invalid_expires_days")
		return
	}

	batchNo := strings.TrimSpace(req.BatchNo)
	if batchNo == "" {
		generated, err := model.GenerateRedeemBatchNo()
		if err != nil {
			s.respondInternalError(c, "生成批次号失败")
			return
		}
		batchNo = generated
	}

	var expiresAt time.Time
	if req.ExpiresDays > 0 {
		expiresAt = time.Now().AddDate(0, 0, req.ExpiresDays)
	}
	remark := truncateRunes(strings.TrimSpace(req.Remark), 200)

	codes := make([]*model.RedeemCode, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		code, err := model.GenerateRedeemCode()
		if err != nil {
			s.respondInternalError(c, "生成兑换码失败")
			return
		}
		codes = append(codes, &model.RedeemCode{
			Code:      code,
			Quota:     req.Quota,
			Status:    model.RedeemStatusUnused,
			ExpiresAt: expiresAt,
			BatchNo:   batchNo,
			Remark:    remark,
		})
	}

	if err := s.deps.RedeemCodes.CreateBatch(c.Request.Context(), codes); err != nil {
		s.respondInternalError(c, "保存兑换码失败")
		return
	}

	dtos := make([]redeemCodeDTO, 0, len(codes))
	for _, code := range codes {
		dtos = append(dtos, toRedeemCodeDTO(code))
	}
	c.JSON(http.StatusOK, gin.H{
		"batch_no": batchNo,
		"count":    len(dtos),
		"items":    dtos,
	})
}

// updateRedeemCodeRequest 是更新兑换码的请求体。
//
// 用指针区分"未提供"与"提供了零值"：只传 remark 时不应把状态改成零值。
type updateRedeemCodeRequest struct {
	Status *int    `json:"status"`
	Remark *string `json:"remark"`
}

// handleAdminUpdateRedeemCode 处理 PUT /api/admin/redeem-codes/{id}（改状态/备注）。
func (s *Server) handleAdminUpdateRedeemCode(c *gin.Context) {
	if s.deps.RedeemCodes == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"兑换码模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req updateRedeemCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	if req.Status == nil && req.Remark == nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "未提供任何可更新字段",
			oai.TypeInvalidRequest, "empty_update")
		return
	}

	ctx := c.Request.Context()
	if req.Status != nil {
		status := model.RedeemStatus(*req.Status)
		if !status.IsValid() {
			oai.WriteError(c.Writer, http.StatusBadRequest, "兑换码状态非法",
				oai.TypeInvalidRequest, "invalid_status")
			return
		}
		if err := s.deps.RedeemCodes.UpdateStatus(ctx, id, status); err != nil {
			writeRedeemAdminError(c, err)
			return
		}
	}
	if req.Remark != nil {
		if err := s.deps.RedeemCodes.UpdateRemark(ctx, id, *req.Remark); err != nil {
			writeRedeemAdminError(c, err)
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleAdminDeleteRedeemCode 处理 DELETE /api/admin/redeem-codes/{id}。
func (s *Server) handleAdminDeleteRedeemCode(c *gin.Context) {
	if s.deps.RedeemCodes == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"兑换码模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	if err := s.deps.RedeemCodes.Delete(c.Request.Context(), id); err != nil {
		writeRedeemAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleAdminDeleteInvalidRedeemCodes 处理 DELETE /api/admin/redeem-codes/invalid。
//
// 用途：一键清理"已使用/已过期"的码，避免列表被历史数据淹没。
func (s *Server) handleAdminDeleteInvalidRedeemCodes(c *gin.Context) {
	if s.deps.RedeemCodes == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"兑换码模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	deleted, err := s.deps.RedeemCodes.DeleteInvalid(c.Request.Context())
	if err != nil {
		s.respondInternalError(c, "清理失效兑换码失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

// writeRedeemAdminError 处理后台操作兑换码时的错误响应。
func writeRedeemAdminError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrRedeemCodeNotFound) {
		oai.WriteError(c.Writer, http.StatusNotFound, "兑换码不存在",
			oai.TypeInvalidRequest, "redeem_code_not_found")
		return
	}
	oai.WriteError(c.Writer, http.StatusInternalServerError,
		"网关内部错误", oai.TypeServer, oai.CodeInternal)
}

// ---------------------------------------------------------------------------
// 门户：用户兑换
// ---------------------------------------------------------------------------

// userRedeemRequest 是用户兑换请求体。
type userRedeemRequest struct {
	Code string `json:"code"`
}

// handleUserRedeem 处理 POST /api/user/redeem（用户输入兑换码领取额度）。
//
// 返回本次获得的额度与用户最新额度：前端需要据此刷新余额展示，
// 单独再查一次用户信息会多一次往返，且存在短暂的不一致窗口。
func (s *Server) handleUserRedeem(c *gin.Context) {
	if s.deps.RedeemCodes == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"兑换码模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	user, ok := middleware.CurrentUser(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"未登录", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}

	var req userRedeemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请填写兑换码",
			oai.TypeInvalidRequest, "missing_redeem_code")
		return
	}

	ctx := c.Request.Context()
	quota, err := s.deps.RedeemCodes.Redeem(ctx, code, int64(user.ID))
	if err != nil {
		s.writeRedeemError(c, err)
		return
	}

	// 读取最新额度用于回显；此处失败不影响兑换结果（额度已入账），仅少一个展示字段
	latest, err := s.deps.Users.GetByID(ctx, user.ID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"quota": quota})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"quota":           quota,
		"total_quota":     latest.Quota,
		"remaining_quota": latest.RemainingQuota(),
	})
}

// writeRedeemError 把兑换失败映射为精确的中文提示与 HTTP 状态码。
//
// 为什么不统一返回"兑换码无效"：不同失败原因的处置方式完全不同
// （重输 / 换一张 / 找客服换新），精确提示能显著减少无效客服沟通。
func (s *Server) writeRedeemError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrRedeemCodeNotFound):
		oai.WriteError(c.Writer, http.StatusNotFound,
			"兑换码不存在，请检查是否输入有误", oai.TypeInvalidRequest, "redeem_code_not_found")
	case errors.Is(err, model.ErrRedeemCodeUsed):
		oai.WriteError(c.Writer, http.StatusConflict,
			"该兑换码已被使用", oai.TypeInvalidRequest, "redeem_code_used")
	case errors.Is(err, model.ErrRedeemCodeExpired):
		oai.WriteError(c.Writer, http.StatusGone,
			"该兑换码已过期", oai.TypeInvalidRequest, "redeem_code_expired")
	case errors.Is(err, model.ErrRedeemCodeVoid):
		oai.WriteError(c.Writer, http.StatusConflict,
			"该兑换码已作废", oai.TypeInvalidRequest, "redeem_code_void")
	default:
		s.respondInternalError(c, "兑换失败")
	}
}
