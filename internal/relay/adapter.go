// 本文件定义「下游协议适配器」接口，并实现适配器驱动的转发入口。
//
// 意图（Why）：
//
//	现实中的客户端使用不同协议：OpenAI SDK、Claude SDK（Anthropic Messages）、
//	Gemini SDK 各说各话。若不转换，用户必须换掉自己所有工具才能接入本站；
//	有了转换层，同一份模型资源可以被任意客户端使用。
//
//	架构选择：内部统一用 OpenAI 协议作为"中间表示"。
//	若让每个下游协议直接对接每个上游协议，转换器数量是 N×M；
//	以 OpenAI 为枢纽则只需 N×1（入站转换）+ 1×M（出站透传），
//	而我们的上游绝大多数本就是 OpenAI 兼容实现，M 实际上等于 1。
//
// 流转（Flow）：
//
//	POST /v1/messages（Anthropic）
//	  └─ ServeAnthropicMessages
//	       ├─ adapter.DecodeRequest      下游请求 → OpenAI 请求
//	       ├─ forwardWithFallback(...)    复用既有的路由 / 密钥池 / 计费 / 重试
//	       └─ adapter.EncodeResponse      上游响应 → 下游格式（含流式）
//
// 扩展（Extend）：
//
//	新增协议（如 Cohere、Bedrock）：实现 Adapter 接口并在 router 注册路由即可，
//	不需要改动转发主链路——这正是把转换抽成接口的价值。
package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// Adapter 描述「下游协议 ↔ 内部 OpenAI 协议」的双向转换能力。
//
// 实现约定：
//   - DecodeRequest 必须输出合法的 OpenAI Chat Completions 请求体，
//     因为转发链路（路由、密钥池、计费）全部按该格式工作；
//   - EncodeResponse / EncodeError 只负责"改写为下游格式"，不参与状态码决策——
//     状态码由转发链路决定（它更清楚"这是上游错误还是网关错误"）。
type Adapter interface {
	// Name 返回协议名，用于日志与错误信息。
	Name() string

	// DecodeRequest 解析下游请求体，返回模型名与内部 OpenAI 请求体。
	//
	// path 是请求路径（如 /v1beta/models/gemini-1.5-pro:generateContent）：
	// Gemini 协议把模型名与"是否流式"都放在路径里，因此适配器需要它；
	// 对模型名在 body 里的协议（OpenAI / Anthropic），忽略该参数即可。
	DecodeRequest(body []byte, path string) (modelName string, openAIBody []byte, err error)

	// StreamRequested 判断下游请求是否要求流式返回。
	StreamRequested(body []byte) bool

	// EncodeResponse 把上游的非流式 OpenAI 响应转换为下游格式。
	EncodeResponse(upstreamBody []byte) []byte

	// EncodeError 把错误转换为下游格式的错误响应体。
	//
	// 参数 statusCode 为将要返回给客户端的状态码，upstreamBody 可能是
	// 上游的原始错误体（OpenAI 格式）或网关自己生成的错误体。
	EncodeError(statusCode int, upstreamBody []byte) []byte

	// NewStreamEncoder 创建流式编码器；返回 nil 表示该协议不支持流式转换。
	NewStreamEncoder() StreamEncoder
}

// StreamEncoder 把上游的 OpenAI 流式分片转换为下游协议的 SSE 分片。
//
// 生命周期：Begin（一次）→ Chunk（多次）→ End（一次）。
// 各方法的返回值均为"要写给客户端的完整字节"（含 SSE 的 data: 前缀与空行），
// 返回 nil 表示本次无需输出（例如上游分片不含可转换内容）。
type StreamEncoder interface {
	Begin() []byte
	Chunk(openAIChunk []byte) []byte
	End() []byte
}

// ErrUnsupportedProtocol 表示请求体不符合目标协议。
var ErrUnsupportedProtocol = errors.New("relay: 请求体不符合该协议的格式")

