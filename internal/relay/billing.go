// 本文件实现「按用量计费」：查价、算钱、扣额度。
//
// 意图（Why）：
//
//	在 M2 之前，网关只记录 token 数却从不扣费——usage_logs.quota 恒为 0，
//	令牌与用户的 used_quota 永远是 0，于是"额度耗尽"这个状态永远触发不了，
//	额度字段形同虚设。本文件把计费真正接上。
//
//	为什么单独成组件而不是塞进 relay：
//	  1) 计费规则（价格匹配 + 换算口径）需要被多处复用（后台预估、报表核算）；
//	  2) 价格读取要缓存——每次转发都查一次库会显著放大数据库压力，
//	     而缓存失效策略属于计费语义，不该散落在转发代码里。
//
// 计费口径（唯一真相在 model.ModelPrice 的文件头）：
//
//	quota = (promptTokens × promptPrice + completionTokens × completionPrice) / 1_000_000
//
// 流转（Flow）：
//
//	转发完成 → relay.recordUsage（entry.Group 携带本次请求分组）
//	  └─ Billing.Charge(ctx, group, userID, tokenID, model, prompt, completion)
//	       ├─ priceFor(group, model)   按分组带缓存的价格匹配
//	       ├─ ratioFor(group)          按分组带缓存的倍率
//	       ├─ ComputeQuota(...)        换算额度
//	       ├─ tokens.ConsumeQuota      扣令牌额度（原子）
//	       └─ users.AddUsedQuota       累加用户已用额度（原子）
//
// 扩展（Extend）：
//
//	新增计价维度（缓存命中价、按次计费、图片张数）时：
//	  在 Charge 里扩展入参并同步 model.ModelPrice 的计算函数与迁移脚本。
//	新增缓存维度时：务必保持"按分组隔离"这一前提——缓存键必须能区分分组，
//	  否则不同分组会互相串价（见 Billing.rules 上的说明）。
package relay

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// priceCacheTTL 是价格缓存的存活时间。
//
// 取 30 秒的权衡：
//   - 太长：管理员改价后要等很久才生效，容易被当成"改了没用"；
//   - 太短：每次转发都要查库，失去缓存意义。
//     30 秒在"改价即时感"与"数据库压力"之间比较平衡；
//     后台改价时还会主动清缓存（见 Invalidate），所以实际感知是即时的。
const priceCacheTTL = 30 * time.Second

// defaultBillingGroup 是未指定分组时使用的分组名。
const defaultBillingGroup = "default"

// ratioScale 是分组倍率的换算基数（百分比：100 = 1.0 倍）。
//
// 与 model.ModelGroup 的口径一致；单独在此定义是为了让计费公式里的
// "除以 100"这件事有一个可检索的名字，而不是散落的魔法数字。
const ratioScale int64 = 100

// estimateBytesPerToken 是由请求体字节数估算 prompt token 数的换算系数。
//
// 取 3 的理由（保守上界）：UTF-8 下中文约 3 字节/token、英文约 4 字节/token，
// 用 3 能在中文场景贴近真实值、在英文场景略微高估。预留宁可略高、不可过小——
// 过小会让额度墙失去意义，过大只是暂时多占用一点额度（结算时会退还差额）。
const estimateBytesPerToken = 3

// Billing 负责按用量计费，并发安全。
type Billing struct {
	prices model.ModelPriceRepository
	// groups 用于读取各分组的计费倍率；可为 nil（此时倍率恒为 100）。
	groups model.ModelGroupRepository
	tokens model.TokenRepository
	users  model.UserRepository
	// group 是默认分组：调用方传入空分组时使用它（见 resolveGroup）。
	group string

	// quota 是额度预留台账（可选）。
	//
	// 为 nil 时退化为"响应后扣费"（旧行为）：便于单元测试与"仅统计不限制"的部署形态。
	// 非 nil 时启用"请求前预扣 + 响应后结算"。
	quota model.QuotaRepository

	// rules 是【按分组隔离】的价格与倍率缓存：key 为分组名。
	//
	// 关键（改动前务必理解）：缓存必须按分组隔离。旧实现只存一份，
	// 免费分组请求会把自营分组的价格灌进缓存，随后自营分组读到免费价（或反之），
	// 直接造成错账 —— 这是本次改动最容易写错、后果最严重的地方。
	//
	// 同一分组内，价格与倍率放在同一份缓存里一起刷新（而不是各刷各的），
	// 以保证二者来自同一时刻的配置，避免"新价格 × 旧倍率"的中间态。
	mu    sync.RWMutex
	rules map[string]*cachedRules
}

