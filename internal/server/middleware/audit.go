// 本文件实现管理后台的写操作审计中间件。
//
// 意图（Why）：
//
//	后台的每一次写操作（新增 / 修改 / 删除 / 取消 / 退款……）都必须可追溯：
//	"谁、什么时候、对哪个对象、做了什么、结果如何"。没有审计时，出现误操作或
//	越权只能靠猜，既无法定责也无法复盘。
//	因此本中间件挂在管理后台分组上，只记录【写操作】——GET 是只读且量极大，
//	记录它既无追责价值，也会把审计表迅速撑爆。
//
// 流转（Flow）：
//
//	router 在 admin 分组挂载 AdminAudit
//	  └─ 命中写方法（POST/PUT/PATCH/DELETE）
//	       ├─ 先读取请求体摘要（脱敏 + 截断），随后【还原】请求体，供业务处理器正常读取
//	       ├─ c.Next() 执行业务处理器
//	       └─ 组装 AuditLog（含响应状态码与耗时）→ 同步写入仓储
//
// 扩展（Extend）：
//
//	需要审计只读接口（极少见，如导出全量数据）时：把方法加进 isAuditedMethod 并补注释。
//	新增敏感字段名：追加到 sensitiveKeyParts，即可对所有请求体自动脱敏。
//	新增可读动作文案：在 auditActionLabels 里补一条（未登记的会回退为 "方法 路径"）。
package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 审计摘要的长度与读取约束。
const (
	// maxAuditDetailRunes 是请求体摘要的最大字符数（按 rune 计，避免截断多字节字符）。
	// 取 1000：足够看清"这次改了什么关键字段"，又不至于让审计表被长文本（如批量密钥）撑爆。
	maxAuditDetailRunes = 1000

	// maxAuditUserAgentRunes 是 User-Agent 的最大字符数。
	// UA 可能被客户端随意填得极长，截断可防止单条记录异常膨胀。
	maxAuditUserAgentRunes = 512

	// auditBodyReadLimit 是读取请求体用于生成摘要的字节上限（64 KiB）。
	// 只读一个前缀即可（摘要本就要截断），读取后会把前缀与剩余流拼回，
	// 保证业务处理器仍能读到完整请求体。
	auditBodyReadLimit = 64 << 10

	// auditWriteTimeout 是写审计日志的超时。
	//
	// 为什么同步写 + 短超时，而不是异步队列：
	//   同步写最简单、可测且"写完即返回"，不会丢失记录；异步队列则需要额外的
	//   缓冲、关闭时排空、失败重试，复杂度与出错面都大很多。
	//   风险是"库慢会拖慢响应"，用一个短超时兜底——超时只记 Warn 日志，
	//   绝不影响业务响应（见 recordAudit）。
	auditWriteTimeout = 2 * time.Second
)

// maskPlaceholder 是敏感字段被替换后的占位值。
const maskPlaceholder = "***"

// sensitiveKeyParts 是触发脱敏的字段名子串（小写、满足任一即脱敏）。
//
// 说明：用"子串包含"而非精确匹配——真实请求里的字段名花样很多
// （api_key / apiKey / access_token / client_secret / password_confirm …），
// 精确匹配必然漏网。这里宁可多脱敏（把 key_strategy 之类也掩掉），也不冒险泄露。
var sensitiveKeyParts = []string{"password", "secret", "key", "token", "credential"}

// sensitivePairPattern 是"非 JSON 请求体"的兜底脱敏正则：
// 匹配形如 "oauth_token":"abc" 的键值对（键名含敏感子串），把字符串值替换为 ***。
var sensitivePairPattern = regexp.MustCompile(
	`(?i)("(?:[^"\\]|\\.)*(?:password|secret|key|token|credential)(?:[^"\\]|\\.)*"\s*:\s*)"(?:[^"\\]|\\.)*"`)

