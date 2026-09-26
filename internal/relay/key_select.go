// 本文件实现「上游凭据池的调度算法」：可用性过滤 → 策略选择 → 会话粘性 → 失败处置。
//
// 意图（Why）：
//
//	一个渠道可能挂几百把凭据（如批量申请的免费额度密钥）。它们之间存在明确的差异：
//	有的额度大、有的快、有的当下正被限流。"随机挑一把"既无法表达运维意图，
//	也无法在个别凭据临时受限时把流量及时挪开。本文件给出可解释、可配置的调度：
//	  1) 过滤：先剔除「状态非启用 / 冷却中 / 限速窗口已满」的凭据；
//	  2) 择优：按渠道配置的策略（sequential / round_robin / weighted_random /
//	     least_recent / least_in_flight）选一把；
//	  3) 粘性：同一会话尽量固定用同一把凭据（缓存命中、上游侧会话连续性更友好）；
//	  4) 处置：把失败分成「冷却（时间语义，自动恢复）」与「摘除（永久语义）」。
//
//	关键概念区分（务必保持）：
//	  · 「冷却」= cooldown_until 到点自动恢复，属临时状态，写在 channel_keys 上；
//	  · 「摘除」= status=3，只有上游明确说凭据永久无效（吊销/失效）才置位。
//	  把 429 这类临时失败当作摘除，会让池容量在事故中单调缩小、无法自愈。
//
// 流转（Flow）：
//
//	resolveChatKey → Relay.SelectKey → keyPicker.pick
//	  ├─ filterUsableKeys：状态 / 冷却 / 限速
//	  ├─ 粘性命中（LRU + TTL，进程内）
//	  ├─ 按策略挑选（round_robin 会写回 channels.key_cursor）
//	  └─ 回写粘性绑定
//	失败时：forwardChat → Relay.applyCredentialFailure
//	  └─ classifyCredentialFailure → SetCooldown（冷却）或 MarkPermanentFailure（摘除）
//
// 扩展（Extend）：
//
//	新增策略：在 model 增加常量并同步 IsValid/NormalizeKeyStrategy，
//	再在本文件的 selectByStrategy 增加分支，最后补 key_select_test.go 用例。
package relay

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// ErrNoUsableCredential 表示渠道的凭据池当前没有可用凭据
// （全部被禁用 / 冷却中 / 限速窗口已满）。
//
// 调用方据此决定"是否换渠道"：它不是致命错误，而是"这个渠道暂时用不了"。
var ErrNoUsableCredential = errors.New("relay: 渠道没有可用凭据（禁用/冷却/限速）")

const (
	// credentialSessionHeader 是会话标识的请求头。
	//
	// 约定：客户端若在连续对话中携带该头，同一对话会尽量固定用同一把凭据；
	// 不携带则退化为无粘性的普通调度（不做任何猜测，避免误绑定）。
	credentialSessionHeader = "X-Session-Id"

	// defaultStickyTTL 是会话粘性绑定的存活时间。
	//
	// 取 1 小时：一次对话/一个任务通常远短于此；过期即释放，避免绑定表无限膨胀。
	defaultStickyTTL = time.Hour
	// defaultStickyCapacity 是粘性绑定的最大条目数（LRU 淘汰上限）。
	//
	// 有多实例部署时，粘性【不共享】：各实例各自维护进程内的绑定表，
	// 因此同一会话可能落到不同实例、绑定到不同凭据——
	// 这是"不引入共享存储、不增加跨实例依赖"的取舍，粘性只做尽力而为的优化。
	defaultStickyCapacity = 4096

	// defaultInFlightStaleAfter 是"在途计数"的陈旧阈值。
	//
	// 进程崩溃会残留 in_flight（Acquire 了却没有 Release），若不做兜底，
	// 残留计数会让 least_in_flight 长期避开这把凭据，等于永久性地损失池容量。
	// 因此超过该时长仍未更新 last_used_at 的在途计数视为陈旧并忽略。
	defaultInFlightStaleAfter = 30 * time.Minute

	// rpmWindow 是限速统计的窗口长度（固定窗口，1 分钟）。
	rpmWindow = time.Minute

	// 各类失败的冷却时长（起点与上限），用于指数退避。
	cooldownRateLimitedBase = 30 * time.Second // 429 起始冷却
	cooldownRateLimitedCap  = 10 * time.Minute // 429 冷却上限
	cooldownServerErrBase   = 15 * time.Second // 5xx / 超时起始冷却
	cooldownServerErrCap    = 5 * time.Minute  // 5xx / 超时冷却上限
	cooldownAuthFailure     = 30 * time.Minute // 401 / 403 / 402 的长冷却
)

