// 本文件实现结构化访问日志。
//
// 意图（Why）：
//
//	原先用 gin.Logger() 打访问日志，它有两个对网关来说不可接受的问题：
//	  1) 输出的是非结构化文本，无法被日志系统按字段检索与聚合；
//	  2) 没有请求标识——用户报障时报不出任何可对照的东西，
//	     站长只能靠时间戳在几十行里猜哪一行是那次请求。
//	本文件改为 slog 结构化输出，并把追踪 ID 写进每一行，让"用户给一个 ID
//	→ 一次 grep 命中"成为可能。
//
// 流转（Flow）：
//
//	AccessLog() → 记开始时刻 → c.Next() → slog.Info(trace_id/method/path/
//	status/latency/client_ip) → 交给采集端
//
// 扩展（Extend）：
//
//	需要按路径单独调采样（如静态资源不记）：在 shouldLog 里加判定，
//	不要在输出格式里做条件——那会让日志格式随请求类型变形，采集端解析规则会碎。
package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// slowRequestThreshold 是"慢请求"日志的门槛。
//
// 为什么单独提示：SSE 流式响应动辄几分钟，若对所有长请求都告警，
// 这条日志就永远不会安静；真正值得关注的是"本该很快却慢了"的非流式请求。
// 流式请求由各自的 trace_id 在调用记录里查，不靠这条日志。
const slowRequestThreshold = 3 * time.Second

// AccessLog 返回记录访问日志的中间件。
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"latency_ms", latency.Milliseconds(),
			"client_ip", c.ClientIP(),
		}
		// 只在有查询串时记：绝大多数接口没有，记一个空串只会给每行日志多加 20 字节。
		if query != "" {
			attrs = append(attrs, "query", query)
		}
		// 追踪 ID 放在最前面：日志检索通常按它定位，位置靠前便于人工扫读。
		if id, ok := TraceIDFrom(c); ok {
			attrs = append([]any{"trace_id", id}, attrs...)
		}

		switch {
		case status >= 500:
			// 服务端错误必须留痕：这是事后复盘"当时到底发生了什么"的唯一线索。
			slog.Error("请求处理失败", attrs...)
		case latency > slowRequestThreshold:
			slog.Warn("请求处理缓慢", attrs...)
		default:
			slog.Info("请求已处理", attrs...)
		}
	}
}