// AdminAudit 返回记录后台写操作的审计中间件。
//
// repo 为 nil 时退化为"直接放行"（便于测试或明确不需要审计的场景）。
func AdminAudit(repo model.AuditLogRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		if repo == nil || !isAuditedMethod(c.Request.Method) {
			c.Next()
			return
		}

		start := time.Now()
		// 必须在业务处理器读取请求体【之前】取摘要，并在取完后还原。
		detail := captureBodySummary(c.Request)

		c.Next()

		recordAudit(c, repo, start, detail)
	}
}

// recordAudit 组装并同步写入一条审计记录。
//
// 写库失败只记 Warn，绝不改变已写给客户端的响应——审计是旁路能力，
// 不能因为它出问题而让后台操作失败。
func recordAudit(c *gin.Context, repo model.AuditLogRepository, start time.Time, detail string) {
	entry := &model.AuditLog{
		Method:     c.Request.Method,
		Path:       c.Request.URL.Path,
		Action:     describeAction(c.Request.Method, c.FullPath(), c.Request.URL.Path),
		Target:     formatTarget(c.Params),
		Detail:     detail,
		StatusCode: c.Writer.Status(),
		LatencyMS:  int(time.Since(start).Milliseconds()),
		ClientIP:   ClientIP(c),
		UserAgent:  truncateRunes(c.Request.UserAgent(), maxAuditUserAgentRunes),
		CreatedAt:  time.Now(),
	}
	if user, ok := CurrentUser(c); ok {
		entry.AdminID = user.ID
		entry.AdminUsername = user.Username
	}

	// 用 WithoutCancel 切断请求取消信号：客户端断开时仍应把"操作发生过"这一事实记下来；
	// 再叠加短超时，避免库慢拖住本该结束的请求。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), auditWriteTimeout)
	defer cancel()

	if err := repo.Create(ctx, entry); err != nil {
		slog.Warn("审计日志写入失败",
			"method", entry.Method, "path", entry.Path, "admin_id", entry.AdminID, "error", err)
	}
}

// isAuditedMethod 判断该方法是否属于需要审计的写操作。
func isAuditedMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// captureBodySummary 读取请求体并生成脱敏、截断后的摘要，同时还原请求体。
//
// 关键（务必保留）：读取后必须把"已读前缀 + 剩余流"重新拼回 req.Body，
// 否则业务处理器（如 ShouldBindJSON）会读到被截断或空的请求体——
// 表现为"审计一挂，后台就报请求体格式错误"，且极难定位。
func captureBodySummary(req *http.Request) string {
	if req.Body == nil {
		return ""
	}

	// 多读 1 字节用于判断是否超限（LimitReader 会在上限处静默截断）。
	prefix, err := io.ReadAll(io.LimitReader(req.Body, auditBodyReadLimit+1))
	// 无论如何都先还原完整请求体（读出错时也把已读部分拼回，尽力保证业务可用）。
	req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), req.Body))
	if err != nil {
		return ""
	}
	if len(prefix) > auditBodyReadLimit {
		prefix = prefix[:auditBodyReadLimit]
	}
	return summarizeBody(prefix)
}

// summarizeBody 对请求体做脱敏并截断为摘要。
func summarizeBody(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	return truncateRunes(redactJSON(trimmed), maxAuditDetailRunes)
}

// redactJSON 对 JSON 请求体做结构化脱敏；非 JSON 时回退为正则脱敏。
//
// 结构化脱敏（递归遍历键值）比正则更可靠：能覆盖嵌套对象与数组里的敏感字段。
func redactJSON(raw []byte) string {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return redactText(string(raw))
	}
	out, err := json.Marshal(redactValue(parsed))
	if err != nil {
		return redactText(string(raw))
	}
	return string(out)
}

// redactValue 递归遍历解析后的 JSON，把敏感键的值替换为掩码占位。
func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if isSensitiveKey(key) {
				result[key] = maskPlaceholder
				continue
			}
			result[key] = redactValue(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			result[i] = redactValue(item)
		}
		return result
	default:
		return value
	}
}

