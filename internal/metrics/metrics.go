// Package metrics 提供进程内指标注册与 Prometheus 文本协议输出。
//
// 意图（Why）：
//
//	一个网关必须能被外部监控抓取，否则站长只能靠"用户说慢了"这种事后线索排查。
//	本包只解决一件事：把运行期发生的计数与耗时，按 Prometheus 文本协议暴露成
//	GET /metrics，让 Prometheus / Grafana / 云监控直接抓。
//
// 为什么手写而不引 client_golang（取舍）：
//
//  1. 依赖面：本项目刻意保持精简依赖，且 SQLite 驱动也选了纯 Go 版以避免 CGO——
//     依赖是"每次升级都要重新评估的负债"，为一个只读端点引入一棵带传递依赖的树
//     并不划算；
//  2. 需求面：网关真正需要的指标很有限（请求量、错误率、延迟分位、在途数），
//     client_golang 的价值（自动注册表、Go runtime 指标、pushgateway）用不上；
//  3. 可控面：自己实现时，"哪些指标对外暴露"是一份可枚举的白名单，
//     不会像自动注册那样在某次升级后把内部细节悄悄暴露出去。
//     代价是若将来需要复杂聚合（native histogram、exemplars）得重写——
//     届时再评估换库，届时迁移成本也远低于现在就背上依赖。
//
// 流转（Flow）：
//
//	middleware.Metrics(reg) 每次请求 → reg.Inc/Observe(...) 累加计数与耗时
//	  → reg.Render() 渲染成文本 → Prometheus 抓取 GET /metrics
//
// 扩展（Extend）：
//
//	新增指标分两步：① 在 New() 之外显式声明名字、help、标签与类型（未声明的名字
//	一律被拒绝写入，避免拼错指标名后静默丢数据）；
//	② 在业务侧调用对应的 Inc/Add/Sub/SetGauge/Observe。
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 指标类型（对应 Prometheus 的三种基本类型）。
const (
	TypeCounter   = "counter"
	TypeGauge     = "gauge"
	TypeHistogram = "histogram"
)

// DefaultBuckets 是 HTTP 延迟直方图的默认分桶（秒）。
//
// 取值覆盖"从本地缓存命中到慢上游"的完整区间：0.005 是本进程纯内存处理，
// 1 以上才是真正的上游往返。分桶过密会让时间序列爆炸、过疏则 P95/P99 不可信，
// 这套是 HTTP 服务的常规档位。
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// series 是「一个指标名 + 一组标签取值」对应的时序。
type series struct {
	labels []string // 标签取值，顺序与指标声明的标签顺序一致
	value  float64  // counter / gauge 的当前值
	// 直方图专用
	counts []uint64 // 各桶累计计数（含最后一个 +Inf 桶）
	sum    float64
	total  uint64
}

// metric 是注册表中的一条指标定义（名字 + help + 类型 + 标签名）。
type metric struct {
	name       string
	help       string
	kind       string
	labelNames []string
	buckets    []float64
	series     map[string]*series
	// collect 非 nil 时该指标是「抓取时求值」的 gauge：
	// 运行时长这类值不是由事件累加出来的，只能在被抓取的那一刻算。
	collect func() float64
}

// Registry 是指标注册表。并发安全。
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*metric
	order   []string // 保持声明顺序，输出稳定，便于人读与 diff
}

// New 创建空注册表。
func New() *Registry {
	return &Registry{metrics: make(map[string]*metric)}
}

// RegisterCounter 声明一个带标签的累加指标。
//
// 重复声明同名指标是编程错误（多半是复制粘贴后忘了改名字），直接 panic：
// 静默覆盖会让先注册的那条指标悄悄停止更新，而这正是监控最容易骗人的失效方式。
func (r *Registry) RegisterCounter(name, help string, labelNames ...string) {
	r.register(&metric{
		name: name, help: help, kind: TypeCounter,
		labelNames: labelNames, series: make(map[string]*series),
	})
}

// RegisterGauge 声明一个带标签的瞬时值指标。
func (r *Registry) RegisterGauge(name, help string, labelNames ...string) {
	r.register(&metric{
		name: name, help: help, kind: TypeGauge,
		labelNames: labelNames, series: make(map[string]*series),
	})
}

