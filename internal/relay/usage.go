// 本文件负责在转发完成后记录调用用量。
//
// 意图（Why）：
//
//	仪表盘与用户门户的所有统计（今日请求数、趋势、模型排行、成功率）都来自调用日志。
//	若只转发不记录，后台就是空壳。因此每次转发（无论成功失败）都应落一条日志。
//
// 关键取舍：
//   - 【不阻断响应】日志写入发生在响应体已回传给客户端之后，即使写库失败，
//     用户也不该受影响（宁可少一条统计，也不能让调用失败）；
//   - 【token 数尽力而为，但绝不静默记 0】usage 由 usageSniffer 【增量】解析：
//     无论响应多长（OpenAI 兼容协议把 usage 放在流的最后一个事件里），
//     只要上游返回过 usage 就能取到；确实拿不到时，token 记为 0，
//     但会在日志的失败原因列标注"未取得 usage"，让计费缺口可见、可追。
//   - 【不默认改写请求体】注入 stream_options.include_usage 能提升流式 usage 的
//     返回率，但该字段并非所有 OpenAI 兼容上游都认识（严格校验者会直接 400）。
//     因此默认关闭，仅保留开关与实现（见 injectStreamUsageOption）。
//
// 流转（Flow）：
//
//	forwardChat 结束 → recordUsage(entry)
//	  ├─ 解析响应中提取到的 usage（见 usageSniffer）
//	  ├─ 写入 usage_logs
//	  └─ 更新令牌的 last_used_at
//
// 扩展（Extend）：
//
//	接入计费后：在此处按上游价格与倍率把 tokens 换算为 quota 写入，
//	  并把"额度扣减"与"日志写入"纳入同一事务或补偿流程。
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
)

// openAIUsage 对应一次调用最终采用的用量（内部统一为 OpenAI 口径）。
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// rawUsage 是 usage 对象的宽松表示，用于兼容不同上游的字段命名：
//   - OpenAI 风格：prompt_tokens / completion_tokens / total_tokens
//   - Anthropic 风格：input_tokens / output_tokens
//
// 之所以要兼容两套字段：部分 OpenAI 兼容网关（尤其是代理 Claude 的实现）
// 会原样回吐 Anthropic 口径的 usage；若只认 prompt_tokens，这些调用会被记 0。
type rawUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

// normalize 把宽松表示归一化为内部口径。
//
// 返回 ok=false 表示该对象"全零"（流式响应里常见的占位对象 `"usage":null`
// 或 `{"prompt_tokens":0,...}`），应被忽略而不是当作有效用量。
func (r rawUsage) normalize() (openAIUsage, bool) {
	usage := openAIUsage{
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
	}
	// Anthropic 口径回退：仅在本字段为空时采用，避免覆盖 OpenAI 口径的显式值
	if usage.PromptTokens == 0 {
		usage.PromptTokens = r.InputTokens
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = r.OutputTokens
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		return openAIUsage{}, false
	}
	return usage, true
}

// usageMarker 是响应中承载用量的字段名（对象内部的键）。
const usageMarker = `"usage"`

// usageTailMaxBytes 是"等待一个 usage 对象接收完整"时允许保留的最大尾部字节数。
//
// 作用是内存兜底：一个 usage 对象绝不可能达到这个量级，因此一旦超过就说明
// 上游发来的并不是合法 JSON（或该对象永远不会闭合）。此时放弃等待、跳过该标记，
// 避免尾部随响应无限增长。
const usageTailMaxBytes = 1 << 20

// usageSniffer 是一个 io.Writer：在响应体流经网关的同时【增量】解析其中的 usage。
//
// 为什么必须增量（这是本文件的核心）：
//
//	OpenAI 兼容协议把 usage 放在流的【最后一个】事件里，而回答本身可能有数 MB。
//	旧实现只保留响应开头的固定字节数（256KB），长回答末尾的 usage 会被整段丢弃，
//	于是 extractUsage 返回零值、该次调用被按 0 token 计费——直接造成收入流失。
//
// 内存有界：
//
//	解析器只保留"尚未解析完的尾部"（通常是一个 SSE 事件，几十到几百字节），
//	已消费的内容在每次扫描后立即丢弃，因此占用不随响应总长度增长。
//
// 兼容多种形态：
//
//	按字段名 `"usage"` 定位、按 JSON 对象解析，因此不关心它嵌在何处——
//	顶层 usage、choices[].usage、Anthropic 风格 message.usage 都能取到；
//	字段名同时兼容 prompt_tokens/completion_tokens 与 input_tokens/output_tokens。
//
// 并发：仅在单个转发 goroutine 内被 Write，无需加锁。
type usageSniffer struct {
	tail  []byte      // 尚未解析完的尾部（可能含未闭合的 usage 对象）
	usage openAIUsage // 最后一次解析到的非空 usage
	found bool
}

