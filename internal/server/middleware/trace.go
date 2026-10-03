// 本文件实现全链路追踪 ID 中间件。
//
// 意图（Why）：
//
//	"用户说刚才那次请求失败了"是网关最常见的一线工单，而没有共同标识时，
//	站长得在日志里靠时间戳 + 模型名 + 耗时做近似猜测——几十条日志里捞一条，
//	且用户往往说不清时间。追踪 ID 的作用就是把这次对话变成一次精确检索：
//	响应头里回给用户一个 ID，日志、调用记录、指标时序上都带着同一个值。
//
//	为什么必须校验入站值：X-Request-Id 是客户端可控的输入。若原样回显，
//	攻击者可以塞入超长串（撑爆日志与指标标签）或带引号换行的串
//	（污染日志文本，伪造出"不存在的日志行"），因此只接受有限字符集。
//
// 流转（Flow）：
//
//	Trace() → 读取并校验 X-Request-Id（不合规则丢弃）
//	  → 无有效入站值时生成 tr-<32 hex>
//	  → 写入 X-Request-Id 响应头 + gin 上下文 + reqctx（供 relay 落调用日志）
//	  → 处理器与下游统一从 reqctx.RequestID 取用
//
// 扩展（Extend）：
//
//	要把追踪 ID 透传给上游：在处理器里取 reqctx.RequestID(ctx)，
//	写进发往上游的请求头（注意不要透传客户端的 Authorization/Host）。
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/reqctx"
)

// TraceHeader 是追踪 ID 的请求/响应头名称。
//
// 刻意沿用 X-Request-Id 这个业界通用名字：网关前面往往还串着 CDN / LB / Ingress，
// 用同一个头名才能让整条链路的 ID 串起来，而不是每个组件各起一个名字。
const TraceHeader = "X-Request-Id"

// maxTraceIDLength 是接受的入站追踪 ID 最大长度。
const maxTraceIDLength = 64

// traceIDPattern 是允许的入站追踪 ID 形态。
//
// 刻意只放行字母数字与三种连接符：足够 W3C Trace Context 与常见网关使用，
// 又排除了引号、反斜杠、空白与控制字符（它们是日志注入与文本协议污染的原料）。
var traceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// contextKeyTraceID 是追踪 ID 在 gin 上下文中的键。
const contextKeyTraceID = "aqua.context.trace_id"

// TraceIDFrom 取出本次请求的追踪 ID。
//
// 第二个返回值为 false 表示该路由没挂 Trace 中间件——属装配错误，
// 调用方应按"没有追踪能力"降级处理，而不是自己造一个 ID。
func TraceIDFrom(c *gin.Context) (string, bool) {
	value, exists := c.Get(contextKeyTraceID)
	if !exists {
		return "", false
	}
	id, ok := value.(string)
	return id, ok
}

// Trace 返回生成/透传追踪 ID 的中间件。
//
// 放在路由树最外层：只有最外层才能保证「每个请求都有一个 ID」，
// 挂在其后的中间件（鉴权、限流、敏感词）报错时才能带上它。
func Trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		traceID := sanitizeTraceID(c.GetHeader(TraceHeader))
		if traceID == "" {
			traceID = newTraceID()
		}

		c.Set(contextKeyTraceID, traceID)
		c.Header(TraceHeader, traceID)
		// 写回标准库 context：relay 层不依赖 gin，只能从这里取。
		c.Request = c.Request.WithContext(reqctx.WithRequestID(c.Request.Context(), traceID))

		c.Next()
	}
}

// sanitizeTraceID 校验并归一入站追踪 ID，不合格时返回空串（由调用方生成新的）。
//
// 为什么要归一（TrimSpace）而不只是拒绝：部分网关会在尾部补一个空格，
// 这属于"同一个 ID 的无意义差异"，直接丢弃会让追踪链在这一环断掉。
func sanitizeTraceID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > maxTraceIDLength {
		return ""
	}
	if !traceIDPattern.MatchString(trimmed) {
		return ""
	}
	return trimmed
}

// newTraceID 生成服务端的追踪 ID。
//
// 前缀 "tr-" 的作用是让人在日志里一眼分辨「追踪 ID」与额度「幂等键」（req- 前缀），
// 两者长得极像却用途完全不同，混看日志时很容易误用。
func newTraceID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 随机源不可用时不能退回常量：常量会让所有请求的 ID 相同，
		// 追踪功能等于没有，且比"没有 ID"更危险（看起来有、其实不可用）。
		return ""
	}
	return "tr-" + hex.EncodeToString(buf)
}
