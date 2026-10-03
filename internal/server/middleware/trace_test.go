// 本文件是追踪 ID 中间件的单元测试。
//
// 意图（Why）：
//
//	追踪 ID 的两个安全性质一旦被改坏，后果不会立刻显现，但很难排查：
//	  1) 入站值必须白名单校验——否则超长串或带引号换行的串会被原样回显，
//	     撑爆日志、污染文本，等于把日志注入的口子交给客户端；
//	  2) 服务端必须能自己生成 ID——入站值不合规时要能兜底，
//	     保证"每个请求都有 ID"，否则后续保存日志、查工单的链路就断了。
//	本文件把这两条钉死，并验证 ID 同时落到响应头与 reqctx（relay 层只认后者）。
//
// 流转（Flow）：
//
//	go test ./internal/server/middleware/ → 直接调 sanitizeTraceID / newTraceID 做表驱动，
//	  再用 gin 最小路由跑一次请求，断言响应头与 reqctx 中的取值一致
//
// 扩展（Extend）：
//
//	调整白名单字符集或长度上限时，同步更新本文件的用例，别只改实现。
package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/reqctx"
)

// TestSanitizeTraceID_各类入站值_按白名单决定取舍 覆盖归一与拒绝两类行为。
func TestSanitizeTraceID_各类入站值_按白名单决定取舍(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "标准十六进制原样保留", raw: "4bf92f3577b34da6a3ce929d0e0e4736", want: "4bf92f3577b34da6a3ce929d0e0e4736"},
		{name: "允许的三种连接符", raw: "tr-abc_123.def:456", want: "tr-abc_123.def:456"},
		{name: "前后空白被归一", raw: "  tr-abc123  ", want: "tr-abc123"},
		{name: "空串拒绝", raw: "", want: ""},
		{name: "纯空白拒绝", raw: "   ", want: ""},
		{name: "超长（65 字符）拒绝", raw: strings.Repeat("a", maxTraceIDLength+1), want: ""},
		{name: "含空格的内部值拒绝", raw: "tr-ab cd", want: ""},
		{name: "含双引号拒绝（日志注入原料）", raw: "tr-\"abc\"", want: ""},
		{name: "含内部换行拒绝（日志注入原料）", raw: "tr-abc\ninjected", want: ""},
		{name: "含中文拒绝", raw: "追踪-id", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTraceID(tc.raw); got != tc.want {
				t.Fatalf("sanitizeTraceID(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestSanitizeTraceID_恰好上限_保留 验证边界值 64 不被误拒（与 65 形成对照）。
func TestSanitizeTraceID_恰好上限_保留(t *testing.T) {
	raw := strings.Repeat("a", maxTraceIDLength)
	if got := sanitizeTraceID(raw); got != raw {
		t.Fatalf("长度 %d 应被接受，实际得到 %q", maxTraceIDLength, got)
	}
}

// TestNewTraceID_格式_带前缀且长度固定 验证服务端生成的 ID 形态可辨识。
func TestNewTraceID_格式_带前缀且长度固定(t *testing.T) {
	id := newTraceID()

	if !strings.HasPrefix(id, "tr-") {
		t.Fatalf("生成的追踪 ID 应带 tr- 前缀，实际 %q", id)
	}
	// 16 字节的十六进制 = 32 字符，加前缀共 35。
	if len(id) != 3+32 {
		t.Fatalf("生成的追踪 ID 长度应为 %d，实际 %d（%q）", 3+32, len(id), id)
	}
	if id == newTraceID() {
		t.Fatalf("两次生成的追踪 ID 不应相同：%q", id)
	}
}

// TestTrace_无入站值_生成并回写 验证默认路径：ID 同时进响应头与 reqctx。
func TestTrace_无入站值_生成并回写(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Trace())

	var fromReqCtx string
	r.GET("/ping", func(c *gin.Context) {
		fromReqCtx = reqctx.RequestID(c.Request.Context())
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.ServeHTTP(rec, req)

	headerID := rec.Header().Get(TraceHeader)
	if !strings.HasPrefix(headerID, "tr-") {
		t.Fatalf("响应头 %s 应为服务端生成的 tr- ID，实际 %q", TraceHeader, headerID)
	}
	if fromReqCtx != headerID {
		t.Fatalf("reqctx 中的 ID (%q) 应与响应头 (%q) 一致", fromReqCtx, headerID)
	}
}

// TestTrace_合法入站值_原样透传 验证客户端带入合规 ID 时全链路沿用同一个值。
func TestTrace_合法入站值_原样透传(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Trace())

	const inbound = "client-trace-001"
	var fromReqCtx string
	var fromGin string
	var ok bool
	r.GET("/ping", func(c *gin.Context) {
		fromReqCtx = reqctx.RequestID(c.Request.Context())
		fromGin, ok = TraceIDFrom(c)
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(TraceHeader, inbound)
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get(TraceHeader); got != inbound {
		t.Fatalf("响应头应透传入站 ID %q，实际 %q", inbound, got)
	}
	if fromReqCtx != inbound {
		t.Fatalf("reqctx 应为入站 ID %q，实际 %q", inbound, fromReqCtx)
	}
	if !ok || fromGin != inbound {
		t.Fatalf("gin 上下文应为入站 ID %q，实际 ok=%v id=%q", inbound, ok, fromGin)
	}
}

// TestTrace_非法入站值_不原样回显 验证注入类输入被丢弃并替换为服务端 ID。
func TestTrace_非法入站值_不原样回显(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Trace())
	r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	const forged = "evil\"\ninjected-line"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(TraceHeader, forged)
	r.ServeHTTP(rec, req)

	got := rec.Header().Get(TraceHeader)
	if strings.Contains(got, "injected-line") {
		t.Fatalf("非法入站 ID 不能被回显，实际响应头为 %q", got)
	}
	if !strings.HasPrefix(got, "tr-") {
		t.Fatalf("非法入站值应替换为服务端生成的 tr- ID，实际 %q", got)
	}
}

// TestTraceIDFrom_未挂中间件_返回 false 验证装配缺失时的降级语义。
func TestTraceIDFrom_未挂中间件_返回false(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if id, ok := TraceIDFrom(c); ok || id != "" {
		t.Fatalf("未挂中间件时应返回 (\"\", false)，实际 (%q, %v)", id, ok)
	}
}

// TestReqctxRequestID_未写入_返回空串 验证 reqctx 的零值语义（无 ID 时安全返回）。
func TestReqctxRequestID_未写入_返回空串(t *testing.T) {
	if got := reqctx.RequestID(context.Background()); got != "" {
		t.Fatalf("未写入时应返回空串，实际 %q", got)
	}
}
