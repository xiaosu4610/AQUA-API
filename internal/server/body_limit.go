// 本文件实现「请求体大小上限」中间件。
//
// 意图（Why）：
//
//	网关此前只有 /v1 端点带 32MiB 限额（见 oai.ReadBody），其余 POST 端点
//	（登录、注册、验证码、后台写接口）都是 ShouldBindJSON 直接读 body，
//	字节数没有任何上限；而 http.Server 又刻意不设 ReadTimeout
//	（见 server.go：SSE 长响应不能被掐断）。攻击者因此可以用一个超大请求体
//	让每个并发请求各持有一份全量字节副本，在 2 核小机器上直接打出 OOM。
//	本中间件只约束"读多少"，不改变任何业务语义。
//
// 流转（Flow）：
//
//	请求 → bodyLimit（按路径选限额，用 http.MaxBytesReader 包一层）
//	  ├─ /v1、/v1beta  → oai.MaxRequestBodyBytes+1：恰好让 oai.ReadBody 自己判超限，
//	  │                  保持既有的 413 与错误文案不变
//	  ├─ 备份校验端点   → 豁免：处理器内部已有 512MiB 限额（handler_maintenance.go）
//	  └─ 其余 POST 接口 → 4MiB：JSON 接口足够
//	→ 后续处理器/中间件（含限流 keyFunc）读到的是被限额的 body
//
// 扩展（Extend）：
//
//	新增大体积端点（文件上传等）时：在 bodyLimitExemptPath 显式登记路径并注明
//	"它自己设限"，不要整体调大默认限额——那等于把闸门重新打开。
//	新增 /v1 之外的流式上传：确认其读取路径自带 LimitReader 后再登记豁免。
//
// 参考：本实现取自 PR #8（作者 @jghuihui）的 internal/server/body_limit.go，
// 经维护者复核后采纳（本项目当时确实缺少这一层防护）。
package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/oai"
)

const (
	// defaultBodyLimitBytes 是普通 JSON 接口的请求体上限。
	//
	// 取 4MiB 的依据：本站最大的批量输入是敏感词单次 2000 条、
	// 公告与计价规则整表导入，实测都在 1MiB 以内；4MiB 留足余量，
	// 同时把"每请求内存占用 × 并发数"封顶在可控范围内。
	defaultBodyLimitBytes = 4 << 20

	// relayBodyLimitBytes 是 /v1 与 /v1beta 的上限：比 oai 的判定线多 1 字节。
	//
	// 多 1 是刻意的：oai.ReadBody 读到 MaxRequestBodyBytes+1 字节才判超限，
	// 若这里卡得更紧，超限会先以 "io: read error" 形式出现，
	// 用户拿到的就不再是明确的 413「请求体超过上限」。
	relayBodyLimitBytes = oai.MaxRequestBodyBytes + 1

	// bodyLimitExemptPath 是不套用默认限额的路径（处理器自行设限）。
	//
	// 备份校验允许 512MiB：那是站长上传 SQLite 快照做一致性检查的入口，
	// 大文件是它的真实使用场景；handler_maintenance.go 里已有
	// http.MaxBytesReader(512MiB) 收口，这里再套一层只会让它永远传不上来。
	bodyLimitExemptPath = "/api/admin/maintenance/backup/inspect"
)

// bodyLimit 返回按路径分档给请求体加读取上限的中间件。
//
// 必须在任何会读 body 的中间件/处理器之前装配（限流 keyFunc 也要读 body）。
// GET/HEAD/OPTIONS 没有请求体，直接跳过，避免无谓的包装。
func bodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}

		path := c.Request.URL.Path
		switch {
		case path == bodyLimitExemptPath:
			// 自行设限的端点，见 bodyLimitExemptPath 的说明
		case isRelayPath(path):
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, relayBodyLimitBytes)
		default:
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, defaultBodyLimitBytes)
		}
		c.Next()
	}
}

// isRelayPath 判断是否为转发协议端点（OpenAI 与 Gemini 两套前缀）。
//
// 单独抽出来是因为豁免与限额都只关心"路径族"，散落的前缀判断容易漏掉
// /v1beta 这一支（Gemini 协议的模型名就在它的 URL 路径里）。
func isRelayPath(path string) bool {
	return path == "/v1" ||
		strings.HasPrefix(path, "/v1/") ||
		strings.HasPrefix(path, "/v1beta/")
}