// cachedRules 是「某一个分组」的价格规则与倍率的快照。
//
// 按分组各存一份，读时互不干扰：分组 A 的缓存内容绝不会被分组 B 的请求覆盖。
type cachedRules struct {
	prices   []*model.ModelPrice
	ratio    int64
	cachedAt time.Time
}

// NewBilling 创建计费组件。
//
// group 为空时使用 default；groups / tokens / users 均允许为 nil
// （groups 为 nil 时倍率恒为 1.0；tokens/users 为 nil 时只计算不扣减，
// 便于单元测试与"仅统计不限制"的部署形态）。
func NewBilling(prices model.ModelPriceRepository, groups model.ModelGroupRepository,
	tokens model.TokenRepository, users model.UserRepository, group string) *Billing {
	if group == "" {
		group = defaultBillingGroup
	}
	return &Billing{
		prices: prices,
		groups: groups,
		tokens: tokens,
		users:  users,
		group:  group,
		rules:  make(map[string]*cachedRules),
	}
}

// WithQuotaRepository 注入额度预留台账，启用"请求前预扣 + 响应后结算"。
//
// 返回 b 本身以便链式装配（main 中一次性构造）。
// 不注入（或注入 nil）时保持旧行为：只在响应之后按实际用量扣费。
func (b *Billing) WithQuotaRepository(quota model.QuotaRepository) *Billing {
	if b != nil {
		b.quota = quota
	}
	return b
}

// Invalidate 清空【全部分组】的价格与倍率缓存。
//
// 调用时机：后台新增/修改/删除计价规则或分组倍率之后。
// 不清理的话，管理员改完价格会看到"新价格要等半分钟才生效"，容易误判为没生效。
//
// 直接丢弃整张缓存表（而非逐组标记过期）：实现简单且不会遗漏任何分组，
// 代价只是"改价后第一次请求各分组会各查一次库"，可以忽略。
func (b *Billing) Invalidate() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.rules = make(map[string]*cachedRules)
	b.mu.Unlock()
}

// resolveGroup 把"调用方传入的分组"归一化；空字符串表示使用 Billing 的默认分组。
//
// 之所以约定"传空 = 用默认分组"：新增分组参数后，任何尚未补上分组的调用点
// 都会落到默认分组（与改动前行为一致），而不会落到一个不存在的分组导致计费错乱。
func (b *Billing) resolveGroup(group string) string {
	if g := strings.TrimSpace(group); g != "" {
		return g
	}
	return b.group
}

// rulesFor 返回指定分组的最新价格与倍率缓存；缓存过期时重新加载该分组。
//
// 注意 group 已按 resolveGroup 归一化，因此缓存键必然是"真实分组名"，
// 不会出现 "a" 与 " a" 被当成两个分组各缓存一份的浪费。
func (b *Billing) rulesFor(ctx context.Context, group string) *cachedRules {
	group = b.resolveGroup(group)

	b.mu.RLock()
	cached := b.rules[group]
	fresh := cached != nil && time.Since(cached.cachedAt) < priceCacheTTL
	b.mu.RUnlock()

	if fresh {
		return cached
	}
	return b.refresh(ctx, group, cached)
}

