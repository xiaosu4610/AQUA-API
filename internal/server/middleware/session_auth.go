// 本文件实现「网站登录态」的鉴权中间件与管理权限校验。
//
// 意图（Why）：
//
//	网站有另一套与模型接口完全不同的鉴权需求：
//	  1) 管理者登录后台、使用者登录门户，持有的是「会话令牌」（不透明随机串）；
//	  2) 会话需要可即时吊销（用户被禁用或改密后必须立刻失效）；
//	  3) 需要区分「已登录」与「是管理员」两级权限。
//	因此与 TokenAuth（校验 sk- 访问令牌）分开实现，避免两套语义混在一起。
//
// 流转（Flow）：
//
//	SessionAuth：取 Bearer → 查会话摘要 → 校验过期 → 载入用户 → 校验启用状态 → 注入上下文
//	RequireAdmin：从上下文取用户 → 判断角色 → 放行或 403
//
// 扩展（Extend）：
//
//	新增权限级别（如"渠道管理员"）时：在 UserRole 增加取值，并在此新增
//	RequireRole(roles...) 一类的中间件，而不是在各个处理器里散布角色判断。
package middleware

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// contextKeyUser 是登录用户在 gin 上下文中的键。
const contextKeyUser = "aqua.context.user"

// SetUser 把已认证用户写入上下文（仅供本包中间件调用）。
func SetUser(c *gin.Context, user *model.User) {
	c.Set(contextKeyUser, user)
}

// CurrentUser 取出当前登录用户。
//
// 第二个返回值为 false 表示未登录——通常意味着该路由没有挂在 SessionAuth 之后。
func CurrentUser(c *gin.Context) (*model.User, bool) {
	value, exists := c.Get(contextKeyUser)
	if !exists {
		return nil, false
	}
	user, ok := value.(*model.User)
	return user, ok
}

// SessionAuth 返回校验网站会话令牌的中间件。
//
// 参数：
//   - sessions：会话仓储（按令牌摘要查找）
//   - users：用户仓储（载入用户，校验是否被禁用）
//
// 校验顺序经过刻意设计：先验会话有效性（廉价），再载入用户（昂贵），
// 避免为无效会话付出额外的数据库查询。
func SessionAuth(sessions model.SessionRepository, users model.UserRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken := extractAPIKey(c.Request)
		if rawToken == "" {
			abortWithErrorKey(c, http.StatusUnauthorized,
				"auth.credentials_missing", oai.TypeAuthentication, oai.CodeMissingAPIKey)
			return
		}

		// 按摘要查找会话：数据库里没有明文令牌，泄露也无法直接使用
		session, err := sessions.GetByTokenHash(c.Request.Context(), crypto.SHA256Hex(rawToken))
		if err != nil {
			if errors.Is(err, model.ErrSessionNotFound) {
				abortWithErrorKey(c, http.StatusUnauthorized,
					"auth.session_invalid", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
				return
			}
			abortWithError(c, http.StatusInternalServerError,
				"网关内部错误", oai.TypeServer, oai.CodeInternal)
			return
		}

		// 过期校验：会话表虽有过期时间，但清理是异步的，
		// 因此这里必须实时判断，不能假设"表里存在即有效"。
		if session.IsExpired(time.Now()) {
			// 顺手清理这条过期会话，避免无效数据长期堆积
			_ = sessions.DeleteByTokenHash(c.Request.Context(), session.TokenHash)
			abortWithErrorKey(c, http.StatusUnauthorized,
				"auth.session_expired", oai.TypeAuthentication, oai.CodeTokenExpired)
			return
		}

		user, err := users.GetByID(c.Request.Context(), session.UserID)
		if err != nil {
			if errors.Is(err, model.ErrUserNotFound) {
				// 用户已被删除，但其会话仍在：清理掉并拒绝
				_ = sessions.DeleteByUserID(c.Request.Context(), session.UserID)
				abortWithErrorKey(c, http.StatusUnauthorized,
					"auth.account_missing", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
				return
			}
			abortWithError(c, http.StatusInternalServerError,
				"网关内部错误", oai.TypeServer, oai.CodeInternal)
			return
		}

		// 被禁用的账号不得继续使用：这是"禁用"这一操作能否真正生效的关键。
		// 若只靠登录时校验，已登录的会话仍可长期访问，禁用形同虚设。
		if !user.IsActive() {
			_ = sessions.DeleteByUserID(c.Request.Context(), user.ID)
			abortWithErrorKey(c, http.StatusForbidden,
				"auth.account_disabled", oai.TypePermission, oai.CodeTokenDisabled)
			return
		}

		SetUser(c, user)
		c.Next()
	}
}

// RequireAdmin 返回校验管理员权限的中间件，必须挂在 SessionAuth 之后。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := CurrentUser(c)
		if !ok {
			// 走到这里说明路由装配有误（未挂 SessionAuth），按未登录处理而非放行
			abortWithErrorKey(c, http.StatusUnauthorized,
				"auth.not_logged_in", oai.TypeAuthentication, oai.CodeMissingAPIKey)
			return
		}
		if !user.IsAdmin() {
			abortWithErrorKey(c, http.StatusForbidden,
				"auth.admin_required", oai.TypePermission, "insufficient_privileges")
			return
		}
		c.Next()
	}
}