// credentialStickyBook 是进程级共享的会话粘性绑定表。
//
// 为什么放进程内：粘性是"降低抖动"的优化项，不值得为它引入外部存储依赖。
// 代价是多实例部署下粘性不共享（见 defaultStickyCapacity 的说明）。
var credentialStickyBook = newStickyBook(defaultStickyTTL, defaultStickyCapacity)

// SelectKey 按渠道策略从给定凭据中挑选一条。
//
// 参数 keys 为该渠道当前"处于启用状态"的凭据（通常来自 ListUsable）；
// sessionHash 为空表示不启用粘性。
//
// 返回 ErrNoUsableCredential 表示过滤后没有任何可用凭据。
func (r *Relay) SelectKey(ctx context.Context, ch *model.Channel, keys []*model.ChannelKey, sessionHash string) (*model.ChannelKey, error) {
	if ch == nil {
		return nil, ErrNoUsableCredential
	}
	picker := keyPicker{repo: r.keys, sticky: credentialStickyBook}
	return picker.pick(ctx, ch.ID, keys, sessionHash)
}

// keyPicker 承载挑选所需的外部依赖，便于在测试中注入可控的时间与粘性表。
type keyPicker struct {
	repo   model.ChannelKeyRepository
	sticky *stickyBook
	// now 返回当前时间；为 nil 时使用 time.Now（测试可注入固定时钟）。
	now func() time.Time
	// inFlightStaleAfter 覆盖默认的在途陈旧阈值（测试用）。
	inFlightStaleAfter time.Duration
}

// clock 返回当前时间。
func (p *keyPicker) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// staleAfter 返回在途陈旧阈值。
func (p *keyPicker) staleAfter() time.Duration {
	if p.inFlightStaleAfter > 0 {
		return p.inFlightStaleAfter
	}
	return defaultInFlightStaleAfter
}

// pick 执行一次完整的「过滤 → 粘性 → 择优 → 绑定」流程。
func (p *keyPicker) pick(ctx context.Context, channelID uint64, keys []*model.ChannelKey, sessionHash string) (*model.ChannelKey, error) {
	now := p.clock()

	usable := filterUsableKeys(keys, now)
	if len(usable) == 0 {
		return nil, fmt.Errorf("%w（渠道 %d）", ErrNoUsableCredential, channelID)
	}

	strategy, cursor := p.strategyOf(ctx, channelID)

	// 粘性优先：命中且目标仍可用则直接返回。
	if sessionHash != "" && p.sticky != nil {
		if id, ok := p.sticky.get(sessionHash, now); ok {
			if bound := findKeyByID(usable, id); bound != nil {
				return bound, nil
			}
			// 粘性目标已不可用（冷却/禁用/限速）：放弃粘性并清除绑定，
			// 而不是报错——粘性只是优化，绝不能因为一个失效绑定让请求失败。
			p.sticky.remove(sessionHash)
		}
	}

	picked := p.selectByStrategy(ctx, channelID, strategy, cursor, usable, now)
	if picked == nil {
		return nil, fmt.Errorf("%w（渠道 %d）", ErrNoUsableCredential, channelID)
	}

	if sessionHash != "" && p.sticky != nil {
		p.sticky.put(sessionHash, picked.ID, now)
	}
	return picked, nil
}

