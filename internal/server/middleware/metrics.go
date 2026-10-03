// 本文件把 HTTP 层发生的事情汇成 Prometheus 指标。
//
// 意图（Why）：
//
//	指标的用途是回答两类问题：「现在有没有在坏」与「哪里在慢」。
//	因此只采集能直接支撑这两问的量：请求量、状态码分布、延迟分位、在途请求数。
//	刻意不采集"什么都记"式的细粒度数据——指标基数会随标签取值爆炸，
//	而监控系统自身被打爆时，恰恰是最需要它可用的时候。
//
//	为什么标签用【路由模板】而不是原始 URL：
//	/c/1/tokens/2 与 /c/1/tokens/3 是同一类事件，用原始 URL 会让时序数量
//	随用户数线性增长，最终把监控存储拖垮。gin 的 FullPath 给出的是
//	注册时的路由模板（/api/channels/:id），基数与业务实体数无关。
//
// 流转（Flow）：
//
//	Metrics(reg) → 记录 in-flight++ → c.Next() → 记录耗时与状态码 → in-flight--
//
// 扩展（Extend）：
//
//	新增业务指标（如渠道成功率、额度余量）：在 internal/metrics 声明，
//	在对应业务处调用 Inc/Add/Sub/SetGauge，不要在中间件里堆业务判断。
package middleware

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/metrics"
)

// 指标名（对外契约，改名等于让所有历史时序断裂，慎重）。
//
// 刻意导出：后台的运维面板与测试都要按名字引用这些指标，
// 藏在包内会迫使调用方复制字符串常量——那正是指标名拼错的源头。
const (
	MetricRequestsTotal   = "aqua_http_requests_total"
	MetricDurationSeconds = "aqua_http_request_duration_seconds"
	MetricInFlight        = "aqua_http_requests_in_flight"
)

// labelUnmatched 是「未匹配到任何路由」的占位标签值。
//
// 不能用原始路径：未匹配路径来自客户端输入（可任意构造），直接当标签
// 等于把指标存储变成一个可被任意撑爆的公共写入口。
const labelUnmatched = "other"

// RegisterHTTPMetrics 向注册表声明 HTTP 层的三条指标。
//
// 单独一个函数（而不是在 Metrics 里隐式声明）是为了让"指标白名单"
// 在代码里可枚举：新增指标必须先在这里显式登记。
func RegisterHTTPMetrics(reg *metrics.Registry) {
	reg.RegisterCounter(MetricRequestsTotal,
		"HTTP 请求总数（按方法、路由模板与状态码类别）",
		"method", "route", "status")
	reg.RegisterHistogram(MetricDurationSeconds,
		"HTTP 请求耗时分布（秒）", nil, "method", "route")
	reg.RegisterGauge(MetricInFlight, "当前在途请求数")
}

// Metrics 返回采集 HTTP 指标的中间件。
//
// 装配位置很重要：应尽量靠外层（紧随 Trace 之后），
// 这样被请求体限额等更内层中间件提前拒绝的请求也能计入——
// 否则错误率会被系统性低估（最能说明"在坏"的恰恰是这些拒绝）。
func Metrics(reg *metrics.Registry) gin.HandlerFunc {
	RegisterHTTPMetrics(reg)

	return func(c *gin.Context) {
		// 静态资源与指标端点自身不计入：
		// 它们由进程内嵌文件系统直接返回，业务上无意义，
		// 而 /assets 下的请求量通常比 API 高一个数量级，会把时序图淹没。
		if skipMetrics(c.Request.URL.Path) {
			c.Next()
			return
		}

		start := time.Now()
		reg.Add(MetricInFlight, 1)
		// 用 Sub 而非 Add(-1)：Add 对 counter 语义（拒绝负增量），
		// 用来递减会让在途数只增不减、永远回不到 0。
		defer reg.Sub(MetricInFlight, 1)

		c.Next()

		route := routeLabel(c)
		reg.Inc(MetricRequestsTotal, c.Request.Method, route, statusClass(c.Writer.Status()))
		reg.Observe(MetricDurationSeconds, time.Since(start).Seconds(),
			c.Request.Method, route)
	}
}

// skipMetrics 判断该路径是否不计入业务指标。
func skipMetrics(path string) bool {
	switch {
	case path == "/metrics":
		return true
	case strings.HasPrefix(path, "/assets/"):
		return true
	default:
		return false
	}
}

// routeLabel 取路由模板作为标签值；未匹配时归一到常量占位符。
func routeLabel(c *gin.Context) string {
	if full := c.FullPath(); full != "" {
		return full
	}
	return labelUnmatched
}

// statusClass 把状态码归成类别。
//
// 为什么不直接用完整状态码：404 与 405、400 与 422 在告警上属于同一件事
// （"这批请求被本站拒了"），而逐个状态码建时序会让常用规则写成一长串。
// 需要精确状态码时看日志与调用记录即可，指标只负责回答"有没有在坏"。
func statusClass(status int) string {
	switch {
	case status >= 100 && status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	case status < 600:
		return "5xx"
	default:
		return "other"
	}
}
