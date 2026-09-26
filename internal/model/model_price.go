// 本文件定义「模型计费价格」领域模型与仓储接口，并给出计费计算。
//
// 意图（Why）：
//
//	网关的核心商业能力是"用量可计量、可结算"。没有价格表，
//	额度就只是一个永远不减少的数字——站长无法限制用户消耗，
//	也无法对上游成本做任何核算。
//
//	把"价格怎么匹配、额度怎么算"放在领域层而不是 relay 或 SQL 里，原因：
//	  1) 计费口径必须唯一：任何一处算错都直接造成多收/少收；
//	  2) 需要被多处复用（转发后扣费、后台预估、报表核算）。
//
// 计费口径（唯一真相，改动时必须同步迁移脚本注释与前端说明）：
//
//	quota = (promptTokens × promptPrice + completionTokens × completionPrice) / 1_000_000
//
//	价格字段表示「每 100 万 token 消耗的站点额度单位」，
//	与上游官方报价口径一致（如 $3 / 1M tokens），避免二次换算。
//
// 金额一律用 int64 整数（不用浮点）：浮点累加会产生"用了一万次之后差 0.3"这类
// 难以复现的账目问题，而额度扣减是每天发生几十万次的高频操作。
//
// 流转（Flow）：
//
//	后台维护：PricesView → ModelPriceRepository.Create/Update/Delete
//	转发计费：relay 拿到 usage → Billing.Quote(model, group, usage) → 扣减令牌/用户额度
//
// 扩展（Extend）：
//
//	新增计价维度（如按缓存命中价、按图片张数）时：
//	  1) 在本文件加字段与计算分支；
//	  2) 建新迁移加列（切勿改动已发布的 0006）；
//	  3) 同步前端表单与后台接口。
package model

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrModelPriceNotFound 表示不存在匹配的计价规则。
var ErrModelPriceNotFound = errors.New("model: 计价规则不存在")

// ErrModelPriceDuplicated 表示同分组下已存在同名规则。
var ErrModelPriceDuplicated = errors.New("model: 该分组下已存在同名计价规则")

// quotaScale 是价格口径的换算基数：价格字段为"每 100 万 token"。
//
// 单独定义成常量而不是在公式里写 1_000_000，是为了让"口径"这件事在代码里
// 有一个可被检索的名字——将来若改成"每千 token"，只需改这一处并同步注释。
const quotaScale int64 = 1_000_000

// wildcardAll 是"匹配全部模型"的通配规则。
const wildcardAll = "*"

// ModelPrice 表示一条模型计价规则。
type ModelPrice struct {
	ID              uint64    // 主键
	Model           string    // 模型名或通配模式（"gpt-4*"、"*"）
	PromptPrice     int64     // 每 1M 输入 token 的额度
	CompletionPrice int64     // 每 1M 输出 token 的额度
	Group           string    // 适用分组
	Enabled         bool      // 是否启用（停用即视为未定价）
	Remark          string    // 备注（便于说明定价依据）
	CreatedAt       time.Time // 创建时间
	UpdatedAt       time.Time // 更新时间
}

// Validate 校验计价规则。
func (p *ModelPrice) Validate() error {
	if strings.TrimSpace(p.Model) == "" {
		return errors.New("模型名不能为空（可用 * 表示全部模型）")
	}
	if strings.TrimSpace(p.Group) == "" {
		return errors.New("分组不能为空")
	}
	// 价格为负会变成"调用反而加额度"，必须拦住
	if p.PromptPrice < 0 || p.CompletionPrice < 0 {
		return fmt.Errorf("价格不能为负数（输入 %d / 输出 %d）", p.PromptPrice, p.CompletionPrice)
	}
	return nil
}

// PatternKind 描述规则模式的类型，用于匹配优先级判定。
type PatternKind int

const (
	// PatternExact 精确匹配（如 "gpt-4o"）
	PatternExact PatternKind = iota
	// PatternPrefix 前缀通配（如 "gpt-4*"）
	PatternPrefix
	// PatternAll 全局通配（"*"）
	PatternAll
)

