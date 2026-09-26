// 令牌仓储的单元测试。
//
// 意图（Why）：
//
//	令牌是使用者的凭据，其存储方式直接决定安全边界。除常规 CRUD 外，
//	这里专门锁定三条安全不变量：
//	  1) 数据库里没有令牌明文（只有摘要与密文）；
//	  2) 摘要必须等于 SHA-256(key)，否则鉴权查找会失效；
//	  3) 摘要唯一索引生效，禁止重复令牌。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 真实 SQLite 上执行读写与加解密往返
//
// 扩展（Extend）：
//
//	新增查询条件后，在 TestTokenRepository_List 中补过滤断言。
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestTokenRepo 构造一个基于临时数据库的令牌仓储，并返回底层连接用于安全断言。
func newTestTokenRepo(t *testing.T) (model.TokenRepository, *sql.DB) {
	t.Helper()

	st := newTestStore(t) // 复用 store_test.go 中的辅助函数（含建库与迁移）

	cipher, err := crypto.New(testEncryptionKey)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}
	return NewTokenRepository(st.DB(), cipher), st.DB()
}

// newValidToken 返回一个字段合法、可直接入库的令牌。
func newValidToken(t *testing.T) *model.Token {
	t.Helper()

	key, err := model.GenerateTokenKey()
	if err != nil {
		t.Fatalf("生成令牌 KEY 失败: %v", err)
	}
	return &model.Token{
		Name:           "测试令牌",
		Key:            key,
		Status:         model.TokenStatusEnabled,
		RemainQuota:    1000,
		UnlimitedQuota: false,
		UsedQuota:      0,
		Models:         []string{"gpt-4o", "gpt-4o-mini"},
	}
}

// TestTokenRepository_CreateAndGet_RoundTrip 验证创建后能完整读回。
func TestTokenRepository_CreateAndGet_RoundTrip(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	tk := newValidToken(t)
	if err := repo.Create(ctx, tk); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if tk.ID == 0 {
		t.Fatal("Create 后未回填 ID")
	}
	if tk.CreatedAt.IsZero() || tk.UpdatedAt.IsZero() {
		t.Error("Create 后未回填时间戳")
	}

	got, err := repo.GetByID(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}

	if got.Name != tk.Name {
		t.Errorf("Name = %q，期望 %q", got.Name, tk.Name)
	}
	if got.Key != tk.Key {
		t.Errorf("Key 解密后与原文不一致")
	}
	if got.Status != model.TokenStatusEnabled {
		t.Errorf("Status = %v，期望 启用", got.Status)
	}
	if got.RemainQuota != 1000 || got.UsedQuota != 0 {
		t.Errorf("额度字段不一致：remain=%d used=%d", got.RemainQuota, got.UsedQuota)
	}
	if got.UnlimitedQuota {
		t.Error("UnlimitedQuota 应为 false")
	}
	if strings.Join(got.Models, ",") != "gpt-4o,gpt-4o-mini" {
		t.Errorf("Models = %v，期望 [gpt-4o gpt-4o-mini]", got.Models)
	}
}

// TestTokenRepository_StoredFormIsSafe 验证令牌的存储形式满足安全要求。
//
// 这是本包最重要的断言：
//   - 数据库不得出现令牌明文；
//   - key_hash 必须等于 SHA-256(key)（否则鉴权查找会失效）；
//   - key_enc 必须是密文且可解密回原文。
func TestTokenRepository_StoredFormIsSafe(t *testing.T) {
	repo, rawDB := newTestTokenRepo(t)
	ctx := context.Background()

	tk := newValidToken(t)
	if err := repo.Create(ctx, tk); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	var storedHash, storedEnc string
	if err := rawDB.QueryRowContext(ctx,
		"SELECT key_hash, key_enc FROM tokens WHERE id = ?", tk.ID).Scan(&storedHash, &storedEnc); err != nil {
		t.Fatalf("读取原始存储字段失败: %v", err)
	}

	// 1) 摘要必须与 SHA-256 一致（算法不匹配会导致鉴权全部失败）
	if storedHash != crypto.SHA256Hex(tk.Key) {
		t.Errorf("key_hash 与 SHA-256(key) 不一致：%q", storedHash)
	}

	// 2) 密文不得包含明文
	if storedEnc == tk.Key {
		t.Fatal("key_enc 存的是明文（严重安全问题）")
	}
	if strings.Contains(storedEnc, tk.Key) {
		t.Fatal("密文中包含明文（严重安全问题）")
	}

	// 3) 整行数据中不得出现明文 KEY
	var allColumns string
	if err := rawDB.QueryRowContext(ctx,
		"SELECT id || name || key_hash || key_enc || models FROM tokens WHERE id = ?",
		tk.ID).Scan(&allColumns); err != nil {
		t.Fatalf("拼接整行数据失败: %v", err)
	}
	if strings.Contains(allColumns, tk.Key) {
		t.Fatal("数据库整行记录中包含令牌明文（严重安全问题）")
	}
}