// strategyOf 读取渠道的调度策略与轮询游标；读取失败退化为默认策略。
func (p *keyPicker) strategyOf(ctx context.Context, channelID uint64) (model.KeyStrategy, int64) {
	if p.repo == nil {
		return model.DefaultKeyStrategy(), 0
	}
	strategy, cursor, err := p.repo.ChannelKeyStrategy(ctx, channelID)
	if err != nil {
		// 调度属优化项：读不到就用默认，绝不因为它让请求失败。
		return model.DefaultKeyStrategy(), 0
	}
	return strategy, cursor
}

// selectByStrategy 按策略挑选一条凭据。
func (p *keyPicker) selectByStrategy(ctx context.Context, channelID uint64, strategy model.KeyStrategy, cursor int64, usable []*model.ChannelKey, now time.Time) *model.ChannelKey {
	switch strategy {
	case model.KeyStrategySequential:
		return pickSequential(usable)
	case model.KeyStrategyRoundRobin:
		return p.pickRoundRobin(ctx, channelID, usable, cursor)
	case model.KeyStrategyWeightedRandom:
		return pickWeightedRandom(usable)
	case model.KeyStrategyLeastRecent:
		return pickLeastRecent(usable)
	default: // 含 KeyStrategyLeastInFlight 与未知值
		return pickLeastInFlight(usable, now, p.staleAfter())
	}
}

// filterUsableKeys 过滤出当前可参与选择的凭据。
//
// 三类排除：
//   - 状态不是"启用"（手动禁用 / 已摘除）；
//   - 处于冷却期（时间语义，到点自动恢复）；
//   - 限速窗口内已用满（rpm_limit > 0 且当前窗口计数已达上限）。
func filterUsableKeys(keys []*model.ChannelKey, now time.Time) []*model.ChannelKey {
	usable := make([]*model.ChannelKey, 0, len(keys))
	for _, k := range keys {
		if k == nil || !k.IsUsable() {
			continue
		}
		if k.CoolingDown(now) {
			continue
		}
		if rpmExhausted(k, now) {
			continue
		}
		usable = append(usable, k)
	}
	return usable
}

// rpmExhausted 判断凭据在当前 RPM 窗口内是否已用满。
//
// 窗口过期后计数视为失效（不再限速），等待下一次请求把窗口重置。
func rpmExhausted(k *model.ChannelKey, now time.Time) bool {
	if k.RPMLimit <= 0 || k.WindowCount < k.RPMLimit {
		return false
	}
	if k.WindowStart.IsZero() {
		return false
	}
	return now.Sub(k.WindowStart) < rpmWindow
}

// pickSequential 顺序策略：取优先级最高者，相同则取 id 最小者。
func pickSequential(usable []*model.ChannelKey) *model.ChannelKey {
	if len(usable) == 0 {
		return nil
	}
	best := usable[0]
	for _, k := range usable[1:] {
		if k.Priority > best.Priority || (k.Priority == best.Priority && k.ID < best.ID) {
			best = k
		}
	}
	return best
}

// pickRoundRobin 轮询策略：按环形游标取，并把游标自增写回渠道。
//
// 游标以"已分配出去的序号"语义持久化：索引 = cursor % len(usable)，
// 这样即使可用集大小在两次选择间发生变化，也能继续往前推进而不退回起点。
func (p *keyPicker) pickRoundRobin(ctx context.Context, channelID uint64, usable []*model.ChannelKey, cursor int64) *model.ChannelKey {
	if len(usable) == 0 {
		return nil
	}
	if cursor < 0 {
		cursor = 0
	}
	picked := usable[int(cursor%int64(len(usable)))]
	if p.repo != nil {
		// 游标写回失败不阻断选择：最多退化为"本轮重复上一次位置"。
		_ = p.repo.AdvanceKeyCursor(ctx, channelID, cursor+1)
	}
	return picked
}