// newUsageSniffer 创建抓取器。
func newUsageSniffer() *usageSniffer {
	return &usageSniffer{}
}

// Write 实现 io.Writer：始终返回全部写入长度（抓取绝不能因解析失败而中断转发）。
func (u *usageSniffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	u.tail = append(u.tail, p...)
	u.scan()
	return len(p), nil
}

// Usage 返回解析到的最佳用量：最后一次非空 usage；found 为 false 表示整段响应里没有用量。
func (u *usageSniffer) Usage() (openAIUsage, bool) {
	return u.usage, u.found
}

// scan 在尾部缓冲中增量查找并解析 usage 对象。
//
// 算法：从上次消费位置起查找 `"usage"` 标记 → 跳过冒号与空白 → 尝试解析其后的
// JSON 对象。解析成功则记录（并越过该对象继续找，取最后一次非空值）；
// 对象尚未接收完整（截断）则保留尾部等待下次 Write；不是对象（如 null）
// 或非法 JSON 则跳过该标记继续查找。已消费部分一律丢弃，保证内存有界。
func (u *usageSniffer) scan() {
	searchFrom := 0
	for {
		rel := bytes.Index(u.tail[searchFrom:], []byte(usageMarker))
		if rel < 0 {
			// 之后不再有标记：丢弃已扫描内容，仅保留可能被拆散的标记前缀
			u.discardBefore(len(u.tail) - (len(usageMarker) - 1))
			return
		}
		markerPos := searchFrom + rel
		valueStart := markerPos + len(usageMarker)

		// 跨过 `:` 与空白，定位对象的起始 '{'
		i := valueStart
		for i < len(u.tail) && isUsageSeparator(u.tail[i]) {
			i++
		}
		if i >= len(u.tail) {
			// 字段名已到达但取值还没来：从头保留，等待后续写入
			u.discardBefore(markerPos)
			return
		}
		if u.tail[i] != '{' {
			// 取值不是对象（如 `"usage":null`）：跳过该标记继续找
			searchFrom = valueStart
			continue
		}

		var raw rawUsage
		decoder := json.NewDecoder(bytes.NewReader(u.tail[i:]))
		if err := decoder.Decode(&raw); err != nil {
			if isTruncatedJSON(err) {
				if len(u.tail)-markerPos > usageTailMaxBytes {
					// 超过兜底上限：上游发的不是合法 JSON，跳过以免尾部无限增长
					searchFrom = valueStart
					continue
				}
				u.discardBefore(markerPos)
				return
			}
			// 非法 JSON：跳过该标记继续找
			searchFrom = valueStart
			continue
		}
		if usage, ok := raw.normalize(); ok {
			// 取最后一次非空值：流式响应可能先后出现多个 usage 事件
			u.usage = usage
			u.found = true
		}
		// 越过已解析的对象继续查找
		searchFrom = i + int(decoder.InputOffset())
		if searchFrom >= len(u.tail) {
			u.discardBefore(searchFrom)
			return
		}
	}
}

// discardBefore 丢弃尾部缓冲中 offset 之前的内容。
//
// 用 copy 把保留部分前移到切片头部（而非重新分配），避免底层数组随响应增长
// 不断膨胀；容量上限由"单个未闭合 usage 对象的大小"决定，与响应总长无关。
func (u *usageSniffer) discardBefore(offset int) {
	if offset <= 0 {
		return
	}
	if offset >= len(u.tail) {
		u.tail = u.tail[:0]
		return
	}
	n := copy(u.tail, u.tail[offset:])
	u.tail = u.tail[:n]
}

