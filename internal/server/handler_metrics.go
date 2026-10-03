// 本文件实现 Prometheus 指标端点（/metrics）。
//
// 意图（Why）：
//
//	没有指标端点时，站长判断"现在到底有没有在坏"只有三条路：等用户投诉、
//	翻日志、或者凭感觉。而日志回答不了"错误率是 1% 还是 20%"这类问题——
//	那需要聚合。把聚合下放到进程内并以标准文本协议暴露，任何监控系统都能直接接。
//
//	流转（Flow）：
//
//	GET /metrics → 未启用则 404 →（已启用且配了令牌则校验 Bearer）→
//	  reg.Render() → text/plain
//
//	默认不开放（本站整改要求）：Metrics.Enabled 默认为 false，即默认连路由
//	行为都等同不存在（返回 404）。站长显式打开（AQUA_METRICS_ENABLED=true）
//	并可同时设置令牌（AQUA_METRICS_TOKEN）后才对外可用。
//
//	暴露范围的自我约束（重要）：只输出聚合值，不含任何用户身份、密钥、
//	请求内容。标签只用路由模板与状态码类别，不含 URL 里的 ID。
//
// 扩展（Extend）：
//
//	新增业务指标：在 internal/metrics 声明后，于对应业务处 Inc/Add/Sub/SetGauge；
//	不要在这里写业务聚合（那会让"指标定义"与"HTTP 层"耦合）。
package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/store"
	"github.com/LTZY-ACU/ltzy-api/internal/version"
)

// 进程级指标名。
const (
	metricUptimeSeconds = "aqua_uptime_seconds"
	metricBuildInfo     = "aqua_build_info"
	metricSchemaVersion = "aqua_schema_version"
)

// registerProcessMetrics 声明与"进程本身"有关的指标。
//
// 拆成独立函数是为了让"指标白名单"在一处可枚举：审阅这一段就知道对外暴露了什么。
func (s *Server) registerProcessMetrics() {
	// 运行时长：抓取时现算，而不是每秒自增——自增会在进程卡顿时累积误差，
	// 而"卡顿时长是否还在走"恰恰是判断假死最直接的信号。
	s.metrics.RegisterGaugeFunc(metricUptimeSeconds,
		"进程已运行的秒数",
		func() float64 { return time.Since(s.startedAt).Seconds() })

	// 构建信息：值恒为 1，真正的信息在标签里。
	// 这是 Prometheus 的标准做法——让 "aqua_build_info" 成为一个可被 join 的维度，
	// 便于回答"这个异常是哪次发布引入的"。
	s.metrics.RegisterGauge(metricBuildInfo, "构建信息（值恒为 1，信息在标签里）", "version")
	s.metrics.SetGauge(metricBuildInfo, 1, version.Get().Version)

	// 数据库结构版本：升级后指标突然变形（字段含义变了）时，
	// 有了它就能一眼看出是不是"版本对不上导致的面板错位"。
	//
	// 取【二进制支持的最高版本】而非库里实际版本：前者是常量，采集时零查询；
	// 后者每次抓取都要查库，且在迁移失败时会给出"看起来正常"的值。
	// 两者是否一致由 /healthz 负责（它查库并返回真实版本）。
	s.metrics.RegisterGauge(metricSchemaVersion, "当前二进制支持的数据库结构版本")
	s.metrics.SetGauge(metricSchemaVersion, float64(store.SupportedSchemaVersion()))
}

// handleMetrics 输出 Prometheus 文本格式的指标。
func (s *Server) handleMetrics(c *gin.Context) {
	cfg := s.deps.Config.Metrics

	// 未启用：按"端点不存在"处理（404 且不写任何指标内容）。
	// 默认关闭是本站的整改要求：运营规模与路由清单虽不含密钥与用户数据，
	// 但仍属内部信息，不应在站长未显式开启时对公网敞开。
	if !cfg.Enabled {
		c.Status(http.StatusNotFound)
		return
	}

	// 令牌鉴权：只在配置了令牌时启用。没配就等于内网/本机采集，省掉一次判断。
	if token := strings.TrimSpace(cfg.Token); token != "" {
		if !metricsTokenMatches(c.GetHeader("Authorization"), token) {
			// 用 401 而非 403：语义是"没通过身份校验"，且必须带 WWW-Authenticate，
			// 否则 Prometheus 只会看到一个无解释的状态码。
			c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"message": "指标端点需要有效的 Bearer 令牌", "type": "authentication_error", "code": "invalid_metrics_token"},
			})
			return
		}
	}

	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	// 明确禁缓存：否则中间代理会把第一次抓取的快照反复返回，
	// 表现为"指标卡在某个值不动"，而监控场景下这种假象代价很高。
	c.Header("Cache-Control", "no-store")
	_, _ = c.Writer.WriteString(s.metrics.Render())
}

// metricsTokenMatches 校验 Authorization 头。
//
// 令牌比较用【恒定时间】：一旦配了令牌就意味着"有隐私预期"，
// 逐字节提前返回会成为时序侧信道。
func metricsTokenMatches(header, token string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	presented := strings.TrimSpace(header[len(prefix):])
	return constantTimeEqual(presented, token)
}

// constantTimeEqual 恒定时间比较两个字符串。
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// 长度不同直接返回：长度本身不是秘密（令牌长度由站长自己定），
		// 而继续比较需要额外的分支，反而掩盖真正的比较逻辑。
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
