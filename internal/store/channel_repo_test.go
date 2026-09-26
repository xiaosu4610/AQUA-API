// 渠道仓储的单元测试。
//
// 意图（Why）：
//
//	渠道是网关的核心数据，且其密钥属于敏感信息。因此除了常规 CRUD 之外，
//	必须专门验证一条安全不变量：**数据库里存的绝不是明文密钥**。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 在临时 SQLite 上执行真实 SQL（含加解密往返）
//
// 扩展（Extend）：
//
//	新增查询条件后，请在 TestChannelRepository_List 中补充过滤断言。
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// testEncryptionKey 是测试用密钥材料（非真实密钥）。
const testEncryptionKey = "test-only-key-material-0123456789abcdef0123456789abcdef"

// newTestChannelRepo 构造一个基于临时数据库的渠道仓储，并返回底层连接用于断言。
func newTestChannelRepo(t *testing.T) (model.ChannelRepository, *sql.DB) {
	t.Helper()

	st := newTestStore(t) // 复用 store_test.go 中的辅助函数（含建库与迁移）

	cipher, err := crypto.New(testEncryptionKey)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}

	return NewChannelRepository(st.DB(), cipher), st.DB()
}

// newValidChannel 返回一个字段合法、可直接入库的渠道对象。
func newValidChannel() *model.Channel {
	return &model.Channel{
		Name:     "测试渠道",
		Type:     1,
		BaseURL:  "https://api.example.com",
		APIKey:   "sk-test-abcdefghijklmnopqrstuvwxyz",
		Models:   []string{"gpt-4o", "gpt-4o-mini"},
		Group:    "default",
		Priority: 10,
		Weight:   5,
		Status:   model.ChannelStatusEnabled,
	}
}

// TestChannelRepository_CreateAndGet_RoundTrip 验证创建后能完整读回（含明文密钥解密）。
func TestChannelRepository_CreateAndGet_RoundTrip(t *testing.T) {
	repo, _ := newTestChannelRepo(t)
	ctx := context.Background()

	ch := newValidChannel()
	if err := repo.Create(ctx, ch); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	// Create 应回填 ID 与时间戳
	if ch.ID == 0 {
		t.Fatal("Create 后未回填 ID")
	}
	if ch.CreatedAt.IsZero() || ch.UpdatedAt.IsZero() {
		t.Error("Create 后未回填时间戳")
	}

	got, err := repo.GetByID(ctx, ch.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}

	// 逐字段比对，确保读回的数据与写入一致
	if got.Name != ch.Name {
		t.Errorf("Name = %q，期望 %q", got.Name, ch.Name)
	}
	if got.Type != ch.Type {
		t.Errorf("Type = %d，期望 %d", got.Type, ch.Type)
	}
	if got.BaseURL != ch.BaseURL {
		t.Errorf("BaseURL = %q，期望 %q", got.BaseURL, ch.BaseURL)
	}
	if got.APIKey != ch.APIKey {
		t.Errorf("APIKey 解密后 = %q，期望 %q", got.APIKey, ch.APIKey)
	}
	if got.Group != ch.Group {
		t.Errorf("Group = %q，期望 %q", got.Group, ch.Group)
	}
	if got.Priority != ch.Priority || got.Weight != ch.Weight {
		t.Errorf("Priority/Weight = %d/%d，期望 %d/%d", got.Priority, got.Weight, ch.Priority, ch.Weight)
	}
	if got.Status != model.ChannelStatusEnabled {
		t.Errorf("Status = %v，期望 启用", got.Status)
	}
	// 模型列表往返（顺序保持）
	if strings.Join(got.Models, ",") != strings.Join(ch.Models, ",") {
		t.Errorf("Models = %v，期望 %v", got.Models, ch.Models)
	}
}

// TestChannelRepository_KeyStoredEncrypted 验证密钥以密文落库。
//
// 这是本包最重要的安全断言：即使数据库文件泄露，也无法直接得到上游密钥。
func TestChannelRepository_KeyStoredEncrypted(t *testing.T) {
	repo, rawDB := newTestChannelRepo(t)
	ctx := context.Background()

	ch := newValidChannel()
	if err := repo.Create(ctx, ch); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 直接读原始列，绕过仓储的解密逻辑
	var stored string
	if err := rawDB.QueryRowContext(ctx,
		"SELECT api_key_enc FROM channels WHERE id = ?", ch.ID).Scan(&stored); err != nil {
		t.Fatalf("读取原始密钥列失败: %v", err)
	}

	if stored == "" {
		t.Fatal("密钥列为空，未正确写入")
	}
	if stored == ch.APIKey {
		t.Fatal("数据库中存的是明文密钥，加密未生效（严重安全问题）")
	}
	// 明文不应作为子串出现在密文中
	if strings.Contains(stored, ch.APIKey) {
		t.Fatal("密文中包含明文密钥，加密实现有误")
	}
}

