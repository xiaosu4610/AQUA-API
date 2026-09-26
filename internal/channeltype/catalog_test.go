// 本文件用不变量测试钉住「渠道目录」的几条硬约束。
//
// 意图（Why）：
//
//	catalog.go 是一份纯数据，最容易出的问题不是编译错误，而是"悄悄写错一格"：
//	漏填 Label、Key 打重、把尚未适配的类型标成 Available —— 后者尤其危险，
//	因为后台一旦把它呈现给站长，就会造成"配好了却调不通"的假象。
//	因此这里把关键约定写成断言，改目录时若破坏约定会立刻失败。
//
// 流转（Flow）：
//
//	go test ./internal/channeltype/...
//	  ├─ 逐条遍历 Types()：字段完整性、Key 唯一性
//	  ├─ 安全底线：Available=true 的类型必须走 OpenAI 协议 + 已实现的鉴权方式
//	  ├─ 反向约束：SigV4/服务账号/OAuth/Cookie 一律不可用
//	  └─ 结构约束：分类覆盖、分组总数、Azure 必填额外参数
//
// 扩展（Extend）：
//
//	新增约定（例如"凡 Available 的类型必须有默认地址"）时，在此追加一条 test，
//	并在失败信息里写清"为什么这条约定重要"，方便后来人判断是改数据还是改约定。
package channeltype

import (
	"testing"
)

// allowedAuthModes 是「当前已实现的鉴权方式」白名单。
//
// 为什么是这几种：转发链路现在会拼 Authorization: Bearer、把密钥放进查询参数、
// 用自定义头（如 Azure 的 api-key）或 x-api-key（Anthropic 系）承载密钥，
// 以及对本地服务不带凭据；其余鉴权（SigV4 签名、服务账号、OAuth 续期、
// Cookie 会话）都还没有实现。
var allowedAuthModes = map[AuthMode]bool{
	AuthBearer:       true,
	AuthQueryKey:     true,
	AuthNone:         true,
	AuthAPIKeyHeader: true,
	AuthXAPIKey:      true,
}

// implementedProtocols 是「当前已实现的协议适配器」白名单。
//
// Azure（部署名 + api-version 进路径与查询）与 Anthropic（Messages 协议）的
// 出站适配器已实现并通过测试；Gemini / Vertex / Bedrock / PaLM / Ollama /
// 自定义等协议尚未实现，一律不能标为可用。
var implementedProtocols = map[Protocol]bool{
	ProtocolOpenAI:    true,
	ProtocolAzure:     true,
	ProtocolAnthropic: true,
}

// TestTypes_必填字段非空 保证每条类型都具备后台展示与路由所需的最小信息。
//
// 缺 Key/Label 会让界面出现空白项；缺 Category/Protocol/AuthMode 会让分组
// 或转发逻辑拿到空值；缺 Notes 则站长无从判断"这类上游是什么"。
func TestTypes_必填字段非空(t *testing.T) {
	for i, item := range Types() {
		if item.Key == "" {
			t.Errorf("第 %d 条类型缺少 Key（无法写入渠道记录）", i)
		}
		if item.Label == "" {
			t.Errorf("类型 %q 缺少 Label（后台会显示空白）", item.Key)
		}
		if item.Category == "" {
			t.Errorf("类型 %q 缺少 Category（无法分组展示）", item.Key)
		}
		if item.Protocol == "" {
			t.Errorf("类型 %q 缺少 Protocol（转发时无法选适配器）", item.Key)
		}
		if item.AuthMode == "" {
			t.Errorf("类型 %q 缺少 AuthMode（转发时无法带凭据）", item.Key)
		}
		if item.Notes == "" {
			t.Errorf("类型 %q 缺少 Notes（站长无法理解其特点）", item.Key)
		}
	}
}

// TestTypes_Key唯一 保证类型标识不重复。
//
// Key 是渠道记录对外的主键语义：一旦重复，Find 只会命中第一条，
// 第二条将永远无法被选中，属于"登记了却用不上"的静默故障。
func TestTypes_Key唯一(t *testing.T) {
	seen := make(map[string]bool)
	for _, item := range Types() {
		if seen[item.Key] {
			t.Errorf("Key %q 重复登记（Find 只会命中第一条，后者将永远无法被选中）", item.Key)
		}
		seen[item.Key] = true
	}
}

// TestAvailable_协议必须已实现 钉住安全底线之一。
//
// 只有实现了协议适配器的类型才能标为 Available=true；否则后台会放行选用，
// 但转发时没有任何适配器能处理它，结果是"配好了却调不通"。
// 当前已实现的协议见 implementedProtocols（OpenAI 兼容 / Azure / Anthropic）。
func TestAvailable_协议必须已实现(t *testing.T) {
	for _, item := range Types() {
		if item.Available && !implementedProtocols[item.Protocol] {
			t.Errorf("类型 %q（%s）标为可用，但协议为 %q 尚未实现；"+
				"误标会导致配好后调不通",
				item.Key, item.Label, item.Protocol)
		}
	}
}