// pickWeightedRandom 加权随机策略：按 weight 倾斜。
//
// 权重全为 0 时【退化为等概率】，而不是"谁都选不到"：
// 权重字段的默认值是 1，但人工把整池权重清零（或全部为负）完全可能，
// 此时若严格按权重比例，池子会永远选不出凭据，等价于渠道不可用。
func pickWeightedRandom(usable []*model.ChannelKey) *model.ChannelKey {
	if len(usable) == 0 {
		return nil
	}
	total := 0
	for _, k := range usable {
		if k.Weight > 0 {
			total += k.Weight
		}
	}
	if total <= 0 {
		return usable[rand.IntN(len(usable))]
	}

	remaining := rand.IntN(total)
	for _, k := range usable {
		if k.Weight <= 0 {
			continue
		}
		remaining -= k.Weight
		if remaining < 0 {
			return k
		}
	}
	return usable[len(usable)-1]
}

// pickLeastRecent 最久未用策略：取 last_used_at 最早者（从未用过的最优先）。
func pickLeastRecent(usable []*model.ChannelKey) *model.ChannelKey {
	if len(usable) == 0 {
		return nil
	}
	best := usable[0]
	for _, k := range usable[1:] {
		if lessRecent(k, best) {
			best = k
		}
	}
	return best
}

// lessRecent 判断 a 是否比 b "更久未被使用"。
//
// 排序键：从未使用（零值）优先 → 更早的时间优先 → id 小者优先（保证确定性）。
func lessRecent(a, b *model.ChannelKey) bool {
	aZero := a.LastUsedAt.IsZero()
	bZero := b.LastUsedAt.IsZero()
	if aZero != bZero {
		return aZero
	}
	if !aZero && !a.LastUsedAt.Equal(b.LastUsedAt) {
		return a.LastUsedAt.Before(b.LastUsedAt)
	}
	return a.ID < b.ID
}

// pickLeastInFlight 最少在途策略：取当前在途请求数最少者，相同则取更久未用者。
func pickLeastInFlight(usable []*model.ChannelKey, now time.Time, staleAfter time.Duration) *model.ChannelKey {
	if len(usable) == 0 {
		return nil
	}
	best := usable[0]
	for _, k := range usable[1:] {
		if lessInFlight(k, best, now, staleAfter) {
			best = k
		}
	}
	return best
}

// lessInFlight 判断 a 是否比 b 更适合被选中（在途更少）。
func lessInFlight(a, b *model.ChannelKey, now time.Time, staleAfter time.Duration) bool {
	ai := effectiveInFlight(a, now, staleAfter)
	bi := effectiveInFlight(b, now, staleAfter)
	if ai != bi {
		return ai < bi
	}
	return lessRecent(a, b)
}

// effectiveInFlight 返回"可信的在途计数"。
//
// 若计数为正、但最近使用时间已超过 staleAfter，说明它极可能是进程崩溃残留的
// 陈旧计数（Acquire 后没有 Release），此时按 0 处理，避免它长期被策略避开。
func effectiveInFlight(k *model.ChannelKey, now time.Time, staleAfter time.Duration) int {
	if k.InFlight <= 0 {
		return 0
	}
	if !k.LastUsedAt.IsZero() && now.Sub(k.LastUsedAt) > staleAfter {
		return 0
	}
	return k.InFlight
}

// findKeyByID 在给定集合中按 id 查找凭据。
func findKeyByID(keys []*model.ChannelKey, id uint64) *model.ChannelKey {
	for _, k := range keys {
		if k != nil && k.ID == id {
			return k
		}
	}
	return nil
}

// credentialFailureAction 描述一次失败对凭据的处置方式。
type credentialFailureAction struct {
	// Cooldown 为冷却时长；<=0 表示不冷却。
	Cooldown time.Duration
	// Remove 为 true 表示永久摘除（仅用于上游明确判定凭据永久无效）。
	Remove bool
	// Reason 为可读的失败原因（写入 last_error，已脱敏）。
	Reason string
}

