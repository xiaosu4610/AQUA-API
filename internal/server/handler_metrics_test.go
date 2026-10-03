// /metrics 端点的端到端测试。
//
// 意图（Why）：
//
//	指标链路横跨三个环节（中间件采集 → 注册表聚合 → 端点渲染），
//	任何一环装配错位都会表现为"端点能访问但没有数据"——
//	这是最常见也最容易被误判为"监控坏了"的故障，因此必须有一条例集成用例。
//	另需钉住两件事：① 默认关闭（不对公网敞开）；② 配了令牌就必须拦住无令牌请求。
//
// 流转（Flow）：
//
//	newAnnouncementFixture（真实 Server）→ 按需开关 Metrics.Enabled → 打业务请求
//	  → 抓 /metrics → 断言状态码与内容
//
// 扩展（Extend）：
//
//	新增进程级指标时，在此补一条"它能在输出里被看到"的断言。
package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LTZY-ACU/ltzy-api/internal/config"
)

// TestMetrics端点_默认关闭 钉住"开箱即安全"：未显式开启时端点等同不存在。
func TestMetrics端点_默认关闭(t *testing.T) {
	fx := newAnnouncementFixture(t)

	rec, _ := doAnnouncementJSON(t, fx.srv, http.MethodGet, "/metrics", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("默认配置下 /metrics 应返回 404，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "aqua_") {
		t.Errorf("关闭状态下不应输出任何指标内容，实际：%s", firstLines(body, 5))
	}
}

// TestMetrics端点_默认配置关闭 钉住"默认值本身"（配置层契约）。
func TestMetrics端点_默认配置关闭(t *testing.T) {
	if config.Default().Metrics.Enabled {
		t.Error("指标端点必须默认关闭：运营规模与路由清单属内部信息，不应默认对公网敞开")
	}
}

// TestMetrics端点_启用后输出可被抓取的指标 验证"业务请求 → 指标可见"的完整链路。
func TestMetrics端点_启用后输出可被抓取的指标(t *testing.T) {
	fx := newAnnouncementFixture(t)
	fx.srv.deps.Config.Metrics.Enabled = true

	// 先产生一次业务请求（公告列表是公开接口，无需令牌）
	rec, _ := doAnnouncementJSON(t, fx.srv, http.MethodGet, "/api/announcements", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("前置业务请求应返回 200，实际 %d", rec.Code)
	}

	mrec, _ := doAnnouncementJSON(t, fx.srv, http.MethodGet, "/metrics", "", "")
	if mrec.Code != http.StatusOK {
		t.Fatalf("/metrics 应返回 200，实际 %d", mrec.Code)
	}
	body := mrec.Body.String()

	// 业务请求必须出现在指标里（这条最能说明"接线接对了"）
	if !strings.Contains(body, `aqua_http_requests_total`) {
		t.Errorf("输出缺少请求计数指标：\n%s", firstLines(body, 10))
	}
	if !strings.Contains(body, `route="/api/announcements"`) {
		t.Errorf("业务请求的路由模板未出现在指标中（采集中间件与端点可能没共用注册表）：\n%s", firstLines(body, 20))
	}
	// 进程级指标：抓取时求值那一类
	if !strings.Contains(body, "aqua_uptime_seconds") {
		t.Errorf("缺少运行时长指标：\n%s", firstLines(body, 10))
	}
	if !strings.Contains(body, "aqua_build_info") {
		t.Errorf("缺少构建信息指标：\n%s", firstLines(body, 10))
	}
	// 指标端点自身不得计入
	if strings.Contains(body, `route="/metrics"`) {
		t.Errorf("/metrics 把自己的请求也计入了，会在指标里自我噪声：\n%s", firstLines(body, 20))
	}
	if ct := mrec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q，Prometheus 要求 text/plain", ct)
	}
	if cc := mrec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q，必须禁缓存否则代理会反复返回同一份快照", cc)
	}
}

// TestMetrics端点_配置令牌后必须鉴权 钉住"配了令牌就真的拦住人"。
func TestMetrics端点_配置令牌后必须鉴权(t *testing.T) {
	fx := newAnnouncementFixture(t)
	fx.srv.deps.Config.Metrics.Enabled = true
	fx.srv.deps.Config.Metrics.Token = "s3cr3t-token"

	// 无令牌：拒绝，且必须带 WWW-Authenticate（否则采集端只看到一个无解释的 401）
	rec, _ := doAnnouncementJSON(t, fx.srv, http.MethodGet, "/metrics", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带令牌应返回 401，实际 %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 响应缺少 WWW-Authenticate 头，采集端无法判断该用哪种认证方式")
	}

	// 错误令牌：同样拒绝
	rec, _ = doAnnouncementJSON(t, fx.srv, http.MethodGet, "/metrics", "wrong-token", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误令牌应返回 401，实际 %d", rec.Code)
	}

	// 正确令牌：放行
	rec, _ = doAnnouncementJSON(t, fx.srv, http.MethodGet, "/metrics", "s3cr3t-token", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("正确令牌应放行，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "aqua_uptime_seconds") {
		t.Error("放行后的响应里没有指标内容")
	}
}

// firstLines 取正文前 n 行，用于失败时保持报错可读。
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
