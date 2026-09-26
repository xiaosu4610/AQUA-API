// Package model 定义领域模型与仓储接口（不包含任何 SQL 与 HTTP 细节）。
//
// 意图（Why）：
//
//	把「业务概念」与「技术实现」解耦：本包只描述渠道、令牌等实体长什么样、
//	有哪些行为与约束；具体存到哪、怎么存，由 internal/store 实现本包定义的接口。
//	这样做的收益：
//	  1) 上层（server / relay）只依赖接口，替换存储实现无需改动业务代码；
//	  2) 领域规则（校验、状态判断）集中一处，避免散落在各处造成不一致。
//
// 流转（Flow）：
//
//	internal/store 实现 ChannelRepository 接口
//	  └─ cmd/aqua/main.go 装配后注入 internal/server 与 internal/relay
//	       └─ 业务代码仅面对 model.Channel 与 model.ChannelRepository
//
// 扩展（Extend）：
//
//	新增实体（如 Token、User）：
//	  1) 在本包新增实体文件（如 token.go），定义结构体 + 校验 + 仓储接口；
//	  2) 在 internal/store 下新增对应实现；
//	  3) 若需要新表，同步在 store/schema.sql 追加迁移。
//	注意：本包不得导入 internal/store、internal/server 等上层包，避免循环依赖。
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 领域错误定义。
//
// 设计说明：业务层通过 errors.Is 判断错误类型，而不是比较错误字符串，
// 因此这些错误应由仓储实现原样返回（可用 %w 包装）。
var (
	// ErrChannelNotFound 表示按条件未找到渠道。
	ErrChannelNotFound = errors.New("model: 渠道不存在")
)

// ChannelStatus 表示渠道的可用状态。
//
// 取值与数据库字段 channels.status 一一对应，禁止随意改动数值
// （已落库的数据依赖这些值）。
type ChannelStatus int

const (
	// ChannelStatusEnabled 启用：可参与路由选择。
	ChannelStatusEnabled ChannelStatus = 1
	// ChannelStatusDisabled 手动禁用：由管理员主动关闭，自动健康检查不会将其恢复。
	ChannelStatusDisabled ChannelStatus = 2
	// ChannelStatusAutoDisabled 自动禁用：由熔断/健康检查判定不可用而关闭，
	// 后续健康检查通过后可由系统自动恢复。
	ChannelStatusAutoDisabled ChannelStatus = 3
)

// String 返回状态的中文名，便于日志与前端展示。
func (s ChannelStatus) String() string {
	switch s {
	case ChannelStatusEnabled:
		return "启用"
	case ChannelStatusDisabled:
		return "手动禁用"
	case ChannelStatusAutoDisabled:
		return "自动禁用"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

// IsValid 判断状态值是否合法（用于校验外部输入）。
func (s ChannelStatus) IsValid() bool {
	switch s {
	case ChannelStatusEnabled, ChannelStatusDisabled, ChannelStatusAutoDisabled:
		return true
	default:
		return false
	}
}

// Channel 表示一个上游渠道（一个供应商接入配置）。
//
// 重要安全约定：
//
//	APIKey 字段在内存中是【明文】，仅在进程内流转；
//	落库时由仓储负责加密（见 internal/store 与 internal/crypto），
//	读取时由仓储解密。因此本结构体绝不可被直接序列化返回给前端——
//	对外输出请使用 MaskedAPIKey()。
type Channel struct {
	ID        uint64        // 主键，新建时为 0（由数据库生成）
	Name      string        // 显示名，如 "OpenAI 官方"
	Type      int           // 渠道类型编号（M1 统一按 OpenAI 兼容处理）
	BaseURL   string        // 上游基础地址，如 https://api.openai.com
	APIKey    string        // 上游密钥【明文，仅内存】
	Models    []string      // 可用模型列表
	Group     string        // 所属分组，用于按分组路由与计费
	Priority  int           // 优先级，数值越大越优先
	Weight    int           // 同优先级内的随机权重，需 > 0
	Status    ChannelStatus // 可用状态
	CreatedAt time.Time     // 创建时间
	UpdatedAt time.Time     // 更新时间

	// LastTestAt 是最近一次测活时间；零值表示从未测活。
	LastTestAt time.Time
	// LastTestOK 表示最近一次测活是否通过。
	//
	// 说明：这是"最近一次"的瞬时结果，不是健康状态本身——
	// 渠道是否可用要看 Status（自动禁用由健康检查写入）。
	LastTestOK bool
}

// Validate 校验渠道的必要字段，供创建与更新时调用。
//
// 设计原则：把校验放在领域层而非 HTTP 层，保证无论从哪条路径写入
// （API、CLI、批量导入）都遵守同一套规则。
func (c *Channel) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("渠道名称不能为空")
	}
	if c.Type <= 0 {
		return fmt.Errorf("渠道类型非法: %d（必须为正整数）", c.Type)
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("渠道 base_url 不能为空")
	}
	// 只做基础格式检查，不引入 net/url 解析：允许使用者填写内网地址等特殊形式
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("渠道 base_url 必须以 http:// 或 https:// 开头，当前为 %q", c.BaseURL)
	}
	if c.Weight <= 0 {
		return fmt.Errorf("渠道权重必须大于 0，当前为 %d（权重为 0 会导致该渠道永远不会被选中）", c.Weight)
	}
	if c.Priority < 0 {
		return fmt.Errorf("渠道优先级不能为负数，当前为 %d", c.Priority)
	}
	if !c.Status.IsValid() {
		return fmt.Errorf("渠道状态非法: %d", int(c.Status))
	}
	if strings.TrimSpace(c.Group) == "" {
		return errors.New("渠道分组不能为空")
	}
	return nil
}