// TestAvailable_鉴权必须是已实现方式 钉住安全底线之二。
//
// 即便协议是 OpenAI 兼容，只要鉴权方式还没实现（签名、服务账号、
// OAuth、Cookie），类型同样不能标为可用。
func TestAvailable_鉴权必须是已实现方式(t *testing.T) {
	for _, item := range Types() {
		if item.Available && !allowedAuthModes[item.AuthMode] {
			t.Errorf("类型 %q（%s）标为可用，但鉴权方式 %q 尚未实现；"+
				"误标会导致配好后鉴权失败", item.Key, item.Label, item.AuthMode)
		}
	}
}

// TestUnavailable_未实现鉴权方式一律不可用 是上一条的反向约束。
//
// 用 AuthSigV4 / AuthServiceAccount / AuthOAuth / AuthCookie 的类型
// 一定是未完成的接入，必须显式标为不可用，避免被误开。
func TestUnavailable_未实现鉴权方式一律不可用(t *testing.T) {
	unimplemented := map[AuthMode]bool{
		AuthSigV4:          true,
		AuthServiceAccount: true,
		AuthOAuth:          true,
		AuthCookie:         true,
	}
	for _, item := range Types() {
		if unimplemented[item.AuthMode] && item.Available {
			t.Errorf("类型 %q（%s）使用未实现的鉴权方式 %q，却被标为可用",
				item.Key, item.Label, item.AuthMode)
		}
	}
}

// TestTypes_数量下限 防止目录被误删导致静默少上游。
//
// 这类事故很隐蔽：删掉几条数据不会编译失败，站长只会发现"某个上游
// 在后台找不到了"，却无从判断是被删了还是从未登记。因此设一个下限。
func TestTypes_数量下限(t *testing.T) {
	const minCount = 70
	if got := len(Types()); got < minCount {
		t.Fatalf("渠道目录仅 %d 条，少于下限 %d 条；"+
			"目录被误删会静默少上游（后台看不到、站长无从察觉），请核对 catalog.go",
			got, minCount)
	}
}

// TestTypesByCategory_覆盖全部分类且总数一致 校验分组逻辑与目录是否自洽。
//
// 两个关注点：
//  1. 八个大类都必须有类型，否则后台某个分组会长期空白（说明目录漏登记）；
//  2. 各组条目数之和必须等于 len(Types())，否则说明 TypesByCategory 丢项。
func TestTypesByCategory_覆盖全部分类且总数一致(t *testing.T) {
	allCategories := []Category{
		CategoryText, CategoryImage, CategoryVideo, CategoryAudio,
		CategoryEmbedding, CategoryAggregator, CategorySelfHosted, CategorySubscription,
	}

	grouped := TypesByCategory()
	total := 0
	for _, category := range allCategories {
		items := grouped[category]
		if len(items) == 0 {
			t.Errorf("分类 %q（%s）没有任何类型，后台该分组会长期空白，疑似漏登记",
				category, CategoryLabel(category))
		}
		total += len(items)
	}
	if total != len(Types()) {
		t.Errorf("分组条目数之和 %d 与目录总数 %d 不一致，说明 TypesByCategory 丢项",
			total, len(Types()))
	}
}

// TestAzure_必须声明部署名与API版本 验证「类型专属必填参数」机制确实生效。
//
// Azure 与标准 OpenAI 最大的不同就在于这两个参数：没有部署名请求会打到
// 不存在的路径，没有 api-version 会被直接拒绝。它们必须以 Required 形式
// 出现在 ExtraFields 里，后台才会在选中 Azure 时强制站长填写。
func TestAzure_必须声明部署名与API版本(t *testing.T) {
	azure, ok := Find("azure_openai")
	if !ok {
		t.Fatal("未登记 azure_openai 类型")
	}

	required := make(map[string]bool)
	for _, field := range azure.RequiredExtraFields() {
		required[field.Key] = true
	}
	for _, key := range []string{"deployment", "api_version"} {
		if !required[key] {
			t.Errorf("azure_openai 的必填额外参数缺少 %q；"+
				"缺少它后台就不会强制填写，线上会以 400/404 的形式暴露", key)
		}
	}

	// api_version 应带默认值，减少站长填写负担。
	for _, field := range azure.ExtraFields {
		if field.Key == "api_version" && field.Default == "" {
			t.Error("azure_openai 的 api_version 应有默认值，否则站长每次都要查文档")
		}
	}
}
