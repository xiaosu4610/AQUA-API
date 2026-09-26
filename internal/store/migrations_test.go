// 迁移脚本加载逻辑的单元测试。
//
// 意图（Why）：
//
//	迁移脚本的命名与加载顺序直接决定数据库结构演进是否正确。
//	命名写错、版本号重复这类"看起来很小"的错误，会导致线上缺表或结构错乱，
//	因此这里把约定用测试固化下来。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 校验文件名解析规则与加载结果
//
// 扩展（Extend）：
//
//	新增迁移脚本后，请确认 TestLoadMigrations_EmbeddedScripts 中的版本连续性断言仍成立。
package store

import (
	"testing"
)

// TestParseMigrationFileName 验证文件名解析规则。
func TestParseMigrationFileName(t *testing.T) {
	cases := []struct {
		name        string
		fileName    string
		wantVersion int
		wantName    string
		wantErr     bool
	}{
		{name: "标准命名", fileName: "0001_init.sql", wantVersion: 1, wantName: "init"},
		{name: "多位版本号", fileName: "0042_add_tokens.sql", wantVersion: 42, wantName: "add_tokens"},
		{name: "名称含下划线", fileName: "0002_add_token_index.sql", wantVersion: 2, wantName: "add_token_index"},

		{name: "缺少下划线", fileName: "0001init.sql", wantErr: true},
		{name: "名称为空", fileName: "0001_.sql", wantErr: true},
		{name: "版本号非数字", fileName: "abcd_init.sql", wantErr: true},
		{name: "版本号为零", fileName: "0000_init.sql", wantErr: true},
		{name: "版本号为负", fileName: "-001_init.sql", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version, name, err := parseMigrationFileName(tc.fileName)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望解析失败，实际得到 version=%d name=%s", version, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if version != tc.wantVersion {
				t.Errorf("版本号 = %d，期望 %d", version, tc.wantVersion)
			}
			if name != tc.wantName {
				t.Errorf("名称 = %q，期望 %q", name, tc.wantName)
			}
		})
	}
}

// TestLoadMigrations_EmbeddedScripts 验证嵌入的脚本被正确加载。
//
// 断言重点：
//   - 至少存在一个脚本（否则启动会 panic）；
//   - 按版本号严格升序（顺序错乱会导致依赖关系被破坏）；
//   - 版本号唯一且从 1 开始连续（缺号通常意味着文件被误删）。
func TestLoadMigrations_EmbeddedScripts(t *testing.T) {
	loaded, err := loadMigrations(canonicalMigrationsDir)
	if err != nil {
		t.Fatalf("加载迁移脚本失败: %v", err)
	}

	if len(loaded) == 0 {
		t.Fatal("未加载到任何迁移脚本")
	}

	// 首个脚本必须是版本 1
	if loaded[0].Version != 1 {
		t.Errorf("首个迁移版本 = %d，期望 1", loaded[0].Version)
	}
	if loaded[0].Name != "init" {
		t.Errorf("首个迁移名称 = %q，期望 init", loaded[0].Name)
	}

	for i, m := range loaded {
		if m.SQL == "" {
			t.Errorf("迁移 %d(%s) 的 SQL 为空", m.Version, m.Name)
		}
		// 版本号应从 1 开始连续递增，便于人工核对"有没有漏文件"
		if m.Version != i+1 {
			t.Errorf("第 %d 个迁移版本 = %d，期望 %d（版本号应连续）", i+1, m.Version, i+1)
		}
	}
}

// TestMigrationDirs_AllDialectsShareSameVersions 校验各方言迁移目录的版本号集合一致。
//
// 为什么必须一致：
//
//	迁移版本号是跨方言共享的语义（0011 在哪个库上都代表"新增模型分组表"）。
//	若某个方言目录漏了一个文件，那个库就会缺表，而程序不会报错——
//	只会在运行时出现"表不存在"，属于最难排查的一类故障。
//	把这条不变量写成测试，新增方言或新增迁移时漏改会立刻暴露。
func TestMigrationDirs_AllDialectsShareSameVersions(t *testing.T) {
	want := versionSet(t, canonicalMigrationsDir)

	checked := 0
	for _, key := range DriverKeys() {
		dialect, err := dialectFor(key)
		if err != nil {
			// 尚未实现连接/方言的驱动（元数据已在册）不参与校验
			continue
		}
		checked++

		got := versionSet(t, dialect.MigrationsDir())
		if len(got) != len(want) {
			t.Fatalf("方言 %s 的迁移数量 = %d，基准 %s = %d",
				key, len(got), canonicalMigrationsDir, len(want))
		}
		for version := range want {
			if _, ok := got[version]; !ok {
				t.Errorf("方言 %s 缺少迁移版本 %d（基准目录有）", key, version)
			}
		}
	}

	if checked == 0 {
		t.Fatal("没有任何已实现方言参与校验，测试形同虚设")
	}
}

// versionSet 读出某目录下的迁移版本号集合。
func versionSet(t *testing.T, dir string) map[int]bool {
	t.Helper()

	loaded, err := loadMigrations(dir)
	if err != nil {
		t.Fatalf("加载迁移目录 %s 失败: %v", dir, err)
	}

	set := make(map[int]bool, len(loaded))
	for _, m := range loaded {
		set[m.Version] = true
	}
	return set
}

// TestDrivers_AvailableMustBeUsable 校验「标记为可用的驱动」确实能被用起来。
//
// 为什么要有这条：驱动元数据会被安装向导直接展示。一旦出现
// 「界面上可选但没有实现」或「有实现却忘了登记字段」，
// 使用者会卡在安装第一步且看不到任何有用提示。
func TestDrivers_AvailableMustBeUsable(t *testing.T) {
	available := AvailableDrivers()
	if len(available) == 0 {
		t.Fatal("没有任何可用驱动，安装向导将无法继续")
	}

	for _, info := range available {
		if info.Key == "" || info.Label == "" {
			t.Errorf("驱动 %+v 缺少 Key 或 Label", info)
		}
		if len(info.Fields) == 0 {
			t.Errorf("驱动 %s 未登记任何字段，安装向导会渲染出一个没有输入框的步骤", info.Key)
		}
		for _, field := range info.Fields {
			if field.Key == "" || field.Label == "" {
				t.Errorf("驱动 %s 存在缺少 Key 或 Label 的字段：%+v", info.Key, field)
			}
		}
		// 可用驱动必须同时具备方言实现与迁移目录
		if _, err := dialectFor(info.Key); err != nil {
			t.Errorf("驱动 %s 标记为可用但缺少方言实现: %v", info.Key, err)
		}
	}
}