// RegisterHistogram 声明一个带标签的直方图指标。
//
// buckets 为 nil 时使用 DefaultBuckets。
func (r *Registry) RegisterHistogram(name, help string, buckets []float64, labelNames ...string) {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	// 复制一份：调用方复用/修改自己的切片不应影响已注册的指标。
	own := make([]float64, len(buckets))
	copy(own, buckets)
	sort.Float64s(own)
	r.register(&metric{
		name: name, help: help, kind: TypeHistogram,
		labelNames: labelNames, buckets: own, series: make(map[string]*series),
	})
}

// RegisterGaugeFunc 声明一个「抓取时求值」的瞬时值指标（无标签）。
//
// 与 RegisterGauge 的区别：后者由业务在事件发生时 SetGauge，
// 适合"计数/余额"这类被事件驱动的值；而运行时长、当前连接数这类
// 「只在被问的那一刻才知道真值」的量，只能给一个求值函数——
// 若让业务每秒去 Set 一次，不仅浪费，还会因为没被事件覆盖而永久停留在旧值。
func (r *Registry) RegisterGaugeFunc(name, help string, fn func() float64) {
	r.register(&metric{
		name: name, help: help, kind: TypeGauge, collect: fn,
	})
}

func (r *Registry) register(m *metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.metrics[m.name]; dup {
		panic("metrics: 指标重复注册 " + m.name)
	}
	r.metrics[m.name] = m
	r.order = append(r.order, m.name)
}

// lookup 取出指标定义；labelValues 的个数必须与声明一致。
//
// 标签个数不匹配是调用点写错了（例如给 2 个标签的指标传 3 个值）。
// 这里不静默补空串——那会让 Prometheus 收到语义错乱的时序，排查成本极高。
func (r *Registry) lookup(name string, labelValues []string) (*metric, *series, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		// 未声明的指标名一律拒绝：允许动态写入等于让拼错的指标名静默消失。
		panic("metrics: 使用了未注册的指标 " + name)
	}
	if len(labelValues) != len(m.labelNames) {
		panic(fmt.Sprintf("metrics: 指标 %s 需要 %d 个标签值，收到 %d 个",
			name, len(m.labelNames), len(labelValues)))
	}
	key := strings.Join(labelValues, "\x00")
	s, ok := m.series[key]
	if !ok {
		s = &series{labels: append([]string(nil), labelValues...)}
		if m.kind == TypeHistogram {
			s.counts = make([]uint64, len(m.buckets))
		}
		m.series[key] = s
	}
	return m, s, key
}

// Inc 把某条时序的计数加 1。
func (r *Registry) Inc(name string, labelValues ...string) {
	r.Add(name, 1, labelValues...)
}

// Add 把某条时序的值加上 delta。
//
// delta <= 0 时直接返回：counter 一旦出现回退，Prometheus 会把整条时序判为
// 异常（rate 变为负）——需要"减少"语义的 gauge（如在途请求数）请用 Sub。
func (r *Registry) Add(name string, delta float64, labelValues ...string) {
	if delta <= 0 {
		return
	}
	_, s, _ := r.lookup(name, labelValues)
	r.mu.Lock()
	s.value += delta
	r.mu.Unlock()
}

// Sub 把某条时序的值减去 delta，用于 gauge 的"回落"（如请求结束时的在途数减一）。
//
// 与 Add 分开而不是让 Add 接受负值：counter 与 gauge 的语义必须泾渭分明——
// 允许负值会让"计数回退"这种明显异常静默通过，而这正是监控要抓的东西。
func (r *Registry) Sub(name string, delta float64, labelValues ...string) {
	if delta <= 0 {
		return
	}
	_, s, _ := r.lookup(name, labelValues)
	r.mu.Lock()
	s.value -= delta
	r.mu.Unlock()
}

// SetGauge 设置某条瞬时值指标的当前值。
func (r *Registry) SetGauge(name string, value float64, labelValues ...string) {
	_, s, _ := r.lookup(name, labelValues)
	r.mu.Lock()
	s.value = value
	r.mu.Unlock()
}

// Observe 记录一次观测值（直方图专用）。
func (r *Registry) Observe(name string, value float64, labelValues ...string) {
	m, s, _ := r.lookup(name, labelValues)
	if math.IsNaN(value) {
		// NaN 会污染 sum 与桶计数，且在 Prometheus 里表现为诡异的 +Inf。
		return
	}
	r.mu.Lock()
	s.sum += value
	s.total++
	for i, upper := range m.buckets {
		if value <= upper {
			s.counts[i]++
		}
	}
	r.mu.Unlock()
}

