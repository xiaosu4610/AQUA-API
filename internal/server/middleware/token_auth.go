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
//	  ├─ EffectiveStatus      结合时间与额度判定实际状态
//	  ├─ 模型白名单校验        仅当白名单非空时才读请求体（省开销）
//	  └─ SetToken → c.Next()  放行并把令牌写入上下文
//
// 扩展（Extend）：
//
//	新增校验维度（IP 白名单、RPM 限制）时：在"模型白名单校验"之后插入新的步骤，
//	  并保持"先廉价判定、后昂贵判定"的顺序（如先查内存/缓存，再读请求体）。
//	新增鉴权方式（如 JWT）：新建同级文件，复用 SetToken 的上下文约定。
package middleware

import (
	"errors"
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

// TokenAuth 返回校验下游令牌的 gin 中间件。
//
// 参数 tokens 为令牌仓储；它由 main 装配后注入，便于替换实现与单元测试。
func TokenAuth(tokens model.TokenRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ── 步骤 1：提取令牌 ────────────────────────────────────
		rawKey := extractAPIKey(c.Request)
		if rawKey == "" {
			abortWithError(c, http.StatusUnauthorized,
				"缺少访问令牌，请在 Authorization 头中携带 Bearer <令牌>",
				oai.TypeAuthentication, oai.CodeMissingAPIKey)
			return
		}

		// ── 步骤 2：查库校验令牌是否存在 ────────────────────────
		// 实现上按 key_hash 唯一索引查找，不会解密全表，因此该步骤开销很低。
		token, err := tokens.GetByKey(c.Request.Context(), rawKey)
		if err != nil {
			if errors.Is(err, model.ErrTokenNotFound) {
				// 统一回复"无效"而不区分"不存在"与"格式错误"，
				// 避免向攻击者提供可用于枚举有效令牌的差异信息。
				abortWithError(c, http.StatusUnauthorized,
					"访问令牌无效", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
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
			abortWithError(c, http.StatusForbidden,
				"访问令牌已被禁用", oai.TypePermission, oai.CodeTokenDisabled)
			return
		case model.TokenStatusExpired:
			abortWithError(c, http.StatusUnauthorized,
				"访问令牌已过期", oai.TypeAuthentication, oai.CodeTokenExpired)
			return
		case model.TokenStatusExhausted:
			// 429 而非 403：客户端按"稍后重试/更换令牌"处理更自然
			abortWithError(c, http.StatusTooManyRequests,
				"访问令牌额度已用尽", oai.TypeRateLimit, oai.CodeInsufficientQuota)
			return
		}

		// ── 步骤 4：模型白名单校验 ──────────────────────────────
		// 性能考量：仅当令牌配置了白名单时才读取请求体。
		// 未配置白名单（不限模型）是最常见的情况，此时零额外开销。
		if len(token.Models) > 0 {
			if !checkModelAllowed(c, token) {
				return
			}
		}

		// ── 步骤 5：放行 ────────────────────────────────────────
		SetToken(c, token)

		// 把调用者身份写入请求 context，供转发引擎在结束时落调用日志。
		// 用标准库 context 而非 gin 上下文，是为了让 relay 不必依赖 Web 框架。
		identityCtx := reqctx.WithIdentity(c.Request.Context(), reqctx.Identity{
			UserID:  token.OwnerID,
			TokenID: token.ID,
		})
		c.Request = c.Request.WithContext(identityCtx)

		c.Next()
	}
}

// checkModelAllowed 读取请求体并校验模型是否在令牌白名单内。
//
// 返回值 false 表示已写出错误响应并中止请求。
//
// 说明：oai.ReadBody 会还原请求体，因此鉴权之后转发引擎仍能读到完整内容——
// 这是本中间件与转发能共存的关键。
func checkModelAllowed(c *gin.Context, token *model.Token) bool {
	body, err := oai.ReadBody(c.Request)
	if err != nil {
		if errors.Is(err, oai.ErrRequestTooLarge) {
			abortWithError(c, http.StatusRequestEntityTooLarge,
				"请求体超过上限", oai.TypeInvalidRequest, oai.CodeRequestTooLarge)
			return false
		}
		abortWithError(c, http.StatusBadRequest,
			"读取请求体失败", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return false
	}

	modelName, err := oai.PeekModel(body)
	if err != nil {
		if errors.Is(err, oai.ErrMissingModel) {
			abortWithError(c, http.StatusBadRequest,
				"缺少 model 字段", oai.TypeInvalidRequest, oai.CodeMissingModel)
			return false
		}
		abortWithError(c, http.StatusBadRequest,
			"请求体不是合法的 JSON", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return false
	}

	if !token.AllowsModel(modelName) {
		abortWithError(c, http.StatusForbidden,
			"该令牌无权访问指定模型", oai.TypePermission, oai.CodeModelNotAllowed)
		return false
	}
	return true
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
func abortWithError(c *gin.Context, status int, message, errType, code string) {
	oai.WriteError(c.Writer, status, message, errType, code)
	c.Abort()
}
