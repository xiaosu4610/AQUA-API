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
//   - 【token 数尽力而为】非流式响应可解析 usage 字段；流式响应需从 SSE 分片里找，
//     部分上游不返回 usage，此时 token 记为 0 但请求数与成功率仍然准确。
//     这是刻意的取舍：为了拿 token 数去改客户端请求体（注入 stream_options）
//     会引入兼容性风险，得不偿失。
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
	"log/slog"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/reqctx"
)

// usageCaptureLimit 是抓取响应内容用于解析 usage 的最大字节数。
//
// 只关心末尾的 usage 对象，但流式响应可能很长；保留 256KB 已足够覆盖绝大多数响应，
// 同时避免大响应把内存占满。
const usageCaptureLimit = 256 << 10

// openAIUsage 对应 OpenAI 响应中的 usage 字段。
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// usageSniffer 是一个 io.Writer，用于在流式拷贝响应体的同时抓取内容片段，
// 供后续解析 usage。写入长度超过上限后自动停止记录（不影响正常转发）。
type usageSniffer struct {
	buf   bytes.Buffer
	limit int
}

// newUsageSniffer 创建抓取器。
func newUsageSniffer() *usageSniffer {
	return &usageSniffer{limit: usageCaptureLimit}
}

// Write 实现 io.Writer：始终返回全部写入长度（不能让上游拷贝因抓取失败而中断）。
func (u *usageSniffer) Write(p []byte) (int, error) {
	if remaining := u.limit - u.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			u.buf.Write(p)
		} else {
			u.buf.Write(p[:remaining])
		}
	}
	return len(p), nil
}

// Bytes 返回已抓取的内容。
func (u *usageSniffer) Bytes() []byte {
	return u.buf.Bytes()
}

// extractUsage 从响应（或 SSE 分片流）中提取 usage 信息。
//
// 实现思路：扫描全部 `"usage"` 出现位置，逐个尝试解析其后的 JSON 对象，
// 取最后一个解析成功且非零的结果。
//
// 为什么要遍历全部而不仅看最后一处：OpenAI 流式响应会在中间分片写 `"usage":null`，
// 真正的 usage 在最后；而个别兼容实现会把 null 放在最后。遍历可兼容两种情况。
func extractUsage(raw []byte) (openAIUsage, bool) {
	const marker = `"usage"`

	var (
		found openAIUsage
		ok    bool
	)
	searchFrom := 0
	for {
		idx := bytes.Index(raw[searchFrom:], []byte(marker))
		if idx < 0 {
			break
		}
		absIdx := searchFrom + idx
		searchFrom = absIdx + len(marker)

		rest := raw[searchFrom:]
		start := bytes.IndexByte(rest, '{')
		if start < 0 {
			continue
		}

		var parsed openAIUsage
		decoder := json.NewDecoder(bytes.NewReader(rest[start:]))
		if err := decoder.Decode(&parsed); err != nil {
			// 该处不是合法对象（如 usage 为 null），继续找下一处
			continue
		}
		if parsed.PromptTokens == 0 && parsed.CompletionTokens == 0 && parsed.TotalTokens == 0 {
			// 全零视为无效（多为占位对象），继续找下一处
			continue
		}
		found = parsed
		ok = true
	}
	return found, ok
}

// usageEntry 描述一条待记录的用量。
type usageEntry struct {
	UserID     uint64
	TokenID    uint64
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
	// TODO(relay): 接入计费后在此按倍率换算 quota
	logEntry.Quota = 0

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
