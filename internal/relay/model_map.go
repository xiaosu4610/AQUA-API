// 本文件把「渠道级模型 ID 映射」真正落到转发链路上：请求方向把对外模型名改写为
// 上游模型名，响应方向把上游回包的模型名回写为对外模型名，并在转发前缓存映射列表。
//
// 意图（Why）：
//
//	映射此前只做了存储与后台接口，转发链路完全没用上——用户请求 model=B，
//	上游收到的仍是 B，映射成了"看得见、不起作用"的半成品。本文件补齐这最后一公里：
//	  1) 请求改写：命中 B→A 映射时，向上游发出的请求体 model 字段改为 A；
//	     上游路径模板里的 {model}/{deployment} 也一并使用 A（Azure 部署名、
//	     Gemini 路径都依赖它）。
//	  2) 响应回写：上游回包里的 model 是 A（或其变体），回写给客户端时改回 B，
//	     否则客户端会看到"我请求的是 B，却收到 A"这种不一致。
//	  3) 缓存：映射是"配置类"数据且位于转发热路径，故按渠道 ID 做 TTL 缓存，
//	     避免每次转发都查库；查库失败一律退化为"无映射"，绝不成为链路的单点故障。
//
// 关键取舍（重要，改动前先读）：
//   - 【无映射时逐字节零影响】：只有命中映射且确实改变了模型名时，才会重新序列化
//     请求体；未命中时原样透传原始字节，不因 JSON 重排/空白变化而改变上游可见行为。
//   - 【流式响应不整段缓冲】：SSE 以 \n\n 分帧、无长度前缀，故逐行增量改写首个含
//     model 的数据事件后再直通；绝不 io.ReadAll 整段流，否则会破坏"逐字输出"。
//   - 【响应只改写非流式的整体 JSON】：非流式响应是单个 JSON 对象，整体替换 model
//     值即可；超过读取上限时放弃改写并原样继续，避免为超大响应冒内存风险。
//
// 流转（Flow）：
//
//	forwardChat
//	  ├─ resolveUpstreamModel(ctx, ch.ID, publicModel, body)
//	  │    ├─ modelMappingsForChannel → 缓存命中/查库（失败则降级为无映射）
//	  │    ├─ model.ResolveMapping(mappings, publicModel) → upstreamModel
//	  │    └─ rewriteRequestModel(body, upstreamModel)     → 改写后的请求体
//	  ├─ prepareChannelUpstream(..., upstreamModel, outboundBody, ...)
//	  └─ 回写前：命中映射时改写响应体 model → publicModel
//	       ├─ 流式：newStreamModelRewriter（逐行，不缓冲整段）
//	       └─ 非流式：rewriteNonStreamModel（整体 JSON 替换）
//
// 扩展（Extend）：
//
//	后台改完映射要立即生效：调用 Relay.InvalidateChannelModelMappings(channelID)
//	或 Relay.InvalidateAllModelMappings()（否则最多等一个 TTL 周期）。
//	调整缓存规模：改 defaultMappingCacheTTL / defaultMappingCacheMax。
package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 映射缓存的默认参数。
const (
	// defaultMappingCacheTTL 是单条映射缓存的有效期。
	//
	// 取 60 秒：映射是极少变更的配置，1 分钟的陈旧窗口对业务无影响；
	// 同时后台改完映射可主动失效（见 InvalidateChannelModelMappings），
	// 需要立即生效时无需等待 TTL。
	defaultMappingCacheTTL = 60 * time.Second
	// defaultMappingCacheMax 是缓存的渠道数上限。
	//
	// 取 512：单个进程服务的渠道数通常远小于它；超出时清空重建（缓存只是加速手段，
	// 清空最多让后续请求多查一次库，代价可忽略，且能防止内存无界增长）。
	defaultMappingCacheMax = 512
)

// mappingCacheEntry 是某渠道映射列表的缓存条目。
type mappingCacheEntry struct {
	mappings []*model.ChannelModelMapping
	expires  time.Time
}

// channelMappingCache 按渠道 ID 缓存映射列表。
//
// 并发安全：内部用互斥锁保护；只被 Relay 持有，生命周期与进程一致。
type channelMappingCache struct {
	mu      sync.Mutex
	entries map[uint64]mappingCacheEntry
	ttl     time.Duration
	max     int
}