// isSensitiveKey 判断字段名是否敏感。
func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// redactText 是非 JSON 请求体的兜底脱敏：用正则掩掉敏感键的字符串值。
func redactText(text string) string {
	return sensitivePairPattern.ReplaceAllString(text, `${1}"`+maskPlaceholder+`"`)
}

// truncateRunes 按字符（rune）截断字符串，超长时以省略号结尾。
//
// 用 rune 而非 byte：避免把多字节汉字截成半个字符导致乱码。
func truncateRunes(text string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

// formatTarget 把路径参数格式化为可读的"目标"描述，如 "id=12"。
func formatTarget(params gin.Params) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, 0, len(params))
	for _, param := range params {
		parts = append(parts, param.Key+"="+param.Value)
	}
	return strings.Join(parts, ", ")
}

// describeAction 把方法与路由模式翻译为可读动作描述。
//
// fullPath 为 gin 的路由模式（如 /api/admin/channels/:id）；未在表中登记的路由
// 回退为"方法 + 实际路径"。用路由模式而非实际路径查表，保证 /channels/12 与
// /channels/34 命中同一条文案。
func describeAction(method, fullPath, actualPath string) string {
	key := method + " " + fullPath
	if label, ok := auditActionLabels[key]; ok {
		return label
	}
	fallback := fullPath
	if fallback == "" {
		fallback = actualPath
	}
	return method + " " + fallback
}

// auditActionLabels 是管理后台写接口的可读动作文案表。
//
// 维护说明：新增后台写接口时在此补一条（键为 "方法 路由模式"）。
// 漏补不会出错——未登记的路由会回退为"方法 路径"，审计仍然可用，只是可读性略差。
var auditActionLabels = map[string]string{
	"POST /api/admin/channels":                  "新建渠道",
	"PUT /api/admin/channels/:id":               "更新渠道",
	"DELETE /api/admin/channels/:id":            "删除渠道",
	"POST /api/admin/channels/:id/test":         "渠道测活",
	"PUT /api/admin/keys/:keyId":                "更新渠道密钥",
	"POST /api/admin/fetch-models":              "拉取上游模型列表",
	"POST /api/admin/tokens":                    "新建令牌",
	"PUT /api/admin/tokens/:id":                 "更新令牌",
	"DELETE /api/admin/tokens/:id":              "删除令牌",
	"POST /api/admin/users":                     "新建用户",
	"PUT /api/admin/users/:id":                  "更新用户",
	"DELETE /api/admin/users/:id":               "删除用户",
	"POST /api/admin/prices":                    "新建计价规则",
	"PUT /api/admin/prices/:id":                 "更新计价规则",
	"DELETE /api/admin/prices/:id":              "删除计价规则",
	"POST /api/admin/groups":                    "新建分组",
	"PUT /api/admin/groups/:id":                 "更新分组",
	"DELETE /api/admin/groups/:id":              "删除分组",
	"POST /api/admin/models":                    "新建模型",
	"PUT /api/admin/models/:id":                 "更新模型",
	"DELETE /api/admin/models/:id":              "删除模型",
	"PUT /api/admin/channels/:id/mappings":      "更新渠道模型映射",
	"POST /api/admin/oauth-providers":           "新建 OAuth 提供方",
	"PUT /api/admin/oauth-providers/:id":        "更新 OAuth 提供方",
	"DELETE /api/admin/oauth-providers/:id":     "删除 OAuth 提供方",
	"PUT /api/admin/settings":                   "更新系统设置",
	"POST /api/admin/tasks/:ref/cancel":         "取消异步任务",
	"POST /api/admin/redeem-codes":              "生成兑换码",
	"DELETE /api/admin/redeem-codes/invalid":    "清理失效兑换码",
	"PUT /api/admin/redeem-codes/:id":           "更新兑换码",
	"DELETE /api/admin/redeem-codes/:id":        "删除兑换码",
	"POST /api/admin/orders/:tradeNo/mark-paid": "确认订单入账",
	"POST /api/admin/orders/:tradeNo/close":     "关闭订单",
	"POST /api/admin/orders/:tradeNo/refund":    "订单退款",
}
