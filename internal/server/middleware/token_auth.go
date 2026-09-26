// 本文件实现下游令牌的鉴权中间件。
//
// 意图（Why）：
//
//	网关对外的第一道关口：确认"调用者是谁、能不能调用、能调用哪些模型、还有没有额度"。
//	没有鉴权，网关就是一个开放的转发器——任何人拿到地址即可白嫖上游额度，
//	并且所有用量都会记在站长头上。
//
// 流转（Flow）：
//
//	请求 → TokenAuth
//	  ├─ extractAPIKey        从请求头提取令牌明文
//	  ├─ tokens.GetByKey      按摘要索引查库（O(1)，不解密全表）
//	  ├─ EffectiveStatus      结合时间与额度判定令牌自身状态
//	  ├─ users.GetByID        账号级校验：是否禁用、可用额度是否耗尽
//	  ├─ 模型白名单校验        仅当白名单非空时才读请求体（省开销）
//	  ├─ 额度预留             计费模型且额度紧张时预扣额度（额度不足 → 429），按令牌分组计价
//	  └─ SetToken → c.Next()  放行并把令牌、幂等键与分组写入上下文
//
// 扩展（Extend）：
//
//	新增校验维度（IP 白名单、RPM 限制）时：在"模型白名单校验"之后插入新的步骤，
//	  并保持"先廉价判定、后昂贵判定"的顺序（如先查内存/缓存，再读请求体）。
//	新增鉴权方式（如 JWT）：新建同级文件，复用 SetToken 的上下文约定。
//	新增预留的例外情形：改 tryReserveQuota，务必保持"不计费模型跳过预留"
//	  这一条（否则免费模型会被额度墙挡住）。
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
)

// bearerPrefix 是 Authorization 头中令牌的标准前缀。
const bearerPrefix = "Bearer "

// trustQuotaBypassThreshold 是「信任额度旁路」的阈值（内部额度单位）。
//
// 当账号【可用额度】不低于该阈值时跳过预留（不写预留台账、不预扣额度），
// 只做响应后的常规扣费。取舍如下：
//   - 减少写库：额度充足的账号通常占多数，为其每次调用都写一条预留记录
//     会显著放大数据库写压力；
//   - 风险可控：可用额度远大于单次调用成本时，最坏情况的"超支"也不会造成
//     实质资损（这类账号本就额度充足）；
//   - 额度紧张的账号（低于阈值）仍走严格预扣，并发超支漏洞依旧被堵住。
//
// 取值 1_000_000 与"每 1M token 对应的额度"同量级，即"大致够一次百万 token 级调用"。
const trustQuotaBypassThreshold int64 = 1_000_000

// reservationTTL 是预留的在途有效期。
//
// 必须大于上游首字节超时（relay.UpstreamTimeout = 300 秒）并留足余量，
// 否则一个"慢但正常"的请求会在结算前就被当成陈旧预留回收。
const reservationTTL = 15 * time.Minute

// QuotaReserver 是鉴权层做「额度预留」所需的最小能力集，由计费组件（relay.Billing）实现。
//
// 在消费方定义接口（而非依赖具体实现），是为了让本包不依赖 internal/relay，
// 保持"鉴权只关心能否预留，不关心价格怎么算"的分层。
type QuotaReserver interface {
	// EstimateReserve 估算一次调用的预留额度；priced=false 表示该模型不计费（应跳过预留）。
	// group 为本次请求的分组；空字符串表示未指定，由计费组件回退到默认分组。
	EstimateReserve(ctx context.Context, group, modelName string, promptBytes int) (amount int64, priced bool)
	// Reserve 预扣额度；可用额度不足时返回 model.ErrQuotaInsufficient。
	Reserve(ctx context.Context, req model.ReserveRequest) (*model.QuotaReservation, error)
	// PendingReserved 返回某用户在途预留的合计额度（用于计算可用额度）。
	PendingReserved(ctx context.Context, userID uint64) (int64, error)
}

