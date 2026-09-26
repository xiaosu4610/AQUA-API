// 本文件实现 OpenAI 兼容协议的请求转发（M1 为纯透传）。
//
// 意图（Why）：
//
//	绝大多数客户端工具（SDK、Cursor、LobeChat 等）都按 OpenAI 协议发起请求，
//	因此把 OpenAI 兼容格式作为网关的"母语"最省事：上游若是 OpenAI 兼容实现，
//	我们只需改写目标地址与鉴权，其余原样转发，几乎零转换成本与信息损失。
//
// 流转（Flow）：
//
//	ServeChatCompletions(w, req)
//	  ├─ 步骤1 读取请求体（限长，防内存打爆）
//	  ├─ 步骤2 仅解析 model 字段用于路由
//	  ├─ 步骤3 SelectChannel 选渠道（失败返回 OpenAI 风格错误）
//	  ├─ 步骤4 构造上游请求：改写 URL / Authorization，保留 Accept 与 User-Agent
//	  ├─ 步骤5 回写状态码与响应头（过滤逐跳头）
//	  └─ 步骤6 流式拷贝响应体并逐段 Flush（SSE 关键）
//
// 扩展（Extend）：
//
//	新增端点（/v1/embeddings、/v1/images/generations 等）：
//	  在本文件增加常量与 ServeXxx 方法，复用 readAndProbe / forward 逻辑，
//	  并在 server/router.go 注册路由。
//	新增协议转换（M2）：在步骤 4 之前插入"请求体转换"，在步骤 6 处插入"响应体转换"。
package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// OpenAI 兼容端点路径。
const openAIChatCompletionsPath = "/v1/chat/completions"

// maxRequestBodyBytes 是允许的请求体上限。
//
// 取值说明：对话请求可能携带 Base64 编码的图片（体积会膨胀约 1/3），
// 32MB 足以覆盖多图场景，同时避免异常请求耗尽内存。
// TODO(relay): 后续改为按配置项可调，并支持流式读取以进一步降低内存占用。
const maxRequestBodyBytes = 32 << 20 // 32 MiB

// 上游拷贝缓冲区大小。
//
// 取 32KB：既能减少系统调用次数，又能让首字尽早到达客户端
// （缓冲区过大会导致分片被攒住，破坏"逐字输出"的体验）。
const copyBufferBytes = 32 * 1024

// chatCompletionProbe 只承载路由所需的字段。
//
// 设计说明：刻意只声明 model 一个字段——我们不做完整反序列化，
// 既避免因上游字段变化而解析失败，也避免无谓的 CPU 开销。
// 请求体会被原样转发，客户端发送的其他字段（messages、temperature、stream 等）不受影响。
type chatCompletionProbe struct {
	Model string `json:"model"`
}

// openAIErrorBody 是 OpenAI 风格的错误响应体。
//
// 为什么必须遵循该格式：客户端 SDK 通常按此结构解析错误，
// 若返回自定义格式，调用方只能看到"未知错误"，排查成本很高。
type openAIErrorBody struct {
	Error openAIErrorDetail `json:"error"`
}

// openAIErrorDetail 是错误详情。
type openAIErrorDetail struct {
	Message string `json:"message"` // 面向人的可读信息（不得包含内部细节）
	Type    string `json:"type"`    // 错误类别
	Code    string `json:"code"`    // 机器可判定的错误码
}

// ServeChatCompletions 处理 POST /v1/chat/completions（透传）。
//
// 参数使用标准库类型而非框架类型，目的是让 relay 包不依赖具体 Web 框架，
// 便于测试与将来替换框架。
func (r *Relay) ServeChatCompletions(w http.ResponseWriter, req *http.Request) {
	// ── 步骤 1：读取请求体 ──────────────────────────────────────
	// 多读 1 字节用于判断是否超限（LimitReader 会在达到上限处直接截断，
	// 若不额外读 1 字节，无法区分"恰好等于上限"与"超过上限"）。
	body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "读取请求体失败", "invalid_request_error", "read_body_failed")
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("请求体超过上限（%d 字节）", maxRequestBodyBytes),
			"invalid_request_error", "request_too_large")
		return
	}

	// ── 步骤 2：解析 model 用于路由 ─────────────────────────────
	var probe chatCompletionProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "请求体不是合法的 JSON", "invalid_request_error", "invalid_json")
		return
	}
	if strings.TrimSpace(probe.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error", "missing_model")
		return
	}

	// ── 步骤 3：选择渠道 ────────────────────────────────────────
	ch, err := r.SelectChannel(req.Context(), probe.Model)
	if err != nil {
		if errors.Is(err, ErrNoAvailableChannel) {
			// 503 而非 500：这是"暂时无可用后端"，客户端稍后重试可能成功
			writeOpenAIError(w, http.StatusServiceUnavailable,
				"当前没有可用的上游渠道能处理该模型", "server_error", "no_available_channel")
			return
		}
		// 查询渠道时的内部错误：不向客户端暴露底层细节，只记录在服务端
		// TODO(relay): 接入结构化日志后在此记录 err
		writeOpenAIError(w, http.StatusInternalServerError,
			"网关内部错误", "server_error", "internal_error")
		return
	}

	// ── 步骤 4~6：转发并回写 ────────────────────────────────────
	r.forwardChat(w, req, ch, body)
}

