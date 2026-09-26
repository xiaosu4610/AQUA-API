// 本文件实现充值 / 支付相关的对外接口。
//
// 意图（Why）：
//
//	把"下单 → 支付 → 回调 → 入账"这条链路完整暴露出来，并保证三件事：
//	  1) 下单金额与额度由服务端计算（客户端传来的金额一律不信）；
//	  2) 回调必须验签 + 比对金额（见 internal/payment 的说明）；
//	  3) 入账必须幂等（重复回调不重复给额度）。
//
// 三类接口的分工：
//
//	公开（无需登录）：GET  /api/payment/public      —— 充值页需要的通道与限额
//	用户（需登录）  ：POST /api/user/orders         —— 下单
//	                GET  /api/user/orders         —— 我的充值记录
//	                GET  /api/user/orders/{no}    —— 单笔状态（前端轮询支付结果）
//	管理（需管理员）：GET  /api/admin/orders       —— 全部订单
//	                POST /api/admin/orders/{no}/mark-paid —— 人工确认入账
//	                POST /api/admin/orders/{no}/close     —— 关闭订单
//	                POST /api/admin/orders/{no}/refund    —— 退款（扣回额度）
//	回调（无需登录）：POST /api/payments/{method}/notify
//
// 扩展（Extend）：
//
//	新增支付通道：在 internal/payment 实现 Provider 并在 NewRegistry 注册，
//	本文件与路由都无需改动（回调路径已按 {method} 通配）。
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/payment"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
)

// maxNotifyBodyBytes 是支付回调请求体的读取上限。
//
// 取 256 KiB：Stripe 事件体通常几 KB，留足余量即可；
// 上限的作用是防止有人拿超大 body 打这个"无需鉴权"的公开接口。
const maxNotifyBodyBytes = 256 << 10

