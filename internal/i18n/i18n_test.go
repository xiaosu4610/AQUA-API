// i18n 包的单元测试。
//
// 意图（Why）：
//
//	多语言改造最怕两件事：一是语言解析规则被写错（用户明明发英文却收到中文），
//	二是目录出现"半翻译"（某些键缺某门语言，界面上中英混杂）。
//	本文件把这两类风险用测试锁死。
//
// 流转（Flow）：
//
//	go test ./internal/i18n/... → 覆盖 Parse / Message / 目录一致性
//
// 扩展（Extend）：
//
//	新增语言后，supported 会随之变化，键集合一致性用例会自动把新语言的
//	缺口暴露出来——这正是它存在的价值。
package i18n

import "testing"

// TestParse_按权重与多值选择语言 验证 Accept-Language 的解析与权重处理。
func TestParse_按权重与多值选择语言(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   Locale
	}{
		{"多值按权重取最高", "fr-CA,fr;q=0.9,en;q=0.8", Fr},
		{"带书写系统与地区", "zh-Hans-CN", ZhCN},
		{"未知语言回退默认", "xx-YY", Default},
		{"空串回退默认", "", Default},
		{"权重覆盖出现顺序", "en;q=0.5, fr;q=0.9", Fr},
		{"简体中文前缀", "zh-CN,zh;q=0.9,en;q=0.8", ZhCN},
		{"下划线形式归一", "en_US", En},
		{"显式排除(q=0)被跳过", "en;q=0, ru", Ru},
		{"全部未知回退默认", "pt-BR, nl", Default},
		{"单一语言", "ar", Ar},
		{"俄语", "ru-RU", Ru},
		{"西班牙语", "es-419", Es},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.header); got != tc.want {
				t.Errorf("Parse(%q) = %q，期望 %q", tc.header, got, tc.want)
			}
		})
	}
}

// TestMessage_各语言返回对应文案 验证六种语言都能命中。
func TestMessage_各语言返回对应文案(t *testing.T) {
	bundle := New()
	const key = "auth.invalid_token"

	if got := bundle.Message(ZhCN, key); got != "访问令牌无效" {
		t.Errorf("zh-CN 文案 = %q，期望 %q", got, "访问令牌无效")
	}
	if got := bundle.Message(En, key); got != "Invalid access token" {
		t.Errorf("en 文案 = %q，期望 %q", got, "Invalid access token")
	}
	if got := bundle.Message(Fr, key); got != "Jeton d'accès non valide" {
		t.Errorf("fr 文案 = %q，期望 %q", got, "Jeton d'accès non valide")
	}

	// 六种语言的同一键必须各不相同（避免误把中文复制到别的语言）
	seen := make(map[string]Locale)
	for _, locale := range Locales() {
		msg := bundle.Message(locale, key)
		if msg == "" {
			t.Fatalf("语言 %q 缺少键 %q 的文案", locale, key)
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("语言 %q 与 %q 的文案相同（%q），疑似漏翻译", locale, prev, msg)
		}
		seen[msg] = locale
	}
}

// TestMessage_缺语言回退中文 验证缺失某门语言时回退中文。
func TestMessage_缺语言回退中文(t *testing.T) {
	// 刻意构造一个只有中文与英文的目录，模拟"某门语言漏配"
	partial := &Bundle{catalog: Catalog{
		"demo.key": {ZhCN: "中文文案", En: "English text"},
	}}

	if got := partial.Message(Fr, "demo.key"); got != "中文文案" {
		t.Errorf("缺失 fr 时应回退中文，实际 %q", got)
	}
	if got := partial.Message(En, "demo.key"); got != "English text" {
		t.Errorf("命中 en 时应返回英文，实际 %q", got)
	}
}

// TestMessage_键不存在返回键名 验证漏配键时直接回显键名，便于发现。
func TestMessage_键不存在返回键名(t *testing.T) {
	bundle := New()
	const missing = "no.such.key"

	if got := bundle.Message(En, missing); got != missing {
		t.Errorf("键不存在时应返回键名 %q，实际 %q", missing, got)
	}
	if got := bundle.Message(ZhCN, missing); got != missing {
		t.Errorf("键不存在时应返回键名 %q，实际 %q", missing, got)
	}
}

// TestCatalog_键集合六语言一致 验证不存在"半翻译"。
//
// 这是本包最重要的一条断言：任何一个键只要缺某门语言，界面就会出现
// 中文与目标语言混杂的"半翻译"状态。
func TestCatalog_键集合六语言一致(t *testing.T) {
	bundle := New()
	if len(bundle.catalog) == 0 {
		t.Fatal("消息目录为空，疑似加载失败")
	}

	for key, entry := range bundle.catalog {
		for _, locale := range Locales() {
			msg, ok := entry[locale]
			if !ok {
				t.Errorf("键 %q 缺少语言 %q 的词条", key, locale)
				continue
			}
			if msg == "" {
				t.Errorf("键 %q 的语言 %q 词条为空", key, locale)
			}
		}
		// 反向校验：词条里不应出现 supported 之外的语言（防止拼写错误产生僵尸语言）
		for locale := range entry {
			if !isSupported(locale) {
				t.Errorf("键 %q 含未知语言 %q", key, locale)
			}
		}
	}
}

// isSupported 报告某语言是否在受支持集合内。
func isSupported(locale Locale) bool {
	for _, s := range supported {
		if s == locale {
			return true
		}
	}
	return false
}
