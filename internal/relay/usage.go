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
	"strings"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/corpus"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/reqctx"
)

// openAIUsage 对应一次调用最终采用的用量（内部统一为 OpenAI 口径）。
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens 是输入中命中上游缓存的 token 数（0 表示上游未提供该维度）。
	//
	// 单独采集的原因：这部分通常按更低价计费，是核对账单与评估
	// "提示词前缀复用"收益的唯一依据。
	CachedTokens int `json:"cached_tokens"`
	// ReasoningTokens 是输出中属于推理过程的 token 数（0 表示上游未提供）。
	//
	// 它计入输出但使用者看不到，是出账争议的主要来源，必须单独可查。
	ReasoningTokens int `json:"reasoning_tokens"`
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
	// 嵌套明细：各上游的字段位置并不统一（OpenAI 放 prompt_tokens_details，
	// DeepSeek 等也放同一处；Anthropic 则直接给 cache_read_input_tokens），
	// 因此两处都解析，取到非零值即采用。
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// Anthropic 口径的缓存命中字段（写在 usage 顶层）。
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
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
	// 缓存命中：优先 OpenAI 的嵌套字段，其次 Anthropic 的顶层字段
	usage.CachedTokens = r.PromptTokensDetails.CachedTokens
	if usage.CachedTokens == 0 {
		usage.CachedTokens = r.CacheReadInputTokens
	}
	usage.ReasoningTokens = r.CompletionTokensDetails.ReasoningTokens
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		return openAIUsage{}, false
	}
	return usage, true
}

// usageMarker 是响应中承载用量的字段名（对象内部的键）。
const usageMarker = `"usage"`

// "等待一个 usage 对象接收完整"时允许保留的最大尾部字节数改为运行期可调
// （默认 1 MiB），取值与区间定义在 model.LimitSettings，
// 读取见 StreamLimitsNow().UsageTailBytes。
//
// 作用是内存兜底：一个 usage 对象绝不可能达到这个量级，因此一旦超过就说明
// 上游发来的并不是合法 JSON（或该对象永远不会闭合）。此时放弃等待、跳过该标记，
// 避免尾部随响应无限增长。

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
	// firstByteAt 是首个非空数据块到达的时刻（TTFB）。
	//
	// 为什么在这里记：抓取器是响应体流经网关的唯一必经之处，
	// 在这里打点不需要改动转发主循环，也不会漏掉任何一个分片。
	firstByteAt time.Time
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
	// 首个非空分片即"首字节"：TTFB 的语义就是"上游多久开始吐内容"。
	if u.firstByteAt.IsZero() {
		u.firstByteAt = time.Now()
	}
	u.tail = append(u.tail, p...)
	u.scan()
	return len(p), nil
}