// newChannelMappingCache 创建映射缓存；ttl/max 非正数时取默认值。
func newChannelMappingCache(ttl time.Duration, max int) *channelMappingCache {
	if ttl <= 0 {
		ttl = defaultMappingCacheTTL
	}
	if max <= 0 {
		max = defaultMappingCacheMax
	}
	return &channelMappingCache{
		entries: make(map[uint64]mappingCacheEntry),
		ttl:     ttl,
		max:     max,
	}
}

// get 返回渠道的映射列表：命中且未过期时直接用缓存，否则查库并写回缓存。
//
// 查库失败会把错误返回给调用方（由调用方降级为"无映射"，见 modelMappingsForChannel）——
// 本层不吞错误，是为了让降级决策集中在一处、并留下告警。
func (c *channelMappingCache) get(
	ctx context.Context, repo model.ChannelModelMappingRepository, channelID uint64,
) ([]*model.ChannelModelMapping, error) {
	if cached, ok := c.lookup(channelID); ok {
		return cached, nil
	}

	mappings, err := repo.ListByChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	c.store(channelID, mappings)
	return mappings, nil
}

// lookup 读取未过期的缓存项。
func (c *channelMappingCache) lookup(channelID uint64) ([]*model.ChannelModelMapping, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[channelID]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.mappings, true
}

// store 写入缓存；达到容量上限时先腾出空间。
func (c *channelMappingCache) store(channelID uint64, mappings []*model.ChannelModelMapping) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[channelID] = mappingCacheEntry{
		mappings: mappings,
		expires:  time.Now().Add(c.ttl),
	}
}

// evictLocked 在容量上限处腾空间：先清过期项，仍满则整体清空。
//
// 调用方必须已持有锁。
func (c *channelMappingCache) evictLocked() {
	now := time.Now()
	for id, entry := range c.entries {
		if now.After(entry.expires) {
			delete(c.entries, id)
		}
	}
	if len(c.entries) >= c.max {
		c.entries = make(map[uint64]mappingCacheEntry)
	}
}

// invalidate 删除指定渠道的缓存项。
func (c *channelMappingCache) invalidate(channelID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, channelID)
}

// invalidateAll 清空全部缓存项。
func (c *channelMappingCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[uint64]mappingCacheEntry)
}

// InvalidateChannelModelMappings 清除某渠道的映射缓存，使后台改动立即生效。
//
// 未启用映射仓储时为空操作：这样调用方无需判断"映射功能是否开启"。
func (r *Relay) InvalidateChannelModelMappings(channelID uint64) {
	if r.mappingCache != nil {
		r.mappingCache.invalidate(channelID)
	}
}

// InvalidateAllModelMappings 清空全部映射缓存（例如批量导入映射后）。
func (r *Relay) InvalidateAllModelMappings() {
	if r.mappingCache != nil {
		r.mappingCache.invalidateAll()
	}
}

// modelMappingsForChannel 返回渠道的映射列表；未启用或查询失败时返回 nil（无映射）。
//
// 为什么查询失败不返回错误：映射是"增强能力"，转发本身不依赖它。
// 若因一次查库抖动就中断请求，等于把可选能力变成了链路的单点故障。
// 失败时记告警，保留可观测性。
func (r *Relay) modelMappingsForChannel(ctx context.Context, channelID uint64) []*model.ChannelModelMapping {
	if r.channelMappings == nil || r.mappingCache == nil {
		return nil
	}
	mappings, err := r.mappingCache.get(ctx, r.channelMappings, channelID)
	if err != nil {
		slog.Warn("查询渠道模型映射失败，本次按无映射转发",
			"error", err, "channel_id", channelID)
		return nil
	}
	return mappings
}