// orderDTO 是充值订单的对外表示。
type orderDTO struct {
	TradeNo    string `json:"trade_no"`
	Amount     int64  `json:"amount_cents"`
	AmountText string `json:"amount_text"`
	Currency   string `json:"currency"`
	Quota      int64  `json:"quota"`
	Method     string `json:"method"`
	SubMethod  string `json:"sub_method"`
	Status     int    `json:"status"`
	StatusText string `json:"status_text"`
	PayURL     string `json:"pay_url"`
	Remark     string `json:"remark"`
	UserID     uint64 `json:"user_id"`
	Credited   bool   `json:"credited"`
	CreatedAt  int64  `json:"created_at"`
	PaidAt     int64  `json:"paid_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

// toOrderDTO 把领域模型转为对外 DTO。
//
// 刻意不输出 provider_trade_no 与 notify_payload：前者是第三方标识，
// 对使用者无意义；后者是回调原文，可能包含第三方内部信息。
func toOrderDTO(order *model.PaymentOrder) orderDTO {
	if order == nil {
		return orderDTO{}
	}
	return orderDTO{
		TradeNo:    order.TradeNo,
		Amount:     order.Amount,
		AmountText: order.AmountYuan(),
		Currency:   order.Currency,
		Quota:      order.Quota,
		Method:     order.Method,
		SubMethod:  order.SubMethod,
		Status:     int(order.Status),
		StatusText: order.Status.String(),
		PayURL:     order.PayURL,
		Remark:     order.Remark,
		UserID:     order.UserID,
		Credited:   order.IsCredited(),
		CreatedAt:  unixOrZero(order.CreatedAt),
		PaidAt:     unixOrZero(order.PaidAt),
		ExpiresAt:  unixOrZero(order.ExpiresAt),
	}
}

// handlePublicPaymentInfo 处理 GET /api/payment/public（无需登录）。
//
// 用途：登录页/充值页需要在用户登录前就能显示"充值是否开放、充值比例是多少"，
// 因此这个接口不要求鉴权，且只暴露非敏感参数。
func (s *Server) handlePublicPaymentInfo(c *gin.Context) {
	settings, err := model.LoadSiteSettings(c.Request.Context(), s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取支付设置失败")
		return
	}

	methods := make([]gin.H, 0, len(settings.Payment.Methods))
	for _, method := range s.deps.Payment.Names() {
		if !settings.Payment.MethodEnabled(method) {
			continue
		}
		methods = append(methods, gin.H{
			"name":  method,
			"label": paymentMethodLabel(method),
			"ready": s.paymentMethodReady(method),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"enabled":        settings.Payment.Enabled,
		"methods":        methods,
		"exchange_rate":  settings.Payment.ExchangeRate,
		"currency":       settings.Payment.CurrencyOrDefault(),
		"min_cents":      settings.Payment.MinCents,
		"max_cents":      settings.Payment.MaxCents,
		"max_quota_auto": settings.Payment.ExchangeRate,
	})
}

// paymentMethodReady 判断某通道的密钥是否已就绪（只返回布尔值，不回传密钥）。
func (s *Server) paymentMethodReady(method string) bool {
	switch method {
	case model.PaymentMethodEPay:
		return strings.TrimSpace(s.deps.Config.Payment.EPayKey) != ""
	case model.PaymentMethodStripe:
		return strings.TrimSpace(s.deps.Config.Payment.StripeSecretKey) != "" &&
			strings.TrimSpace(s.deps.Config.Payment.StripeWebhookSecret) != ""
	case model.PaymentMethodManual:
		return true
	default:
		return false
	}
}

// paymentMethodLabel 返回通道的中文名。
func paymentMethodLabel(method string) string {
	switch method {
	case model.PaymentMethodEPay:
		return "在线支付"
	case model.PaymentMethodStripe:
		return "Stripe"
	case model.PaymentMethodManual:
		return "人工确认"
	default:
		return method
	}
}

// createOrderRequest 是下单请求体。
//
// 重要：只接受"金额"与"通道"，绝不接受客户端传来的 quota——
// 额度必须由服务端按当前兑换比例计算，否则用户可以自己填一个天文数字。
type createOrderRequest struct {
	AmountCents int64  `json:"amount_cents"`
	Method      string `json:"method"`
	SubMethod   string `json:"sub_method"`
	Remark      string `json:"remark"`
}

// handleCreateOrder 处理 POST /api/user/orders（下单）。
func (s *Server) handleCreateOrder(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"未登录", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}

	var req createOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	settings, err := model.LoadSiteSettings(ctx, s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取支付设置失败")
		return
	}
	pay := settings.Payment

	if !pay.Enabled {
		oai.WriteError(c.Writer, http.StatusForbidden,
			"本站未开放充值", oai.TypePermission, "payment_disabled")
		return
	}
	if req.AmountCents < pay.MinCents {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			fmt.Sprintf("单笔充值不能少于 %s 元", model.FormatCents(pay.MinCents)),
			oai.TypeInvalidRequest, "amount_too_small")
		return
	}
	if pay.MaxCents > 0 && req.AmountCents > pay.MaxCents {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			fmt.Sprintf("单笔充值不能超过 %s 元", model.FormatCents(pay.MaxCents)),
			oai.TypeInvalidRequest, "amount_too_large")
		return
	}

	method := strings.TrimSpace(req.Method)
	if !pay.MethodEnabled(method) {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"该支付方式未启用", oai.TypeInvalidRequest, "payment_method_disabled")
		return
	}
	provider, err := s.deps.Payment.Get(method)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "payment_method_unknown")
		return
	}

	quota := pay.AmountToQuota(req.AmountCents)
	if quota <= 0 {
		// 兑换比例配置错误时不应产生"付钱但到账 0 额度"的订单
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"充值兑换比例未正确配置，请联系管理员", oai.TypeServer, "payment_misconfigured")
		return
	}

	ttl := pay.OrderTTLMinutes
	if ttl <= 0 {
		ttl = 30
	}

	tradeNo, err := model.GenerateTradeNo()
	if err != nil {
		s.respondInternalError(c, "生成订单号失败")
		return
	}

	order := &model.PaymentOrder{
		TradeNo:   tradeNo,
		UserID:    user.ID,
		Amount:    req.AmountCents,
		Currency:  pay.CurrencyOrDefault(),
		Quota:     quota,
		Method:    method,
		SubMethod: strings.TrimSpace(req.SubMethod),
		Status:    model.PaymentStatusPending,
		Remark:    truncateRunes(strings.TrimSpace(req.Remark), 200),
		ExpiresAt: time.Now().Add(time.Duration(ttl) * time.Minute),
	}

	base := s.notifyBase(c, pay)
	result, err := provider.Create(ctx, &payment.Request{
		Order:     order,
		Subject:   fmt.Sprintf("充值 %s %s（%s）", order.AmountYuan(), order.Currency, user.Username),
		NotifyURL: base + "/api/payments/" + method + "/notify",
		ReturnURL: base + "/console/recharge?trade_no=" + tradeNo,
		ClientIP:  c.ClientIP(),
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	order.PayURL = result.PayURL
	order.ProviderTradeNo = result.ProviderTradeNo

	if err := s.deps.Orders.Create(ctx, order); err != nil {
		s.respondInternalError(c, "保存订单失败")
		return
	}

	c.JSON(http.StatusOK, toOrderDTO(order))
}

// notifyBase 返回回调基址。
//
// 优先使用后台配置的 NotifyBase：网关自身看到的 Host 可能是内网地址
// 或反向代理后的地址，而支付平台必须能回调到公网地址。
// 未配置时按请求推导（适合直接用公网域名访问的简单部署）。
func (s *Server) notifyBase(c *gin.Context, pay model.PaymentSettings) string {
	if base := strings.TrimSpace(pay.NotifyBase); base != "" {
		return strings.TrimRight(base, "/")
	}

	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	// 反向代理场景下真实协议在 X-Forwarded-Proto 里
	if forwarded := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}
	return scheme + "://" + c.Request.Host
}

// handleMyListOrders 处理 GET /api/user/orders（我的充值记录）。
func (s *Server) handleMyListOrders(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"未登录", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}
	s.respondOrderList(c, model.PaymentOrderQuery{UserID: user.ID})
}

// handleMyGetOrder 处理 GET /api/user/orders/{tradeNo}。
//
// 前端在用户支付完成后轮询这个接口以确认到账；
// 归属校验不可省——否则凭订单号就能看到他人的充值金额。
func (s *Server) handleMyGetOrder(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"未登录", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}

	order, err := s.deps.Orders.GetByTradeNo(c.Request.Context(), c.Param("tradeNo"))
	if err != nil {
		writeOrderLookupError(c, err)
		return
	}
	if order.UserID != user.ID {
		writeOrderNotFound(c)
		return
	}
	c.JSON(http.StatusOK, toOrderDTO(order))
}

// handleAdminListOrders 处理 GET /api/admin/orders。
func (s *Server) handleAdminListOrders(c *gin.Context) {
	query := model.PaymentOrderQuery{Method: strings.TrimSpace(c.Query("method"))}

	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			query.UserID = parsed
		}
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			status := model.PaymentStatus(parsed)
			query.Status = &status
		}
	}
	s.respondOrderList(c, query)
}

// respondOrderList 输出订单列表（分页）。
func (s *Server) respondOrderList(c *gin.Context, query model.PaymentOrderQuery) {
	page, size, offset := parsePagination(c)
	query.Limit = size
	query.Offset = offset

	ctx := c.Request.Context()
	total, err := s.deps.Orders.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计订单数失败")
		return
	}
	orders, err := s.deps.Orders.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询订单列表失败")
		return
	}

	items := make([]orderDTO, 0, len(orders))
	for _, order := range orders {
		items = append(items, toOrderDTO(order))
	}
	c.JSON(http.StatusOK, newPagedResponse(items, int(total), page, size))
}

// handleAdminMarkOrderPaid 处理 POST /api/admin/orders/{tradeNo}/mark-paid。
//
// 用途：人工确认通道的核心动作（收到款后手动入账）。
// 也用于处理"用户确实付了款但回调丢失"的异常，因此必须走同一套幂等入账逻辑。
func (s *Server) handleAdminMarkOrderPaid(c *gin.Context) {
	ctx := c.Request.Context()

	order, err := s.deps.Orders.GetByTradeNo(ctx, c.Param("tradeNo"))
	if err != nil {
		writeOrderLookupError(c, err)
		return
	}
	if order.Status == model.PaymentStatusPaid {
		// 已支付：直接走入账补偿（可能正处于"已支付未入账"的中间态）
		if err := s.creditOrder(ctx, order.TradeNo); err != nil {
			s.respondInternalError(c, "入账失败")
			return
		}
	} else if order.Status != model.PaymentStatusPending {
		oai.WriteError(c.Writer, http.StatusConflict,
			"仅待支付订单可以确认入账（当前状态："+order.Status.String()+"）",
			oai.TypeInvalidRequest, "order_not_pending")
		return
	} else {
		if _, err := s.deps.Orders.MarkPaid(ctx, order.TradeNo, "", "管理员手动确认", time.Now()); err != nil {
			s.respondInternalError(c, "标记订单已支付失败")
			return
		}
		if err := s.creditOrder(ctx, order.TradeNo); err != nil {
			s.respondInternalError(c, "入账失败")
			return
		}
	}

	updated, err := s.deps.Orders.GetByTradeNo(ctx, order.TradeNo)
	if err != nil {
		s.respondInternalError(c, "读取订单失败")
		return
	}
	c.JSON(http.StatusOK, toOrderDTO(updated))
}

// handleAdminCloseOrder 处理 POST /api/admin/orders/{tradeNo}/close。
func (s *Server) handleAdminCloseOrder(c *gin.Context) {
	ctx := c.Request.Context()

	order, err := s.deps.Orders.GetByTradeNo(ctx, c.Param("tradeNo"))
	if err != nil {
		writeOrderLookupError(c, err)
		return
	}
	if order.Status.IsTerminal() {
		oai.WriteError(c.Writer, http.StatusConflict,
			"订单已是终态（"+order.Status.String()+"），无需关闭",
			oai.TypeInvalidRequest, "order_already_closed")
		return
	}

	if err := s.deps.Orders.UpdateStatus(ctx, order.TradeNo, model.PaymentStatusClosed); err != nil {
		s.respondInternalError(c, "关闭订单失败")
		return
	}
	order.Status = model.PaymentStatusClosed
	c.JSON(http.StatusOK, toOrderDTO(order))
}

// handleAdminRefundOrder 处理 POST /api/admin/orders/{tradeNo}/refund。
//
// 语义：把已入账的额度扣回（退款）。这是"用户已用掉额度"时唯一的正确处理方式——
// 不能只改状态，否则账面上用户白拿了额度。
//
// 实现：用 AddUsedQuota 施加负增量（相当于把"已用额度"抵回），
// 从而在不改动总额度的前提下回收额度，历史用量记录保持完整可审计。
func (s *Server) handleAdminRefundOrder(c *gin.Context) {
	ctx := c.Request.Context()

	order, err := s.deps.Orders.GetByTradeNo(ctx, c.Param("tradeNo"))
	if err != nil {
		writeOrderLookupError(c, err)
		return
	}
	if order.Status != model.PaymentStatusPaid {
		oai.WriteError(c.Writer, http.StatusConflict,
			"仅已支付订单可以退款（当前状态："+order.Status.String()+"）",
			oai.TypeInvalidRequest, "order_not_paid")
		return
	}

	// 允许负数入参是刻意的：AddUsedQuota 的语义是"调整已用额度"，
	// 负数即把已用额度抵回，等价于退还。这里用 -quota 表示"退回这笔充值"。
	if order.Quota > 0 {
		if err := s.deps.Users.AddUsedQuota(ctx, order.UserID, -order.Quota); err != nil {
			s.respondInternalError(c, "退还额度失败")
			return
		}
	}
	if err := s.deps.Orders.UpdateStatus(ctx, order.TradeNo, model.PaymentStatusRefunded); err != nil {
		s.respondInternalError(c, "更新订单状态失败")
		return
	}

	order.Status = model.PaymentStatusRefunded
	c.JSON(http.StatusOK, toOrderDTO(order))
}

// handlePaymentNotify 处理 POST /api/payments/{method}/notify（支付平台回调）。
//
// 这是全站唯一无需鉴权、且会产生资损副作用的公开接口，因此每一步都必须严格：
//  1. 读原始 body（限额），并解析表单/查询参数；
//  2. 由通道适配器验签——验签失败一律拒绝；
//  3. 校验订单存在且金额一致；
//  4. 幂等入账（重复回调只入账一次）；
//  5. 按第三方要求应答（易支付必须回 "success"，否则会持续重试）。
//
// 注意：任何分支都【不】向调用者回显内部错误细节——
// 回调接口的错误信息是攻击者最好的调试工具。
func (s *Server) handlePaymentNotify(c *gin.Context) {
	method := strings.TrimSpace(c.Param("method"))
	provider, err := s.deps.Payment.Get(method)
	if err != nil {
		c.String(http.StatusNotFound, "unknown payment method")
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxNotifyBodyBytes+1))
	if err != nil {
		c.String(http.StatusBadRequest, "read body failed")
		return
	}
	if int64(len(body)) > maxNotifyBodyBytes {
		c.String(http.StatusRequestEntityTooLarge, "body too large")
		return
	}

	form := url.Values{}
	if err := c.Request.ParseForm(); err == nil {
		form = c.Request.PostForm
	}

	result, err := provider.ParseNotify(c.Request.Context(), &payment.Notify{
		Header: c.Request.Header,
		Query:  c.Request.URL.Query(),
		Form:   form,
		Body:   body,
	})
	if err != nil {
		// 验签失败/不支持回调：明确拒绝，但不回显原因
		c.String(http.StatusBadRequest, "invalid notify")
		return
	}
	if !result.Paid {
		// 非"支付成功"的事件（如退款通知）：应答成功但不动账
		s.ackNotify(c, result)
		return
	}

	ctx := c.Request.Context()
	order, err := s.deps.Orders.GetByTradeNo(ctx, result.TradeNo)
	if err != nil {
		// 订单不存在：可能是伪造的回调，也可能是发到了错误的部署实例。
		// 无论哪种都不应入账，但应答 200 以避免第三方无限重试（没有意义的重试）。
		c.String(http.StatusOK, "order not found")
		return
	}

	// 金额校验：验签只证明"请求来自支付平台"，不排除金额被篡改
	if result.AmountCents > 0 && result.AmountCents != order.Amount {
		// 不回显两个金额：这会帮助攻击者推断出订单金额
		c.String(http.StatusBadRequest, "amount mismatch")
		return
	}

	if _, err := s.deps.Orders.MarkPaid(ctx, order.TradeNo, result.ProviderTradeNo, string(body), time.Now()); err != nil {
		s.respondInternalError(c, "更新订单状态失败")
		return
	}
	if err := s.creditOrder(ctx, order.TradeNo); err != nil {
		// 入账失败：返回 500 让支付平台重试——重试会再次走到这里，
		// 而 CreditOrder 是幂等的，不会重复给额度。
		c.String(http.StatusInternalServerError, "credit failed")
		return
	}

	s.ackNotify(c, result)
}

// ackNotify 按通道要求应答回调。
func (s *Server) ackNotify(c *gin.Context, result *payment.NotifyResult) {
	contentType := result.AckContentType
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	ackBody := result.AckBody
	if ackBody == "" {
		ackBody = "success"
	}
	c.Data(http.StatusOK, contentType, []byte(ackBody))
}

// creditOrder 执行幂等入账。
//
// 为什么单独抽出来：人工确认、在线回调、启动补偿三条路径都要入账，
// 三者必须走完全相同的逻辑，否则必然出现"某条路径漏了幂等"的重复给钱缺陷。
func (s *Server) creditOrder(ctx context.Context, tradeNo string) error {
	_, err := s.deps.Orders.CreditOrder(ctx, tradeNo, time.Now())
	return err
}

// writePaymentError 把支付通道错误映射为 HTTP 响应。
func writePaymentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, payment.ErrProviderUnknown):
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "payment_method_unknown")
	case errors.Is(err, payment.ErrProviderDisabled):
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "payment_method_disabled")
	case errors.Is(err, payment.ErrNotConfigured):
		// 配置问题属于服务端问题：明确告诉使用者"通道还没配好"，
		// 而不是让他反复尝试而不知原因。
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, err.Error(),
			oai.TypeServer, "payment_not_configured")
	case errors.Is(err, payment.ErrUnsupportedNotify):
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "payment_notify_unsupported")
	default:
		oai.WriteError(c.Writer, http.StatusBadGateway,
			"支付通道请求失败，请稍后重试", oai.TypeServer, "payment_upstream_failed")
	}
}

// writeOrderLookupError 处理"按订单号查询失败"的统一响应。
func writeOrderLookupError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrPaymentOrderNotFound) {
		writeOrderNotFound(c)
		return
	}
	oai.WriteError(c.Writer, http.StatusInternalServerError,
		"网关内部错误", oai.TypeServer, oai.CodeInternal)
}

// writeOrderNotFound 统一返回 404（不区分"不存在"与"不属于你"）。
func writeOrderNotFound(c *gin.Context) {
	oai.WriteError(c.Writer, http.StatusNotFound,
		"订单不存在", oai.TypeInvalidRequest, "order_not_found")
}

// truncateRunes 按字符截断文本（避免把超长备注写进数据库）。
func truncateRunes(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}