// isUsageSeparator 判断字节是否属于"字段名与取值之间"的合法分隔符。
func isUsageSeparator(c byte) bool {
	switch c {
	case ':', ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

// isTruncatedJSON 判断解析错误是否表示"JSON 尚未接收完整"（而非内容非法）。
//
// 之所以要区分：不完整要保留尾部等待下一次写入；非法则应跳过该标记，
// 否则一个坏对象会让解析器原地卡死、永远等不到后面的正确 usage。
func isTruncatedJSON(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// extractUsage 是一次性解析入口：对"已完整拿到"的响应体（非流式 JSON）解析 usage。
//
// 流式响应请使用 usageSniffer 增量解析——它对长回答不会保留整段内容。
func extractUsage(raw []byte) (openAIUsage, bool) {
	sniffer := newUsageSniffer()
	_, _ = sniffer.Write(raw)
	return sniffer.Usage()
}

// injectStreamUsageOption 控制是否为"流式对话请求"注入
// `stream_options: {"include_usage": true}`。
//
// 当前【默认关闭】，原因如下（刻意的取舍）：
//   - 该字段是 OpenAI 后来新增的约定，并非所有 OpenAI 兼容上游都认识它；
//     遇到严格校验请求体的实现会直接返回 400，把一次本可成功的调用打挂。
//   - 网关面对大量第三方/自建上游，无法逐一确认其兼容性，因此不能默认开启。
//
// 关闭的代价：上游若默认不返回 usage，流式调用的 token 只能记 0
//
//	（此时日志会标注"未取得 usage"）；非流式调用不受影响。
//
// 若要启用：把常量改成 true 即可。withStreamUsageOption 已限定
//
//	"仅流式 + 客户端未显式指定 stream_options"时才注入，并会尊重客户端已有设置。
const injectStreamUsageOption = false

// withStreamUsageOption 在 enabled 且请求体确实是"未指定 stream_options 的流式请求"时，
// 注入 include_usage，让上游在最后一个事件里带上 usage。
//
// 返回原切片（不改动）的情况：功能开关关闭、请求体非法、非流式请求、
// 客户端已显式指定 stream_options（此时必须尊重客户端意图，不能覆盖）。
//
// 注意：注入会重新序列化请求体，键顺序可能变化——这也是默认关闭的原因之一。
func withStreamUsageOption(body []byte, enabled bool) []byte {
	if !enabled {
		return body
	}

	var probe struct {
		Stream        *bool           `json:"stream"`
		StreamOptions json.RawMessage `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return body
	}
	if probe.Stream == nil || !*probe.Stream {
		return body
	}
	if len(probe.StreamOptions) > 0 {
		return body
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	patched, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return patched
}

// usageMissingNote 是"本次调用未取得 usage"的日志标注。
//
// 为什么需要它：上游不返回 usage 时 token 只能记 0，若不留痕就会被静默计 0 费，
// 站长无从发现计费缺口。写入调用日志的失败原因列（该列仅作提示，
// 成功/失败统计依据的是 status_code，不受影响）。
const usageMissingNote = "未取得 usage（上游未返回用量，token 记 0）"

// usageEntry 描述一条待记录的用量。
type usageEntry struct {
	UserID  uint64
	TokenID uint64
	// Group 是本次请求的分组（由转发层解析一次后透传）。
	//
	// 为什么要把分组一路带到这里：计费必须用"与选渠道相同的分组"，
	// 若在此处重新解析，可能因来源不同而与渠道选择不一致，造成错账。
	// 空字符串表示未指定，由 Billing 回退到默认分组。
	Group      string
	ChannelID  uint64
	Model      string
	Usage      openAIUsage
	LatencyMS  int
	IsStream   bool
	StatusCode int
	ErrorText  string
}

// recordUsage 记录调用用量。
//
// 设计约束：本方法绝不影响客户端响应——它可能在响应已回传后执行，
// 因此内部对错误只做记录，不向上返回。
func (r *Relay) recordUsage(ctx context.Context, entry usageEntry) {
	if r.usageLogs == nil {
		return
	}

	// 用独立的超时 context：原请求的 context 可能已因客户端断开而取消，
	// 若直接复用会导致"用户取消请求 → 日志丢失"，而这恰恰是最需要记录的情况之一。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	logEntry := &model.UsageLog{
		UserID:           entry.UserID,
		TokenID:          entry.TokenID,
		ChannelID:        entry.ChannelID,
		Model:            entry.Model,
		PromptTokens:     entry.Usage.PromptTokens,
		CompletionTokens: entry.Usage.CompletionTokens,
		TotalTokens:      entry.Usage.TotalTokens,
		LatencyMS:        entry.LatencyMS,
		IsStream:         entry.IsStream,
		StatusCode:       entry.StatusCode,
		Error:            entry.ErrorText,
		CreatedAt:        time.Now(),
	}
	// 计费：优先走"预留 → 结算/退还"（鉴权阶段已预扣），未预留时退化为响应后扣费。
	//
	// 放在写日志之前，这样日志里的 quota 就是本次真实计入额度——
	// 若先写日志再计费，日志中的额度会永远是 0（这正是此前的缺陷）。
	//
	// 计费失败不会影响客户端：本方法内部只记录错误不返回错误，
	// 用户不该因为"记账失败"而收到一个报错。
	logEntry.Quota = r.settleQuota(writeCtx, entry)

	if err := r.usageLogs.Create(writeCtx, logEntry); err != nil {
		// 日志写入失败不影响用户，但要留下痕迹便于排查
		slog.Warn("写入调用日志失败", "error", err, "model", entry.Model, "channel_id", entry.ChannelID)
	}

	// 更新令牌最近使用时间：让使用者能辨认哪些 key 还在用
	if entry.TokenID > 0 && r.tokens != nil {
		if err := r.tokens.RecordUsage(writeCtx, entry.TokenID, time.Now()); err != nil {
			slog.Warn("更新令牌使用时间失败", "error", err, "token_id", entry.TokenID)
		}
	}
}

// identityFromRequest 从请求 context 中取出调用者身份。
//
// 取不到时返回零值：表示该请求未经令牌鉴权（如内部调用或未来开放模式），
// 日志中表现为 user_id=0、token_id=0，语义清晰且不会误归属到某个用户。
func identityFromRequest(ctx context.Context) reqctx.Identity {
	identity, _ := reqctx.IdentityFrom(ctx)
	return identity
}

// settleQuota 结算本次调用的额度，返回应写入日志的"实际计入额度"。
//
// 两条路径（由鉴权阶段是否做了预留决定）：
//
//  1. 做过预留（context 中带 requestID）：
//     成功 → Settle（按实际 usage 多退少补；拿不到 usage 时按预留量收，绝不退成 0）；
//     失败（HTTP >= 400）→ Release 全额退还。
//  2. 未做预留（模型不计费 / 信任额度旁路 / 未启用）：
//     沿用旧的响应后扣费 Charge（未定价模型 Charge 会返回 0）。
//
// 关键约束：本方法绝不向上返回错误——它发生在响应已回传之后，
// 用户不该因为"记账失败"而收到报错；但失败必须留下【错误级别】日志，
// 因为额度错账属于必须被发现的资损风险，不能静默 warn。
func (r *Relay) settleQuota(ctx context.Context, entry usageEntry) int64 {
	if r.billing == nil {
		return 0
	}

	requestID := identityFromRequest(ctx).RequestID
	if requestID == "" {
		// 未预留：退化路径。Charge 内部同样只记录错误不返回错误。
		// 计费分组沿用 entry.Group（与选渠道同一分组），空值由 Billing 回退到默认分组。
		return r.billing.Charge(ctx, entry.Group, entry.UserID, entry.TokenID, entry.Model,
			int64(entry.Usage.PromptTokens), int64(entry.Usage.CompletionTokens))
	}

	// 请求失败（含上游 4xx/5xx、无可用渠道等）：全额退还预扣额度。
	if entry.StatusCode >= http.StatusBadRequest {
		if err := r.billing.Release(ctx, requestID); err != nil {
			slog.Error("退还预留额度失败（用户额度可能被误扣，需人工核查）",
				"error", err, "request_id", requestID,
				"user_id", entry.UserID, "token_id", entry.TokenID, "model", entry.Model)
		}
		return 0
	}

	// 成功：按实际用量结算。拿不到 usage 时传 QuotaUnknown（按预留量收）。
	actual := int64(model.QuotaUnknown)
	if hasUsage(entry.Usage) {
		actual = r.billing.Quote(ctx, entry.Group, entry.Model,
			int64(entry.Usage.PromptTokens), int64(entry.Usage.CompletionTokens))
	}

	reservation, err := r.billing.Settle(ctx, requestID, actual)
	if err != nil {
		// 结算失败：不阻断用户，但必须记错误级别日志（额度可能停留在预扣值）。
		slog.Error("结算预留额度失败（额度可能不准，需人工核查）",
			"error", err, "request_id", requestID,
			"user_id", entry.UserID, "token_id", entry.TokenID, "model", entry.Model)
		return 0
	}
	if reservation == nil {
		return 0
	}
	if actual >= 0 && reservation.Settled != actual {
		// 补扣受限（可用额度不足）：此时按可用量扣减，账目有缺口，必须可被发现。
		slog.Error("结算补扣受限：实际入账低于应扣额度",
			"request_id", requestID, "want", actual, "settled", reservation.Settled,
			"user_id", entry.UserID, "model", entry.Model)
	}
	return reservation.Settled
}

// hasUsage 判断是否取得了有效用量（全零视为未取得）。
func hasUsage(usage openAIUsage) bool {
	return usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0
}