// TestTokenRepository_GetByKey 验证按明文查找令牌（鉴权热路径）。
func TestTokenRepository_GetByKey(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	tk := newValidToken(t)
	if err := repo.Create(ctx, tk); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	got, err := repo.GetByKey(ctx, tk.Key)
	if err != nil {
		t.Fatalf("GetByKey 失败: %v", err)
	}
	if got.ID != tk.ID {
		t.Errorf("查到的令牌 ID = %d，期望 %d", got.ID, tk.ID)
	}

	// 不存在的 KEY
	if _, err := repo.GetByKey(ctx, "sk-000000000000000000000000000000000000000000000000"); !errors.Is(err, model.ErrTokenNotFound) {
		t.Errorf("未知 KEY 的错误 = %v，期望 ErrTokenNotFound", err)
	}
}

// TestTokenRepository_DuplicateKey_Rejected 验证重复令牌被唯一索引拒绝。
//
// 意义：令牌是唯一凭据，重复会导致鉴权结果不确定（取到哪一条取决于查询计划）。
func TestTokenRepository_DuplicateKey_Rejected(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	first := newValidToken(t)
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("首次 Create 失败: %v", err)
	}

	second := newValidToken(t)
	second.Key = first.Key // 刻意使用相同 KEY
	if err := repo.Create(ctx, second); err == nil {
		t.Fatal("重复令牌应被拒绝，实际创建成功")
	}
}

// TestTokenRepository_Create_RejectsInvalid 验证非法数据被领域校验拦下。
func TestTokenRepository_Create_RejectsInvalid(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*model.Token)
	}{
		{name: "名称为空", mutate: func(tk *model.Token) { tk.Name = "" }},
		{name: "KEY 缺前缀", mutate: func(tk *model.Token) { tk.Key = "0123456789" }},
		{name: "状态非法", mutate: func(tk *model.Token) { tk.Status = 99 }},
		{name: "剩余额度为负", mutate: func(tk *model.Token) { tk.RemainQuota = -5 }},
		{name: "分组名含大写", mutate: func(tk *model.Token) { tk.GroupName = "VIP" }},
		{name: "分组名含空格", mutate: func(tk *model.Token) { tk.GroupName = "vip gold" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk := newValidToken(t)
			tc.mutate(tk)
			if err := repo.Create(ctx, tk); err == nil {
				t.Error("非法令牌应被拒绝，实际写入成功")
			}
		})
	}
}