// resolveUpstreamModel 解析本次转发应使用的上游模型名，并给出应发送的请求体。
//
// 返回值：
//   - upstreamModel：上游应使用的模型名（无映射或未命中时与 publicModel 相同）；
//   - outboundBody：命中且确实改变模型名时改写后的请求体，否则原样返回 body；
//   - applied：是否真的做了改写（供响应回写与调用日志判断）。
//
// 三个"宁可不改也不改坏"的护栏：
//  1. 映射为空 / 未命中 / 结果与对外名相同 → 完全不动请求体（零影响）；
//  2. 请求体不是合法 JSON 或没有 model 字段 → 放弃改写，原样转发；
//  3. 结果为空串 → 视为未命中（避免把 model 写成空导致上游报错）。
func (r *Relay) resolveUpstreamModel(
	ctx context.Context, channelID uint64, publicModel string, body []byte,
) (upstreamModel string, outboundBody []byte, applied bool) {
	mappings := r.modelMappingsForChannel(ctx, channelID)
	if len(mappings) == 0 {
		return publicModel, body, false
	}

	upstreamModel, ok := model.ResolveMapping(mappings, publicModel)
	if !ok || upstreamModel == "" || upstreamModel == publicModel {
		return publicModel, body, false
	}

	rewritten, ok := rewriteRequestModel(body, upstreamModel)
	if !ok {
		return publicModel, body, false
	}
	return upstreamModel, rewritten, true
}

// rewriteRequestModel 把请求体 JSON 的 model 字段改写为 upstreamModel。
//
// 用 map[string]json.RawMessage 而非 map[string]any 改写：
//   - 只动 model 一个字段，其余字段的原始字节被原样保留（数字精度、嵌套结构不受影响）；
//   - 相比 map[string]any，RawMessage 不会把整数变成 float、也不会重建所有子结构。
//
// 返回 ok=false 表示请求体不是 JSON 对象或没有 model 字段，调用方应原样透传。
func rewriteRequestModel(body []byte, upstreamModel string) ([]byte, bool) {
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, false
	}
	if _, exists := fields["model"]; !exists {
		return body, false
	}

	encoded, err := json.Marshal(upstreamModel)
	if err != nil {
		return body, false
	}
	fields["model"] = encoded

	patched, err := json.Marshal(fields)
	if err != nil {
		return body, false
	}
	return patched, true
}

// applyResponseModelRewrite 在命中映射时把上游响应体中的 model 回写为对外名。
//
// 就地替换 resp.Body：流式用增量改写器（不缓冲整段），非流式整体替换 JSON。
// 仅当 applied=true（即确实改写请求时才需要回写）才应调用本函数。
func applyResponseModelRewrite(resp *http.Response, wantStream bool, publicModel string) {
	if resp == nil || resp.Body == nil {
		return
	}
	if wantStream {
		resp.Body = &modelRewritingBody{
			Reader: newStreamModelRewriter(resp.Body, publicModel),
			closer: resp.Body,
		}
		return
	}
	rewriteNonStreamModel(resp, publicModel)
}

// modelRewritingBody 把"改写后的读取器"与"原始关闭器"组合为 ReadCloser。
//
// 单独包一层而不是复用 io.NopCloser：必须把 Close 透传到上游响应体，
// 否则会泄漏连接（高并发下会耗尽连接池）。
type modelRewritingBody struct {
	io.Reader
	closer io.Closer
}

// Close 关闭底层响应体。
func (b *modelRewritingBody) Close() error { return b.closer.Close() }

// rewriteNonStreamModel 在非流式响应中把 model 字段整体改写为 publicModel。
//
// 内存保护：最多读取 maxAdaptedBodyBytes+1 字节。
//   - 若在窗口内读完（<= 上限）→ 改写后替换响应体；
//   - 若超过上限或读取出错 → 放弃改写，把"已读字节 + 剩余流"拼回，逐字节还原。
//
// 为什么必须拼回而不是丢弃：响应体一旦被读到内存，必须保证客户端仍收到完整内容，
// 否则一次"改写失败"会变成"响应被截断"。
func rewriteNonStreamModel(resp *http.Response, publicModel string) {
	original := resp.Body
	prefix, err := io.ReadAll(io.LimitReader(original, maxAdaptedBodyBytes+1))
	if err != nil || len(prefix) > maxAdaptedBodyBytes {
		// 读取出错或超过上限：放弃改写，把"已读字节 + 剩余流"拼回，逐字节还原。
		// 注意必须保留 original 里尚未读到的部分，否则会把响应截断。
		resp.Body = &modelRewritingBody{
			Reader: io.MultiReader(bytes.NewReader(prefix), original),
			closer: original,
		}
		resp.ContentLength = -1
		return
	}

	_ = original.Close()
	rewritten := replaceModelField(prefix, publicModel)
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
}