// forwardChat 把请求转发到指定渠道，并把上游响应回写给客户端。
func (r *Relay) forwardChat(w http.ResponseWriter, req *http.Request, ch *model.Channel, body []byte) {
	// 拼接上游地址：去掉 base_url 末尾多余的斜杠，避免出现 "//v1/..." 这类路径
	upstreamURL := strings.TrimRight(ch.BaseURL, "/") + openAIChatCompletionsPath

	// 用请求 context：客户端断开时自动取消上游请求，避免无谓的上游消耗
	upReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "网关内部错误", "server_error", "build_request_failed")
		return
	}

	// 请求头策略：只设置必要的头，【绝不复用客户端的 Authorization】——
	// 客户端带的是本网关的令牌，上游需要的是渠道密钥，二者不可混用。
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+ch.APIKey)

	// 保留 Accept：客户端可能要求 text/event-stream（流式），这是协议协商的一部分
	if accept := req.Header.Get("Accept"); accept != "" {
		upReq.Header.Set("Accept", accept)
	}
	// 保留 User-Agent：部分上游会按 UA 做风控或功能分级，透传可减少非预期差异
	if ua := req.Header.Get("User-Agent"); ua != "" {
		upReq.Header.Set("User-Agent", ua)
	}

	resp, err := r.client.Do(upReq)
	if err != nil {
		// 502：网关作为代理无法从上游获得有效响应（超时、连接被拒、TLS 失败等）
		// 注意：不暴露 upstreamURL，避免泄露内部渠道地址
		writeOpenAIError(w, http.StatusBadGateway, "上游渠道请求失败", "server_error", "upstream_request_failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// ── 步骤 5：回写响应头 ──────────────────────────────────────
	// 注意：上游返回的错误状态码（如 401、429）原样透传，
	// 这对客户端很重要——它据此判断"是我的密钥问题还是上游限流"。
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// ── 步骤 6：流式拷贝响应体 ──────────────────────────────────
	flushCopy(w, resp.Body)
}

// copyResponseHeaders 把上游响应头复制到客户端，并过滤掉不应转发的头。
//
// 过滤原因：
//   - 逐跳头（hop-by-hop）仅在单段连接内有效，代理不应转发；
//   - Content-Length / Transfer-Encoding 描述的是"上游连接"的消息边界，
//     我们回写时由 Go 的 http 包重新决定帧格式，直接复制可能造成长度不一致。
func copyResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// hopByHopHeaders 是 HTTP/1.1 规范定义的逐跳头（不应被代理转发）。
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	// 消息边界由本机重新生成，不透传上游的值
	"Content-Length": {},
}

// isHopByHopHeader 判断响应头是否应被过滤（大小写不敏感）。
func isHopByHopHeader(key string) bool {
	_, found := hopByHopHeaders[http.CanonicalHeaderKey(key)]
	return found
}

// flushCopy 流式拷贝响应体，并在每个分片后立即 Flush。
//
// 为什么必须 Flush：大模型流式回答依赖 SSE，若数据被缓冲在网关或 HTTP 层，
// 客户端的体验会从"逐字出现"退化为"等全文生成完再一次性蹦出来"，
// 这与不经网关直连相比是明显的体验倒退。
func flushCopy(w http.ResponseWriter, src io.Reader) {
	// gin 的 ResponseWriter 实现了 http.Flusher；用类型断言以兼容不支持刷新的实现
	flusher, canFlush := w.(http.Flusher)

	buf := make([]byte, copyBufferBytes)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// 客户端已断开（例如用户取消），无需继续读取上游，直接结束
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			// io.EOF 表示正常结束；其他错误（上游中断）此时已无法补救，
			// 因为响应头早已发出，只能结束本次传输。
			return
		}
	}
}

// writeOpenAIError 以 OpenAI 兼容格式输出错误响应。
//
// 安全约束：message 参数只允许填写面向用户的描述，
// 严禁放入文件路径、SQL 语句、上游地址或密钥。
func writeOpenAIError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(openAIErrorBody{
		Error: openAIErrorDetail{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}