// TokenAuth 返回校验下游令牌的 gin 中间件。
//
// 参数：
//   - tokens 为令牌仓储；
//   - users 为用户仓储（用于账号级额度校验），可为 nil（此时跳过该层校验）；
//   - reservers 为可选的额度预留器（通常传入计费组件）。不传时不做预留，
//     保持"只在响应后扣费"的旧行为——便于测试与"仅统计不限制"的部署形态。
//
// 三者都由 main 装配后注入，便于替换实现与单元测试。
func TokenAuth(tokens model.TokenRepository, users model.UserRepository, reservers ...QuotaReserver) gin.HandlerFunc {
	var reserver QuotaReserver
	if len(reservers) > 0 {
		reserver = reservers[0]
	}

	return func(c *gin.Context) {
		// ── 步骤 1：提取令牌 ────────────────────────────────────
		rawKey := extractAPIKey(c.Request)
		if rawKey == "" {
			abortWithErrorKey(c, http.StatusUnauthorized,
				"auth.missing_token", oai.TypeAuthentication, oai.CodeMissingAPIKey)
			return
		}

		// ── 步骤 2：查库校验令牌是否存在 ────────────────────────
		// 实现上按 key_hash 唯一索引查找，不会解密全表，因此该步骤开销很低。
		token, err := tokens.GetByKey(c.Request.Context(), rawKey)
		if err != nil {
			if errors.Is(err, model.ErrTokenNotFound) {
				// 统一回复"无效"而不区分"不存在"与"格式错误"，
				// 避免向攻击者提供可用于枚举有效令牌的差异信息。
				abortWithErrorKey(c, http.StatusUnauthorized,
					"auth.invalid_token", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
				return
			}
			// 仓储故障：属于网关内部问题，不暴露细节
			abortWithError(c, http.StatusInternalServerError,
				"网关内部错误", oai.TypeServer, oai.CodeInternal)
			return
		}

		// ── 步骤 3：状态判定（结合当前时间与额度）──────────────
		// 注意：这里用 EffectiveStatus 而非 token.Status。
		// 数据库中的 status 只记录管理员意图（启用/手动禁用），
		// "是否过期""额度是否耗尽"是随时间变化的事实，必须实时计算，
		// 否则会出现"令牌已过期但仍可调用"的安全漏洞。
		now := time.Now()
		switch token.EffectiveStatus(now) {
		case model.TokenStatusDisabled:
			abortWithErrorKey(c, http.StatusForbidden,
				"auth.token_disabled", oai.TypePermission, oai.CodeTokenDisabled)
			return
		case model.TokenStatusExpired:
			abortWithErrorKey(c, http.StatusUnauthorized,
				"auth.token_expired", oai.TypeAuthentication, oai.CodeTokenExpired)
			return
		case model.TokenStatusExhausted:
			// 429 而非 403：客户端按"稍后重试/更换令牌"处理更自然
			abortWithErrorKey(c, http.StatusTooManyRequests,
				"quota.token_exhausted", oai.TypeRateLimit, oai.CodeInsufficientQuota)
			return
		}

		// 解析令牌的分组：令牌可指定走某个分组的渠道、按该分组的价格与倍率计费。
		//
		// 为空表示令牌未指定分组，由转发层与计费层各自回退到默认分组——
		// 中间件不感知"默认分组"是什么，只负责把令牌自身的分组原样传下去，
		// 保持"只传递、不判断"的职责（路由与计费规则都不该出现在鉴权层）。
		tokenGroup := token.EffectiveGroupName("")

		// ── 步骤 4：账号级额度校验 ──────────────────────────────
		//
		// 为什么令牌之外还要查用户额度：令牌是"发给某个用户的凭据"，
		// 用户额度才是账号级上限。若只校验令牌额度，用户可以随手新建
		// 若干个令牌来绕过总量限制——限额就形同虚设。
		//
		// 可用额度 = 总额度 − 已用 − 在途预留（而不是只看"总额度 − 已用"）。
		// 必须减去在途预留：并发的多个请求会读到同一个 used_quota，
		// 若只看"总额度 − 已用"就会全部通过、各自扣费，最终"已用"超过"总额度"。
		//
		// 两个容易写错的地方（都曾真实踩过）：
		//  1) 必须显式排除「不限额度」：它的 RemainingQuota() 返回 -1，
		//     若直接拿去比较大小，会把所有不限额度的账号全部拦死；
		//  2) 判定要用 <= 0 而不是 == 0：已用超过总额度时剩余为负数，
		//     只判 0 会把"已经超额"的账号放行。
		var (
			owner   *model.User
			pending int64
		)
		if users != nil && token.OwnerID > 0 {
			owner, err = users.GetByID(c.Request.Context(), token.OwnerID)
			if err != nil {
				if errors.Is(err, model.ErrUserNotFound) {
					// 令牌归属的用户已被删除：令牌本身应视为失效
					abortWithErrorKey(c, http.StatusUnauthorized,
						"auth.token_revoked", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
					return
				}
				abortWithError(c, http.StatusInternalServerError,
					"网关内部错误", oai.TypeServer, oai.CodeInternal)
				return
			}

			if !owner.IsActive() {
				abortWithErrorKey(c, http.StatusForbidden,
					"auth.account_disabled", oai.TypePermission, oai.CodeTokenDisabled)
				return
			}

			// 统计在途预留：仅在"有限额度 + 启用了预留"时才需要查库。
			if reserver != nil && owner.Quota != model.QuotaUnlimited {
				if p, perr := reserver.PendingReserved(c.Request.Context(), owner.ID); perr == nil {
					pending = p
				} else {
					// 查询失败不阻断：降级为"只看已用额度"判定，并留下日志。
					slog.Warn("查询在途预留失败，本次按已用额度判定",
						"error", perr, "user_id", owner.ID)
				}
			}

			if owner.Quota != model.QuotaUnlimited && owner.AvailableQuota(pending) <= 0 {
				// 报错里带上具体数值：使用者转述给站长时，"额度 0 / 已用 0"
				// 一眼就能定位到是"默认额度没配"，而不是"上游限流"。
				abortWithErrorKey(c, http.StatusTooManyRequests,
					"quota.account_exhausted", oai.TypeRateLimit, oai.CodeInsufficientQuota,
					owner.Quota, owner.UsedQuota, pending)
				return
			}
		}

		// ── 步骤 5：模型白名单校验 + 额度预留 ───────────────────
		//
		// 两件事都需要模型名，因此这里按需读取请求体并复用同一次解析：
		//   - 白名单非空时【必须】拿到模型名，拿不到就按错误响应返回；
		//   - 白名单为空时，只有"需要预留"的请求才读取请求体，
		//     且拿不到模型名时【跳过预留】而不报错（畸形请求交由转发阶段拒绝）。
		//
		// 性能考量：未配置白名单、也无需预留时（绝大多数请求）零额外开销。
		reserveNeeded := reserver != nil && owner != nil && owner.Quota != model.QuotaUnlimited

		var (
			modelName   string
			promptBytes int
		)
		if len(token.Models) > 0 {
			name, size, err := peekModelFromBody(c)
			if err != nil {
				writeModelBodyError(c, err)
				return
			}
			modelName, promptBytes = name, size
			if !token.AllowsModel(modelName) {
				abortWithErrorKey(c, http.StatusForbidden,
					"model.not_allowed", oai.TypePermission, oai.CodeModelNotAllowed)
				return
			}
		} else if reserveNeeded {
			if name, size, err := peekModelFromBody(c); err == nil {
				modelName, promptBytes = name, size
			}
			// 解析失败（请求体非法/缺 model/超限）：跳过预留，交给转发阶段处理。
		}

		requestID := ""
		if reserveNeeded && modelName != "" {
			id, ok := tryReserveQuota(c, reserver, token, owner, tokenGroup, modelName, promptBytes, pending)
			if !ok {
				// 额度不足：tryReserveQuota 已写出 429
				return
			}
			requestID = id
		}

		// ── 步骤 6：放行 ────────────────────────────────────────
		SetToken(c, token)

		// 把调用者身份与分组写入请求 context，供转发引擎按分组选渠道/计费，
		// 并在结束时落调用日志 / 结算预留。
		// 用标准库 context 而非 gin 上下文，是为了让 relay 不必依赖 Web 框架。
		// RequestID 仅在实际做了预留时非空，转发结束后据此结算或退还。
		reqCtx := reqctx.WithIdentity(c.Request.Context(), reqctx.Identity{
			UserID:    token.OwnerID,
			TokenID:   token.ID,
			RequestID: requestID,
		})
		reqCtx = reqctx.WithGroup(reqCtx, tokenGroup)
		c.Request = c.Request.WithContext(reqCtx)

		c.Next()
	}
}

// tryReserveQuota 尝试为本次调用预扣额度。
//
// group 为本次请求的分组（空表示未指定，由计费组件回退到默认分组）；
// 预留必须与后续结算使用同一分组，否则会出现"按 A 分组预扣、按 B 分组结算"的错账。
//
// 返回的第二个值为 false 表示已写出错误响应（额度不足），调用方应立即返回。
// 返回空 requestID 且 true 表示"本次不预留"（不计费模型 / 信任额度旁路 / 台账降级）。
func tryReserveQuota(c *gin.Context, reserver QuotaReserver, token *model.Token,
	owner *model.User, group, modelName string, promptBytes int, pending int64) (string, bool) {
	ctx := c.Request.Context()

	amount, priced := reserver.EstimateReserve(ctx, group, modelName, promptBytes)
	if !priced || amount <= 0 {
		// 【例外一】该模型未命中任何计价规则：调用不计费，跳过预留。
		// 否则免费模型会被额度墙挡住——这正是此前线上事故的根因。
		return "", true
	}

	// 【例外二】信任额度旁路：可用额度充足时跳过预留，减少一次写库。
	if owner.AvailableQuota(pending) >= trustQuotaBypassThreshold {
		return "", true
	}

	requestID := newRequestID()
	if _, err := reserver.Reserve(ctx, model.ReserveRequest{
		RequestID: requestID,
		UserID:    owner.ID,
		TokenID:   token.ID,
		Amount:    amount,
		TTL:       reservationTTL,
	}); err != nil {
		if errors.Is(err, model.ErrQuotaInsufficient) {
			available := owner.AvailableQuota(pending)
			if available < 0 {
				available = 0
			}
			abortWithErrorKey(c, http.StatusTooManyRequests,
				"quota.insufficient", oai.TypeRateLimit, oai.CodeInsufficientQuota,
				available, amount)
			return "", false
		}
		// 台账故障：不因一次记账故障阻断全部调用，降级为"本次不预留"并记错误级别日志。
		slog.Error("额度预留失败，本次未预留（存在超支风险）",
			"error", err, "user_id", owner.ID, "token_id", token.ID, "model", modelName)
		return "", true
	}
	return requestID, true
}

// peekModelFromBody 读取请求体并取出 model 名与请求体长度。
//
// 返回值 err 非 nil 时表示无法确定模型名（请求体超限 / 非法 JSON / 缺 model）；
// 是否把它变成错误响应由调用方决定（白名单校验必须报错，额度预留可跳过）。
//
// oai.ReadBody 会还原请求体，因此鉴权之后转发阶段仍能读到完整内容——
// 这是本中间件与转发能共存的关键。
func peekModelFromBody(c *gin.Context) (string, int, error) {
	body, err := oai.ReadBody(c.Request)
	if err != nil {
		return "", 0, err
	}
	modelName, err := oai.PeekModel(body)
	if err != nil {
		return "", 0, err
	}
	return modelName, len(body), nil
}

// writeModelBodyError 把"读取 / 解析请求体"的错误映射为 HTTP 响应。
func writeModelBodyError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, oai.ErrRequestTooLarge):
		abortWithErrorKey(c, http.StatusRequestEntityTooLarge,
			"request.too_large", oai.TypeInvalidRequest, oai.CodeRequestTooLarge)
	case errors.Is(err, oai.ErrMissingModel):
		abortWithErrorKey(c, http.StatusBadRequest,
			"request.missing_model", oai.TypeInvalidRequest, oai.CodeMissingModel)
	default:
		abortWithErrorKey(c, http.StatusBadRequest,
			"request.malformed_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
	}
}