// TestChannelRepository_Create_RejectsInvalid 验证非法数据会被领域校验拦下。
func TestChannelRepository_Create_RejectsInvalid(t *testing.T) {
	repo, _ := newTestChannelRepo(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*model.Channel)
	}{
		{name: "名称为空", mutate: func(c *model.Channel) { c.Name = "  " }},
		{name: "类型为 0", mutate: func(c *model.Channel) { c.Type = 0 }},
		{name: "base_url 非法协议", mutate: func(c *model.Channel) { c.BaseURL = "ftp://x.com" }},
		{name: "权重为 0", mutate: func(c *model.Channel) { c.Weight = 0 }},
		{name: "状态非法", mutate: func(c *model.Channel) { c.Status = 99 }},
		{name: "分组为空", mutate: func(c *model.Channel) { c.Group = "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newValidChannel()
			tc.mutate(ch)
			if err := repo.Create(ctx, ch); err == nil {
				t.Error("非法渠道应被拒绝，实际写入成功")
			}
		})
	}
}

// TestChannelRepository_GetByID_NotFound 验证查询不存在的渠道返回领域错误。
func TestChannelRepository_GetByID_NotFound(t *testing.T) {
	repo, _ := newTestChannelRepo(t)

	_, err := repo.GetByID(context.Background(), 99999)
	if !errors.Is(err, model.ErrChannelNotFound) {
		t.Errorf("错误 = %v，期望 model.ErrChannelNotFound", err)
	}
}

