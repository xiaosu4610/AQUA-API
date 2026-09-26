// 分组仓储的单元测试。
//
// 测试重点：
//   - 迁移会初始化默认分组（它是历史数据的隐含分组，必须始终存在）；
//   - 分组名唯一（重名会让倍率取值变得不确定）；
//   - 更新不修改分组标识（标识被渠道/价格引用，改名会让配置静默失联）。
package store

import (
	"context"
	"errors"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestGroupRepo 构造基于临时数据库的分组仓储。
func newTestGroupRepo(t *testing.T) model.ModelGroupRepository {
	t.Helper()
	st := newTestStore(t)
	return NewModelGroupRepository(st.DB())
}

func TestModelGroupRepository_迁移初始化默认分组(t *testing.T) {
	repo := newTestGroupRepo(t)

	group, err := repo.GetByName(context.Background(), model.DefaultGroupName)
	if err != nil {
		t.Fatalf("默认分组应被迁移初始化: %v", err)
	}
	if group.Ratio != 100 {
		t.Fatalf("默认分组倍率应为 100（1.0 倍），实际 %d", group.Ratio)
	}
	if !group.Enabled {
		t.Fatal("默认分组应处于启用状态")
	}
}

func TestModelGroupRepository_创建与查询(t *testing.T) {
	repo := newTestGroupRepo(t)
	ctx := context.Background()

	group := &model.ModelGroup{
		Name:        "vip",
		DisplayName: "VIP 用户",
		Ratio:       150,
		Description: "给付费用户的更高配额分组",
		Enabled:     true,
	}
	if err := repo.Create(ctx, group); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	if group.ID == 0 {
		t.Fatal("创建后应回填 ID")
	}

	got, err := repo.GetByName(ctx, "vip")
	if err != nil {
		t.Fatalf("查询分组失败: %v", err)
	}
	if got.Ratio != 150 || got.DisplayName != "VIP 用户" {
		t.Fatalf("读回的数据不一致: %+v", got)
	}
}

func TestModelGroupRepository_标识统一小写(t *testing.T) {
	repo := newTestGroupRepo(t)
	ctx := context.Background()

	// 直接构造大写标识会被领域校验拦住（避免 "VIP" 与 "vip" 变成两个分组）
	if err := repo.Create(ctx, &model.ModelGroup{Name: "VIP", Ratio: 100, Enabled: true}); err == nil {
		t.Fatal("大写标识应被拒绝")
	}

	// 带空格的输入应被规范化后存储
	if err := repo.Create(ctx, &model.ModelGroup{Name: " vip2 ", Ratio: 100, Enabled: true}); err != nil {
		t.Fatalf("两侧空白应被裁剪: %v", err)
	}
	if _, err := repo.GetByName(ctx, "vip2"); err != nil {
		t.Fatalf("规范化后应能以 vip2 查到: %v", err)
	}
}

func TestModelGroupRepository_同名分组应被拒绝(t *testing.T) {
	repo := newTestGroupRepo(t)
	ctx := context.Background()

	first := &model.ModelGroup{Name: "vip", Ratio: 100, Enabled: true}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}

	err := repo.Create(ctx, &model.ModelGroup{Name: "vip", Ratio: 200, Enabled: true})
	if !errors.Is(err, model.ErrModelGroupDuplicated) {
		t.Fatalf("重名应返回 ErrModelGroupDuplicated，实际 %v", err)
	}
}

func TestModelGroupRepository_查询不存在(t *testing.T) {
	repo := newTestGroupRepo(t)

	_, err := repo.GetByName(context.Background(), "not-exist")
	if !errors.Is(err, model.ErrModelGroupNotFound) {
		t.Fatalf("应返回 ErrModelGroupNotFound，实际 %v", err)
	}
}

func TestModelGroupRepository_Update改倍率但不改标识(t *testing.T) {
	repo := newTestGroupRepo(t)
	ctx := context.Background()

	group := &model.ModelGroup{Name: "vip", Ratio: 100, Enabled: true}
	if err := repo.Create(ctx, group); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}

	group.Ratio = 250
	group.DisplayName = "VIP"
	// 即使把 Name 改成别的值，仓储也不应改写数据库里的标识
	group.Name = "vip-renamed"
	if err := repo.Update(ctx, group); err != nil {
		t.Fatalf("更新分组失败: %v", err)
	}

	got, err := repo.GetByName(ctx, "vip")
	if err != nil {
		t.Fatalf("标识不应被修改: %v", err)
	}
	if got.Ratio != 250 || got.DisplayName != "VIP" {
		t.Fatalf("更新未生效: %+v", got)
	}
	if _, err := repo.GetByName(ctx, "vip-renamed"); !errors.Is(err, model.ErrModelGroupNotFound) {
		t.Fatal("分组标识不允许被改名（渠道与价格表通过它关联）")
	}
}

func TestModelGroupRepository_List与Count(t *testing.T) {
	repo := newTestGroupRepo(t)
	ctx := context.Background()

	for _, name := range []string{"bronze", "silver"} {
		if err := repo.Create(ctx, &model.ModelGroup{Name: name, Ratio: 100, Enabled: true}); err != nil {
			t.Fatalf("创建分组 %s 失败: %v", name, err)
		}
	}
	// 一个被禁用的分组，用于验证 EnabledOnly 过滤
	if err := repo.Create(ctx, &model.ModelGroup{Name: "closed", Ratio: 100, Enabled: false}); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}

	all, err := repo.List(ctx, model.ModelGroupQuery{Limit: 50})
	if err != nil {
		t.Fatalf("查询分组列表失败: %v", err)
	}
	// default + bronze + silver + closed
	if len(all) != 4 {
		t.Fatalf("应有 4 个分组，实际 %d", len(all))
	}
	// 按名称升序：default 排在 bronze 之前？字母序 b < c < d，故 bronze 最先
	if all[0].Name != "bronze" {
		t.Fatalf("列表应按名称升序，实际首项为 %q", all[0].Name)
	}

	enabledOnly, err := repo.List(ctx, model.ModelGroupQuery{EnabledOnly: true, Limit: 50})
	if err != nil {
		t.Fatalf("查询启用分组失败: %v", err)
	}
	if len(enabledOnly) != 3 {
		t.Fatalf("启用分组应有 3 个，实际 %d", len(enabledOnly))
	}

	total, err := repo.Count(ctx, model.ModelGroupQuery{})
	if err != nil {
		t.Fatalf("统计分组数失败: %v", err)
	}
	if total != 4 {
		t.Fatalf("分组总数应为 4，实际 %d", total)
	}
}

func TestModelGroupRepository_Delete(t *testing.T) {
	repo := newTestGroupRepo(t)
	ctx := context.Background()

	group := &model.ModelGroup{Name: "temp", Ratio: 100, Enabled: true}
	if err := repo.Create(ctx, group); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	if err := repo.Delete(ctx, group.ID); err != nil {
		t.Fatalf("删除分组失败: %v", err)
	}
	if err := repo.Delete(ctx, group.ID); !errors.Is(err, model.ErrModelGroupNotFound) {
		t.Fatalf("重复删除应返回 ErrModelGroupNotFound，实际 %v", err)
	}
}