// replaceModelField 把 JSON 对象里的 model 字段替换为 modelName。
//
// 不是 JSON 对象或没有 model 字段时原样返回——"字段不存在"与"字段值不同"
// 都必须被安全处理，绝不能因为改写而破坏响应结构。
func replaceModelField(raw []byte, modelName string) []byte {
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	if _, exists := fields["model"]; !exists {
		return raw
	}

	encoded, err := json.Marshal(modelName)
	if err != nil {
		return raw
	}
	fields["model"] = encoded

	patched, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return patched
}

// streamModelRewriter 在 SSE 字节流中把【首个】含 model 字段的数据事件改写为对外名。
//
// 为什么只改首个事件：上游通常在首个事件里标明本次返回的模型，后续事件重复同一个值；
// 只改首个即可既纠正客户端可见的模型名，又避免无谓地重建每一个事件（保持其余分片逐字节不变）。
//
// 为什么逐行而不整段缓冲：SSE 是纯文本、以 \n\n 分帧、无长度前缀，逐行读取即可
// 在极小的内存占用下完成改写；若 io.ReadAll 整段，会把流式体验退化成"等全文"。
type streamModelRewriter struct {
	reader *bufio.Reader
	to     string
	done   bool
	out    []byte // 待输出的字节
	err    error  // 读取源流的终态错误（EOF 或真实错误）
}

// newStreamModelRewriter 创建 SSE 模型名改写器。
func newStreamModelRewriter(src io.Reader, publicModel string) *streamModelRewriter {
	return &streamModelRewriter{
		reader: bufio.NewReaderSize(src, adaptedReadBufferBytes),
		to:     publicModel,
	}
}

// Read 实现 io.Reader：按行产出，必要时改写首个含 model 的数据行。
//
// 约定：只要有数据就返回 (n, nil)；仅在无数据可读时才返回源流的错误（含 io.EOF）。
func (s *streamModelRewriter) Read(p []byte) (int, error) {
	if len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		s.fill()
		if len(s.out) == 0 {
			if s.err == nil {
				s.err = io.EOF
			}
			return 0, s.err
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// fill 读取源流的一行（含行尾），必要时改写后放入输出缓冲。
func (s *streamModelRewriter) fill() {
	line, err := s.reader.ReadBytes('\n')
	if len(line) > 0 {
		if !s.done {
			line = s.rewriteLine(line)
		}
		s.out = append(s.out, line...)
	}
	if err != nil {
		s.err = err
	}
}

// rewriteLine 把一行 SSE 文本改写为对外名；非数据行或不含 model 时原样返回。
func (s *streamModelRewriter) rewriteLine(line []byte) []byte {
	// 分离行尾（\n 或 \r\n），改写后需要原样保留，避免改变分帧方式
	body := bytes.TrimRight(line, "\r\n")
	suffix := line[len(body):]

	payload, ok := sseDataPayload(body)
	if !ok {
		return line
	}

	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(payload, &fields); err != nil {
		return line
	}
	if _, exists := fields["model"]; !exists {
		return line
	}

	encoded, err := json.Marshal(s.to)
	if err != nil {
		return line
	}
	fields["model"] = encoded

	patched, err := json.Marshal(fields)
	if err != nil {
		return line
	}

	s.done = true
	out := make([]byte, 0, len("data: ")+len(patched)+len(suffix))
	out = append(out, "data: "...)
	out = append(out, patched...)
	out = append(out, suffix...)
	return out
}

// upstreamModelForLog 返回应写入调用日志的上游模型名。
//
// 语义约定：仅当映射确实改写了模型名时才记录上游名；无映射时记空串，
// 表示"上游模型与对外模型一致"。这样日志既可读（绝大多数行不重复同一名字），
// 又能一眼看出"这条请求经过了映射"。
func upstreamModelForLog(upstreamModel string, applied bool) string {
	if !applied {
		return ""
	}
	return upstreamModel
}