// FirstByteAt 返回首个非空分片的时刻；零值表示没有任何响应体。
func (u *usageSniffer) FirstByteAt() time.Time {
	return u.firstByteAt
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
				if len(u.tail)-markerPos > StreamLimitsNow().UsageTailBytes {
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
//
// 更细的控制见 injectStreamUsageFor：**渠道级扩展参数可以覆盖本常量**
// （填 true 开启、填 false 强制关闭），这样"按量真实计费"的上游可以单独开启，
// 而不必让全部渠道一起承担兼容性风险。
const injectStreamUsageOption = false

// channelInjectStreamUsageKey 是渠道扩展参数里"是否注入 include_usage"的键名。
//
// 该键声明在 channeltype 的「自定义 OpenAI 兼容」类型上（后台可见可填）。
const channelInjectStreamUsageKey = "inject_stream_usage"

// injectStreamUsageFor 判断本次转发（针对某个渠道）是否注入 stream_options.include_usage。
//
// 优先级：**渠道级扩展参数 > 全局默认常量**。
//
// 为什么需要渠道级：这是"计费精度"与"兼容性"之间唯一的折中点——
//   - 开启：上游会在最后一个事件里带回 usage，流式调用的计费才精准；
//     但严格校验请求体的上游会因未知字段直接返回 400；
//   - 关闭：兼容性最好，但上游若默认不回 usage，流式调用只能按预留量估算，
//     对"按量真实计费"的上游就等于账对不上。
//
// 取值识别：true/1/yes/on 视为开启，false/0/no/off 视为强制关闭；
// 其它值（含空串、未配置）回退到全局常量。
func injectStreamUsageFor(ch *model.Channel) bool {
	if ch == nil {
		return injectStreamUsageOption
	}
	switch strings.ToLower(strings.TrimSpace(ch.ExtraConfig[channelInjectStreamUsageKey])) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return injectStreamUsageOption
	}
}

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

// tokensPerSecond 计算输出速率（tokens/s）。
//
// 口径：输出 token / (总耗时 − 首 token 延迟)。
//
// 为什么要扣掉首包时间：首包时长由上游排队与预填充决定，与"生成速度"无关。
// 若用总耗时做分母，同一个模型在长回答上会显示得更慢，
// 指标就失去可比性（这也是 new-api 等同类站点的通行口径）。
//
// 返回 0 表示无法计算：
//   - 没有输出 token（例如纯工具调用或失败响应）；
//   - 或扣除首包后剩余时长不足 minGenerateMS（响应在一个分片内返回完，无从测速）。
//
// minGenerateMS 这个下限不是"美化"，而是防错：只要分母是 1~2 毫秒，
// 哪怕只有几十个输出 token 也会算出几万 t/s 的荒谬速率（线上实测出现过 9.9 万 t/s）。
// 这种记录对使用者只有误导，因此按"无法计算"处理。
func tokensPerSecond(completionTokens, totalMS, firstTokenMS int) float64 {
	if completionTokens <= 0 || totalMS <= 0 {
		return 0
	}
	generateMS := totalMS
	// 仅当首包时间合理（>0 且小于总时长）时才扣除：
	// 非流式请求首包时间为 0（未采集），此时总耗时本身就是生成时长。
	if firstTokenMS > 0 && totalMS > firstTokenMS {
		generateMS = totalMS - firstTokenMS
	}
	if generateMS < minGenerateMS {
		return 0
	}
	return float64(completionTokens) * 1000 / float64(generateMS)
}

// minGenerateMS 是可测速的最短生成区间（毫秒）。
//
// 取 50ms：正常流式回答的生成阶段都在数百毫秒以上，而"看起来像测速、
// 实际只是测量误差"的区间普遍在几毫秒内。用它做门槛可以在不误伤正常数据的前提下
// 挡掉由取整误差放大出来的荒谬速率。
const minGenerateMS = 50

// usageEntry 描述一条待记录的用量。
type usageEntry struct {
	UserID  uint64
	TokenID uint64
	// Group 是本次请求的分组（由转发层解析一次后透传）。
	//
	// 为什么要把分组一路带到这里：计费必须用"与选渠道相同的分组"，
	// 若在此处重新解析，可能因来源不同而与渠道选择不一致，造成错账。
	// 空字符串表示未指定，由 Billing 回退到默认分组。
	Group     string
	ChannelID uint64
	// ChannelKeyID 是本次实际使用的池内密钥记录 ID（0 = 单密钥模式或未采集）。
	//
	// 它让"这把密钥被用了多少"成为可计算的事实，进而能结合上游进价
	// 算出余额剩余；失败请求不计消耗，因此失败路径允许留 0。
	ChannelKeyID uint64
	Model        string
	// UpstreamModel 是本次实际发给上游的模型名（映射改写后的名字）。
	//
	// 语义约定：无映射或映射未改变模型名时为空串（表示"上游模型 = 对外模型"），
	// 只有真正经过映射改写时才填入。这样日志既能一眼看出"哪些请求走了映射"，
	// 又不会让绝大多数行重复记录同一个名字。
	UpstreamModel string
	Usage         openAIUsage
	LatencyMS     int
	// FirstTokenMS 是首个响应分片的到达时间（TTFB）；0 = 未采集（无响应体）。
	FirstTokenMS int
	// TokensPerSecond 是输出速率；0 = 无法计算（无输出 token 或时长不可用）。
	TokensPerSecond float64
	IsStream        bool
	StatusCode      int
	ErrorText       string
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

	// 记录用的 request_id：优先复用鉴权阶段生成的（做了预留时非空）。
	//
	// 未做预留的调用（免费模型 / 福利账户 / 信任额度旁路）在鉴权阶段不会生成它，
	// 但调用日志与语料样本仍需要一个能互相关联的标识，否则"这次花了多少"与
	// "这次说了什么"就对不上账。因此这里补生成一个。
	//
	// 【重要】补出来的这个只用于记录，绝不参与结算：结算读的是
	// identityFromRequest(ctx).RequestID（为空即表示没做过预留，走反响扣费路径）。
	recordRequestID := identityFromRequest(ctx).RequestID
	if recordRequestID == "" {
		recordRequestID = model.NewRequestID()
	}

	logEntry := &model.UsageLog{
		UserID:           entry.UserID,
		TokenID:          entry.TokenID,
		ChannelID:        entry.ChannelID,
		ChannelKeyID:     entry.ChannelKeyID,
		Model:            entry.Model,
		UpstreamModel:    entry.UpstreamModel,
		PromptTokens:     entry.Usage.PromptTokens,
		CompletionTokens: entry.Usage.CompletionTokens,
		TotalTokens:      entry.Usage.TotalTokens,
		CachedTokens:     entry.Usage.CachedTokens,
		ReasoningTokens:  entry.Usage.ReasoningTokens,
		FirstTokenMS:     entry.FirstTokenMS,
		TokensPerSecond:  entry.TokensPerSecond,
		LatencyMS:        entry.LatencyMS,
		IsStream:         entry.IsStream,
		StatusCode:       entry.StatusCode,
		Error:            entry.ErrorText,
		// request_id 是"这一次调用"的唯一标识，也是把调用日志与语料样本对上账的
		// 唯一依据（此前该列从未被写入，等于死列，一并在此补上）。
		RequestID: recordRequestID,
		CreatedAt: time.Now(),
	}
	// 计费：优先走"预留 → 结算/退还"（鉴权阶段已预扣），未预留时退化为响应后扣费。
	//
	// 放在写日志之前，这样日志里的 quota 就是本次真实计入额度——
	// 若先写日志再计费，日志中的额度会永远是 0（这正是此前的缺陷）。
	//
	// 计费失败不会影响客户端：本方法内部只记录错误不返回错误，
	// 用户不该因为"记账失败"而收到一个报错。
	logEntry.Quota = r.settleQuota(writeCtx, entry)
	// 定价版本快照：记录本次扣费所依据的计价规则版本（规则 ID@更新时间），
	// 使日后改价后旧账仍可按当时的定价复算。取价与 settleQuota 同源（渠道专用价优先）。
	if r.billing != nil {
		logEntry.PriceVersion = r.billing.PriceVersionForChannel(writeCtx, entry.Group, entry.Model, entry.ChannelID)
	}

	if err := r.usageLogs.Create(writeCtx, logEntry); err != nil {
		// 日志写入失败不影响用户，但要留下痕迹便于排查
		slog.Warn("写入调用日志失败", "error", err, "model", entry.Model, "channel_id", entry.ChannelID)
	}

	// 语料共建：把本次请求的原文落库（仅在入口判定命中采集清单时才有缓冲）。
	r.saveCorpusSample(writeCtx, ctx, entry, recordRequestID)

	// 更新令牌最近使用时间：让使用者能辨认哪些 key 还在用
	if entry.TokenID > 0 && r.tokens != nil {
		if err := r.tokens.RecordUsage(writeCtx, entry.TokenID, time.Now()); err != nil {
			slog.Warn("更新令牌使用时间失败", "error", err, "token_id", entry.TokenID)
		}
	}
}

// saveCorpusSample 把本次请求的原文写入语料样本表（语料共建计划）。
//
// 前置条件：入口处已判定该模型在采集清单内，并把 Recorder 挂到了请求上下文上。
// 两条都不满足时直接返回——绝大多数请求走的就是这条零开销路径。
//
// requestID 由调用方传入（与调用日志同值），保证两张表能一一对上。
//
// 只对成功响应建档：错误正文对语料没有价值，而且上游的报错里常带内部信息
// （渠道名、上游地址），采下来反而是负担。
//
// 与 recordUsage 一样，本方法对错误只记日志、绝不向上抛：
// 它发生在响应已回传之后，采集出问题不该让一次正常调用变成失败。
func (r *Relay) saveCorpusSample(writeCtx, reqCtx context.Context, entry usageEntry, requestID string) {
	if r.corpusSamples == nil {
		return
	}
	recorder := corpus.RecorderFrom(reqCtx)
	if recorder == nil {
		return
	}
	if entry.StatusCode >= http.StatusBadRequest {
		return
	}

	snapshot := recorder.Snapshot()
	sample := &model.CorpusSample{
		RequestID:     requestID,
		UserID:        entry.UserID,
		TokenID:       entry.TokenID,
		Model:         entry.Model,
		UpstreamModel: entry.UpstreamModel,
		ChannelID:     entry.ChannelID,
		ChannelKeyID:  entry.ChannelKeyID,
		IsStream:      entry.IsStream,
		StatusCode:    entry.StatusCode,
		RequestBody:   snapshot.RequestBody,
		ResponseBody:  snapshot.ResponseBody,
		RequestBytes:  snapshot.RequestBytes,
		ResponseBytes: snapshot.ResponseBytes,
		Truncated:     snapshot.Truncated,
		Incomplete:    snapshot.Incomplete,
		CreatedAt:     time.Now(),
	}
	if err := r.corpusSamples.CreateCorpusSample(writeCtx, sample); err != nil {
		if errors.Is(err, model.ErrCorpusSampleExists) {
			// 幂等：同一次请求只会留一条样本，重试不算异常。
			return
		}
		slog.Warn("写入语料样本失败（不影响本次调用）",
			"error", err, "model", entry.Model, "request_id", sample.RequestID)
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
//     成功 → 沿用旧的响应后扣费 Charge（未定价模型 Charge 会返回 0）；
//     失败（HTTP >= 400）→ 直接返回 0，不扣费（与预留路径的 Release 语义对齐）。
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
		// 未预留：退化路径（模型不计费 / 信任额度旁路 / 未启用台账）。
		//
		// 关键修复（2026-10-03 线上事故）：本路径【必须一并判断 HTTP 状态码】。
		// 旧实现无条件 Charge，于是"账号额度充足 → 命中信任额度旁路、不建预留"的
		// 失败请求（上游 4xx/5xx）照样被扣费；而做过预留的失败请求却走 Release
		// 全额退还——两条路径语义不一致，表现为"接口报错还计费"。
		// 现与预留路径对齐：失败（>=400）一律不计费，直接返回 0。
		if entry.StatusCode >= http.StatusBadRequest {
			return 0
		}
		// Charge 内部同样只记录错误不返回错误。
		// 计费分组沿用 entry.Group（与选渠道同一分组），空值由 Billing 回退到默认分组。
		// 同时带上 entry.ChannelID：本次实际命中的渠道可享受"渠道专用价优先"的取值。
		charged := r.billing.ChargeForChannel(ctx, entry.Group, entry.UserID, entry.TokenID, entry.Model,
			int64(entry.Usage.PromptTokens), int64(entry.Usage.CompletionTokens),
			int64(entry.Usage.CachedTokens), entry.ChannelID)
		// 折扣分组的重试率统计：本次产生了计费（charged > 0），记一笔"计费请求"。
		// 上游调用次数已在 forwardChat 的每次真实调用时累加（见 openai.go），
		// 两者相除即真实重试率 r = 上游调用次数 / 计费请求次数。
		if charged > 0 {
			r.billing.recordUpstreamCall(entry.Group, true)
		}
		return charged
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
		// 结算价同样按"渠道专用价优先"取：与 Charge 路径口径一致，
		// 否则会出现"按渠道 A 的专用价预留、却按分组默认价结算"的错账。
		actual = r.billing.QuoteForChannel(ctx, entry.Group, entry.Model,
			int64(entry.Usage.PromptTokens), int64(entry.Usage.CompletionTokens),
			int64(entry.Usage.CachedTokens), entry.ChannelID)
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
	// 折扣分组的重试率统计：本次产生了计费（settled > 0），记一笔"计费请求"。
	if reservation.Settled > 0 {
		r.billing.recordUpstreamCall(entry.Group, true)
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