// openAIChatRequest 是内部中间表示（只保留转换需要的字段）。
//
// 为什么用结构体而不是 map[string]any：
//
//	结构体能在编译期约束字段名，也能让"哪些字段被我们改写"一目了然；
//	而 map 全透传容易把下游协议的私有字段混进上游请求，造成难以排查的兼容问题。
type openAIChatRequest struct {
	Model       string            `json:"model"`
	Messages    []openAIMessage   `json:"messages"`
	Stream      bool              `json:"stream,omitempty"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	Tools       []json.RawMessage `json:"tools,omitempty"`
	Extra       map[string]any    `json:"-"`
}

// openAIMessage 是 OpenAI 的消息结构。
type openAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// openAIStreamChunk 是上游流式分片的结构（只取需要的字段）。
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// openAIResponse 是上游非流式响应的结构（只取需要的字段）。
//
// 用量字段复用 usage.go 中已定义的 openAIUsage，避免同包内出现两份
// 结构相同但名字不同的类型（那会让"到底该用哪个"变成每次都要确认的问题）。
type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

// serveWithAdapter 是协议转换的统一入口。
//
// 它把"下游请求 → OpenAI 请求 → 转发 → 转换回写"串起来，并复用转发链路的
// 全部能力（多渠道路由、密钥池轮询、失败重试、用量计费）。
func (r *Relay) serveWithAdapter(w http.ResponseWriter, req *http.Request, adapter Adapter) {
	body, err := oai.ReadBody(req)
	if err != nil {
		if errors.Is(err, oai.ErrRequestTooLarge) {
			writeAdaptedError(w, adapter, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("请求体超过上限（%d 字节）", oai.MaxRequestBodyBytes),
				oai.TypeInvalidRequest, oai.CodeRequestTooLarge)
			return
		}
		writeAdaptedError(w, adapter, http.StatusBadRequest, "读取请求体失败",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	modelName, openAIBody, err := adapter.DecodeRequest(body, req.URL.Path)
	if err != nil {
		writeAdaptedError(w, adapter, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	if strings.TrimSpace(modelName) == "" {
		writeAdaptedError(w, adapter, http.StatusBadRequest, "缺少 model 字段",
			oai.TypeInvalidRequest, oai.CodeMissingModel)
		return
	}

	// 复用既有转发链路：路由、密钥池、重试、计费全部沿用
	r.forwardWithFallback(w, req, modelName, openAIBody, adapter, oai.ChatCompletionsPath)
}

// writeAdaptedError 以目标协议的错误格式写出错误。
//
// 为什么需要它：转换为 Anthropic / Gemini 的客户端会按各自协议的
// 错误结构解析响应，若这里回一个 OpenAI 格式的错误体，
// 客户端会显示"未知错误"，用户完全不知道发生了什么。
func writeAdaptedError(w http.ResponseWriter, adapter Adapter, status int, message, errType, code string) {
	if adapter == nil {
		oai.WriteError(w, status, message, errType, code)
		return
	}

	// 生成与 oai.WriteError 完全一致的 OpenAI 错误体，再交给适配器改写——
	// 这样适配器只需处理"结构映射"，不必重复实现错误语义。
	//
	// 为什么不直接调用 oai.WriteError：它要求传入 http.ResponseWriter，
	// 而此处需要先拿到字节再转换协议。手工构造的字段顺序与内容与它保持一致，
	// 保证同一错误在三种协议下语义完全相同。
	openAIError, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
	if err != nil {
		openAIError = []byte(`{"error":{"message":"网关内部错误","type":"server_error","code":"internal_error"}}`)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, writeErr := w.Write(adapter.EncodeError(status, openAIError)); writeErr != nil {
		// 客户端可能已断开：无法补救，忽略
		return
	}
}

// writeAdapted 把上游（OpenAI 格式）响应转换后写回客户端。
//
// 两条路径：
//   - 非流式（含一切错误响应）：整体读取 → 转换 → 一次性写出；
//   - 流式：逐行解析上游 SSE → 逐块转换 → 立即 Flush。
//
// 为什么流式必须"逐行读、逐块写"而不是"读完再转"：
//
//	后者会把整段回答攒到最后一次性下发，客户端的体验从"逐字出现"
//	退化成"等半天蹦出全文"，与不经网关直连相比是明显倒退。
func (r *Relay) writeAdapted(w http.ResponseWriter, req *http.Request, resp *http.Response,
	adapter Adapter, sniffer *usageSniffer, requestBody []byte) {

	// 上游返回错误：不管客户端要的是不是流式，都用一次性错误响应
	// （流式协议下的错误同样是一个完整的错误对象，没有"增量错误"的概念）
	if resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := readAllLimited(resp.Body, maxAdaptedBodyBytes)
		_, _ = sniffer.Write(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(adapter.EncodeError(resp.StatusCode, raw)); err != nil {
			return
		}
		return
	}

	if !adapter.StreamRequested(requestBody) {
		raw, err := readAllLimited(resp.Body, maxAdaptedBodyBytes)
		if err != nil {
			writeAdaptedError(w, adapter, http.StatusBadGateway, "读取上游响应失败",
				oai.TypeServer, oai.CodeUpstreamRequestFailed)
			return
		}
		_, _ = sniffer.Write(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(adapter.EncodeResponse(raw)); err != nil {
			return
		}
		return
	}

	encoder := adapter.NewStreamEncoder()
	if encoder == nil {
		// 该协议暂不支持流式转换：明确报错而不是退化成非流式——
		// 客户端按 SSE 解析非流式响应会得到难以理解的解析错误，
		// 不如直接告诉它"不支持流式"，便于使用者改用非流式调用。
		writeAdaptedError(w, adapter, http.StatusNotImplemented,
			"当前版本暂不支持该协议的流式转换，请改用非流式调用",
			oai.TypeInvalidRequest, "stream_not_supported")
		return
	}

	// SSE 必须关闭一切缓冲并逐块 Flush，否则客户端会看到"憋住不动"的效果
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)
	flush := func() {
		if canFlush {
			flusher.Flush()
		}
	}

	if begin := encoder.Begin(); len(begin) > 0 {
		if _, err := w.Write(begin); err != nil {
			return
		}
		flush()
	}

	reader := bufio.NewReaderSize(resp.Body, adaptedReadBufferBytes)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			// 抓 usage 时必须喂原始 SSE 文本（含 data: 前缀），
			// 与直通路径保持一致，这样 extractUsage 的解析逻辑无需分叉。
			_, _ = sniffer.Write(line)

			if payload, ok := sseDataPayload(line); ok {
				if out := encoder.Chunk(payload); len(out) > 0 {
					if _, err := w.Write(out); err != nil {
						// 客户端断开：终止转发，避免继续消耗上游
						return
					}
					flush()
				}
			}
		}
		if readErr != nil {
			// io.EOF 表示正常结束；其他错误此时已无法补救（响应头早已发出）
			break
		}
	}

	if end := encoder.End(); len(end) > 0 {
		if _, err := w.Write(end); err != nil {
			return
		}
		flush()
	}
}

// readAllLimited 读取上游响应体（带上限保护）。
//
// 上限的作用：异常上游可能返回超大响应体，无限制读取会撑爆网关内存。
func readAllLimited(reader io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, limit))
}

// maxAdaptedBodyBytes 是非流式转换路径允许读取的响应体上限。
//
// 取 32MiB：与请求体上限保持一致。超长回答通常走流式，
// 非流式响应很少超过这个量级，同时避免异常上游撑爆内存。
const maxAdaptedBodyBytes = 32 << 20

// adaptedReadBufferBytes 是流式转换的读取缓冲大小。
//
// 取 32KiB：足够容纳单个 SSE 事件（含较长 JSON），
// 又不至于攒住数据破坏"逐字输出"的手感。
const adaptedReadBufferBytes = 32 * 1024

// sseDataPayload 从一行 SSE 文本中取出 data 部分。
//
// 返回 false 表示该行不是数据行（如空行、注释行、event: 行）。
// 上游的结束标记 [DONE] 也会被过滤掉——转换为 Anthropic/Gemini 时，
// "结束"由适配器自己发出的结束事件表达，不需要转发这个 OpenAI 专有标记。
func sseDataPayload(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	payload, ok := bytes.CutPrefix(trimmed, []byte("data:"))
	if !ok {
		return nil, false
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

// decodeJSONStrict 解析 JSON 并给出可读的错误信息。
func decodeJSONStrict(raw []byte, target any, protocolName string) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("%w（%s）", ErrUnsupportedProtocol, protocolName)
	}
	return nil
}

// openAIErrorEnvelope 是 OpenAI 风格错误体的结构，用于把上游错误
// 翻译成下游协议的"message + type"字段。
type openAIErrorEnvelope struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// extractErrorMessage 从 OpenAI 风格错误体中提取人可读信息。
//
// 取不到时回退为通用文案：绝不能把内部错误细节（SQL、路径）透出去。
func extractErrorMessage(raw []byte) string {
	var envelope openAIErrorEnvelope
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != nil &&
		strings.TrimSpace(envelope.Error.Message) != "" {
		return envelope.Error.Message
	}
	return "上游请求失败"
}

// upstreamErrorTypeToAnthropic 把 OpenAI 的错误类型映射为 Anthropic 的错误类型。
//
// 映射表见 Anthropic 官方错误类型；未识别的类型统一归为 api_error，
// 这样客户端至少能正确区分"是我的请求有问题"还是"服务端出错"。
func upstreamErrorTypeToAnthropic(status int, openAIType string) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	}
	switch openAIType {
	case oai.TypeInvalidRequest:
		return "invalid_request_error"
	case oai.TypeAuthentication:
		return "authentication_error"
	case oai.TypePermission:
		return "permission_error"
	case oai.TypeRateLimit:
		return "rate_limit_error"
	}
	return "api_error"
}

// contextWithoutCancel 复用标准库能力：在客户端断开后仍完成计费与日志写入。
//
// 单独包一层是为了让调用点语义清晰（"这里的写入不应被请求取消影响"）。
func contextWithoutCancel(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
