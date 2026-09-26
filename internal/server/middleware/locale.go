// 本文件实现「按 Accept-Language 选择语言」的轻量中间件。
//
// 意图（Why）：
//
//	错误消息需要按用户的语言偏好返回。若让每个处理器各自解析 Accept-Language，
//	解析规则会散落各处、且极易出现"有的处理器忘了解析 → 退化为中文"的不一致。
//	因此集中为一个中间件：进入业务处理前解析一次，写入请求 context，
//	后续处理器与鉴权中间件统一从 context 取用。
//
// 流转（Flow）：
//
//	engine.Use(middleware.Locale())
//	  └─ i18n.Parse(Accept-Language) → reqctx.WithLocale 写入请求 context
//	       └─ 处理器 / 鉴权中间件 → reqctx.Locale(ctx) → oai.WriteErrorKey
//
// 扩展（Extend）：
//
//	新增语言：只需扩展 internal/i18n（Parse 的匹配与目录），本文件无需改动。
package middleware

import (
	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/i18n"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
)

// Locale 返回解析 Accept-Language 并写入请求 context 的中间件。
//
// 未携带该请求头时，i18n.Parse 返回 i18n.Default（中文），
// 从而与改动前"一律中文"的行为完全一致（零回归）。
func Locale() gin.HandlerFunc {
	return func(c *gin.Context) {
		locale := i18n.Parse(c.GetHeader("Accept-Language"))
		ctx := reqctx.WithLocale(c.Request.Context(), locale)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