// refresh 重新加载【指定分组】的价格规则与倍率，并写回该分组的缓存槽。
//
// 容错（保持旧实现语义）：读库失败时不覆盖旧值——宁可短时间沿用旧价，
// 也不要因为一次读库失败就把该分组的调用变成"未定价"（不扣费），那等于白送。
// 由于缓存按分组隔离，某个分组读库失败只影响它自己，不会污染其他分组。
func (b *Billing) refresh(ctx context.Context, group string, old *cachedRules) *cachedRules {
	rules := &cachedRules{ratio: ratioScale}
	if old != nil {
		// 先继承旧快照：读库失败时据此沿用，而不是清成空。
		rules.prices = old.prices
		rules.ratio = old.ratio
	}

	if b.prices != nil {
		prices, err := b.prices.List(ctx, group, true)
		if err != nil {
			slog.Warn("读取计价规则失败，本次沿用旧缓存", "error", err, "group", group)
		} else {
			rules.prices = prices
		}
	}

	ratio := ratioScale
	if b.groups != nil {
		got, err := b.groups.GetByName(ctx, group)
		switch {
		case err == nil:
			if got.Ratio > 0 {
				ratio = got.Ratio
			}
		case errors.Is(err, model.ErrModelGroupNotFound):
			// 分组不存在（历史数据里的分组名从未登记，或令牌指定的分组已被删除）：
			// 按 1.0 倍处理，与本次升级前的行为一致，不产生意外扣费；并留下告警，
			// 让站长能发现"有请求打到了一个不存在的分组"。
			slog.Warn("计费分组不存在，本次按默认倍率计费", "group", group)
			ratio = ratioScale
		default:
			// 读库失败：保留旧倍率，避免"一次故障把倍率变成 1.0"导致少收钱
			slog.Warn("读取分组倍率失败，本次沿用旧倍率", "error", err, "group", group)
			ratio = rules.ratio
			if ratio <= 0 {
				ratio = ratioScale
			}
		}
	}
	rules.ratio = ratio
	rules.cachedAt = time.Now()

	b.mu.Lock()
	if b.rules == nil {
		b.rules = make(map[string]*cachedRules)
	}
	b.rules[group] = rules
	b.mu.Unlock()

	return rules
}

// ratioFor 返回指定分组的计费倍率（百分比）；空分组用 Billing 的默认分组。
func (b *Billing) ratioFor(ctx context.Context, group string) int64 {
	ratio := b.rulesFor(ctx, group).ratio
	if ratio <= 0 {
		return ratioScale
	}
	return ratio
}

// priceFor 返回适用于该模型的最优计价规则；无匹配时返回 nil（表示未定价）。
//
// 缓存实现：按【分组】整表缓存（仅启用），过期后重新加载该分组。
// 选择"整表"而不是"按模型缓存"，是因为规则数极少而模型名组合无限，
// 按模型缓存反而会让缓存无限膨胀。
//
// 参数 group 为空表示使用 Billing 的默认分组（见 resolveGroup）。
func (b *Billing) priceFor(ctx context.Context, group, modelName string) *model.ModelPrice {
	if b == nil || b.prices == nil {
		return nil
	}
	return model.MatchModelPrice(b.rulesFor(ctx, group).prices, modelName)
}

// Quote 计算该次用量应扣的额度（只算不扣）。
//
// 参数 group 为空表示使用 Billing 的默认分组；扣费请用 Charge。
func (b *Billing) Quote(ctx context.Context, group, modelName string, promptTokens, completionTokens int64) int64 {
	price := b.priceFor(ctx, group, modelName)
	return applyRatio(price.ComputeQuota(promptTokens, completionTokens), b.ratioFor(ctx, group))
}

// QuoteOnce 计算"调用一次该模型"应扣的额度（只算不扣）。
//
// 用途：异步任务在【提交时】就要扣费（见 model.Task 的文件头说明），
// 因此任务链路需要一个与 token 无关的计价入口。
// count 为本次生成的份数（如一次画 4 张图），<=0 时按 1 次处理。
// 参数 group 为空表示使用 Billing 的默认分组。
func (b *Billing) QuoteOnce(ctx context.Context, group, modelName string, count int64) int64 {
	price := b.priceFor(ctx, group, modelName)
	return applyRatio(price.ComputePerCallQuota(count), b.ratioFor(ctx, group))
}

