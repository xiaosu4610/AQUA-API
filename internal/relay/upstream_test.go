// 上游请求构建器的单元测试。
//
// 意图（Why）：
//
//	构建器决定了"每个请求带什么凭据、打到哪个地址、附带哪些固定头"，
//	任何一处写错都会以线上 401/404 的形式暴露，且极难定位。
//	因此这里既测纯函数（URL / 头的组成），也用 httptest 起假上游，
//	断言"实际发出的请求"确实符合预期。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ 纯函数断言：鉴权头、固定头、路径模板、地址回退
//	  └─ httptest：把构建器产物真正发出去，断言上游收到的 URL / 头
//
// 扩展（Extend）：
//
//	新增鉴权方式或类型专属路径时，在此补一条子测试，并在失败信息里写清
//	"期望哪种形态、为什么"。
package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
)

// mustType 取出渠道类型规格，取不到即测试失败。
func mustType(t *testing.T, key string) channeltype.Type {
	t.Helper()
	item, ok := channeltype.Find(key)
	if !ok {
		t.Fatalf("渠道目录缺少类型 %q", key)
	}
	return item
}

// TestBuildUpstreamRequest_鉴权方式注入 验证四种鉴权头 + 无鉴权各自的形态。
func TestBuildUpstreamRequest_鉴权方式注入(t *testing.T) {
	const key = "sk-test-credential"

	cases := []struct {
		name         string
		typeKey      string
		wantHeader   string // 期望的请求头名
		wantValue    string // 期望的请求头值
		wantQuery    string // 期望落在查询参数里的密钥（query_key 用）
		wantInjected bool
	}{
		{
			name: "bearer_Authorization", typeKey: "custom_openai",
			wantHeader: "Authorization", wantValue: "Bearer " + key, wantInjected: true,
		},
		{
			name: "api_key_header_Azure", typeKey: "azure_openai",
			wantHeader: "Api-Key", wantValue: key, wantInjected: true,
		},
		{
			name: "x_api_key_Anthropic", typeKey: "anthropic",
			wantHeader: "X-Api-Key", wantValue: key, wantInjected: true,
		},
		{
			name: "query_key_Gemini", typeKey: "gemini",
			wantQuery: key, wantInjected: true,
		},
		{
			name: "none_本地服务", typeKey: "vllm",
			wantInjected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := mustType(t, tc.typeKey)
			built, err := buildUpstreamRequest(upstreamRequestInput{
				Type:    spec,
				BaseURL: "https://upstream.example.com",
				APIKey:  key,
				Model:   "gpt-4o",
				Path:    "/v1/chat/completions",
			})
			if err != nil {
				t.Fatalf("组装请求失败: %v", err)
			}

			if built.authInjected != tc.wantInjected {
				t.Errorf("authInjected = %v，期望 %v", built.authInjected, tc.wantInjected)
			}
			if tc.wantHeader != "" {
				if got := built.Header.Get(tc.wantHeader); got != tc.wantValue {
					t.Errorf("%s = %q，期望 %q", tc.wantHeader, got, tc.wantValue)
				}
			}
			if tc.wantQuery != "" {
				if !strings.Contains(built.URL, "key="+tc.wantQuery) {
					t.Errorf("URL = %q，期望查询参数含 key=%s", built.URL, tc.wantQuery)
				}
			}
			// 无鉴权类型不得带任何凭据
			if !tc.wantInjected && built.Header.Get("Authorization") != "" {
				t.Errorf("AuthNone 不应带 Authorization，实际 = %q", built.Header.Get("Authorization"))
			}
		})
	}
}

