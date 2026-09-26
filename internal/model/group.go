// 本文件定义「模型分组」领域模型与仓储接口。
//
// 意图（Why）：
//
//	"分组"是本网关的运营抓手：渠道归属于某个分组，计价规则也按分组区分，
//	于是"给不同人群不同的价格与不同的上游"这件事只需要改分组配置。
//
//	但分组一直只是散落在渠道与价格表里的字符串，没有任何实体。
//	把分组提升为一等实体后，能得到三件事：
//	  1) 管理员能看到"系统里有哪些分组"，而不是靠翻渠道列表去猜；
//	  2) 可以给分组设置【计费倍率】——按客群差异化定价的常见诉求；
//	  3) 分组名写错时能在界面上被检查出来（未知分组不再静默生效）。
//
// 倍率口径（与全项目一致，改动时必须同步迁移注释与前端说明）：
//
//	ratio 是百分比整数：100 = 1.0 倍、150 = 1.5 倍、50 = 0.5 倍。
//	最终额度 = 基础额度 × ratio / 100，向下取整。
//	用整数而不是浮点：额度扣减每天发生几十万次，浮点误差会累积成对不上的账。
//
// 流转（Flow）：
//
//	后台维护：GroupsView → ModelGroupRepository.Create/Update/Delete
//	转发计费：relay.Billing 查价格时同时查倍率 → quota = base × ratio / 100
//	模型广场：按分组聚合展示"该分组下有哪些模型、价格多少"
//
// 扩展（Extend）：
//
//	新增分组维度（如按用户等级自动分组、分组可用模型白名单）时：
//	在本结构体加字段 + 建新迁移加列 + 同步 store/group_repo.go 的列清单三处。
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 分组相关的领域错误。
var (
	// ErrModelGroupNotFound 表示分组不存在。
	ErrModelGroupNotFound = errors.New("model: 模型分组不存在")
	// ErrModelGroupDuplicated 表示分组名已存在。
	ErrModelGroupDuplicated = errors.New("model: 分组名已存在")
)

// DefaultGroupName 是默认分组名。
//
// 与迁移 0011 中初始化的分组保持一致：未指定分组时一律使用它，
// 因此它必须始终存在（迁移中已用 INSERT OR IGNORE 保证）。
const DefaultGroupName = "default"

// ratioScale 是倍率的换算基数（百分比）。
const ratioScale int64 = 100

// ModelGroup 表示一个模型分组。
type ModelGroup struct {
	ID          uint64 // 主键
	Name        string // 分组标识（小写，被渠道与价格表引用）
	DisplayName string // 展示名（留空时界面回退 Name）
	// Ratio 是计费倍率（百分比整数，100 = 1.0 倍）。
	Ratio       int64
	Description string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Validate 校验分组的必要字段。
func (g *ModelGroup) Validate() error {
	name := strings.TrimSpace(g.Name)
	if name == "" {
		return errors.New("分组标识不能为空")
	}
	// 强制小写：否则 "VIP" 与 "vip" 会被当成两个分组，
	// 而渠道里填的是哪一个完全取决于使用者的手滑，排查成本极高。
	if name != strings.ToLower(name) {
		return fmt.Errorf("分组标识只能使用小写字母（当前为 %q）", name)
	}
	if strings.ContainsAny(name, " \t\n/\\,，") {
		return fmt.Errorf("分组标识不能包含空格、逗号或斜杠（当前为 %q）", name)
	}
	if len(name) > 64 {
		return fmt.Errorf("分组标识最多 64 个字符，当前 %d", len(name))
	}
	// 倍率下限 1（0 或负数会让所有调用变成免费或"倒给额度"）
	if g.Ratio <= 0 {
		return fmt.Errorf("计费倍率必须大于 0（100 表示 1.0 倍），当前 %d", g.Ratio)
	}
	// 上限 100000（1000 倍）：超过这个数量级几乎必然是填错了单位
	// （例如把"1.5 倍"写成了 15 万），拦下来比事后追账便宜得多。
	if g.Ratio > 100_000 {
		return fmt.Errorf("计费倍率过大（当前 %d，100 表示 1.0 倍），请检查是否填错单位", g.Ratio)
	}
	return nil
}

// Label 返回界面展示名（未设置展示名时回退标识）。
func (g *ModelGroup) Label() string {
	if strings.TrimSpace(g.DisplayName) != "" {
		return g.DisplayName
	}
	return g.Name
}

// ApplyRatio 按本分组的倍率换算额度（向下取整）。
//
// 说明：倍率为 100 时完全不改变数值，因此默认分组不会引入任何误差。
func (g *ModelGroup) ApplyRatio(base int64) int64 {
	if g == nil || base <= 0 {
		return base
	}
	ratio := g.Ratio
	if ratio <= 0 {
		ratio = ratioScale
	}
	return base * ratio / ratioScale
}

// ModelGroupQuery 是分组列表的查询条件。
type ModelGroupQuery struct {
	// EnabledOnly 为 true 时只返回启用的分组。
	EnabledOnly bool
	Limit       int
	Offset      int
}

// ModelGroupRepository 定义分组的持久化操作。
type ModelGroupRepository interface {
	// Create 新增分组，同名冲突时返回 ErrModelGroupDuplicated。
	Create(ctx context.Context, group *ModelGroup) error

	// GetByName 按标识查询，不存在时返回 ErrModelGroupNotFound。
	GetByName(ctx context.Context, name string) (*ModelGroup, error)

	// List 查询分组列表（按名称升序，保证界面顺序稳定）。
	List(ctx context.Context, query ModelGroupQuery) ([]*ModelGroup, error)

	// Count 统计分组数量。
	Count(ctx context.Context, query ModelGroupQuery) (int64, error)

	// Update 按 ID 更新分组。
	//
	// 约定：不改分组标识（name）。标识被渠道与价格表引用，
	// 改名等于让历史配置全部失联；需要改名时应新建分组再迁移。
	Update(ctx context.Context, group *ModelGroup) error

	// Delete 按 ID 删除分组，不存在时返回 ErrModelGroupNotFound。
	//
	// 注意：调用方应自行确认没有渠道/价格仍引用该分组（见 server 层的校验）。
	Delete(ctx context.Context, id uint64) error
}