// Value 读取某条时序的当前值，主要供测试断言。
func (r *Registry) Value(name string, labelValues ...string) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		return 0, false
	}
	s, ok := m.series[strings.Join(labelValues, "\x00")]
	if !ok {
		return 0, false
	}
	return s.value, true
}

// Count 读取某条直方图时序的观测总数，主要供测试断言。
func (r *Registry) Count(name string, labelValues ...string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		return 0, false
	}
	s, ok := m.series[strings.Join(labelValues, "\x00")]
	if !ok {
		return 0, false
	}
	return s.total, true
}

// WriteTo 渲染为 Prometheus 文本协议。
func (r *Registry) WriteTo(w *strings.Builder) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, name := range r.order {
		m := r.metrics[name]
		fmt.Fprintf(w, "# HELP %s %s\n", m.name, escapeHelp(m.help))
		fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.kind)
		if m.collect != nil {
			// 抓取时求值：此刻函数出错/返回 NaN 时输出 0，
			// 宁可少一个指标值，也不能让整份 /metrics 变成 NaN 而被 Prometheus 拒绝。
			v := m.collect()
			if math.IsNaN(v) || math.IsInf(v, 0) {
				v = 0
			}
			fmt.Fprintf(w, "%s%s %s\n", m.name, renderLabels(m, &series{}, "", ""), formatFloat(v))
			continue
		}
		for _, s := range sortedSeries(m) {
			switch m.kind {
			case TypeHistogram:
				for i, upper := range m.buckets {
					fmt.Fprintf(w, "%s_bucket%s %d\n", m.name,
						renderLabels(m, s, "le", formatFloat(upper)), s.counts[i])
				}
				fmt.Fprintf(w, "%s_bucket%s %d\n", m.name,
					renderLabels(m, s, "le", "+Inf"), s.total)
				fmt.Fprintf(w, "%s_sum%s %s\n", m.name, renderLabels(m, s, "", ""), formatFloat(s.sum))
				fmt.Fprintf(w, "%s_count%s %d\n", m.name, renderLabels(m, s, "", ""), s.total)
			default:
				fmt.Fprintf(w, "%s%s %s\n", m.name, renderLabels(m, s, "", ""), formatFloat(s.value))
			}
		}
	}
}

// Render 返回 Prometheus 文本协议内容（Handler 与测试共用）。
func (r *Registry) Render() string {
	var sb strings.Builder
	r.WriteTo(&sb)
	return sb.String()
}

// ServeHTTP 让注册表可直接作为 http.Handler 挂在 /metrics 上。
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(r.Render()))
}

// sortedSeries 按标签取值排序，保证同一指标的输出顺序稳定。
//
// 为什么要排序：Prometheus 本身不关心顺序，但稳定输出让「两次抓取做文本 diff」
// 成为可用的排查手段；顺序随机会让每次 diff 都变成噪音。
func sortedSeries(m *metric) []*series {
	out := make([]*series, 0, len(m.series))
	for _, s := range m.series {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].labels, "\x00") < strings.Join(out[j].labels, "\x00")
	})
	return out
}

// renderLabels 渲染标签集合；extraName/extraValue 非空时附加一个标签（用于 le）。
func renderLabels(m *metric, s *series, extraName, extraValue string) string {
	if len(m.labelNames) == 0 && extraName == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, ln := range m.labelNames {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(ln)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabel(s.labels[i]))
		sb.WriteByte('"')
	}
	if extraName != "" {
		if len(m.labelNames) > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(extraName)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabel(extraValue))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

// escapeLabel 转义标签取值中的协议特殊字符。
//
// 标签值来自 URL 路径与状态码，理论上可含引号与换行；不转义会产出无法解析的文本，
// 表现为"Prometheus 抓取直接 400"这种与业务毫无关系的故障。
func escapeLabel(v string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(v)
}

// escapeHelp 转义 HELP 行文本（换行必须折行处理）。
func escapeHelp(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), "\n", " ")
}

// formatFloat 按 Prometheus 期望的格式输出浮点数。
//
// 整数值（如计数）输出为十进制整数，其余用最短往返表示；
// 极大/极小值用 %g 的常规形式即可（Prometheus 文本协议接受科学计数法）。
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}
