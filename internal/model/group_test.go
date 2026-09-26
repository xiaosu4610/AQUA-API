// 模型分组领域模型的单元测试。
//
// 测试重点：
//   - 分组标识必须小写且不含分隔符（否则 "VIP"/"vip" 会被当成两个分组）；
//   - 倍率必须有合理上下限（0 会让调用免费、超大值几乎必然是把单位填错）；
//   - 倍率换算用整数向下取整。
package model

import "testing"

func TestModelGroup_Validate(t *testing.T) {
	valid := &ModelGroup{Name: "vip", Ratio: 100, Enabled: true}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法分组不应报错: %v", err)
	}

	cases := map[string]*ModelGroup{
		"空标识":  {Name: "", Ratio: 100},
		"大写标识": {Name: "VIP", Ratio: 100},
		"含空格":  {Name: "my group", Ratio: 100},
		"含逗号":  {Name: "a,b", Ratio: 100},
		"含斜杠":  {Name: "a/b", Ratio: 100},
		"倍率为零": {Name: "vip", Ratio: 0},
		"倍率为负": {Name: "vip", Ratio: -10},
		"倍率过大": {Name: "vip", Ratio: 100001},
	}
	for name, group := range cases {
		if err := group.Validate(); err == nil {
			t.Errorf("%s 的分组应校验失败: %+v", name, group)
		}
	}
}

func TestModelGroup_Label与ApplyRatio(t *testing.T) {
	group := &ModelGroup{Name: "vip", DisplayName: "VIP 用户", Ratio: 150}
	if group.Label() != "VIP 用户" {
		t.Fatalf("展示名应优先，实际 %q", group.Label())
	}

	// 未设置展示名时回退标识
	plain := &ModelGroup{Name: "vip", Ratio: 100}
	if plain.Label() != "vip" {
		t.Fatalf("展示名应回退为标识，实际 %q", plain.Label())
	}

	if got := group.ApplyRatio(1000); got != 1500 {
		t.Fatalf("1.5 倍应得 1500，实际 %d", got)
	}
	if got := group.ApplyRatio(101); got != 151 {
		t.Fatalf("151.5 应向下取整为 151，实际 %d", got)
	}
	// 1.0 倍必须完全不变，默认分组不能引入误差
	if got := plain.ApplyRatio(101); got != 101 {
		t.Fatalf("1.0 倍不应改变数值，实际 %d", got)
	}
	// nil 接收者与非法输入不应 panic 或放大数值
	var nilGroup *ModelGroup
	if got := nilGroup.ApplyRatio(500); got != 500 {
		t.Fatalf("nil 分组应原样返回，实际 %d", got)
	}
	if got := group.ApplyRatio(0); got != 0 {
		t.Fatalf("基础额为 0 时应返回 0，实际 %d", got)
	}
}
