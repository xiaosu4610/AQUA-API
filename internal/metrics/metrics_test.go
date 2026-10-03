// 指标注册表与 HTTP 指标采集的测试。
//
// 意图（Why）：
//
//	指标是"平时没人看、出事时唯一能看"的东西，因此最危险的失败方式是
//	【看起来在工作、实际记的是错的】：
//	  1) 标签基数失控——把原始 URL 当标签，一个爬虫就能把监控存储打爆；
//	  2) 分桶写错——P95 算出来永远等于最大值，分位数失去意义；
//	  3) 时序拼接错误——输出的文本协议不合法，Prometheus 直接拒绝采集；
//	  4) 静默丢数据——指标名拼错后写入被忽略，监控"一直有数据但少了某类"。
//	本文件逐条钉住这些不变量。
//
// 流转（Flow）：
//
//	httptest 起一个装了 Trace + Metrics 中间件的引擎 → 打若干请求
//	  → 断言响应头、指标文本内容与标签取值
//
// 扩展（Extend）：
//
//	新增指标时，在下方补一条针对其"不可回退 / 不泄漏 / 可解析"的断言；
//	Trace 中间件的行为在 middleware/trace_test.go 中单独覆盖，此处不重复。
package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/metrics"
	"github.com/LTZY-ACU/ltzy-api/internal/server/middleware"
)

// newInstrumentedEngine 起一个带追踪与指标的测试引擎。
func newInstrumentedEngine() (*gin.Engine, *metrics.Registry) {
	gin.SetMode(gin.TestMode)
	reg := metrics.New()
	r := gin.New()
	r.Use(middleware.Trace(), middleware.Metrics(reg))
	return r, reg
}