// TestTokenRepository_Update 验证更新生效且创建时间不可变。
func TestTokenRepository_Update(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	tk := newValidToken(t)
	if err := repo.Create(ctx, tk); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	originalCreatedAt := tk.CreatedAt

	tk.Name = "改名后的令牌"
	tk.Status = model.TokenStatusDisabled
	tk.RemainQuota = 42
	tk.UnlimitedQuota = false
	tk.Models = []string{"claude-3"}

	if err := repo.Update(ctx, tk); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	got, err := repo.GetByID(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if got.Name != "改名后的令牌" {
		t.Errorf("Name = %q，期望已更新", got.Name)
	}
	if got.Status != model.TokenStatusDisabled {
		t.Errorf("Status = %v，期望 手动禁用", got.Status)
	}
	if got.RemainQuota != 42 {
		t.Errorf("RemainQuota = %d，期望 42", got.RemainQuota)
	}
	if strings.Join(got.Models, ",") != "claude-3" {
		t.Errorf("Models = %v，期望 [claude-3]", got.Models)
	}
	// 创建时间不可变（数据库以秒存储，故比较秒值）
	if got.CreatedAt.Unix() != originalCreatedAt.Unix() {
		t.Errorf("CreatedAt 被改写：%v → %v", originalCreatedAt, got.CreatedAt)
	}
}

// TestTokenRepository_Update_NotFound 验证更新不存在的令牌返回领域错误。
func TestTokenRepository_Update_NotFound(t *testing.T) {
	repo, _ := newTestTokenRepo(t)

	tk := newValidToken(t)
	tk.ID = 99999

	if err := repo.Update(context.Background(), tk); !errors.Is(err, model.ErrTokenNotFound) {
		t.Errorf("错误 = %v，期望 ErrTokenNotFound", err)
	}
}

// TestTokenRepository_Delete 验证删除与重复删除的行为。
func TestTokenRepository_Delete(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	tk := newValidToken(t)
	if err := repo.Create(ctx, tk); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	if err := repo.Delete(ctx, tk.ID); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if _, err := repo.GetByID(ctx, tk.ID); !errors.Is(err, model.ErrTokenNotFound) {
		t.Errorf("删除后查询错误 = %v，期望 ErrTokenNotFound", err)
	}
	if err := repo.Delete(ctx, tk.ID); !errors.Is(err, model.ErrTokenNotFound) {
		t.Errorf("重复删除错误 = %v，期望 ErrTokenNotFound", err)
	}
}

// TestTokenRepository_List 验证过滤条件与返回顺序。
func TestTokenRepository_List(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	mk := func(name string, owner uint64, status model.TokenStatus) *model.Token {
		tk := newValidToken(t)
		tk.Name = name
		tk.OwnerID = owner
		tk.Status = status
		if err := repo.Create(ctx, tk); err != nil {
			t.Fatalf("Create(%s) 失败: %v", name, err)
		}
		return tk
	}
	mk("系统令牌A", 0, model.TokenStatusEnabled)
	mk("系统令牌B", 0, model.TokenStatusDisabled)
	mk("用户令牌", 7, model.TokenStatusEnabled)

	// 1) 按归属过滤
	owner := uint64(7)
	list, err := repo.List(ctx, model.TokenQuery{OwnerID: &owner})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 1 || list[0].Name != "用户令牌" {
		t.Errorf("按归属过滤结果异常: %d 条", len(list))
	}

	// 2) 按状态过滤
	enabled := model.TokenStatusEnabled
	list, err = repo.List(ctx, model.TokenQuery{Status: &enabled})
	if err != nil {
		t.Fatalf("List(状态过滤) 失败: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("启用令牌数 = %d，期望 2", len(list))
	}

	// 3) 无条件列表应包含全部 3 条，且按 ID 升序
	list, err = repo.List(ctx, model.TokenQuery{})
	if err != nil {
		t.Fatalf("List(无条件) 失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("令牌总数 = %d，期望 3", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID > list[i].ID {
			t.Errorf("返回结果未按 ID 升序：%d 在 %d 之前", list[i-1].ID, list[i].ID)
		}
	}
}

// TestTokenRepository_List_EmptyResult 验证无数据时返回空切片而非 nil。
func TestTokenRepository_List_EmptyResult(t *testing.T) {
	repo, _ := newTestTokenRepo(t)

	list, err := repo.List(context.Background(), model.TokenQuery{})
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

// TestTokenRepository_ExpiresAtRoundTrip 验证过期时间的往返语义。
//
// 关键点：零值时间（永不过期）必须原样还原，不能被写成一个大负数的 Unix 时间戳。
func TestTokenRepository_ExpiresAtRoundTrip(t *testing.T) {
	repo, rawDB := newTestTokenRepo(t)
	ctx := context.Background()

	t.Run("永不过期存为 0 并原样还原", func(t *testing.T) {
		tk := newValidToken(t)
		tk.ExpiresAt = time.Time{} // 零值 = 永不过期
		if err := repo.Create(ctx, tk); err != nil {
			t.Fatalf("Create 失败: %v", err)
		}

		var stored int64
		if err := rawDB.QueryRowContext(ctx,
			"SELECT expires_at FROM tokens WHERE id = ?", tk.ID).Scan(&stored); err != nil {
			t.Fatalf("读取 expires_at 失败: %v", err)
		}
		if stored != 0 {
			t.Errorf("expires_at = %d，期望 0（永不过期）", stored)
		}

		got, err := repo.GetByID(ctx, tk.ID)
		if err != nil {
			t.Fatalf("GetByID 失败: %v", err)
		}
		if !got.ExpiresAt.IsZero() {
			t.Errorf("还原后的过期时间 = %v，期望零值", got.ExpiresAt)
		}
	})

	t.Run("具体时间按秒还原", func(t *testing.T) {
		expireAt := time.Now().Add(24 * time.Hour).Truncate(time.Second)
		tk := newValidToken(t)
		tk.ExpiresAt = expireAt
		if err := repo.Create(ctx, tk); err != nil {
			t.Fatalf("Create 失败: %v", err)
		}

		got, err := repo.GetByID(ctx, tk.ID)
		if err != nil {
			t.Fatalf("GetByID 失败: %v", err)
		}
		if !got.ExpiresAt.Equal(expireAt) {
			t.Errorf("过期时间 = %v，期望 %v", got.ExpiresAt, expireAt)
		}
	})
}

// TestTokenRepository_GroupName_RoundTrip 验证所属分组的写入与读回，以及空值默认。
//
// 关键语义：未设置分组的（历史）令牌必须落库为空串，并在此后回退到默认分组，
// 从而保证"上线分组能力"不会改变任何既有令牌的行为。
func TestTokenRepository_GroupName_RoundTrip(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	// 1) 显式设置分组：原样写回
	withGroup := newValidToken(t)
	withGroup.GroupName = "vip"
	if err := repo.Create(ctx, withGroup); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	got, err := repo.GetByID(ctx, withGroup.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if got.GroupName != "vip" {
		t.Errorf("GroupName = %q，期望 %q", got.GroupName, "vip")
	}
	if effective := got.EffectiveGroupName(model.DefaultGroupName); effective != "vip" {
		t.Errorf("EffectiveGroupName = %q，期望 %q", effective, "vip")
	}

	// 2) 不设置分组：落库空串，读回仍为空，生效分组回退默认
	noGroup := newValidToken(t)
	if err := repo.Create(ctx, noGroup); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	gotEmpty, err := repo.GetByID(ctx, noGroup.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if gotEmpty.GroupName != "" {
		t.Errorf("未设置分组时 GroupName = %q，期望空串", gotEmpty.GroupName)
	}
	if effective := gotEmpty.EffectiveGroupName(model.DefaultGroupName); effective != model.DefaultGroupName {
		t.Errorf("EffectiveGroupName 回退 = %q，期望 %q", effective, model.DefaultGroupName)
	}
}

// TestTokenRepository_Update_GroupName 验证更新能改写所属分组。
func TestTokenRepository_Update_GroupName(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	tk := newValidToken(t)
	tk.GroupName = "vip"
	if err := repo.Create(ctx, tk); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	tk.GroupName = "svip"
	if err := repo.Update(ctx, tk); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	got, err := repo.GetByID(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if got.GroupName != "svip" {
		t.Errorf("GroupName = %q，期望 %q", got.GroupName, "svip")
	}
}

// TestTokenRepository_List_ByGroup 验证按分组筛选令牌。
func TestTokenRepository_List_ByGroup(t *testing.T) {
	repo, _ := newTestTokenRepo(t)
	ctx := context.Background()

	mk := func(name, group string) {
		tk := newValidToken(t)
		tk.Name = name
		tk.GroupName = group
		if err := repo.Create(ctx, tk); err != nil {
			t.Fatalf("Create(%s) 失败: %v", name, err)
		}
	}
	mk("vip-1", "vip")
	mk("vip-2", "vip")
	mk("未分组", "")
	mk("其它分组", "other")

	// 按分组精确匹配
	vip := "vip"
	list, err := repo.List(ctx, model.TokenQuery{GroupName: &vip})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("vip 分组令牌数 = %d，期望 2", len(list))
	}
	for _, tk := range list {
		if tk.GroupName != "vip" {
			t.Errorf("筛选结果含非 vip 分组令牌: %q", tk.GroupName)
		}
	}

	// 空串筛选只返回"未设置分组"的令牌（不做默认分组展开）
	empty := ""
	list, err = repo.List(ctx, model.TokenQuery{GroupName: &empty})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 1 || list[0].Name != "未分组" {
		t.Errorf("空串筛选结果异常: %d 条", len(list))
	}
}
