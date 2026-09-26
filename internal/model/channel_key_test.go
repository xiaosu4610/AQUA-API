// 渠道密钥池领域逻辑的单元测试。
//
// 测试重点：
//   - 批量粘贴解析：后台允许使用者直接从记事本粘几百行密钥，
//     必须容忍空行、注释、行尾备注、逗号分隔，并自动去重；
//   - 脱敏：界面上展示的必须是遮蔽后的密钥，绝不能泄露中段；
//   - 随机挑选：池内只有一个元素时不得返回 nil。
package model

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseKeyList_多行与备注与去重(t *testing.T) {
	raw := `# 这是注释行，应被忽略

nvapi-aaa111
nvapi-bbb222  池 #002
nvapi-ccc333,池 #003
  nvapi-ddd444
nvapi-aaa111
   `

	keys, labels := ParseKeyList(raw)

	want := []string{"nvapi-aaa111", "nvapi-bbb222", "nvapi-ccc333", "nvapi-ddd444"}
	if len(keys) != len(want) {
		t.Fatalf("解析出 %d 把密钥，期望 %d 把：%v", len(keys), len(want), keys)
	}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("第 %d 把密钥应为 %q，实际 %q", i+1, k, keys[i])
		}
	}

	// 备注应与密钥一一对应（第一把无备注）
	if len(labels) != len(keys) {
		t.Fatalf("备注数量(%d)应与密钥数量(%d)一致", len(labels), len(keys))
	}
	if labels[0] != "" {
		t.Errorf("第一把密钥没有备注，实际 %q", labels[0])
	}
	if labels[1] != "池 #002" {
		t.Errorf("第二把密钥备注应为 %q，实际 %q", "池 #002", labels[1])
	}
	if labels[2] != "池 #003" {
		t.Errorf("第三把密钥备注（逗号分隔）应为 %q，实际 %q", "池 #003", labels[2])
	}
}

func TestParseKeyList_空输入(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\n\n", "# 只有注释\n"} {
		keys, labels := ParseKeyList(raw)
		if len(keys) != 0 || len(labels) != 0 {
			t.Errorf("输入 %q 应解析为空，实际 keys=%v labels=%v", raw, keys, labels)
		}
	}
}

func TestParseKeyList_支持五百行批量粘贴(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		// 每行必须唯一：解析层会去重，若测试数据自身重复就测不出"能解析 500 行"
		fmt.Fprintf(&sb, "nvapi-key-%04d\n", i)
	}

	keys, _ := ParseKeyList(sb.String())
	if len(keys) != 500 {
		t.Fatalf("应解析出 500 把密钥，实际 %d", len(keys))
	}
	if keys[0] != "nvapi-key-0000" || keys[499] != "nvapi-key-0499" {
		t.Fatalf("解析顺序异常：首=%q 末=%q", keys[0], keys[499])
	}
}

func TestChannelKey_Masked_不泄露中段(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"真实格式", "nvapi-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"短密钥", "short"},
		{"边界长度", "nvapi-1234567890ab"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &ChannelKey{Key: tc.key}
			masked := k.Masked()

			if masked == "" {
				t.Fatal("脱敏结果不应为空")
			}
			if masked == tc.key {
				t.Fatal("脱敏结果与原文相同，等于没有脱敏")
			}
			// 中段必须被遮蔽
			if len(tc.key) > 12 {
				middle := tc.key[8 : len(tc.key)-4]
				if len(middle) > 0 && strings.Contains(masked, middle) {
					t.Fatalf("脱敏结果 %q 泄露了中段 %q", masked, middle)
				}
			}
			if !strings.Contains(masked, "****") && len(tc.key) > 12 {
				t.Fatalf("超过 12 位的密钥应包含掩码标记，实际 %q", masked)
			}
		})
	}
}

func TestPickKey_池内选择(t *testing.T) {
	// 空池返回 nil
	if got := PickKey(nil); got != nil {
		t.Fatalf("空池应返回 nil，实际 %v", got)
	}

	// 单元素直接返回该元素（这是最常见的情况：渠道只配了一把密钥）
	single := []*ChannelKey{{ID: 1, Key: "only"}}
	if got := PickKey(single); got == nil || got.ID != 1 {
		t.Fatalf("单元素池应返回该元素，实际 %v", got)
	}

	// 多元素：只返回池内元素（随机性不在此断言，只验证取值范围与不越界）
	pool := []*ChannelKey{{ID: 1}, {ID: 2}, {ID: 3}}
	seen := make(map[uint64]struct{})
	for i := 0; i < 300; i++ {
		got := PickKey(pool)
		if got == nil {
			t.Fatal("多元素池不应返回 nil")
		}
		seen[got.ID] = struct{}{}
	}
	// 300 次随机若只命中 1 个元素，说明选择逻辑退化为固定取第一个
	if len(seen) < 2 {
		t.Fatalf("随机挑选应在池内分散，实际只命中了 %d 个元素", len(seen))
	}
}
