// Package notify 把关键事件投递到站长的外部通道。
//
// 意图（Why）：
//
//	渠道熔断、成功率自动停用、登录锁定这些事件此前只打到 stdout，
//	等于"站点出事了只有主动翻日志才知道"。凌晨渠道全挂、早上才发现，
//	是中转站最常见也最贵的故障形态——本包就是为压缩这段发现延迟而存在。
//
// 三条设计红线（都是"告警系统自身的失败模式"）：
//
//  1. 【绝不能阻塞业务】投递是异步的，且带独立超时。告警通道挂了、
//     目标站不可达，都不允许让转发请求变慢——为了发一条告警拖慢主链路
//     是本末倒置；
//  2. 【绝不能刷屏】同一事件在窗口内只发一次。渠道连续失败 10 分钟
//     若每轮都告警，站长会先关掉告警，那就等于没有告警；
//  3. 【绝不泄露凭据】钉钉/企微的 URL 自带 access_token，
//     日志与错误信息里只出现脱敏形式。
//
// 流转（Flow）：
//
//	业务触发点 → Dispatcher.Alert(Alert)
//	  → 去重窗口（按 DedupKey + 级别）
//	  → 启用开关判定（enabled 为 nil 或返回 false 时直接丢弃）
//	  → ListEnabled 取通道 → 按 events 过滤
//	  → 逐通道投递（失败只记日志，不影响其它通道）
//
// 扩展（Extend）：
//
//	新增通道类型：在 channels.go 加一个 sender，并在 model 里认新的 kind；
//	调度策略（节流窗口、重试次数）在本文件集中调整。
package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/metrics"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// Level 是告警级别。
type Level string

const (
	// LevelInfo 仅供知悉（如渠道恢复）。
	LevelInfo Level = "info"
	// LevelWarning 需要关注但可继续观察（如单个渠道不可用）。
	LevelWarning Level = "warning"
	// LevelCritical 需要立即处理（如渠道被自动停用、账号被锁定）。
	LevelCritical Level = "critical"
)

// Alert 是一条待投递的告警。
//
// 结构而非一串参数：告警字段天然成组传递，且未来加字段时
// 不会让所有调用点的参数位置发生位移（那是最容易出错的改动）。
type Alert struct {
	// Key 是事件键，取 model 里定义的事件常量。
	Key string
	// Level 决定去重窗口与正文强调色。
	Level Level
	// Title 是一行标题，正文与邮件/群消息的第一行都用它。
	Title string
	// Detail 是可选的补充说明。
	Detail string
	// Fields 是结构化键值对（如"渠道：xxx"），比拼进 Detail 更易读。
	Fields []Field
	// DedupKey 是去重键；留空时用 Key。
	//
	// 为什么需要单独的键：同一类事件的不同实例不该互相抑制——
	// "渠道 A 挂了"与"渠道 B 挂了"必须各发一条，而它们的 Key 相同。
	DedupKey string
	// At 是事件发生时刻；零值时由 Dispatcher 填当前时间。
	At time.Time
}

// Field 是告警正文里的一条键值对。
type Field struct {
	Label string
	Value string
}

// 投递结果相关的指标名（对外契约，谨慎改名）。
//
// 沿用既有 aqua_ 前缀：它是本仓库指标包的统一前缀约定（见 internal/metrics），
// 与品牌名无关；改名会让既有监控面板的时序断档。
const (
	metricAlertsSent       = "aqua_alerts_sent_total"
	metricAlertsFailed     = "aqua_alerts_failed_total"
	metricAlertsSuppressed = "aqua_alerts_suppressed_total"
)

// dedupWindow 是各级别的去重窗口。
//
// 数值取舍：critical 最短（3 分钟）——"渠道被停用"这种事件晚知道一分钟
// 就多赔一分钟的钱；而 info 最长（30 分钟）——恢复类通知本身价值不高，
// 但一条接一条地刷屏会训练站长忽略告警。
var dedupWindow = map[Level]time.Duration{
	LevelInfo:     30 * time.Minute,
	LevelWarning:  10 * time.Minute,
	LevelCritical: 3 * time.Minute,
}

// 投递相关的默认参数。
const (
	// deliverTimeout 是单次投递的超时上限。
	deliverTimeout = 10 * time.Second
	// dispatchTimeout 是整个异步派发协程的存活上限。
	//
	// 为什么要给协程本身一个上限：告警目标可能接受连接后永不响应，
	// 若只靠单次请求超时，goroutine 仍可能因重试/连接池排队而长期堆积。
	dispatchTimeout = 30 * time.Second
)