// TestBuildUpstreamRequest_固定头不可覆盖 验证类型声明的 DefaultHeaders 优先级最高。
//
// 为什么重要：Anthropic 必须携带固定的版本头，若调用方能覆盖它，
// 模板被意外改坏会导致上游直接拒绝请求（且报错含义模糊）。
func TestBuildUpstreamRequest_固定头不可覆盖(t *testing.T) {
	spec := mustType(t, "anthropic")

	headers := http.Header{}
	headers.Set("anthropic-version", "0000-00-00") // 试图覆盖，应被忽略
	headers.Set("Accept", "text/event-stream")     // 普通头应被保留

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://api.anthropic.com",
		APIKey:  "sk-x",
		Model:   "claude-3-5-sonnet",
		Path:    "/v1/messages",
		Headers: headers,
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	if got := built.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q，期望固定值 2023-06-01（不可被调用方覆盖）", got)
	}
	if got := built.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q，普通头应被保留", got)
	}
}

// TestBuildUpstreamRequest_Azure路径与查询 验证部署名与 api-version 的拼装。
func TestBuildUpstreamRequest_Azure路径与查询(t *testing.T) {
	spec := mustType(t, "azure_openai")

	t.Run("部署名进路径_api版本进查询", func(t *testing.T) {
		built, err := buildUpstreamRequest(upstreamRequestInput{
			Type:    spec,
			BaseURL: "https://myres.openai.azure.com",
			APIKey:  "azure-key",
			Model:   "gpt-4o",
			Path:    "/v1/chat/completions",
			Extra:   map[string]string{"deployment": "my-deploy"},
		})
		if err != nil {
			t.Fatalf("组装请求失败: %v", err)
		}

		want := "https://myres.openai.azure.com/openai/deployments/my-deploy/chat/completions?api-version=2024-10-21"
		if built.URL != want {
			t.Errorf("URL = %q，期望 %q", built.URL, want)
		}
	})

	t.Run("部署名缺省回退为模型名", func(t *testing.T) {
		built, err := buildUpstreamRequest(upstreamRequestInput{
			Type:    spec,
			BaseURL: "https://myres.openai.azure.com",
			APIKey:  "azure-key",
			Model:   "gpt-4o-mini",
			Path:    "/v1/chat/completions",
		})
		if err != nil {
			t.Fatalf("组装请求失败: %v", err)
		}
		if !strings.Contains(built.URL, "/deployments/gpt-4o-mini/chat/completions") {
			t.Errorf("URL = %q，期望部署名缺省回退为模型名 gpt-4o-mini", built.URL)
		}
	})

	t.Run("显式api版本优先于默认值", func(t *testing.T) {
		built, err := buildUpstreamRequest(upstreamRequestInput{
			Type:    spec,
			BaseURL: "https://myres.openai.azure.com",
			APIKey:  "azure-key",
			Model:   "gpt-4o",
			Path:    "/v1/chat/completions",
			Extra:   map[string]string{"deployment": "d", "api_version": "2025-01-01-preview"},
		})
		if err != nil {
			t.Fatalf("组装请求失败: %v", err)
		}
		if !strings.HasSuffix(built.URL, "?api-version=2025-01-01-preview") {
			t.Errorf("URL = %q，期望使用显式 api_version", built.URL)
		}
	})
}

// TestBuildUpstreamRequest_默认地址回退 验证渠道未填地址时使用类型默认地址。
func TestBuildUpstreamRequest_默认地址回退(t *testing.T) {
	spec := mustType(t, "anthropic")

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:   spec,
		APIKey: "sk-x",
		Model:  "claude-3-5-sonnet",
		Path:   "/v1/messages",
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	// Anthropic 默认地址已含 /v1，路径模板为 /messages，二者不应重复版本段。
	if built.URL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("URL = %q，期望默认地址 + /messages", built.URL)
	}
}

// TestBuildUpstreamRequest_地址全空报错 验证无法确定目标时给出明确错误。
func TestBuildUpstreamRequest_地址全空报错(t *testing.T) {
	spec := mustType(t, "custom_openai") // 该类型无默认地址

	_, err := buildUpstreamRequest(upstreamRequestInput{
		Type:   spec,
		APIKey: "sk-x",
		Model:  "gpt-4o",
		Path:   "/v1/chat/completions",
	})
	if err == nil {
		t.Fatal("地址与默认地址都为空时应报错，实际未报错")
	}
	if !strings.Contains(err.Error(), "上游地址") {
		t.Errorf("错误信息应明确指出缺少上游地址，实际 = %v", err)
	}
}