// HasModel 判断本渠道是否声明支持指定模型。
//
// 匹配规则为精确匹配（大小写敏感）。M1 暂不支持通配符与模型映射，
// 这些能力会在 M2 引入（届时模型映射会在路由前处理）。
func (c *Channel) HasModel(name string) bool {
	for _, m := range c.Models {
		if m == name {
			return true
		}
	}
	return false
}

// MaskedAPIKey 返回脱敏后的密钥，供日志与接口输出使用。
//
// 脱敏规则：保留前 6 位与后 4 位，中间用 **** 替代。
// 例如 "nvapi-abcdefghijklmn" → "nvapi-****klmn"。
// 说明：截断展示既能帮助运维辨认是"哪把钥匙"，又不足以被直接盗用。
func (c *Channel) MaskedAPIKey() string {
	const (
		keepPrefix = 6
		keepSuffix = 4
	)
	key := c.APIKey
	// 过短的密钥一律完全遮蔽，避免脱敏后仍可推断
	if len(key) <= keepPrefix+keepSuffix {
		if key == "" {
			return ""
		}
		return strings.Repeat("*", len(key))
	}
	return key[:keepPrefix] + "****" + key[len(key)-keepSuffix:]
}

// ChannelQuery 描述渠道列表的查询条件。
//
// 使用指针表示"可选过滤"：Status 为 nil 时表示不按状态过滤。
type ChannelQuery struct {
	Group  string         // 按分组过滤；空字符串表示不过滤
	Status *ChannelStatus // 按状态过滤；nil 表示不过滤
	Limit  int            // 返回条数上限；<=0 时使用默认值
	Offset int            // 偏移量，用于分页
}

// ChannelRepository 定义渠道的持久化操作。
//
// 约定：
//   - 所有方法的实现都必须保证 APIKey 在落库时被加密、读取时被解密；
//   - 未找到记录时，GetByID 返回 ErrChannelNotFound（调用方用 errors.Is 判断）。
type ChannelRepository interface {
	// Create 新增渠道，成功后回填 ID、CreatedAt、UpdatedAt。
	Create(ctx context.Context, ch *Channel) error

	// GetByID 按主键查询渠道，不存在时返回 ErrChannelNotFound。
	GetByID(ctx context.Context, id uint64) (*Channel, error)

	// List 按条件查询渠道列表，固定按「优先级降序、权重降序、ID 升序」返回，
	// 该顺序即路由选取候选时的推荐顺序。
	List(ctx context.Context, q ChannelQuery) ([]*Channel, error)

	// Count 返回符合条件的渠道总数，用于分页。
	Count(ctx context.Context, q ChannelQuery) (int, error)

	// StatusCounts 按状态分组统计渠道数量，用于仪表盘概览。
	StatusCounts(ctx context.Context) (map[ChannelStatus]int, error)

	// RecordTestResult 记录一次测活结果（时间与是否通过）。
	//
	// 之所以单独一个方法而不复用 Update：测活是高频的后台行为，
	// 若走 Update 会把整行配置一并写回，存在"用陈旧副本覆盖管理员刚改的配置"的风险。
	RecordTestResult(ctx context.Context, id uint64, at time.Time, ok bool) error

	// Update 按 ID 更新渠道（不修改创建时间），不存在时返回 ErrChannelNotFound。
	Update(ctx context.Context, ch *Channel) error

	// Delete 按 ID 删除渠道，不存在时返回 ErrChannelNotFound。
	//
	// 说明：M1 采用物理删除；后续若需要保留计费审计关联，会改为软删除。
	Delete(ctx context.Context, id uint64) error
}