func do(t *testing.T, r *gin.Engine, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestMetrics_路由模板作为标签 钉住"标签基数可控"这条底线。
//
// 关键断言：带 ID 的两个不同 URL 必须落在【同一条】时序上。
// 若实现改用原始路径，/items/1 与 /items/2 会各生成一条时序——
// 站点有多少用户，指标表就有多少行，最终把监控存储拖垮。
func TestMetrics_路由模板作为标签(t *testing.T) {
	r, reg := newInstrumentedEngine()
	r.GET("/items/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

	do(t, r, http.MethodGet, "/items/1", nil)
	do(t, r, http.MethodGet, "/items/2", nil)
	do(t, r, http.MethodGet, "/items/999", nil)

	// 三个不同 ID 必须是同一条时序，且计数为 3。
	if got, ok := reg.Value(middleware.MetricRequestsTotal,
		http.MethodGet, "/items/:id", "2xx"); !ok || got != 3 {
		t.Fatalf("路由模板时序计数 = %v（存在=%v），期望 3", got, ok)
	}

	text := reg.Render()
	if strings.Contains(text, "/items/1") || strings.Contains(text, "/items/2") {
		t.Errorf("指标里出现了原始 URL，会导致标签基数随用户数增长：\n%s", text)
	}
}

// TestMetrics_未匹配路由归一化 钉住"不可控输入不得成为标签"。
func TestMetrics_未匹配路由归一化(t *testing.T) {
	r, reg := newInstrumentedEngine()

	do(t, r, http.MethodGet, "/../../etc/passwd", nil)
	do(t, r, http.MethodGet, "/random-attacker-path", nil)

	text := reg.Render()
	if strings.Contains(text, "attacker") || strings.Contains(text, "..") {
		t.Errorf("未匹配路径的原始值泄漏进了指标标签：\n%s", text)
	}
	if !strings.Contains(text, `route="other"`) {
		t.Errorf("未匹配请求应归一到 other 标签，实际输出：\n%s", text)
	}
}

// TestMetrics_静态资源与端点自身不计入 钉住"指标不被噪音淹没"。
func TestMetrics_静态资源与端点自身不计入(t *testing.T) {
	r, reg := newInstrumentedEngine()
	r.GET("/metrics", func(c *gin.Context) { c.String(http.StatusOK, "x") })
	r.GET("/assets/app.js", func(c *gin.Context) { c.Status(http.StatusOK) })

	do(t, r, http.MethodGet, "/metrics", nil)
	do(t, r, http.MethodGet, "/assets/app.js", nil)

	if got, _ := reg.Value(middleware.MetricRequestsTotal, http.MethodGet, "/metrics", "2xx"); got != 0 {
		t.Errorf("/metrics 自身不应计入指标，实际计数 %v", got)
	}
	if got, _ := reg.Value(middleware.MetricRequestsTotal, http.MethodGet, "/assets/app.js", "2xx"); got != 0 {
		t.Errorf("静态资源不应计入指标，实际计数 %v", got)
	}
}

// TestMetrics_状态码分类 验证 4xx/5xx 被正确归类（错误率是告警的基础）。
func TestMetrics_状态码分类(t *testing.T) {
	r, reg := newInstrumentedEngine()
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	r.GET("/nope", func(c *gin.Context) { c.Status(http.StatusNotFound) })

	do(t, r, http.MethodGet, "/ok", nil)
	do(t, r, http.MethodGet, "/boom", nil)
	do(t, r, http.MethodGet, "/nope", nil)

	for _, want := range []struct{ route, class string }{
		{"/ok", "2xx"}, {"/boom", "5xx"}, {"/nope", "4xx"},
	} {
		if got, ok := reg.Value(middleware.MetricRequestsTotal, http.MethodGet, want.route, want.class); !ok || got != 1 {
			t.Errorf("%s 应计入 %s，实际 %v（存在=%v）", want.route, want.class, got, ok)
		}
	}
}

// TestMetrics_在途请求数回落归零 钉住"gauge 必须能减回去"。
//
// 这条用例针对一个真实踩过的坑：在途数用 counter 语义的 Add 递减时，
// 因 Add 拒绝负增量而永远只增不减——表现为"在途请求数单调上涨"，
// 监控上看起来像服务持续积压，实际纯属计数错误。
func TestMetrics_在途请求数回落归零(t *testing.T) {
	r, reg := newInstrumentedEngine()
	r.GET("/inflight", func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 5; i++ {
		do(t, r, http.MethodGet, "/inflight", nil)
	}

	if got, ok := reg.Value(middleware.MetricInFlight); !ok || got != 0 {
		t.Fatalf("请求全部结束后在途数应回到 0，实际 %v（存在=%v）", got, ok)
	}
}

// TestMetrics_直方图分桶单调 钉住"分位数可信"这条不变量。
//
// Prometheus 计算分位数的前提是各桶计数单调不减且末桶等于总数。
// 若实现漏加"值大于所有桶界"的情况，分位数会被系统性高估。
func TestMetrics_直方图分桶单调(t *testing.T) {
	r, reg := newInstrumentedEngine()
	r.GET("/slow", func(c *gin.Context) { c.Status(http.StatusOK) })

	do(t, r, http.MethodGet, "/slow", nil) // 极快，落在第一个桶

	if total, ok := reg.Count(middleware.MetricDurationSeconds, http.MethodGet, "/slow"); !ok || total != 1 {
		t.Fatalf("观测总数 = %d（存在=%v），期望 1", total, ok)
	}
	text := reg.Render()
	if !strings.Contains(text, `le="+Inf"`) {
		t.Errorf("直方图必须输出 +Inf 桶，否则总数无法与桶计数对上：\n%s", text)
	}
}

// TestMetrics_文本协议可解析 钉住"Prometheus 真能吃下我们输出的东西"。
func TestMetrics_文本协议可解析(t *testing.T) {
	r, reg := newInstrumentedEngine()
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	do(t, r, http.MethodGet, "/x", nil)

	for _, line := range strings.Split(strings.TrimSpace(reg.Render()), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		// 合法行必须是 "名字{标签} 值" 或 "名字 值"
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("输出行不符合文本协议（字段数应为 2）：%q", line)
		}
		// 只校验指标名部分：标签块内本来就会出现引号，那是合法的。
		name := fields[0]
		if idx := strings.IndexByte(name, '{'); idx >= 0 {
			name = name[:idx]
		}
		if strings.ContainsAny(name, `"{} `) {
			t.Errorf("指标名含非法字符：%q", name)
		}
		if !strings.HasPrefix(name, "aqua_") {
			t.Errorf("指标名应统一带项目前缀，避免与其它应用的指标冲突：%q", name)
		}
	}
}

// TestMetrics_未注册指标名直接崩溃 钉住"拼错指标名不能静默丢数据"。
func TestMetrics_未注册指标名直接崩溃(t *testing.T) {
	reg := metrics.New()
	reg.RegisterCounter("aqua_test_ok", "help")

	defer func() {
		if recover() == nil {
			t.Error("写入未注册指标名应 panic（否则拼错的指标会静默丢数据）")
		}
	}()
	reg.Inc("aqua_test_typo", "label")
}

// TestMetrics_重复注册直接崩溃 钉住"同名指标不会被静默覆盖"。
func TestMetrics_重复注册直接崩溃(t *testing.T) {
	reg := metrics.New()
	reg.RegisterCounter("aqua_dup", "help")

	defer func() {
		if recover() == nil {
			t.Error("重复注册同名指标应 panic（否则先注册的那条会悄悄停止更新）")
		}
	}()
	reg.RegisterCounter("aqua_dup", "help again")
}

// TestMetrics_标签值转义 钉住"文本协议不被输入污染"。
//
// 标签值含引号/换行时若不转义，产出的文本无法解析，
// 表现为"Prometheus 抓取 400"这种与业务毫无关系的故障。
func TestMetrics_标签值转义(t *testing.T) {
	reg := metrics.New()
	reg.RegisterCounter("aqua_escape", "help", "path")
	reg.Inc("aqua_escape", `/weird"path`+"\nsecond-line")

	text := reg.Render()
	if strings.Contains(text, "\"second-line") {
		t.Errorf("标签值里的换行未被转义，会把一行指标拆成两行：\n%s", text)
	}
	if !strings.Contains(text, `\"`) {
		t.Errorf("标签值里的引号未被转义：\n%s", text)
	}
}

// TestMetrics_计数不回退 钉住"counter 不得被负增量污染"。
func TestMetrics_计数不回退(t *testing.T) {
	reg := metrics.New()
	reg.RegisterCounter("aqua_no_regress", "help")
	reg.Add("aqua_no_regress", 3)
	reg.Add("aqua_no_regress", -1) // 必须被忽略

	if got, _ := reg.Value("aqua_no_regress"); got != 3 {
		t.Fatalf("counter 收到负增量后应保持不变，实际 %v", got)
	}
}
