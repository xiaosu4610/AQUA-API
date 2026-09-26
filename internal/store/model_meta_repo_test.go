// 模型实体与渠道模型映射仓储的单元测试。
//
// 测试重点：
//   - 模型名唯一、能力标签 JSON 往返、按关键词/厂商/启用状态筛选；
//   - UpsertBatch 的"存在即更新、不存在即新增"语义；
//   - 映射整组替换（旧行被清空）、同渠道内上游模型名重复被拒。
//
// 说明：使用真实 SQLite（newTestStore）而非 mock——UPSERT、唯一索引与 JSON
// 存储都必须在真实数据库上才能验证。
package store

import (
	"context"
	"errors"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestModelRepos 构造基于临时数据库的两个仓储。
func newTestModelRepos(t *testing.T) (model.ModelRepository, model.ChannelModelMappingRepository) {
	t.Helper()
	st := newTestStore(t)
	return NewModelMetaRepository(st.DB()), NewChannelModelMappingRepository(st.DB())
}

func TestModelMetaRepository_创建与查询(t *testing.T) {
	models, _ := newTestModelRepos(t)
	ctx := context.Background()

	m := &model.Model{
		Name:          "deepseek-chat",
		DisplayName:   "DeepSeek 对话",
		Vendor:        "deepseek",
		Description:   "通用对话模型",
		ContextLength: 65536,
		Enabled:       true,
		Capabilities:  []string{"chat", "stream", "tools"},
	}
	if err := models.Create(ctx, m); err != nil {
		t.Fatalf("创建模型失败: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("创建后应回填 ID")
	}

	byName, err := models.GetByName(ctx, "deepseek-chat")
	if err != nil {
		t.Fatalf("按名查询失败: %v", err)
	}
	if byName.Vendor != "deepseek" || byName.ContextLength != 65536 {
		t.Fatalf("读回数据不一致: %+v", byName)
	}
	if !byName.HasCapability("tools") || len(byName.Capabilities) != 3 {
		t.Fatalf("能力标签往返异常: %v", byName.Capabilities)
	}

	byID, err := models.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("按 ID 查询失败: %v", err)
	}
	if byID.Name != "deepseek-chat" {
		t.Fatalf("按 ID 读回的模型名不一致: %q", byID.Name)
	}
}

func TestModelMetaRepository_同名模型应被拒绝(t *testing.T) {
	models, _ := newTestModelRepos(t)
	ctx := context.Background()

	if err := models.Create(ctx, &model.Model{Name: "gpt-4o", Enabled: true}); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	err := models.Create(ctx, &model.Model{Name: "gpt-4o", Enabled: true})
	if !errors.Is(err, model.ErrModelMetaDuplicated) {
		t.Fatalf("重名应返回 ErrModelMetaDuplicated，实际 %v", err)
	}
}

func TestModelMetaRepository_List分页关键词厂商与启用筛选(t *testing.T) {
	models, _ := newTestModelRepos(t)

	ctx := context.Background()
	seed := []*model.Model{
		{Name: "deepseek-chat", DisplayName: "对话", Vendor: "deepseek", Enabled: true},
		{Name: "deepseek-r1", DisplayName: "推理", Vendor: "deepseek", Enabled: true},
		{Name: "gpt-4o", DisplayName: "GPT 对话", Vendor: "openai", Enabled: true},
		{Name: "legacy", DisplayName: "旧模型", Vendor: "openai", Enabled: false},
	}
	for _, m := range seed {
		if err := models.Create(ctx, m); err != nil {
			t.Fatalf("创建模型 %s 失败: %v", m.Name, err)
		}
	}

	all, err := models.List(ctx, model.ModelQuery{Limit: 50})
	if err != nil {
		t.Fatalf("查询模型列表失败: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("应有 4 个模型，实际 %d", len(all))
	}
	if all[0].Name != "deepseek-chat" {
		t.Fatalf("列表应按模型名升序，实际首项 %q", all[0].Name)
	}

	// 厂商筛选
	byVendor, err := models.List(ctx, model.ModelQuery{Vendor: "deepseek", Limit: 50})
	if err != nil {
		t.Fatalf("按厂商筛选失败: %v", err)
	}
	if len(byVendor) != 2 {
		t.Fatalf("deepseek 厂商应有 2 个模型，实际 %d", len(byVendor))
	}

	// 关键词筛选（命中展示名）
	byKeyword, err := models.List(ctx, model.ModelQuery{Keyword: "对话", Limit: 50})
	if err != nil {
		t.Fatalf("按关键词筛选失败: %v", err)
	}
	if len(byKeyword) != 2 {
		t.Fatalf("关键词『对话』应命中 2 个模型，实际 %d", len(byKeyword))
	}

	// 启用状态筛选
	disabled := false
	onlyDisabled, err := models.List(ctx, model.ModelQuery{Enabled: &disabled, Limit: 50})
	if err != nil {
		t.Fatalf("按启用状态筛选失败: %v", err)
	}
	if len(onlyDisabled) != 1 || onlyDisabled[0].Name != "legacy" {
		t.Fatalf("停用模型应只有 legacy，实际 %+v", onlyDisabled)
	}

	total, err := models.Count(ctx, model.ModelQuery{Vendor: "deepseek"})
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 2 {
		t.Fatalf("deepseek 模型数应为 2，实际 %d", total)
	}
}

func TestModelMetaRepository_Update不改名与Delete(t *testing.T) {
	models, _ := newTestModelRepos(t)
	ctx := context.Background()

	m := &model.Model{Name: "gpt-4o", Vendor: "openai", Enabled: true}
	if err := models.Create(ctx, m); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	m.ContextLength = 128000
	m.Capabilities = []string{"vision"}
	// 即使把 Name 改成别的值，仓储也不应改写数据库里的模型名
	m.Name = "renamed"
	if err := models.Update(ctx, m); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	got, err := models.GetByName(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("模型名不应被修改: %v", err)
	}
	if got.ContextLength != 128000 || !got.HasCapability("vision") {
		t.Fatalf("更新未生效: %+v", got)
	}
	if _, err := models.GetByName(ctx, "renamed"); !errors.Is(err, model.ErrModelMetaNotFound) {
		t.Fatal("模型名不允许被改名（令牌/渠道/映射通过它关联）")
	}

	if err := models.Delete(ctx, m.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if err := models.Delete(ctx, m.ID); !errors.Is(err, model.ErrModelMetaNotFound) {
		t.Fatalf("重复删除应返回 ErrModelMetaNotFound，实际 %v", err)
	}
}

func TestModelMetaRepository_UpsertBatch(t *testing.T) {
	models, _ := newTestModelRepos(t)
	ctx := context.Background()

	batch := []*model.Model{
		{Name: "m-one", DisplayName: "一号", Vendor: "v1", Enabled: true},
		{Name: "m-two", DisplayName: "二号", Vendor: "v1", Enabled: true},
	}
	if err := models.UpsertBatch(ctx, batch); err != nil {
		t.Fatalf("批量写入失败: %v", err)
	}
	if batch[0].ID == 0 {
		t.Fatal("批量写入后应回填 ID")
	}

	// 再次批量：m-one 更新、m-two 保持不变、m-three 新增
	second := []*model.Model{
		{Name: "m-one", DisplayName: "一号改", Vendor: "v2", Enabled: false},
		{Name: "m-three", DisplayName: "三号", Vendor: "v1", Enabled: true},
	}
	if err := models.UpsertBatch(ctx, second); err != nil {
		t.Fatalf("第二次批量写入失败: %v", err)
	}

	total, err := models.Count(ctx, model.ModelQuery{})
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 3 {
		t.Fatalf("批量 upsert 后应有 3 个模型，实际 %d", total)
	}

	updated, err := models.GetByName(ctx, "m-one")
	if err != nil {
		t.Fatalf("查询 m-one 失败: %v", err)
	}
	if updated.DisplayName != "一号改" || updated.Vendor != "v2" || updated.Enabled {
		t.Fatalf("已存在模型应被更新: %+v", updated)
	}
}

func TestChannelModelMappingRepository_整组替换(t *testing.T) {
	_, mappings := newTestModelRepos(t)
	ctx := context.Background()

	const channelID uint64 = 7
	first := []*model.ChannelModelMapping{
		{UpstreamModel: "deepseek-v3", PublicModel: "deepseek-chat", Priority: 10, Enabled: true},
		{UpstreamModel: "deepseek-r1", PublicModel: "deepseek-reasoner", Enabled: true},
	}
	if err := mappings.ReplaceForChannel(ctx, channelID, first); err != nil {
		t.Fatalf("首次替换失败: %v", err)
	}

	got, err := mappings.ListByChannel(ctx, channelID)
	if err != nil {
		t.Fatalf("查询映射失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条映射，实际 %d", len(got))
	}
	// 按优先级降序：priority=10 的排在前
	if got[0].UpstreamModel != "deepseek-v3" {
		t.Fatalf("应按优先级降序，实际首项 %q", got[0].UpstreamModel)
	}
	if got[0].ChannelID != channelID {
		t.Fatalf("渠道 ID 未被回填: %d", got[0].ChannelID)
	}

	// 整组替换：旧的两条应被清空，只剩新的一条
	if err := mappings.ReplaceForChannel(ctx, channelID, []*model.ChannelModelMapping{
		{UpstreamModel: "gpt-4o", PublicModel: "gpt-4o", Enabled: true},
	}); err != nil {
		t.Fatalf("再次替换失败: %v", err)
	}
	replaced, err := mappings.ListByChannel(ctx, channelID)
	if err != nil {
		t.Fatalf("查询映射失败: %v", err)
	}
	if len(replaced) != 1 || replaced[0].UpstreamModel != "gpt-4o" {
		t.Fatalf("整组替换后应只剩 1 条 gpt-4o，实际 %+v", replaced)
	}

	// 另一个渠道不受影响
	other, err := mappings.ListByChannel(ctx, 8)
	if err != nil {
		t.Fatalf("查询渠道 8 失败: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("渠道 8 不应有映射，实际 %d", len(other))
	}
}

func TestChannelModelMappingRepository_同上游名重复被拒(t *testing.T) {
	_, mappings := newTestModelRepos(t)
	ctx := context.Background()

	err := mappings.ReplaceForChannel(ctx, 1, []*model.ChannelModelMapping{
		{UpstreamModel: "gpt-4o", PublicModel: "a", Enabled: true},
		{UpstreamModel: "gpt-4o", PublicModel: "b", Enabled: true},
	})
	if !errors.Is(err, model.ErrChannelModelMappingDuplicated) {
		t.Fatalf("同上游名重复应返回 ErrChannelModelMappingDuplicated，实际 %v", err)
	}
}