// credentialPermanentInvalidMarkers 是"上游明确判定凭据永久无效"的文本特征。
//
// 为什么需要文本判据：仅凭状态码无法区分"临时封禁"与"永久吊销"。
// 若把 401 一律当作永久失效，那些"被上游临时风控后又恢复"的凭据会被误摘；
// 反之，只有拿到这些明确措辞才摘除，冷却才是 401 的默认处置。
var credentialPermanentInvalidMarkers = []string{
	"revoked",
	"invalid api key",
	"invalid_api_key",
	"api key is invalid",
	"api key has been disabled",
	"key has been disabled",
	"key is disabled",
	"key_not_found",
	"no longer valid",
	"has been deactivated",
}

// classifyCredentialFailure 依据状态码与响应体，决定失败的处置方式。
//
// failCount 为该凭据此前已累计的连续失败次数，用于指数退避（每次翻倍）。
//
// 判定顺序（important）：
//  1. 响应体命中"永久无效"特征 → 摘除（这是唯一的摘除入口）；
//  2. 429 → 冷却（30s 起，指数退避，上限 10 分钟）；
//  3. 408 / 5xx → 冷却（15s 起，上限 5 分钟）；
//  4. 401 / 403 / 402 → 长冷却（30 分钟），【不摘除】；
//  5. 其他 → 不处置（由渠道级逻辑决定）。
func classifyCredentialFailure(status int, body []byte, failCount int) credentialFailureAction {
	if containsPermanentInvalidMarker(body) {
		return credentialFailureAction{
			Remove: true,
			Reason: "上游明确指出凭据已永久无效",
		}
	}

	switch {
	case status == http.StatusTooManyRequests:
		return credentialFailureAction{
			Cooldown: backoff(cooldownRateLimitedBase, failCount, cooldownRateLimitedCap),
			Reason:   fmt.Sprintf("上游限流（HTTP %d），临时冷却", status),
		}
	case status == http.StatusRequestTimeout || status >= http.StatusInternalServerError:
		return credentialFailureAction{
			Cooldown: backoff(cooldownServerErrBase, failCount, cooldownServerErrCap),
			Reason:   fmt.Sprintf("上游服务异常（HTTP %d），临时冷却", status),
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired:
		return credentialFailureAction{
			Cooldown: cooldownAuthFailure,
			Reason:   fmt.Sprintf("上游拒绝鉴权（HTTP %d），长冷却（可能是临时封禁）", status),
		}
	default:
		return credentialFailureAction{}
	}
}

// containsPermanentInvalidMarker 判断响应体是否含"永久无效"特征。
func containsPermanentInvalidMarker(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lowered := strings.ToLower(string(body))
	for _, marker := range credentialPermanentInvalidMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// backoff 计算指数退避：base × 2^(failCount)，上限 max。
//
// failCount 为"此前已失败的次数"：首次失败（0）取 base，其后逐次翻倍。
func backoff(base time.Duration, failCount int, max time.Duration) time.Duration {
	if failCount < 0 {
		failCount = 0
	}
	d := base
	for i := 0; i < failCount; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		d = max
	}
	return d
}

// applyCredentialFailure 依据失败分类，对凭据执行冷却或摘除。
//
// 刻意忽略仓储错误：这是自愈与统计用途，不应影响对客户端的响应。
func (r *Relay) applyCredentialFailure(ctx context.Context, keyID uint64, failCount, status int, body []byte) {
	if r.keys == nil || keyID == 0 {
		return
	}
	action := classifyCredentialFailure(status, body, failCount)
	switch {
	case action.Remove:
		_ = r.keys.MarkPermanentFailure(ctx, keyID, truncateReason(action.Reason))
	case action.Cooldown > 0:
		_ = r.keys.SetCooldown(ctx, keyID, time.Now().Add(action.Cooldown), truncateReason(action.Reason))
	}
}

// ---- 会话粘性 ----

// stickyEntry 是一条粘性绑定。
//
// 额外保存 key 是为了在 LRU 淘汰时能直接从被淘汰元素反查并删除 map 项
// （container/list 的元素本身不带业务键）。
type stickyEntry struct {
	key      string
	credID   uint64
	expireAt time.Time
}

// stickyBook 是带 TTL 与容量上限的 LRU 绑定表（进程内，并发安全）。
//
// 选择 LRU 而非无限 map：会话标识由客户端给出，若不设上限，
// 恶意或异常客户端可以用海量不同标识把内存撑爆。
type stickyBook struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	order    *list.List               // 表头最近使用
	entries  map[string]*list.Element // key → element（Value 为 *stickyEntry）
}