// TestBuildUpstreamRequest_未实现鉴权报错 验证未实现的鉴权方式被明确拒绝。
//
// 现在仍属未实现的是订阅型鉴权（OAuth / Cookie）：用于订阅账号池，需要令牌续期能力。
// Bedrock 的 SigV4 与 Vertex 的服务账号已实现，改由专门用例覆盖（见 signature_test.go
// 与 upstream_signature_test.go），此处不应再拿它们当"未实现"样例。
func TestBuildUpstreamRequest_未实现鉴权报错(t *testing.T) {
	spec := mustType(t, "claude_subscription") // AuthOAuth 尚未实现

	_, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://subscription.example.com",
		APIKey:  "oauth-token",
		Model:   "claude-3",
		Path:    "/v1/chat/completions",
	})
	if err == nil {
		t.Fatal("未实现的鉴权方式应报错，实际未报错")
	}
}

// TestUpstreamRequest_日志不含密钥 验证日志字段绝不泄露凭据。
func TestUpstreamRequest_日志不含密钥(t *testing.T) {
	const secret = "sk-super-secret-must-not-leak"

	cases := []struct{ typeKey string }{
		{"custom_openai"}, // 头里
		{"gemini"},        // 查询参数里
	}

	for _, tc := range cases {
		spec := mustType(t, tc.typeKey)
		built, err := buildUpstreamRequest(upstreamRequestInput{
			Type:    spec,
			BaseURL: "https://upstream.example.com",
			APIKey:  secret,
			Model:   "m",
			Path:    "/v1/chat/completions",
		})
		if err != nil {
			t.Fatalf("组装请求失败: %v", err)
		}

		for field, value := range built.LogFields() {
			if strings.Contains(toText(value), secret) {
				t.Errorf("日志字段 %q 泄露了密钥: %v", field, value)
			}
		}
		// 查询参数里确实带上了密钥（用于转发），但日志里只看 path，不含 query
		if tc.typeKey == "gemini" && len(built.LogFields()["upstream_path"].(string)) == 0 {
			t.Error("upstream_path 应有值")
		}
	}
}

// toText 把日志字段值转为字符串以便检查是否含敏感内容。
func toText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// TestBuildUpstreamRequest_实际发送 用 httptest 假上游断言"真正发出的请求"。
//
// 与纯函数断言的区别：这里把构建器产物交给 http.Client 实际发出去，
// 能同时暴露 URL 解析、请求头规范化等只在真实往返中才显现的问题。
func TestBuildUpstreamRequest_实际发送(t *testing.T) {
	var (
		gotPath  string
		gotQuery string
		gotKey   string
		gotVer   string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("api-key")
		gotVer = r.Header.Get("anthropic-version")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	spec := mustType(t, "azure_openai")
	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: upstream.URL,
		APIKey:  "azure-secret-key",
		Model:   "gpt-4o",
		Path:    "/v1/chat/completions",
		Extra:   map[string]string{"deployment": "prod-deploy"},
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, built.URL, strings.NewReader(`{"model":"gpt-4o"}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header = built.Header

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if gotPath != "/openai/deployments/prod-deploy/chat/completions" {
		t.Errorf("上游收到的路径 = %q", gotPath)
	}
	if gotQuery != "api-version=2024-10-21" {
		t.Errorf("上游收到的查询 = %q", gotQuery)
	}
	if gotKey != "azure-secret-key" {
		t.Errorf("上游收到的 api-key = %q，期望 azure-secret-key", gotKey)
	}
	if gotVer != "" {
		t.Errorf("Azure 请求不应携带 anthropic-version，实际 = %q", gotVer)
	}
}
