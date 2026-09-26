// 本文件实现 OpenAI 兼容协议的请求转发（M2 起支持多渠道路由与故障转移）。
//
// 意图（Why）：
//
//	绝大多数客户端工具（SDK、Cursor、LobeChat 等）都按 OpenAI 协议发起请求，
//	因此把 OpenAI 兼容格式作为网关的「母语」最省事：上游若也是 OpenAI 兼容实现，
//	我们只需改写目标地址与鉴权，其余原样转发，几乎零转换成本与信息损失。
//
// 流转（Flow）：
//
//	ServeChatCompletions(w, req)
//	  ├─ 步骤1 读取请求体（复用 oai.ReadBody：限长 + 还原 body）
//	  ├─ 步骤2 探测 model 字段（oai.PeekModel）
//	  ├─ 步骤3 选渠道并转发，失败按策略换渠道重试（见 forwardWithFallback）
//	  ├─ 步骤4 构造上游请求：改写 URL / Authorization，保留 Accept 与 User-Agent
//	  ├─ 步骤5 回写状态码与响应头（过滤逐跳头）
//	  └─ 步骤6 流式拷贝响应体并逐段 Flush（SSE 关键）
//
// 扩展（Extend）：
//
//	新增端点（/v1/embeddings 等）：复用 oai 包中的协议工具与
//	  forwardWithFallback，仅需处理各自的请求体差异。
//	新增协议转换：在步骤 3 之前插入"请求体转换"，在步骤 6 处插入"响应体转换"。
package relay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// copyBufferBytes 是上游响应的拷贝缓冲区大小。
//
// 取 32KB：既能减少系统调用次数，又能让首字尽早到达客户端
// （缓冲区过大会让分片被攒住，破坏"逐字输出"的体验）。
const copyBufferBytes = 32 * 1024