// newStickyBook 创建绑定表；参数非正时使用默认值。
func newStickyBook(ttl time.Duration, capacity int) *stickyBook {
	if ttl <= 0 {
		ttl = defaultStickyTTL
	}
	if capacity <= 0 {
		capacity = defaultStickyCapacity
	}
	return &stickyBook{
		ttl:      ttl,
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element),
	}
}

// get 查询绑定；命中时刷新为最近使用，过期则清除并返回未命中。
func (b *stickyBook) get(key string, now time.Time) (uint64, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	elem, ok := b.entries[key]
	if !ok {
		return 0, false
	}
	entry := elem.Value.(*stickyEntry)
	if now.After(entry.expireAt) {
		b.removeLocked(key)
		return 0, false
	}
	b.order.MoveToFront(elem)
	return entry.credID, true
}

// put 写入/更新绑定，并在超容量时淘汰最久未使用的条目。
func (b *stickyBook) put(key string, credID uint64, now time.Time) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if elem, ok := b.entries[key]; ok {
		entry := elem.Value.(*stickyEntry)
		entry.credID = credID
		entry.expireAt = now.Add(b.ttl)
		b.order.MoveToFront(elem)
		return
	}

	entry := &stickyEntry{key: key, credID: credID, expireAt: now.Add(b.ttl)}
	b.entries[key] = b.order.PushFront(entry)

	for b.order.Len() > b.capacity {
		oldest := b.order.Back()
		if oldest == nil {
			break
		}
		b.order.Remove(oldest)
		if evicted, ok := oldest.Value.(*stickyEntry); ok {
			delete(b.entries, evicted.key)
		}
	}
}

// remove 删除一条绑定。
func (b *stickyBook) remove(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeLocked(key)
}

// removeLocked 在已持锁的前提下删除绑定。
func (b *stickyBook) removeLocked(key string) {
	elem, ok := b.entries[key]
	if !ok {
		return
	}
	b.order.Remove(elem)
	delete(b.entries, key)
}

// len 返回当前绑定数量（测试用）。
func (b *stickyBook) len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.order.Len()
}

// stickySessionKey 从请求中提取会话标识并哈希。
//
// 没有会话标识（未携带约定请求头）时返回空串，调度将不启用粘性。
func stickySessionKey(req *http.Request) string {
	if req == nil {
		return ""
	}
	raw := strings.TrimSpace(req.Header.Get(credentialSessionHeader))
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// credentialSessionCtxKey 是会话哈希在 context 中的键类型。
type credentialSessionCtxKey struct{}

// withCredentialSession 把会话哈希放入 context（哈希为空时原样返回）。
func withCredentialSession(ctx context.Context, hash string) context.Context {
	if hash == "" {
		return ctx
	}
	return context.WithValue(ctx, credentialSessionCtxKey{}, hash)
}

// credentialSessionFrom 从 context 取回会话哈希。
func credentialSessionFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	hash, _ := ctx.Value(credentialSessionCtxKey{}).(string)
	return hash
}
