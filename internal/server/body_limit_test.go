// 请求体大小上限中间件的回归测试。
//
// 锁住三件事：
//  1. 普通 JSON 接口的 body 读取有上限（此前完全无界，可被打爆内存）；
//  2. /v1 的限额与 oai.ReadBody 的判定线严格对齐（不能更紧，
//     否则超限会从"413 请求体超过上限"退化成一个 io 读错误）；
//  3. 已自行设限的备份校验端点不被外层限额挡住（否则大备份永远传不上来）。
//
// 参考：本测试取自 PR #8（作者 @jghuihui）的 internal/server/body_limit_test.go。
package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newBodyLimitEngine 构造只挂 bodyLimit 中间件的测试引擎，
// 处理器把"body 能读多少字节"写进响应。
func newBodyLimitEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(bodyLimit())
	handler := func(c *gin.Context) {
		n, err := io.Copy(io.Discard, c.Request.Body)
		if err != nil {
			c.String(http.StatusOK, "err:%d", n)
			return
		}
		c.String(http.StatusOK, "ok:%d", n)
	}
	engine.POST("/api/anything", handler)
	engine.POST("/v1/chat/completions", handler)
	engine.POST("/v1beta/models/x:generateContent", handler)
	engine.POST("/api/admin/maintenance/backup/inspect", handler)
	engine.GET("/api/get", func(c *gin.Context) {
		n, err := io.Copy(io.Discard, c.Request.Body)
		if err != nil {
			c.String(http.StatusOK, "err:%d", n)
			return
		}
		c.String(http.StatusOK, "ok:%d", n)
	})
	return engine
}

// postBody 发一个指定大小的请求体，返回响应体。
func postBody(t *testing.T, engine *gin.Engine, path, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestBodyLimit_普通接口读到上限即截断(t *testing.T) {
	engine := newBodyLimitEngine(t)

	// 4MiB 以内的 body 完整可读，业务语义不受影响
	small := strings.Repeat("a", 1024)
	if got := postBody(t, engine, "/api/anything", small); got != "ok:1024" {
		t.Fatalf("小请求体应完整读完，实际 %q", got)
	}

	// 超过上限时读取必须报错，而不是无限吞下去
	huge := strings.Repeat("a", defaultBodyLimitBytes+(1<<20))
	if got := postBody(t, engine, "/api/anything", huge); !strings.HasPrefix(got, "err:") {
		t.Fatalf("超过 %d 字节的请求体应读取失败，实际 %q", defaultBodyLimitBytes, got)
	}
}

func TestBodyLimit_转发端点与判定线对齐(t *testing.T) {
	engine := newBodyLimitEngine(t)

	// 不真分配 32MiB：用响应体里的 n 判断读到哪里停。
	// /v1 应恰好停在 oai 的判定线（MaxRequestBodyBytes+1），
	// 更多的字节读不出来 → err。
	body := strings.Repeat("a", relayBodyLimitBytes)
	got := postBody(t, engine, "/v1/chat/completions", body)
	if got != "ok:"+strconv.Itoa(relayBodyLimitBytes) {
		t.Fatalf("/v1 上限应对齐 oai.ReadBody（%d 字节），实际 %q", relayBodyLimitBytes, got)
	}

	// 多 1 字节就必须读不全（证明外层确实包了限额）
	got = postBody(t, engine, "/v1/chat/completions", body+"a")
	if !strings.HasPrefix(got, "err:") {
		t.Fatalf("/v1 超限应读取失败，实际 %q", got)
	}

	// Gemini 路径走同一档（此前只有 /v1 前缀被覆盖的话就会漏）
	got = postBody(t, engine, "/v1beta/models/x:generateContent", body+"a")
	if !strings.HasPrefix(got, "err:") {
		t.Fatalf("/v1beta 超限应读取失败，实际 %q", got)
	}
}

func TestBodyLimit_备份校验端点豁免(t *testing.T) {
	engine := newBodyLimitEngine(t)

	// 备份文件按默认 4MiB 限额会被截断，但该端点自行设了 512MiB 限额，
	// 外层必须放行，否则大备份永远传不上来。
	body := strings.Repeat("b", defaultBodyLimitBytes+(1<<20))
	got := postBody(t, engine, bodyLimitExemptPath, body)
	if got != "ok:"+strconv.Itoa(len(body)) {
		t.Fatalf("豁免端点应完整读完，实际 %q", got)
	}
}

func TestBodyLimit_无请求体的方法不包装(t *testing.T) {
	engine := newBodyLimitEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/api/get", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Body.String() != "ok:0" {
		t.Fatalf("GET 不应被限额影响，实际 %q", rec.Body.String())
	}
}

// TestLoginUsernameKey_超大请求体退回固定限流键 验证 keyFunc 自己也会限读，
// 且 body 会被完整放回（不截断业务解析）。
//
// 参考：本用例取自 PR #8（作者 @jghuihui）。
func TestLoginUsernameKey_超大请求体退回固定限流键(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 超过 keyFunc 自身上限：返回固定键，body 原样放回
	oversized := bytes.Repeat([]byte("x"), maxLoginKeyBodyBytes+1024)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/auth/login",
		io.NopCloser(bytes.NewReader(oversized)))

	if key := loginUsernameKey(ctx); key != loginBodyTooLargeKey {
		t.Fatalf("超大请求体应返回固定限流键，实际 %q", key)
	}
	restored, err := io.ReadAll(ctx.Request.Body)
	if err != nil {
		t.Fatalf("放回的 body 应可读: %v", err)
	}
	if !bytes.Equal(restored, oversized) {
		t.Fatalf("放回的 body 与原始不一致（读到 %d 字节，期望 %d）", len(restored), len(oversized))
	}

	// 正常登录体：仍按用户名返回，行为与从前一致
	ctx2, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx2.Request = httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":" Admin ","password":"x"}`))
	if key := loginUsernameKey(ctx2); key != "admin" {
		t.Fatalf("正常请求体应返回用户名，实际 %q", key)
	}
	// body 必须原样可读（线上事故：keyFunc 吃掉 body 导致全员无法登录）
	body, err := io.ReadAll(ctx2.Request.Body)
	if err != nil || !strings.Contains(string(body), "password") {
		t.Fatalf("body 未被完整放回（err=%v, body=%q）", err, body)
	}
}