// ServeChatCompletions 处理 POST /v1/chat/completions（透传 + 多渠道路由）。
//
// 参数使用标准库类型而非框架类型，目的是让 relay 包不依赖具体 Web 框架，
// 便于测试（httptest 直接调用）与将来替换框架。
func (r *Relay) ServeChatCompletions(w http.ResponseWriter, req *http.Request) {
	// ── 步骤 1：读取请求体 ──────────────────────────────────────
	// 复用 oai.ReadBody：它同时完成限长检查与 body 还原，
	// 保证鉴权中间件读过 body 后这里仍能完整读取。
	body, err := oai.ReadBody(req)
	if err != nil {
		if errors.Is(err, oai.ErrRequestTooLarge) {
			oai.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("请求体超过上限（%d 字节）", oai.MaxRequestBodyBytes),
				oai.TypeInvalidRequest, oai.CodeRequestTooLarge)
			return
		}
		oai.WriteError(w, http.StatusBadRequest, "读取请求体失败",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	// ── 步骤 2：探测 model 字段（路由与校验的输入）──────────────
	modelName, err := oai.PeekModel(body)
	if err != nil {
		if errors.Is(err, oai.ErrMissingModel) {
			oai.WriteError(w, http.StatusBadRequest, "缺少 model 字段",
				oai.TypeInvalidRequest, oai.CodeMissingModel)
			return
		}
		oai.WriteError(w, http.StatusBadRequest, "请求体不是合法的 JSON",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	// ── 步骤 3~6：转发（含失败换渠道重试）───────────────────────
	r.forwardWithFallback(w, req, modelName, body)
}

// forwardWithFallback 按路由策略选择渠道并转发，失败时换渠道重试。
//
// 重试语义（M2）：
//   - 可重试：连接层失败（超时/连接被拒/TLS 失败），以及上游返回 429 与 5xx；
//   - 不可重试：其他 4xx。这类错误源于请求本身（参数非法、模型不存在、密钥无效），
//     换渠道结果相同，重试只会放大上游压力与延迟；
//   - 已失败的渠道加入本次请求的排除集合，避免重复撞上同一故障渠道。
//     这是参考实现的常见缺陷：同优先级内重新随机，可能再次选中刚失败的渠道。
//
// 重试的硬约束：只有在【尚未向客户端写出任何内容】时才允许重试。
// 因此判断必须发生在 WriteHeader 之前——一旦状态码发出就无法撤回。
//
// 说明：M2 采用"同请求内排除"，不做跨请求的熔断与冷却——
// 那属于健康引擎的范畴（窗口错误率 + 半开恢复）。
func (r *Relay) forwardWithFallback(w http.ResponseWriter, req *http.Request, modelName string, body []byte) {
	// 一次性取出候选集：同一次请求内的多次重试都基于它挑选，避免每次重试都查库
	candidates, err := r.listCandidates(req.Context(), modelName)
	if err != nil {
		// 仓储查询失败：不向客户端暴露细节
		// TODO(relay): 接入结构化日志后在此记录 err
		oai.WriteError(w, http.StatusInternalServerError, "网关内部错误",
			oai.TypeServer, oai.CodeInternal)
		return
	}
	if len(candidates) == 0 {
		// 一个可用渠道都没有：属于"配置/容量"问题。
		// 用 503（而非 500）表达"暂时无可用后端"，客户端稍后重试可能成功。
		//
		// 注意：这种情况也记一条日志——"配置漏了模型"是常见事故，
		// 若不留痕，站长只能看到用户报错却查不到原因。
		r.recordUsage(req.Context(), usageEntry{
			UserID:     identityFromRequest(req.Context()).UserID,
			TokenID:    identityFromRequest(req.Context()).TokenID,
			Model:      modelName,
			IsStream:   oai.PeekStream(body),
			StatusCode: http.StatusServiceUnavailable,
			ErrorText:  "无可用渠道",
		})
		oai.WriteError(w, http.StatusServiceUnavailable,
			"当前没有可用的上游渠道能处理该模型",
			oai.TypeServer, oai.CodeNoAvailableChannel)
		return
	}

	excluded := make(map[uint64]struct{}) // 本次请求已尝试并失败的渠道

	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		ch := pickCandidate(candidates, excluded)
		if ch == nil {
			// 候选已全部尝试过，退出循环统一报错
			break
		}
		excluded[ch.ID] = struct{}{}

		// canRetry 的语义：本次失败时，"还有剩余尝试次数"且"确实还有其他候选"，
		// 才允许丢弃上游响应改投他处。
		//
		// 为什么必须检查"还有其他候选"：若只剩最后一个渠道，丢弃它的 429/5xx 响应
		// 只会让客户端收到含义更模糊的 502（"所有渠道失败"），
		// 不如把上游的真实错误原样透传，客户端据此可自助排查。
		canRetry := attempt < r.maxAttempts && pickCandidate(candidates, excluded) != nil

		if r.forwardChat(w, req, ch, modelName, body, canRetry) == forwardResponded {
			return
		}
		// 未产生任何响应，继续尝试下一个候选
	}

	// 所有候选渠道都尝试失败：记一条日志（channel_id 为 0，因为没有一个渠道成功建立会话）
	r.recordUsage(req.Context(), usageEntry{
		UserID:     identityFromRequest(req.Context()).UserID,
		TokenID:    identityFromRequest(req.Context()).TokenID,
		Model:      modelName,
		IsStream:   oai.PeekStream(body),
		StatusCode: http.StatusBadGateway,
		ErrorText:  "所有候选渠道均请求失败",
	})
	oai.WriteError(w, http.StatusBadGateway, "所有候选渠道均请求失败",
		oai.TypeServer, oai.CodeUpstreamRequestFailed)
}

// forwardOutcome 描述一次转发的结局，用于决定是否还能换渠道重试。
type forwardOutcome int

const (
	// forwardResponded 表示已开始向客户端回写响应（包括上游返回错误码的情况）。
	//
	// 重要：一旦进入该状态就【不能】再重试——HTTP 状态码已经发出，无法撤回。
	// 上游返回 401/429 等错误时，这些信息对客户端同样有价值
	// （它据此判断是密钥问题还是限流），应原样透传而非隐藏后重试。
	forwardResponded forwardOutcome = iota
	// forwardNotStarted 表示未向客户端写出任何内容（如建立连接失败），可安全重试。
	forwardNotStarted
)

// forwardChat 把请求转发到指定渠道，并把上游响应回写给客户端。
//
// 参数 canRetry 表示"本次失败后是否还有别的渠道可试"：
//   - 为 true：遇到可重试状态码（429/5xx）时，可在未写出响应前丢弃本次结果、改投他处；
//   - 为 false：无论上游返回什么都必须原样回写。因为已无退路，
//     丢弃结果只会让客户端收到更含糊的 502。
//
// 返回值表示结局，供上层决定是否继续尝试其他渠道。
func (r *Relay) forwardChat(w http.ResponseWriter, req *http.Request, ch *model.Channel, modelName string, body []byte, canRetry bool) forwardOutcome {
	// 记录起始时间用于计算耗时（写入调用日志）
	start := time.Now()

	// 拼接上游地址：去掉 base_url 末尾多余的斜杠，避免出现 "//v1/..." 这类路径
	upstreamURL := strings.TrimRight(ch.BaseURL, "/") + oai.ChatCompletionsPath

	// 用请求 context：客户端断开时自动取消上游请求，避免无谓的上游消耗
	upReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		// 请求构造失败属于网关侧问题，未向上游发出请求，可换渠道重试
		return forwardNotStarted
	}

	// 请求头策略：只设置必要的头，【绝不复用客户端的 Authorization】——
	// 客户端带的是本网关的令牌，上游需要的是渠道密钥，二者混用会导致
	// 上游鉴权失败，并把网关令牌泄露给第三方上游。
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+ch.APIKey)

	// 保留 Accept：客户端可能要求 text/event-stream（流式），这是协议协商的一部分
	if accept := req.Header.Get("Accept"); accept != "" {
		upReq.Header.Set("Accept", accept)
	}
	// 保留 User-Agent：部分上游按 UA 做风控或功能分级，透传可减少非预期差异
	if ua := req.Header.Get("User-Agent"); ua != "" {
		upReq.Header.Set("User-Agent", ua)
	}

	resp, err := r.client.Do(upReq)
	if err != nil {
		// 连接层面失败（超时、连接被拒、TLS 失败）：未写出任何响应，可安全重试。
		// 注意：不把错误细节回传给客户端，避免泄露内部渠道地址。
		//
		// 此处刻意不记日志：若记录，多次重试会产生多条记录，把请求数统计放大。
		// 最终失败会在 forwardWithFallback 的统一出口处记录一条。
		return forwardNotStarted
	}
	defer func() { _ = resp.Body.Close() }()

	// 上游返回可重试的状态码（限流/过载/服务端错误）时，若还有其他渠道可试就改投他处。
	//
	// 关键前提：此时尚未调用 WriteHeader，客户端还未收到任何内容，因此丢弃本次响应是安全的。
	// 若已无退路（canRetry=false），则跳过此分支，把状态码与错误体原样透传。
	if canRetry && isRetryableStatus(resp.StatusCode) {
		// 先尽力读完错误体再关闭：直接关闭会让底层连接无法复用，
		// 高并发故障场景下会退化为每次重试都重新建连。
		// 仅读有限长度，避免恶意上游返回超大错误体。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
		return forwardNotStarted
	}

	// ── 步骤 5：回写响应头 ──────────────────────────────────────
	// 上游返回的错误状态码（如 401、403）原样透传，便于客户端自助排查。
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// ── 步骤 6：流式拷贝响应体 ──────────────────────────────────
	// 同时把内容喂给抓取器，用于事后解析 usage（token 数）。
	sniffer := newUsageSniffer()
	flushCopy(w, resp.Body, sniffer)

	// 记录用量。此时响应已完整回传，写库不会影响客户端感知的延迟。
	// 若未取到 usage（部分上游流式响应不返回），token 记为 0，
	// 但请求数、成功率与耗时仍然准确——统计不至于因缺一项而完全不可用。
	usage, _ := extractUsage(sniffer.Bytes())
	identity := identityFromRequest(req.Context())
	r.recordUsage(req.Context(), usageEntry{
		UserID:     identity.UserID,
		TokenID:    identity.TokenID,
		ChannelID:  ch.ID,
		Model:      modelName,
		Usage:      usage,
		LatencyMS:  int(time.Since(start).Milliseconds()),
		IsStream:   oai.PeekStream(body),
		StatusCode: resp.StatusCode,
	})

	return forwardResponded
}