// Mailer 是发邮件所需的最小能力（由 internal/mailer.Sender 满足）。
//
// 刻意只声明这一个方法而不是直接依赖具体类型：发送器是可替换的实现细节，
// 测试里要能塞一个假件而不必启动 SMTP。
type Mailer interface {
	Send(ctx context.Context, to, subject, htmlBody string) error
}

// Dispatcher 负责把告警投递到已配置的全部通道。
//
// 并发安全：dedup 有内部锁；投递本身只在各自的 goroutine 里跑。
type Dispatcher struct {
	channels model.AlertChannelRepository
	mailer   Mailer
	senders  map[string]Sender
	metrics  *metrics.Registry

	// enabled 是"是否允许外发"的总开关判定来源（默认关闭）。
	//
	// 为什么不给默认值而是要求显式注入：告警外发会把站内事件发到站外地址，
	// 属于需要站长主动开启的能力。为 nil（或返回 false）时必须按【关闭】处理，
	// 这样任何"忘了接线"的装配错误都表现为"不发"，而不是"悄悄发出去"。
	enabled func(context.Context) bool

	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time // 便于测试注入固定时钟
}

// NewDispatcher 创建告警派发器。
//
// senders 的键为通道类型；缺失的类型在投递时被跳过并记一条错误日志，
// 而不是 panic——新增一种通道但还没接上实现时，系统仍应能跑。
//
// 注意：调用方之后需通过 SetEnabledFunc 注入总开关判定；未注入时派发器
// 视为关闭（见 Dispatcher.enabled 的说明）。
func NewDispatcher(
	channels model.AlertChannelRepository,
	mailer Mailer,
	senders map[string]Sender,
	reg *metrics.Registry,
) *Dispatcher {
	copied := make(map[string]Sender, len(senders))
	for k, v := range senders {
		copied[k] = v
	}
	d := &Dispatcher{
		channels: channels,
		mailer:   mailer,
		senders:  copied,
		metrics:  reg,
		seen:     make(map[string]time.Time),
		now:      time.Now,
	}
	d.registerMetrics()
	return d
}

// SetEnabledFunc 注入"是否允许外发"的判定函数。
//
// 单独开放一个 setter 而不是塞进 NewDispatcher 的参数：判定函数往往由 server
// 提供（它持有设置仓储与缓存），而派发器要先于 server 构造才能注入 Deps，
// 二者存在构造顺序上的先后。用 setter 在 server 就绪后再接线，最直观。
func (d *Dispatcher) SetEnabledFunc(fn func(context.Context) bool) {
	if d == nil {
		return
	}
	d.enabled = fn
}

// registerMetrics 声明告警相关指标。
func (d *Dispatcher) registerMetrics() {
	if d.metrics == nil {
		return
	}
	d.metrics.RegisterCounter(metricAlertsSent, "成功投递的告警数", "channel")
	d.metrics.RegisterCounter(metricAlertsFailed, "投递失败的告警数", "channel")
	d.metrics.RegisterCounter(metricAlertsSuppressed, "被去重抑制的告警数", "event")
}

// Alert 异步投递一条告警。
//
// 刻意不给返回值：调用点在"渠道熔断""登录被拒"这类路径上，
// 拿到投递结果也做不了任何有意义的补偿——能做的只有记日志。
// 同步返回一个 error 只会诱使调用方写出一段假装有用、实则忽略错误的处理。
func (d *Dispatcher) Alert(alert Alert) {
	if d == nil || d.channels == nil {
		return
	}
	if alert.At.IsZero() {
		alert.At = d.now()
	}
	if alert.DedupKey == "" {
		alert.DedupKey = alert.Key
	}
	if !d.allow(alert) {
		d.count(metricAlertsSuppressed, alert.Key)
		return
	}

	// 用独立 context：触发点的 ctx 往往在响应返回后就被取消，
	// 沿用它会让"用户请求结束 → 告警丢失"，而告警恰恰最该在这种时候发出去。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
		defer cancel()
		// 总开关在真正投递之前判定：总开关默认关闭，为 nil 时也必须视为关闭，
		// 让"忘了接线"退化为"不发"而非"悄悄外发"。
		if d.enabled == nil || !d.enabled(ctx) {
			slog.Debug("告警外发总开关未开启，事件仅记录日志", "event", alert.Key, "title", alert.Title)
			return
		}
		d.deliver(ctx, alert)
	}()
}