// applyRatio 按倍率换算额度（向下取整）。
//
// 倍率为 100（即 1.0 倍）时数值完全不变，因此未配置分组的部署不受任何影响。
func applyRatio(base, ratio int64) int64 {
	if base <= 0 || ratio <= 0 || ratio == ratioScale {
		return base
	}
	return base * ratio / ratioScale
}

// ChargeOnce 按次计费并扣减额度，返回实际扣减的额度。
//
// 与 Charge 的关系：口径不同（按次 vs 按 token），扣减目标与容错策略完全一致。
// 参数 group 为空表示使用 Billing 的默认分组。
func (b *Billing) ChargeOnce(ctx context.Context, group string, userID, tokenID uint64, modelName string, count int64) int64 {
	if b == nil {
		return 0
	}

	price := b.priceFor(ctx, group, modelName)
	if price == nil {
		// 未定价：不扣费（与 token 计费保持同一语义，避免"没配价格就报错"）
		return 0
	}

	quota := applyRatio(price.ComputePerCallQuota(count), b.ratioFor(ctx, group))
	if quota <= 0 {
		return 0
	}

	b.applyDelta(ctx, userID, tokenID, quota, "扣减")
	return quota
}

// Refund 退还额度（异步任务失败/取消时调用）。
//
// 为什么必须支持退还：任务在提交时就已扣费，若任务最终失败却不退，
// 用户会为"没有拿到结果"的调用付费——这是最容易被投诉的计费缺陷。
//
// 幂等性说明：本方法自身不做幂等保护，由调用方保证"每个任务最多退一次"
// （store 层的 Finish 通过 status NOT IN (终态) 条件天然实现了这一点）。
func (b *Billing) Refund(ctx context.Context, userID, tokenID uint64, amount int64) {
	if b == nil || amount <= 0 {
		return
	}
	b.applyDelta(ctx, userID, tokenID, -amount, "退还")
}

// applyDelta 对令牌与用户额度施加同一个增量（正数为扣减、负数为退还）。
//
// 抽出来的理由：扣减与退还的目标、容错策略完全相同，
// 各写一遍必然有一天会出现"退还时漏掉用户额度"的不一致。
func (b *Billing) applyDelta(ctx context.Context, userID, tokenID uint64, delta int64, action string) {
	now := time.Now()
	if tokenID > 0 && b.tokens != nil {
		if err := b.tokens.ConsumeQuota(ctx, tokenID, delta, now); err != nil {
			slog.Warn(action+"令牌额度失败", "error", err, "token_id", tokenID, "delta", delta)
		}
	}
	if userID > 0 && b.users != nil {
		if err := b.users.AddUsedQuota(ctx, userID, delta); err != nil {
			slog.Warn(action+"用户已用额度失败", "error", err, "user_id", userID, "delta", delta)
		}
	}
}

// Charge 按用量计费并扣减额度，返回实际扣减的额度。
//
// 安全约束（重要）：本方法绝不能影响客户端响应。
// 它通常在响应已完整回传之后执行，因此内部对错误只记录、不向上返回——
// 用户不该因为"记账失败"而收到一个报错。
//
// 扣减范围：
//   - 令牌额度（remain_quota / used_quota）；
//   - 用户额度（used_quota）。
//
// 两者都扣的原因：令牌是"发给某个用户的凭据"，用户额度是账号级上限。
// 只扣令牌会让用户通过"多建几个令牌"绕过总量限制。
//
// 参数 group 为空表示使用 Billing 的默认分组。
func (b *Billing) Charge(ctx context.Context, group string, userID, tokenID uint64, modelName string,
	promptTokens, completionTokens int64) int64 {
	if b == nil {
		return 0
	}

	price := b.priceFor(ctx, group, modelName)
	if price == nil {
		// 未定价：不扣费但照常记录日志（quota=0）。
		// 这样站长能从日志看出"哪些模型还没定价"，而不是被静默拦住。
		return 0
	}

	quota := applyRatio(price.ComputeQuota(promptTokens, completionTokens), b.ratioFor(ctx, group))
	if quota <= 0 {
		return 0
	}

	b.applyDelta(ctx, userID, tokenID, quota, "扣减")
	return quota
}

