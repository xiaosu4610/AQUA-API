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
//	转发完成 → relay.recordUsage
//	  └─ Billing.Charge(userID, tokenID, model, prompt, completion)
//	       ├─ priceFor(model)          带缓存的价格匹配
//	       ├─ ComputeQuota(...)        换算额度
//	       ├─ tokens.ConsumeQuota      扣令牌额度（原子）
//	       └─ users.AddUsedQuota       累加用户已用额度（原子）
//
// 扩展（Extend）：
//
//	新增计价维度（缓存命中价、按次计费、图片张数）时：
//	  在 Charge 里扩展入参并同步 model.ModelPrice 的计算函数与迁移脚本。
package relay

import (
	"context"
	"log/slog"
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

// Billing 负责按用量计费，并发安全。
type Billing struct {
	prices model.ModelPriceRepository
	tokens model.TokenRepository
	users  model.UserRepository
	group  string

	// 价格缓存：规则数量少（几十条）且读多写少，适合整体缓存
	mu       sync.RWMutex
	cached   []*model.ModelPrice
	cachedAt time.Time
}

// NewBilling 创建计费组件。
//
// group 为空时使用 default；tokens / users 允许为 nil（此时只计算不扣减，
// 便于单元测试与"仅统计不限制"的部署形态）。
func NewBilling(prices model.ModelPriceRepository, tokens model.TokenRepository,
	users model.UserRepository, group string) *Billing {
	if group == "" {
		group = defaultBillingGroup
	}
	return &Billing{
		prices: prices,
		tokens: tokens,
		users:  users,
		group:  group,
	}
}

// Invalidate 清空价格缓存。
//
// 调用时机：后台新增/修改/删除计价规则之后。
// 不清理的话，管理员改完价格会看到"新价格要等半分钟才生效"，容易误判为没生效。
func (b *Billing) Invalidate() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.cachedAt = time.Time{}
	b.mu.Unlock()
}

// priceFor 返回适用于该模型的最优计价规则；无匹配时返回 nil（表示未定价）。
//
// 缓存实现：整表缓存（按分组 + 仅启用），过期后重新加载。
// 选择"整表"而不是"按模型缓存"，是因为规则数极少而模型名组合无限，
// 按模型缓存反而会让缓存无限膨胀。
func (b *Billing) priceFor(ctx context.Context, modelName string) *model.ModelPrice {
	if b == nil || b.prices == nil {
		return nil
	}

	b.mu.RLock()
	fresh := time.Since(b.cachedAt) < priceCacheTTL
	cached := b.cached
	b.mu.RUnlock()

	if !fresh {
		prices, err := b.prices.List(ctx, b.group, true)
		if err != nil {
			// 读价失败不能阻断转发：按"未定价"处理（不扣费），并留下日志痕迹。
			// 取舍理由：宁可少收一点钱，也不能让用户因为后台数据问题而无法调用。
			slog.Warn("读取计价规则失败，本次按未定价处理", "error", err, "group", b.group)
			return nil
		}
		b.mu.Lock()
		b.cached = prices
		b.cachedAt = time.Now()
		b.mu.Unlock()
		cached = prices
	}

	return model.MatchModelPrice(cached, modelName)
}

// Quote 计算该次用量应扣的额度（只算不扣）。
//
// 用于后台预估、测试与展示；扣费请用 Charge。
func (b *Billing) Quote(ctx context.Context, modelName string, promptTokens, completionTokens int64) int64 {
	price := b.priceFor(ctx, modelName)
	return price.ComputeQuota(promptTokens, completionTokens)
}

// QuoteOnce 计算"调用一次该模型"应扣的额度（只算不扣）。
//
// 用途：异步任务在【提交时】就要扣费（见 model.Task 的文件头说明），
// 因此任务链路需要一个与 token 无关的计价入口。
// count 为本次生成的份数（如一次画 4 张图），<=0 时按 1 次处理。
func (b *Billing) QuoteOnce(ctx context.Context, modelName string, count int64) int64 {
	price := b.priceFor(ctx, modelName)
	return price.ComputePerCallQuota(count)
}

// ChargeOnce 按次计费并扣减额度，返回实际扣减的额度。
//
// 与 Charge 的关系：口径不同（按次 vs 按 token），扣减目标与容错策略完全一致。
func (b *Billing) ChargeOnce(ctx context.Context, userID, tokenID uint64, modelName string, count int64) int64 {
	if b == nil {
		return 0
	}

	price := b.priceFor(ctx, modelName)
	if price == nil {
		// 未定价：不扣费（与 token 计费保持同一语义，避免"没配价格就报错"）
		return 0
	}

	quota := price.ComputePerCallQuota(count)
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
func (b *Billing) Charge(ctx context.Context, userID, tokenID uint64, modelName string,
	promptTokens, completionTokens int64) int64 {
	if b == nil {
		return 0
	}

	price := b.priceFor(ctx, modelName)
	if price == nil {
		// 未定价：不扣费但照常记录日志（quota=0）。
		// 这样站长能从日志看出"哪些模型还没定价"，而不是被静默拦住。
		return 0
	}

	quota := price.ComputeQuota(promptTokens, completionTokens)
	if quota <= 0 {
		return 0
	}

	now := time.Now()
	if tokenID > 0 && b.tokens != nil {
		if err := b.tokens.ConsumeQuota(ctx, tokenID, quota, now); err != nil {
			slog.Warn("扣减令牌额度失败", "error", err, "token_id", tokenID, "quota", quota)
		}
	}
	if userID > 0 && b.users != nil {
		if err := b.users.AddUsedQuota(ctx, userID, quota); err != nil {
			slog.Warn("累加用户已用额度失败", "error", err, "user_id", userID, "quota", quota)
		}
	}

	return quota
}
