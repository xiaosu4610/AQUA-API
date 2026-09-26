// 本文件定义「模型」实体、「渠道模型映射」实体，以及两者的仓储接口与两条
// 映射解析纯函数（ResolveMapping / ResolvePublicModel）。
//
// 意图（Why）：
//
//	此前"模型"只是渠道上的一个字符串，没有任何实体：管理员看不到系统里到底
//	有哪些模型、无法登记展示名/厂商/能力标签、也无法把"对外模型名"与"上游
//	模型名"分开。把模型提升为一等实体后，配合渠道级映射，能得到三件事：
//	  1) 一张统一的模型清单（展示名、厂商、上下文长度、能力标签）；
//	  2) 请求方向：对外名 → 上游名；响应方向：上游名 → 对外名（回写）；
//	  3) 删除前可统计"有多少令牌/分组/渠道仍引用该模型"。
//
//	映射解析刻意做成【纯函数】放在领域层，而不是散落在 relay 里，原因：
//	  1) 计费、日志、上游回写、模型广场都可能需要同一套匹配口径；多写一份必然漂移；
//	  2) 纯函数无副作用、易测，配合"最长前缀 + 同优先级按长度与 ID 排序"的规则
//	     可以保证同一份数据每次解析结果完全一致。
//
// 匹配口径（唯一真相，改动时必须同步迁移 0016 的注释与后台界面说明）：
//
//	映射记录一条"对外模型名 ↔ 上游模型名"的对应关系，两侧都允许尾部通配符 `*`
//	（表示前缀族，如 `deepseek-*`）。解析规则：
//	  1) 精确匹配优先于通配匹配；
//	  2) 同为通配时，前缀越长越优先（最长前缀）；
//	  3) 更优先者优先（priority 数值大者胜）；
//	  4) 仍相同则按 id 升序（保证结果确定，绝不依赖遍历/Map 顺序）。
//	若结果一侧带 `*`，用输入侧通配捕获到的后缀做替换（如 对外 `aqua-*` →
//	上游 `deepseek-*`，请求 `aqua-chat` 得到上游 `deepseek-chat`）。
//
// 流转（Flow）：
//
//	后台维护：ModelRepository / ChannelModelMappingRepository（见 store 实现）
//	转发解析：relay 取某渠道的映射 → ResolveMapping(对外名) → 上游名
//	          → 上游回包 → ResolvePublicModel(上游名) → 对外名回写
//
// 扩展（Extend）：
//
//	新增模型字段（如最大输出长度、模态）：在 Model 结构体加字段 + 建新迁移加列
//	+ 同步 store/model_meta_repo.go 的列清单与 scan 四处。
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 与模型实体相关的领域错误。
var (
	// ErrModelMetaNotFound 表示模型不存在（按 ID 或名字都未命中）。
	ErrModelMetaNotFound = errors.New("model: 模型不存在")
	// ErrModelMetaDuplicated 表示对外模型名已存在。
	ErrModelMetaDuplicated = errors.New("model: 该模型名已存在")
	// ErrChannelModelMappingDuplicated 表示同一渠道内上游模型名重复。
	ErrChannelModelMappingDuplicated = errors.New("model: 同一渠道内上游模型名重复")
)

// 模型字段的长度上限：取值宽松但足以拦住"误粘贴整段配置"这类输入。
const (
	// ModelMaxNameLength 是模型名的最大长度。
	ModelMaxNameLength = 200
	// 厂商与展示名通常远短于模型名，单独给一个较小的上限。
	modelMaxVendorLength = 64
	// wildcardSuffix 是映射模式使用的尾部通配符。
	wildcardSuffix = "*"
)

// Model 表示一个对外模型实体。
type Model struct {
	ID          uint64 // 主键
	Name        string // 对外模型名（唯一，客户端请求时使用）
	DisplayName string // 展示名（留空时界面回退 Name）
	Vendor      string // 厂商（如 deepseek）
	Description string // 说明
	// ContextLength 是上下文长度（token）；0 表示未登记。
	ContextLength int64
	Enabled       bool     // 是否启用
	Capabilities  []string // 能力标签，如 ["chat","stream","tools"]
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Validate 校验模型的必要字段。
func (m *Model) Validate() error {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return errors.New("模型名不能为空")
	}
	if len(name) > ModelMaxNameLength {
		return fmt.Errorf("模型名最多 %d 个字符，当前 %d", ModelMaxNameLength, len(name))
	}
	// 模型名里出现空白几乎必然是误输入（如从表格粘贴带了制表符），
	// 且空白会让"看起来一样的两个模型名"实际不相等，排查成本极高。
	if strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("模型名不能包含空白字符（当前为 %q）", name)
	}
	if len(strings.TrimSpace(m.Vendor)) > modelMaxVendorLength {
		return fmt.Errorf("厂商名最多 %d 个字符", modelMaxVendorLength)
	}
	if m.ContextLength < 0 {
		return fmt.Errorf("上下文长度不能为负数（当前 %d）", m.ContextLength)
	}
	return nil
}

