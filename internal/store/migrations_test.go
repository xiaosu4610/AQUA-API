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
	loaded, err := loadMigrations()
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