// ---------------------------------------------------------------------------
// 额度预扣 / 结算 / 退还
// ---------------------------------------------------------------------------
//
// 为什么把这三件事放在计费组件上（而不是让中间件直接操作仓储）：
//   - "该估多少、该怎么收"属于计费语义，只有计费组件知道价格与倍率；
//   - 中间件只需表达"我要预留多少、什么时候结算"，无需理解计价规则。
//
// 三个方法都对 b == nil / 未注入台账 保持零值安全，便于测试与降级部署。

// EstimateReserve 估算一次调用应预留的额度。
//
// 返回 priced=false 表示该模型【未命中任何计价规则】（调用不计费），
// 此时鉴权层必须跳过预留——否则免费模型会被额度墙挡住（这正是线上事故的根因）。
//
// 估算口径（重要：估算只影响"预留"，最终一律以结算为准）：
//
//	按请求体字节数保守估算 prompt token（见 estimateBytesPerToken），
//	并假设输出与输入同量级；对"按次定价"的模型退化为按一次计。
//
// 因此它可能高于真实用量（结算时会把差额退还），也可能低于真实用量
// （结算时补扣），但绝不会把有价格的调用算成免费。
//
// 参数 group 为空表示使用 Billing 的默认分组；预留与最终结算必须用同一分组，
// 否则会出现"按 A 分组预扣、按 B 分组结算"的错账。
func (b *Billing) EstimateReserve(ctx context.Context, group, modelName string, promptBytes int) (int64, bool) {
	if b == nil {
		return 0, false
	}

	price := b.priceFor(ctx, group, modelName)
	if price == nil {
		return 0, false
	}

	promptTokens := estimatePromptTokens(promptBytes)
	base := price.ComputeQuota(promptTokens, promptTokens)
	if base <= 0 {
		// 未配 token 单价但配了按次单价的模型：退化为按一次预留。
		base = price.ComputePerCallQuota(1)
	}
	amount := applyRatio(base, b.ratioFor(ctx, group))
	if amount <= 0 {
		// 命中计价规则但算得极小（如极低单价）：至少预留 1，让额度墙真正生效。
		amount = 1
	}
	return amount, true
}

// Reserve 预扣额度（幂等）。
//
// 未注入台账时返回 (nil, nil)：表示"不做预留"，调用方应退化为响应后扣费。
func (b *Billing) Reserve(ctx context.Context, req model.ReserveRequest) (*model.QuotaReservation, error) {
	if b == nil || b.quota == nil {
		return nil, nil
	}
	return b.quota.Reserve(ctx, req)
}

// Settle 结算第 requestID 号预留（幂等）：按实际用量多退少补。
//
// actualQuota 为 model.QuotaUnknown 时按预留量收取。
// 返回的预留记录中 Settled 是真实入账额度，调用方据此识别"补扣受限"的缺口。
func (b *Billing) Settle(ctx context.Context, requestID string, actualQuota int64) (*model.QuotaReservation, error) {
	if b == nil || b.quota == nil {
		return nil, nil
	}
	return b.quota.Settle(ctx, requestID, actualQuota)
}

// Release 全额退还第 requestID 号预留（幂等，请求失败时调用）。
func (b *Billing) Release(ctx context.Context, requestID string) error {
	if b == nil || b.quota == nil {
		return nil
	}
	return b.quota.Release(ctx, requestID)
}

// PendingReserved 返回某用户在途预留的合计额度（用于计算可用额度）。
func (b *Billing) PendingReserved(ctx context.Context, userID uint64) (int64, error) {
	if b == nil || b.quota == nil {
		return 0, nil
	}
	return b.quota.PendingAmount(ctx, userID)
}

// estimatePromptTokens 由请求体字节数估算 prompt token 数（保守上界）。
func estimatePromptTokens(promptBytes int) int64 {
	if promptBytes <= 0 {
		return 0
	}
	return int64((promptBytes + estimateBytesPerToken - 1) / estimateBytesPerToken)
}
