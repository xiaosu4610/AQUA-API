// 错误消息本地化的 HTTP 层测试。
//
// 意图（Why）：
//
//	多语言改造的价值只有在"完整链路"上才能体现：解析 Accept-Language → 写入
//	context → 业务错误按语言返回。本文件走真实的 gin 链路（含中间件与路由），
//	锁定两条底线：①按请求语言返回对应文案；②error.code 始终不变（前端据此判逻辑）。
//
// 流转（Flow）：
//
//	go test ./internal/server/ → httptest 调用 Handler，构造 /v1 请求
//
// 扩展（Extend）：
//
//	新增语言后，可在本文件补一条断言；新增用户可见错误码时，可参照本例
//	补"带语言 → 对应文案 / 不带语言 → 中文"两类断言。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// postV1WithLanguage 携带指定 Accept-Language 发起一次 /v1 请求（用无效令牌触发鉴权错误）。
func postV1WithLanguage(t *testing.T, srv *Server, acceptLanguage string) (*httptest.ResponseRecorder, oai.ErrorBody) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	// 不存在的令牌：TokenAuth 会在"查库校验"阶段直接返回 invalid_api_key
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var body oai.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v（原文 %s）", err, rec.Body.String())
	}
	return rec, body
}

// TestErrorLocale_按AcceptLanguage本地化且code不变 验证本地化与错误码稳定性。
func TestErrorLocale_按AcceptLanguage本地化且code不变(t *testing.T) {
	srv, _ := newTestServer(t)

	// 1) 带 en：message 为英文，code 保持 invalid_api_key
	recEn, bodyEn := postV1WithLanguage(t, srv, "en")
	if recEn.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", recEn.Code)
	}
	if bodyEn.Error.Message != "Invalid access token" {
		t.Errorf("en message = %q，期望英文文案", bodyEn.Error.Message)
	}
	if bodyEn.Error.Code != oai.CodeInvalidAPIKey {
		t.Errorf("en code = %q，期望 %q（code 不得随语言变化）", bodyEn.Error.Code, oai.CodeInvalidAPIKey)
	}

	// 2) 不带 Accept-Language：回退中文（与改动前完全一致）
	recZh, bodyZh := postV1WithLanguage(t, srv, "")
	if recZh.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", recZh.Code)
	}
	if bodyZh.Error.Message != "访问令牌无效" {
		t.Errorf("无语言头 message = %q，期望中文文案", bodyZh.Error.Message)
	}
	if bodyZh.Error.Code != oai.CodeInvalidAPIKey {
		t.Errorf("无语言头 code = %q，期望 %q", bodyZh.Error.Code, oai.CodeInvalidAPIKey)
	}
}