// TestChannelRepository_Update 验证更新生效且创建时间不被改写。
func TestChannelRepository_Update(t *testing.T) {
	repo, _ := newTestChannelRepo(t)
	ctx := context.Background()

	ch := newValidChannel()
	if err := repo.Create(ctx, ch); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	originalCreatedAt := ch.CreatedAt

	// 修改若干字段
	ch.Name = "改名后的渠道"
	ch.Priority = 99
	ch.APIKey = "sk-updated-key-0123456789"
	ch.Status = model.ChannelStatusDisabled

	if err := repo.Update(ctx, ch); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	got, err := repo.GetByID(ctx, ch.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if got.Name != "改名后的渠道" {
		t.Errorf("Name = %q，期望已更新", got.Name)
	}
	if got.Priority != 99 {
		t.Errorf("Priority = %d，期望 99", got.Priority)
	}
	if got.APIKey != "sk-updated-key-0123456789" {
		t.Errorf("APIKey = %q，期望已更新（重新加密后解密应一致）", got.APIKey)
	}
	if got.Status != model.ChannelStatusDisabled {
		t.Errorf("Status = %v，期望 手动禁用", got.Status)
	}
	// 创建时间必须保持不可变（审计要求）。
	//
	// 注意：数据库以 Unix 秒存储时间，读到的是秒级精度，
	// 因此这里比较秒值而非带纳秒的 Time（内存中的 CreatedAt 含纳秒）。
	if got.CreatedAt.Unix() != originalCreatedAt.Unix() {
		t.Errorf("CreatedAt 被改写：%v → %v（创建时间应不可变）", originalCreatedAt, got.CreatedAt)
	}
}

// TestChannelRepository_Update_NotFound 验证更新不存在的渠道返回领域错误。
func TestChannelRepository_Update_NotFound(t *testing.T) {
	repo, _ := newTestChannelRepo(t)

	ch := newValidChannel()
	ch.ID = 99999

	if err := repo.Update(context.Background(), ch); !errors.Is(err, model.ErrChannelNotFound) {
		t.Errorf("错误 = %v，期望 model.ErrChannelNotFound", err)
	}
}

// TestChannelRepository_Update_RejectsZeroID 验证 ID 为 0 时明确报错（防止误更新）。
func TestChannelRepository_Update_RejectsZeroID(t *testing.T) {
	repo, _ := newTestChannelRepo(t)

	ch := newValidChannel() // ID 为 0
	if err := repo.Update(context.Background(), ch); err == nil {
		t.Error("ID 为 0 的更新应被拒绝，实际成功")
	}
}

// TestChannelRepository_Delete 验证删除成功与重复删除的差异。
func TestChannelRepository_Delete(t *testing.T) {
	repo, _ := newTestChannelRepo(t)
	ctx := context.Background()

	ch := newValidChannel()
	if err := repo.Create(ctx, ch); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	if err := repo.Delete(ctx, ch.ID); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	// 删除后应查不到
	if _, err := repo.GetByID(ctx, ch.ID); !errors.Is(err, model.ErrChannelNotFound) {
		t.Errorf("删除后查询错误 = %v，期望 ErrChannelNotFound", err)
	}
	// 重复删除应返回未找到
	if err := repo.Delete(ctx, ch.ID); !errors.Is(err, model.ErrChannelNotFound) {
		t.Errorf("重复删除错误 = %v，期望 ErrChannelNotFound", err)
	}
}

// TestChannelRepository_List 验证过滤条件与返回顺序。
func TestChannelRepository_List(t *testing.T) {
	repo, _ := newTestChannelRepo(t)
	ctx := context.Background()

	// 构造三个渠道：同组两个（优先级不同）+ 另一组一个
	mk := func(name, group string, priority, weight int, status model.ChannelStatus) *model.Channel {
		ch := newValidChannel()
		ch.Name = name
		ch.Group = group
		ch.Priority = priority
		ch.Weight = weight
		ch.Status = status
		if err := repo.Create(ctx, ch); err != nil {
			t.Fatalf("Create(%s) 失败: %v", name, err)
		}
		return ch
	}
	mk("低优先级", "default", 1, 1, model.ChannelStatusEnabled)
	mk("高优先级", "default", 10, 1, model.ChannelStatusEnabled)
	mk("已禁用", "default", 5, 1, model.ChannelStatusDisabled)
	mk("其他分组", "vip", 20, 1, model.ChannelStatusEnabled)

	// 1) 按分组过滤
	list, err := repo.List(ctx, model.ChannelQuery{Group: "default"})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("default 分组渠道数 = %d，期望 3", len(list))
	}
	// 2) 顺序应为优先级降序：高(10) → 已禁用(5) → 低(1)
	if list[0].Name != "高优先级" || list[1].Name != "已禁用" || list[2].Name != "低优先级" {
		t.Errorf("返回顺序 = [%s, %s, %s]，期望按优先级降序",
			list[0].Name, list[1].Name, list[2].Name)
	}

	// 3) 按状态过滤（仅启用的 default 分组）
	enabled := model.ChannelStatusEnabled
	list, err = repo.List(ctx, model.ChannelQuery{Group: "default", Status: &enabled})
	if err != nil {
		t.Fatalf("List(状态过滤) 失败: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("启用状态的 default 渠道数 = %d，期望 2", len(list))
	}

	// 4) 分页
	list, err = repo.List(ctx, model.ChannelQuery{Limit: 2})
	if err != nil {
		t.Fatalf("List(分页) 失败: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("Limit=2 返回 %d 条，期望 2", len(list))
	}
}

// TestChannelRepository_List_EmptyResult 验证无数据时返回空切片而非 nil。
//
// 说明：返回空切片可让上层直接 range/len 判断，避免 nil 判空带来的边界问题。
func TestChannelRepository_List_EmptyResult(t *testing.T) {
	repo, _ := newTestChannelRepo(t)

	list, err := repo.List(context.Background(), model.ChannelQuery{})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if list == nil {
		t.Error("应返回空切片而非 nil")
	}
	if len(list) != 0 {
		t.Errorf("空库返回 %d 条，期望 0", len(list))
	}
}

// TestEncodeDecodeModels 验证模型列表的落库编码与读取解码。
func TestEncodeDecodeModels(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{name: "空列表", input: nil, want: ""},
		{name: "正常列表", input: []string{"gpt-4o", "gpt-4o-mini"}, want: "gpt-4o,gpt-4o-mini"},
		{name: "去重", input: []string{"a", "b", "a"}, want: "a,b"},
		{name: "去空白与空项", input: []string{" a ", "", "  ", "b"}, want: "a,b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeModels(tc.input)
			if got != tc.want {
				t.Errorf("encodeModels(%v) = %q，期望 %q", tc.input, got, tc.want)
			}
		})
	}

	// 解码
	if got := decodeModels(""); got != nil {
		t.Errorf("decodeModels(\"\") = %v，期望 nil", got)
	}
	if got := decodeModels("a, b ,c"); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("decodeModels 结果 = %v，期望 [a b c]", got)
	}
}
