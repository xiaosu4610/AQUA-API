// 上游模型列表拉取与解析的单元测试。
//
// 意图（Why）：
//
//	各家上游的 /models 响应结构并不统一（有的返回对象数组、有的直接返回字符串数组），
//	而模型名一旦解析错，配好的渠道就会"明明有权却提示模型不存在"。
//	这里把四种已知形态与常见失败场景固定下来，避免将来"为了兼容某家上游"
//	改动解析逻辑时破坏其他家。
//
// 流转（Flow）：
//
//	go test ./internal/relay/ → 直接调用解析函数 + httptest 假上游
package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseModelList_兼容四种常见形态(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "OpenAI 标准（data 对象数组）",
			raw:  `{"object":"list","data":[{"id":"meta/llama-3.1-8b-instruct"},{"id":"nvidia/nemotron-4-340b"}]}`,
			want: []string{"meta/llama-3.1-8b-instruct", "nvidia/nemotron-4-340b"},
		},
		{
			name: "简化实现（data 字符串数组）",
			raw:  `{"data":["gpt-4o","gpt-4o-mini"]}`,
			want: []string{"gpt-4o", "gpt-4o-mini"},
		},
		{
			name: "顶层数组",
			raw:  `["m-b","m-a"]`,
			want: []string{"m-a", "m-b"},
		},
		{
			name: "models 字段",
			raw:  `{"models":["z-model","a-model"]}`,
			want: []string{"a-model", "z-model"},
		},
		{
			name: "字段名兜底（name / model）",
			raw:  `{"data":[{"name":"by-name"},{"model":"by-model"},{"id":"by-id"}]}`,
			want: []string{"by-id", "by-model", "by-name"},
		},
		{
			name: "去重并忽略空值",
			raw:  `{"data":[{"id":"same"},{"id":"same"},{"id":""},{"id":"  "},{"id":"other"}]}`,
			want: []string{"other", "same"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseModelList([]byte(tc.raw))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("模型数量应为 %d，实际 %d（%v）", len(tc.want), len(got), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("第 %d 项应为 %q，实际 %q（完整结果 %v）", i, tc.want[i], got[i], got)
				}
			}
		})
	}
}

func TestParseModelList_无法识别的格式应报错(t *testing.T) {
	for _, raw := range []string{
		`<html>not json</html>`,
		`{"error":"unauthorized"}`,
		`{}`,
		`{"object":"list"}`,
	} {
		if _, err := parseModelList([]byte(raw)); err == nil {
			t.Errorf("输入 %q 应判定为无法解析", raw)
		}
	}
}

func TestParseModelList_格式正确但列表为空_应返回空列表而非报错(t *testing.T) {
	// 空列表与"格式不对"是两回事：前者说明该账号可能没有列出模型的权限，
	// 后者说明上游根本不是 OpenAI 兼容实现。二者给管理员的提示应完全不同。
	for _, raw := range []string{`{"data":[]}`, `{"models":[]}`, `[]`} {
		models, err := parseModelList([]byte(raw))
		if err != nil {
			t.Errorf("输入 %q 应被识别为合法但为空的列表，实际报错: %v", raw, err)
			continue
		}
		if len(models) != 0 {
			t.Errorf("输入 %q 应解析为空列表，实际 %v", raw, models)
		}
	}
}

func TestNormalizeModelNames_去重并排序(t *testing.T) {
	got := normalizeModelNames([]string{" b ", "a", "b", "", "   ", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应为 %v，实际 %v", want, got)
		}
	}
}

func TestFetchModels_正常返回并携带鉴权头(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/models" {
			t.Errorf("应请求 /models，实际 %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"meta/llama-3.1-8b-instruct"},{"id":"nvidia/nemotron-4-340b"}]}`))
	}))
	defer upstream.Close()

	r := New(nil, Options{})
	models, err := r.FetchModels(context.Background(), upstream.URL, "nvapi-test-key")
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("应返回 2 个模型，实际 %d", len(models))
	}
	if gotAuth != "Bearer nvapi-test-key" {
		t.Fatalf("鉴权头应为 Bearer 形式，实际 %q", gotAuth)
	}
}

func TestFetchModels_上游报错时透传可读信息(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	r := New(nil, Options{})
	_, err := r.FetchModels(context.Background(), upstream.URL, "bad-key")
	if err == nil {
		t.Fatal("上游返回 401 时应报错")
	}
	// 上游的具体原因要能看到，否则管理员无从判断是密钥错还是地址错
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("错误信息应包含状态码与上游原因，实际: %v", err)
	}
}

func TestFetchModels_空模型列表应给出明确提示(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	r := New(nil, Options{})
	_, err := r.FetchModels(context.Background(), upstream.URL, "k")
	if err == nil {
		t.Fatal("空列表应报错而不是静默返回空")
	}
	if !strings.Contains(err.Error(), "空") {
		t.Fatalf("错误信息应说明是空列表，实际: %v", err)
	}
}

func TestFetchModels_地址非法应快速拒绝(t *testing.T) {
	r := New(nil, Options{})

	if _, err := r.FetchModels(context.Background(), "", "k"); err == nil {
		t.Error("空地址应报错")
	}
	if _, err := r.FetchModels(context.Background(), "integrate.api.nvidia.com/v1", "k"); err == nil {
		t.Error("缺少 scheme 的地址应报错（否则会被当作相对路径，报出难以理解的错误）")
	}
}
