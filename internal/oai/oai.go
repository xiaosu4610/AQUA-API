// Package oai 提供 OpenAI 兼容协议的公共定义与工具。
//
// 意图（Why）：
//
//	OpenAI 兼容格式是本网关的「母语」：下游客户端按它调用，多数上游也按它响应。
//	把协议常量、错误体结构、请求体读取与字段探测集中在此，可以避免
//	relay（转发）与 server/middleware（鉴权）各实现一份。重复实现非常危险——
//	例如两处对请求体上限的取值不同，就会出现「鉴权通过、转发被拒」这类
//	让人百思不得其解的现象。
//
// 流转（Flow）：
//
//	server/middleware（鉴权需探测 model）
//	  └─ oai.ReadBody → oai.PeekModel
//	relay（转发需读取原样请求体）
//	  └─ oai.ReadBody →（转发）→ oai.WriteError（出错时）
//
// 扩展（Extend）：
//
//	新增端点：在此添加路径常量；若其请求体结构与对话接口差异较大，
//	  在本包新增对应的 Peek 函数，不要在调用处内联解析（内联会散落协议知识）。
//	新增错误码：在此集中定义，保持命名风格一致（小写下划线）。
package oai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 支持的端点路径。
//
// 说明：路径同时被路由注册与上游地址拼接使用，必须集中定义，
// 否则很容易出现"注册了 /v1/chat/completions 但转发到 /v1/chat/completion"的笔误。
const (
	// ChatCompletionsPath 是 OpenAI 对话补全端点。
	ChatCompletionsPath = "/v1/chat/completions"
	// EmbeddingsPath 是 OpenAI 向量嵌入端点。
	//
	// 为什么网关也要转发它：NVIDIA 等平台的免费模型里有相当一部分是
	// embedding / rerank / clip 类模型，它们【只提供 /v1/embeddings】，
	// 对 /v1/chat/completions 一律返回 404。若不支持这个端点，
	// 这些模型在网关里就等于"上架了但调不通"，只能从清单里剔掉。
	EmbeddingsPath = "/v1/embeddings"
)

// MaxRequestBodyBytes 是允许的请求体上限。
//
// 取值说明：对话请求可能携带 Base64 编码的图片（体积膨胀约 1/3），
// 32 MiB 足以覆盖多图场景，同时避免异常请求耗尽内存。
const MaxRequestBodyBytes = 32 << 20 // 32 MiB

// 错误类型（对应 OpenAI 错误体中的 error.type）。
const (
	TypeInvalidRequest = "invalid_request_error" // 客户端请求有误
	TypeAuthentication = "authentication_error"  // 身份认证失败
	TypePermission     = "permission_error"      // 已认证但无权访问
	TypeRateLimit      = "rate_limit_error"      // 触发限流或额度不足
	TypeServer         = "server_error"          // 服务端问题
)

// 错误码（对应 OpenAI 错误体中的 error.code，供客户端程序化判断）。
const (
	CodeMissingAPIKey         = "missing_api_key"         // 未提供令牌
	CodeInvalidAPIKey         = "invalid_api_key"         // 令牌不存在
	CodeTokenDisabled         = "token_disabled"          // 令牌被手动禁用
	CodeTokenExpired          = "token_expired"           // 令牌已过期
	CodeInsufficientQuota     = "insufficient_quota"      // 额度不足
	CodeModelNotAllowed       = "model_not_allowed"       // 模型不在令牌白名单内
	CodeInvalidJSON           = "invalid_json"            // 请求体不是合法 JSON
	CodeMissingModel          = "missing_model"           // 缺少 model 字段
	CodeRequestTooLarge       = "request_too_large"       // 请求体超过上限
	CodeNoAvailableChannel    = "no_available_channel"    // 无可用上游渠道
	CodeUpstreamRequestFailed = "upstream_request_failed" // 上游请求失败
	CodeInternal              = "internal_error"          // 网关内部错误
)

// 本包对外暴露的哨兵错误，供调用方用 errors.Is 精确判断。
var (
	// ErrRequestTooLarge 表示请求体超过 MaxRequestBodyBytes。
	ErrRequestTooLarge = errors.New("oai: 请求体超过上限")
	// ErrInvalidJSON 表示请求体不是合法 JSON。
	ErrInvalidJSON = errors.New("oai: 请求体不是合法 JSON")
	// ErrMissingModel 表示请求体缺少 model 字段。
	ErrMissingModel = errors.New("oai: 缺少 model 字段")
)

// ErrorBody 是 OpenAI 风格的错误响应体。
//
// 为什么必须遵循该格式：客户端 SDK 通常按此结构解析错误，
// 若返回自定义格式，调用方只能看到"未知错误"，几乎无法自助排查。
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 是错误详情。
type ErrorDetail struct {
	Message string `json:"message"` // 面向人的可读信息（严禁包含内部细节）
	Type    string `json:"type"`    // 错误类别
	Code    string `json:"code"`    // 机器可判定的错误码
}

// WriteError 以 OpenAI 兼容格式输出错误响应。
//
// 安全约束：message 只允许填写面向用户的描述，
// 严禁放入文件路径、SQL 语句、上游地址或密钥——错误响应是最容易泄露内部信息的渠道。
func WriteError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	// 编码失败时无能为力（响应头已发出），此处忽略错误是合理的
	_ = json.NewEncoder(w).Encode(ErrorBody{
		Error: ErrorDetail{Message: message, Type: errType, Code: code},
	})
}

// ReadBody 读取请求体（带长度上限），并把 Body 还原以便后续再次读取。
//
// 为什么必须还原：鉴权中间件为了校验模型白名单需要读一次请求体，
// 而随后的转发又要读一次。若不还原，转发阶段会读到空 body，
// 表现为"上游返回 400，提示缺少 messages"——这类问题排查成本很高。
//
// 实现细节：多读 1 字节用于区分"恰好等于上限"与"超过上限"
// （io.LimitReader 会在上限处静默截断，仅凭长度无法判断）。
func ReadBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, MaxRequestBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("oai: 读取请求体失败: %w", err)
	}
	if int64(len(body)) > MaxRequestBodyBytes {
		return nil, ErrRequestTooLarge
	}

	// 还原请求体：后续处理器（鉴权之后的转发）需要再次完整读取
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// chatRequestProbe 只承载我们关心的字段。
//
// 设计说明：刻意只声明 model 一个字段——不做完整反序列化，
// 既避免因上游/客户端字段变化导致解析失败，也省去无谓的 CPU 开销。
// 请求体会被原样转发，其他字段（messages、temperature、stream 等）不受影响。
type chatRequestProbe struct {
	Model string `json:"model"`
}

// PeekModel 从对话请求体中提取 model 字段。
//
// 返回 ErrInvalidJSON / ErrMissingModel 以便调用方映射为对应的 HTTP 错误。
func PeekModel(body []byte) (string, error) {
	var probe chatRequestProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", ErrInvalidJSON
	}

	model := strings.TrimSpace(probe.Model)
	if model == "" {
		return "", ErrMissingModel
	}
	return model, nil
}

// streamProbe 只承载"是否流式"这一字段。
//
// 说明：与 PeekModel 一样只声明关心的字段，避免因客户端字段差异导致解析失败。
type streamProbe struct {
	Stream bool `json:"stream"`
}

// PeekStream 判断请求是否为流式（SSE）。
//
// 用途：调用日志需要区分流式与非流式请求（两者在延迟特征与计费口径上不同）。
// 解析失败时返回 false——此时请求本身大概率也不合法，会被后续校验拦下。
func PeekStream(body []byte) bool {
	var probe streamProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}