// Label 返回界面展示名（未设置展示名时回退模型名）。
func (m *Model) Label() string {
	if strings.TrimSpace(m.DisplayName) != "" {
		return m.DisplayName
	}
	return m.Name
}

// HasCapability 判断模型是否声明了某项能力（大小写敏感）。
func (m *Model) HasCapability(capability string) bool {
	for _, c := range m.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// NormalizedCapabilities 返回去掉空白项后的能力列表。
//
// 放在领域层而不是仓储里：写入路径（单个创建 / 批量 upsert）都要做同样的归一，
// 集中一处才能保证"落库的能力标签"始终干净。
func (m *Model) NormalizedCapabilities() []string {
	result := make([]string, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if v := strings.TrimSpace(c); v != "" {
			result = append(result, v)
		}
	}
	return result
}

// ChannelModelMapping 表示一条渠道级的模型映射。
//
// 语义：对外模型名 public_model ↔ 上游模型名 upstream_model，两侧都支持尾部通配符。
type ChannelModelMapping struct {
	ID            uint64 // 主键
	ChannelID     uint64 // 所属渠道 ID
	UpstreamModel string // 上游模型名（支持尾部通配符 *）
	PublicModel   string // 对外模型名（支持尾部通配符 *）
	Priority      int    // 优先级，数值越大越优先
	Enabled       bool   // 是否启用
	Remark        string // 备注
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Validate 校验映射的必要字段。
func (m *ChannelModelMapping) Validate() error {
	if m.ChannelID == 0 {
		return errors.New("渠道 ID 不能为 0")
	}
	upstream := strings.TrimSpace(m.UpstreamModel)
	if upstream == "" {
		return errors.New("上游模型名不能为空")
	}
	public := strings.TrimSpace(m.PublicModel)
	if public == "" {
		return errors.New("对外模型名不能为空")
	}
	if strings.ContainsAny(upstream, " \t\r\n") {
		return fmt.Errorf("上游模型名不能包含空白字符（当前为 %q）", upstream)
	}
	if strings.ContainsAny(public, " \t\r\n") {
		return fmt.Errorf("对外模型名不能包含空白字符（当前为 %q）", public)
	}
	return nil
}

// ModelQuery 是模型列表的查询条件。
type ModelQuery struct {
	Keyword string // 按模型名/展示名模糊匹配；空表示不过滤
	Vendor  string // 按厂商精确过滤；空表示不过滤
	// Enabled 为 nil 表示不按启用状态过滤；true 只看启用，false 只看停用。
	Enabled *bool
	Limit   int
	Offset  int
}

// ModelRepository 定义模型实体的持久化操作。
type ModelRepository interface {
	// Create 新增模型，同名冲突时返回 ErrModelMetaDuplicated。
	Create(ctx context.Context, m *Model) error

	// GetByID 按主键查询，不存在时返回 ErrModelMetaNotFound。
	GetByID(ctx context.Context, id uint64) (*Model, error)

	// GetByName 按对外模型名查询，不存在时返回 ErrModelMetaNotFound。
	GetByName(ctx context.Context, name string) (*Model, error)

	// List 查询模型列表（按模型名升序，保证界面稳定分页）。
	List(ctx context.Context, query ModelQuery) ([]*Model, error)

	// Count 统计符合条件的模型数量。
	Count(ctx context.Context, query ModelQuery) (int64, error)

	// Update 按 ID 更新模型。
	//
	// 约定：不改模型名（name）。它被令牌白名单、渠道清单与映射引用，
	// 改名等于让历史配置静默失联；需要改名时应新建模型再迁移引用。
	Update(ctx context.Context, m *Model) error

	// Delete 按 ID 删除，不存在时返回 ErrModelMetaNotFound。
	Delete(ctx context.Context, id uint64) error

	// UpsertBatch 批量写入：按模型名存在则更新、不存在则新增。
	//
	// 用途：从上游拉取模型清单后一键导入。逐条 Create 会因重名失败，
	// 因此需要"存在即更新"的语义。
	UpsertBatch(ctx context.Context, models []*Model) error
}

// ChannelModelMappingRepository 定义渠道级模型映射的持久化操作。
type ChannelModelMappingRepository interface {
	// ListByChannel 查询某渠道的全部映射（含停用），
	// 固定按「优先级降序、ID 升序」返回，供后台展示与转发取用。
	ListByChannel(ctx context.Context, channelID uint64) ([]*ChannelModelMapping, error)

	// ReplaceForChannel 用给定映射【整组替换】该渠道的映射。
	//
	// 之所以是整组替换而不是逐条增删：后台界面通常是一张表一次性提交，
	// 整组替换能保证"界面所见 = 落库结果"，避免中间态与孤儿行。
	// 同一渠道内上游模型名重复时返回 ErrChannelModelMappingDuplicated。
	ReplaceForChannel(ctx context.Context, channelID uint64, mappings []*ChannelModelMapping) error
}

// ---------------------------------------------------------------------------
// 映射解析（纯函数）
// ---------------------------------------------------------------------------

// ResolveMapping 按对外模型名解析出应向上游请求的模型名。
//
// 参数 mappings 通常来自某渠道的映射列表（可含停用项，本函数会跳过停用项）；
// publicModel 是客户端请求的模型名。
//
// 匹配优先级见文件头注释：精确 > 最长前缀 > 高优先级 > 小 ID。
// 返回值 ok 为 false 表示没有任何映射命中（调用方应原样透传模型名）。
func ResolveMapping(mappings []*ChannelModelMapping, publicModel string) (string, bool) {
	return resolveMapping(mappings, publicModel,
		func(m *ChannelModelMapping) string { return m.PublicModel },
		func(m *ChannelModelMapping) string { return m.UpstreamModel })
}

// ResolvePublicModel 按上游返回的模型名解析出对外的模型名（用于回写响应体）。
//
// 例如上游把请求的 `deepseek-chat` 回包成 `deepseek-chat-0731`，
// 若配置了 upstream `deepseek-*` ↔ public `deepseek-*`，则回写为 `deepseek-chat`。
// 返回值 ok 为 false 表示未命中（调用方应保留上游原始模型名）。
func ResolvePublicModel(mappings []*ChannelModelMapping, upstreamModel string) (string, bool) {
	return resolveMapping(mappings, upstreamModel,
		func(m *ChannelModelMapping) string { return m.UpstreamModel },
		func(m *ChannelModelMapping) string { return m.PublicModel })
}

// resolveMapping 是两条解析函数的公共实现。
//
// patternOf 取该映射用于"匹配输入"的一侧，resultOf 取"作为结果输出"的一侧。
// 选择最优先候选时线性扫描并用 strict 比较，不依赖任何 Map 遍历顺序，
// 因此对同一份数据结果恒定。
func resolveMapping(
	mappings []*ChannelModelMapping,
	input string,
	patternOf func(*ChannelModelMapping) string,
	resultOf func(*ChannelModelMapping) string,
) (string, bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", false
	}

	var (
		best         *ChannelModelMapping
		bestExact    bool
		bestPrefix   int
		bestCaptured string
	)
	for _, m := range mappings {
		if m == nil || !m.Enabled {
			continue
		}
		exact, prefixLen, captured, ok := patternMatch(patternOf(m), input)
		if !ok {
			continue
		}
		if best == nil || candidateBetter(exact, prefixLen, m, bestExact, bestPrefix, best) {
			best, bestExact, bestPrefix, bestCaptured = m, exact, prefixLen, captured
		}
	}
	if best == nil {
		return "", false
	}
	return applyCapturedWildcard(resultOf(best), bestCaptured), true
}

// candidateBetter 判断候选参加者是否优于当前最优者。
//
// 顺序：精确优先 → 优先级高者 → 前缀长者 → ID 小者。
// 这里的顺序即文件头所述匹配口径，改动时必须与注释同步。
func candidateBetter(
	exact bool, prefixLen int, cand *ChannelModelMapping,
	bestExact bool, bestPrefixLen int, best *ChannelModelMapping,
) bool {
	if exact != bestExact {
		return exact
	}
	if cand.Priority != best.Priority {
		return cand.Priority > best.Priority
	}
	if prefixLen != bestPrefixLen {
		return prefixLen > bestPrefixLen
	}
	return cand.ID < best.ID
}

// patternMatch 判断 pattern（可带尾部 * ）是否匹配 input。
//
// 返回：
//   - exact：是否为精确匹配（pattern 不含通配符且完全相等）；
//   - prefixLen：通配匹配时的前缀长度（用于"最长前缀优先"比较）；
//   - captured：通配捕获到的后缀（用于结果侧通配替换）；
//   - ok：是否命中。
func patternMatch(pattern, input string) (exact bool, prefixLen int, captured string, ok bool) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return false, 0, "", false
	}
	if !strings.HasSuffix(p, wildcardSuffix) {
		if p == input {
			return true, len(p), "", true
		}
		return false, 0, "", false
	}
	prefix := strings.TrimSuffix(p, wildcardSuffix)
	if strings.HasPrefix(input, prefix) {
		return false, len(prefix), input[len(prefix):], true
	}
	return false, 0, "", false
}

// applyCapturedWildcard 在结果侧带 * 时用捕获到的后缀替换之。
//
// 例：结果 `deepseek-*` + 捕获 `chat` → `deepseek-chat`。
// 结果侧不含 * 时原样返回（表示"多个对外名收敛到一个上游名"这类非通配映射）。
func applyCapturedWildcard(result, captured string) string {
	if strings.HasSuffix(result, wildcardSuffix) {
		return strings.TrimSuffix(result, wildcardSuffix) + captured
	}
	return result
}