// maxDrainBytes 是重试前丢弃上游响应体的最大读取量。
//
// 取值 64KiB：典型错误体只有几百字节，64KiB 足够读完以维持连接复用，
// 同时避免异常上游用超大响应体拖慢或撑爆网关。
const maxDrainBytes = 64 << 10

// isRetryableStatus 判断上游状态码是否值得换渠道重试。
//
// 判定原则："换一个渠道很可能成功"才重试：
//   - 429：限流/额度不足，换渠道通常立即改善；
//   - 500/502/503/504：上游服务端故障；
//   - 529：上游过载（Anthropic 等厂商的约定状态码）。
//
// 明确不重试的常见状态码及原因：
//   - 400/422：请求参数有误，换渠道同样失败；
//   - 401/403：渠道密钥无效或权限不足，换渠道无意义（且应触发渠道健康标记）；
//   - 404：模型不存在，属于配置问题；
//   - 408/499：客户端侧问题。
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		529:                            // 上游过载（非标准码，厂商约定）
		return true
	default:
		return false
	}
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
// 参数 tee 可为 nil；非 nil 时会把内容同时写入它（用于抓取响应以解析 usage）。
// tee 的写入始终不返回错误（见 usageSniffer.Write 的实现），因此不会干扰转发。
//
// 为什么必须 Flush：大模型流式回答依赖 SSE，若数据被缓冲在网关或 HTTP 层，
// 客户端的体验会从"逐字出现"退化为"等全文生成完再一次性蹦出来"，
// 与不经网关直连相比是明显的体验倒退。
func flushCopy(w http.ResponseWriter, src io.Reader, tee io.Writer) {
	// gin 的 ResponseWriter 实现了 http.Flusher；用类型断言兼容不支持刷新的实现
	flusher, canFlush := w.(http.Flusher)

	buf := make([]byte, copyBufferBytes)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if tee != nil {
				_, _ = tee.Write(buf[:n])
			}
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
