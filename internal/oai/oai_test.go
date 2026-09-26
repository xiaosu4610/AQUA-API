// Package oai 的单元测试。
//
// 意图（Why）：
//
//	本包被鉴权中间件与转发引擎共用，其中 ReadBody 的"还原请求体"行为是关键：
//	鉴权读一次、转发再读一次，任一处出错都会表现为难以定位的异常
//	（如"上游提示缺少 messages"）。因此这里专门验证读取与还原。
//
// 流转（Flow）：
//
//	go test ./internal/oai/
//
// 扩展（Extend）：
//
//	新增 Peek 函数时，按同样方式补充"正常 / 缺字段 / 非法 JSON / 超长"四类用例。
package oai

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteError_WritesStandardFormat 验证错误响应符合 OpenAI 格式。
//
// 格式契约很重要：客户端 SDK 按 error.code 做程序化判断，
// 字段名或层级写错会导致调用方只能显示"未知错误"。
func TestWriteError_WritesStandardFormat(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteError(rec, http.StatusUnauthorized, "令牌无效", TypeAuthentication, CodeInvalidAPIKey)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d，期望 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望包含 application/json", ct)
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if body.Error.Message != "令牌无效" {
		t.Errorf("message = %q", body.Error.Message)
	}
	if body.Error.Type != TypeAuthentication {
		t.Errorf("type = %q，期望 %q", body.Error.Type, TypeAuthentication)
	}
	if body.Error.Code != CodeInvalidAPIKey {
		t.Errorf("code = %q，期望 %q", body.Error.Code, CodeInvalidAPIKey)
	}
}

// TestReadBody_ReadsAndRestores 验证读取后请求体可被再次完整读取。
//
// 这是本包最关键的契约：鉴权中间件读一次用于校验模型白名单，
// 转发引擎随后再读一次用于发往上游。若不还原，转发阶段将读到空 body。
func TestReadBody_ReadsAndRestores(t *testing.T) {
	const payload = `{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))

	// 第一次读取（模拟鉴权中间件）
	first, err := ReadBody(req)
	if err != nil {
		t.Fatalf("首次 ReadBody 失败: %v", err)
	}
	if string(first) != payload {
		t.Errorf("首次读取内容 = %q，期望 %q", first, payload)
	}

	// 第二次读取（模拟转发引擎）
	second, err := ReadBody(req)
	if err != nil {
		t.Fatalf("二次 ReadBody 失败: %v", err)
	}
	if string(second) != payload {
		t.Errorf("二次读取内容 = %q，期望与首次一致（请求体未被还原？）", second)
	}
}

// TestReadBody_NilBody 验证空 body 不报错（部分 GET 类请求可能没有 body）。
func TestReadBody_NilBody(t *testing.T) {
	req := &http.Request{Body: nil}

	body, err := ReadBody(req)
	if err != nil {
		t.Fatalf("空 body 不应报错: %v", err)
	}
	if body != nil {
		t.Errorf("空 body 应返回 nil，实际 %d 字节", len(body))
	}
}

// TestReadBody_TooLarge 验证超过上限的请求体被拒绝。
//
// 意义：防止单个超大请求耗尽网关内存（大模型请求可能携带多张 Base64 图片）。
func TestReadBody_TooLarge(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), MaxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(oversized))

	_, err := ReadBody(req)
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("超限请求体错误 = %v，期望 ErrRequestTooLarge", err)
	}
}

// TestReadBody_ExactlyAtLimit 验证恰好等于上限的请求体被接受。
//
// 边界意义：实现上用 LimitReader(limit+1) 区分"恰好等于"与"超过"，
// 若少读 1 字节会把合法请求误判为超限。
func TestReadBody_ExactlyAtLimit(t *testing.T) {
	atLimit := bytes.Repeat([]byte("a"), MaxRequestBodyBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(atLimit))

	body, err := ReadBody(req)
	if err != nil {
		t.Fatalf("恰好等于上限的请求体应被接受，实际报错: %v", err)
	}
	if len(body) != MaxRequestBodyBytes {
		t.Errorf("读取长度 = %d，期望 %d", len(body), MaxRequestBodyBytes)
	}
}

// TestPeekModel 验证 model 字段提取。
func TestPeekModel(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantModel string
		wantErr   error
	}{
		{
			name:      "正常请求",
			body:      `{"model":"gpt-4o","messages":[]}`,
			wantModel: "gpt-4o",
		},
		{
			name:      "model 前后有空白应被裁剪",
			body:      `{"model":"  gpt-4o  "}`,
			wantModel: "gpt-4o",
		},
		{
			name:    "缺少 model",
			body:    `{"messages":[]}`,
			wantErr: ErrMissingModel,
		},
		{
			name:    "model 仅空白",
			body:    `{"model":"   "}`,
			wantErr: ErrMissingModel,
		},
		{
			name:    "非法 JSON",
			body:    `{ not json`,
			wantErr: ErrInvalidJSON,
		},
		{
			name:    "空请求体",
			body:    ``,
			wantErr: ErrInvalidJSON,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PeekModel([]byte(tc.body))

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("错误 = %v，期望 %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != tc.wantModel {
				t.Errorf("model = %q，期望 %q", got, tc.wantModel)
			}
		})
	}
}

// TestChatCompletionsPath 验证端点常量与预期一致。
//
// 该常量同时用于路由注册与上游地址拼接；写错会导致"注册成功但转发 404"。
func TestChatCompletionsPath(t *testing.T) {
	if ChatCompletionsPath != "/v1/chat/completions" {
		t.Errorf("ChatCompletionsPath = %q，与预期不符", ChatCompletionsPath)
	}
}