// newRequestID 生成一次调用的幂等键（用于额度预留的去重）。
//
// 使用 crypto/rand：虽然它只是幂等键、不是凭据，但可预测的键会带来
// "不同请求撞到同一键 → 误判为重复预留"的风险，因此仍用密码学随机源。
func newRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 极端情况（随机源不可用）：退化为时间戳，仍能保证基本唯一性。
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(buf)
}

// extractAPIKey 从请求头中提取令牌明文。
//
// 支持的两种形式（覆盖主流客户端的默认行为）：
//   - Authorization: Bearer sk-xxxx   —— OpenAI SDK 的默认方式
//   - x-api-key: sk-xxxx              —— Anthropic SDK 的默认方式
//
// 安全考量：刻意【不支持】从 URL 查询参数读取令牌。
// 查询参数会出现在访问日志、浏览器历史与 Referer 头中，极易泄露凭据。
func extractAPIKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if key, found := strings.CutPrefix(auth, bearerPrefix); found {
			return strings.TrimSpace(key)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// abortWithError 以 OpenAI 兼容格式返回错误并中止后续处理。
//
// 必须调用 c.Abort()：否则 gin 会继续执行同一路由上的后续处理器，
// 导致既返回了错误、又执行了业务逻辑（可能产生额外费用）。
//
// 本函数用于「运维/内部错误」等无需本地化的文案（保持中文，便于日志检索）。
func abortWithError(c *gin.Context, status int, message, errType, code string) {
	oai.WriteError(c.Writer, status, message, errType, code)
	c.Abort()
}

// abortWithErrorKey 是 abortWithError 的多语言版本：按语义化键取词条后输出。
//
// locale 取自 locale 中间件写入的请求 context；未携带 Accept-Language 时为中文。
// args 为可选格式化参数（词条含 %d 等占位符时使用）。
//
// 仅用于「面向最终用户、会被展示」的错误；内部错误请用 abortWithError 保持中文。
func abortWithErrorKey(c *gin.Context, status int, key, errType, code string, args ...any) {
	oai.WriteErrorKey(c.Writer, status, key, errType, code, reqctx.Locale(c.Request.Context()), args...)
	c.Abort()
}