// PatternKind 返回该规则的模式类型。
func (p *ModelPrice) PatternKind() PatternKind {
	pattern := strings.TrimSpace(p.Model)
	if pattern == wildcardAll {
		return PatternAll
	}
	if strings.HasSuffix(pattern, "*") {
		return PatternPrefix
	}
	return PatternExact
}

// Matches 判断该规则是否适用于给定模型名。
//
// 匹配规则：
//   - 精确：完全相等；
//   - 前缀通配："gpt-4*" 匹配 "gpt-4o"、"gpt-4-turbo"；
//   - 全局通配："*" 匹配一切。
//
// 匹配是大小写敏感的：模型名是上游定义的标识符，
// 大小写不同通常代表不同模型（如 "GPT-4" 与 "gpt-4" 在部分上游是两个条目）。
func (p *ModelPrice) Matches(modelName string) bool {
	pattern := strings.TrimSpace(p.Model)
	switch p.PatternKind() {
	case PatternAll:
		return true
	case PatternPrefix:
		return strings.HasPrefix(modelName, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == modelName
	}
}

// specificity 返回模式的具体程度（数值越大越优先）。
//
// 这样排序后即可实现"精确 > 长前缀 > 短前缀 > 全局"的优先级，
// 而不需要写一串嵌套判断。
func (p *ModelPrice) specificity() int {
	switch p.PatternKind() {
	case PatternExact:
		return 10000 + len(p.Model)
	case PatternPrefix:
		return 1000 + len(p.Model)
	default:
		return 0
	}
}

// ComputeQuota 按用量计算应扣额度。
//
// 公式见文件头注释；采用整数运算并向下取整（不足 1 单位不计费），
// 这样小额调用不会因四舍五入而虚增费用。
//
// 溢出安全：promptTokens 与 price 的乘积上限约 10^15，远小于 int64 上限（9.2×10^18），
// 因此不需要额外的溢出保护。
func (p *ModelPrice) ComputeQuota(promptTokens, completionTokens int64) int64 {
	if p == nil {
		return 0
	}
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	return (promptTokens*p.PromptPrice + completionTokens*p.CompletionPrice) / quotaScale
}

// MatchModelPrice 从一组规则中挑出最适用的那一条。
//
// 优先级：精确匹配 → 前缀最长 → 全局通配；同类内若有多条（理论上被唯一索引拦住），
// 取 ID 最小的以保证结果稳定可复现。
//
// 返回 nil 表示没有任何规则适用（此时按"未定价"处理：不扣费但照常记录日志，
// 由站长自行决定是否为该模型补价格）。
func MatchModelPrice(prices []*ModelPrice, modelName string) *ModelPrice {
	var best *ModelPrice
	bestScore := -1

	for _, price := range prices {
		if price == nil || !price.Enabled {
			continue
		}
		if !price.Matches(modelName) {
			continue
		}
		score := price.specificity()
		// 同分时取 ID 更小的，保证同一份数据每次匹配结果一致
		if score > bestScore || (score == bestScore && best != nil && price.ID < best.ID) {
			best = price
			bestScore = score
		}
	}
	return best
}

// SortModelPrices 按"具体到笼统"的顺序排列规则，便于后台展示与人工核对。
func SortModelPrices(prices []*ModelPrice) {
	sort.SliceStable(prices, func(i, j int) bool {
		left, right := prices[i].specificity(), prices[j].specificity()
		if left != right {
			return left > right
		}
		return prices[i].ID < prices[j].ID
	})
}

// ModelPriceRepository 定义计价规则的持久化操作。
type ModelPriceRepository interface {
	// Create 新增规则，同分组同名冲突时返回 ErrModelPriceDuplicated。
	Create(ctx context.Context, price *ModelPrice) error

	// GetByID 按主键查询，不存在时返回 ErrModelPriceNotFound。
	GetByID(ctx context.Context, id uint64) (*ModelPrice, error)

	// List 查询规则；group 为空表示不过滤分组，仅启用的规则由 enabledOnly 控制。
	List(ctx context.Context, group string, enabledOnly bool) ([]*ModelPrice, error)

	// Update 按 ID 更新，不存在时返回 ErrModelPriceNotFound。
	Update(ctx context.Context, price *ModelPrice) error

	// Delete 按 ID 删除，不存在时返回 ErrModelPriceNotFound。
	Delete(ctx context.Context, id uint64) error
}