// SendTo 同步投递一条告警到指定通道（不走去重、不记发送指标、不看总开关）。
//
// 存在的唯一理由是后台的「发送测试」：管理员需要当场看到成败，
// 而 Alert() 是异步且带去重的——用它做测试会得到"点了没反应"的错误信号。
// 因此本方法刻意绕过去重与总开关：测试发送是管理员对单条通道的显式动作，
// 不应因"总开关还关着"而让人误判成"通道坏了"。
func (d *Dispatcher) SendTo(ctx context.Context, ch *model.AlertChannel, alert Alert) error {
	if d == nil || ch == nil {
		return errors.New("notify: 告警通道为空")
	}
	if alert.At.IsZero() {
		alert.At = d.now()
	}
	return d.send(ctx, ch, alert)
}

// allow 判断该告警是否应发送（去重窗口内已发过则抑制）。
func (d *Dispatcher) allow(alert Alert) bool {
	window, ok := dedupWindow[alert.Level]
	if !ok {
		window = dedupWindow[LevelWarning]
	}
	key := alert.Key + "|" + alert.DedupKey
	now := d.now()

	d.mu.Lock()
	defer d.mu.Unlock()
	// 顺带清理过期条目：否则 seen 会随"事件种类 × 实例"无限增长，
	// 一个跑几个月的进程会在这里积累出可观的常驻内存。
	for k, at := range d.seen {
		if now.Sub(at) > 2*time.Hour {
			delete(d.seen, k)
		}
	}
	if last, ok := d.seen[key]; ok && now.Sub(last) < window {
		return false
	}
	d.seen[key] = now
	return true
}

// deliver 取出通道并逐个投递。
func (d *Dispatcher) deliver(ctx context.Context, alert Alert) {
	list, err := d.channels.ListEnabled(ctx)
	if err != nil {
		// 取不到通道时不能放弃告警——这恰恰是最需要它的时刻
		// （数据库抖动往往与站点故障同时发生），因此必须留下错误日志。
		slog.Error("读取告警通道失败，本次事件未投递", "error", err, "event", alert.Key)
		return
	}
	if len(list) == 0 {
		// 没配通道是常见状态（新站点），记 debug 而非 warn：
		// 否则每个事件都会刷一条告警级别的日志，很快被忽略。
		slog.Debug("未配置任何告警通道，事件仅记录日志", "event", alert.Key, "title", alert.Title)
		return
	}

	for _, ch := range list {
		if !ch.SubscribedTo(alert.Key) {
			continue
		}
		if err := d.send(ctx, ch, alert); err != nil {
			slog.Warn("告警投递失败", "channel", ch.Name, "kind", ch.Kind,
				"event", alert.Key, "error", err)
			d.count(metricAlertsFailed, ch.Kind)
			continue
		}
		d.count(metricAlertsSent, ch.Kind)
	}
}

// send 按通道类型投递单条告警。
func (d *Dispatcher) send(ctx context.Context, ch *model.AlertChannel, alert Alert) error {
	if ch.Kind == model.AlertChannelEmail {
		return d.sendEmail(ctx, ch, alert)
	}
	sender, ok := d.senders[ch.Kind]
	if !ok || sender == nil {
		// 不 panic：新增通道类型但尚未接上实现时，整个进程仍应正常运行。
		return errUnsupportedChannel(ch.Kind)
	}
	return sender.Send(ctx, ch.Target, alert)
}

// sendEmail 通过本站 SMTP 投递。
//
// 邮件通道需要单独的分支而不是塞进 senders map：它复用的是 mailer 而不是 HTTP，
// 与 Webhook 类通道的实现形态完全不同，硬塞进同一接口会逼出一个"万能 Send"。
func (d *Dispatcher) sendEmail(ctx context.Context, ch *model.AlertChannel, alert Alert) error {
	if d.mailer == nil {
		return errMailerUnavailable
	}
	subject := "[LTZY-API] " + alert.Title
	return d.mailer.Send(ctx, ch.Target, subject, RenderEmail(alert))
}

// count 记一条指标（未注册注册表时静默跳过）。
func (d *Dispatcher) count(name string, labelValue string) {
	if d.metrics == nil {
		return
	}
	d.metrics.Inc(name, labelValue)
}

// snapshotCounts 便于测试：返回派发器当前的投递统计（成功/失败/抑制）。
func (d *Dispatcher) snapshotCounts() (sent, failed, suppressed float64) {
	if d.metrics == nil {
		return 0, 0, 0
	}
	sent, _ = d.metrics.Value(metricAlertsSent, model.AlertChannelEmail)
	failed, _ = d.metrics.Value(metricAlertsFailed, model.AlertChannelEmail)
	suppressed, _ = d.metrics.Value(metricAlertsSuppressed, model.EventChannelUnhealthy)
	return sent, failed, suppressed
}

// ShortTitle 截断标题用于群消息单行展示。
func ShortTitle(title string, max int) string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) <= max {
		return title
	}
	return string(runes[:max]) + "…"
}
