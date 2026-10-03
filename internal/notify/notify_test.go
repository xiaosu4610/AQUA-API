package notify

// 本文件验证告警派发器与投递器的关键契约：不被订阅过滤误伤、不刷屏、
// 不泄露凭据、默认关闭（总开关）。
//
// 意图（Why）：
//
//	告警是"平时不出事、出事时救命"的组件，它的失败模式都很隐蔽：
//	去重窗口写错会刷屏（站长关掉告警 = 没有告警），
//	目标回显进日志会泄露钉钉/企微 token，
//	事件过滤写反会漏发（最该发的没发），
//	总开关语义写反会把"默认关闭"变成"悄悄外发"。
//	这些都不会让任何测试变红，除非专门为它写一条。
//
// 为什么用 httptest 而不是 mock Sender：
//
//	buildBody 的输出（钉钉 msgtype、企微 markdown）是给外部机器人消费的契约，
//	写错时不会 panic、只会"钉钉收到一条空消息"。
//	用真 HTTP 服务器打一遍，才能同时验证请求体与"非 2xx 视为失败"。
//
// 流转（Flow）：
//
//	Alert()（异步、去重、看总开关）/ SendTo()（同步、绕过去重）→ Sender
//
// 扩展（Extend）：
//
//	新增通道类型时，在此加一条断言请求体的用例；
//	调整去重窗口时，用例里的窗口断言需要同步。
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/metrics"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ── 测试替身 ──────────────────────────────────────────────────────────────

// alwaysEnabled 是"总开关打开"的判定函数，供投递类用例显式接线。
var alwaysEnabled = func(context.Context) bool { return true }

// fakeChannels 是一份内存通道仓储。
type fakeChannels struct {
	mu      sync.Mutex
	list    []*model.AlertChannel
	failErr error
	calls   int
}

func (f *fakeChannels) Create(context.Context, *model.AlertChannel) error { return nil }

func (f *fakeChannels) Update(context.Context, *model.AlertChannel) error { return nil }

func (f *fakeChannels) Delete(context.Context, uint64) error { return nil }

func (f *fakeChannels) Get(context.Context, uint64) (*model.AlertChannel, error) {
	return nil, model.ErrAlertChannelNotFound
}

func (f *fakeChannels) ListEnabled(context.Context) ([]*model.AlertChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failErr != nil {
		return nil, f.failErr
	}
	return f.list, nil
}

func (f *fakeChannels) List(context.Context) ([]*model.AlertChannel, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, len(f.list), nil
}

// fakeMailer 记录发往邮件通道的内容。
type fakeMailer struct {
	mu       sync.Mutex
	sent     []string
	subjects []string
	bodies   []string
	err      error
}

func (f *fakeMailer) Send(_ context.Context, to, subject, htmlBody string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, to)
	f.subjects = append(f.subjects, subject)
	f.bodies = append(f.bodies, htmlBody)
	return nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// captured 是一次收到的 HTTP 请求。
type captured struct {
	body   []byte
	header http.Header
}

// captureServer 启动一个把收到的请求记录下来的测试服务器。
func captureServer(t *testing.T, status int) (*httptest.Server, func() []captured) {
	t.Helper()
	var mu sync.Mutex
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		got = append(got, captured{body: body, header: r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		out := make([]captured, len(got))
		copy(out, got)
		return out
	}
}

// webhookSenderFor 造一个只往指定 URL 投递的 Sender。
func webhookSenderFor(target string) Sender {
	return senderFunc(func(ctx context.Context, tgt string, a Alert) error {
		return NewHTTPSender(model.AlertChannelWebhook).Send(ctx, target, a)
	})
}

type senderFunc func(ctx context.Context, target string, alert Alert) error

func (f senderFunc) Send(ctx context.Context, target string, alert Alert) error {
	return f(ctx, target, alert)
}

// newTestDispatcher 组一个最小可用且总开关打开的派发器。
func newTestDispatcher(t *testing.T, channels model.AlertChannelRepository, mailer Mailer) (*Dispatcher, *metrics.Registry) {
	t.Helper()
	reg := metrics.New()
	d := NewDispatcher(channels, mailer, map[string]Sender{}, reg)
	d.enabled = alwaysEnabled
	return d, reg
}

// waitFor 轮询等待条件成立，用于观测异步投递的最终结果。
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func sampleAlert() Alert {
	return Alert{
		Key:      model.EventChannelUnhealthy,
		Level:    LevelWarning,
		Title:    "渠道 OpenAI 主密钥 连续失败",
		Detail:   "最近 3 次探测均返回 503",
		Fields:   []Field{{Label: "渠道", Value: "OpenAI 主密钥"}, {Label: "状态码", Value: "503"}},
		DedupKey: "channel:12",
		At:       time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC),
	}
}

// ── 用例 ──────────────────────────────────────────────────────────────────

func TestDispatcher_按事件订阅过滤通道(t *testing.T) {
	mail := &fakeMailer{}
	// 只订阅"渠道恢复"的通道，不该收到"渠道不可用"。
	recoverOnly := &model.AlertChannel{
		ID: 1, Name: "只听恢复", Kind: model.AlertChannelEmail,
		Target: "ops@example.com", Events: model.EventChannelRecovered, Enabled: true,
	}
	// 空 Events = 订阅全部，应当收到。
	subscribeAll := &model.AlertChannel{
		ID: 2, Name: "全订阅", Kind: model.AlertChannelEmail,
		Target: "all@example.com", Events: "", Enabled: true,
	}
	// 停用的通道即便订阅了也不该收到。
	disabled := &model.AlertChannel{
		ID: 3, Name: "已停用", Kind: model.AlertChannelEmail,
		Target: "off@example.com", Events: "", Enabled: false,
	}
	repo := &fakeChannels{list: []*model.AlertChannel{recoverOnly, subscribeAll, disabled}}
	d, _ := newTestDispatcher(t, repo, mail)

	d.Alert(sampleAlert())
	if !waitFor(t, func() bool { return mail.count() == 1 }) {
		t.Fatalf("期望恰好投递 1 封，实际 %d 封", mail.count())
	}
	if mail.sent[0] != "all@example.com" {
		t.Fatalf("期望投递到订阅全部的通道，实际 %s", mail.sent[0])
	}
	if !strings.Contains(mail.subjects[0], "OpenAI 主密钥") {
		t.Fatalf("邮件主题应含告警标题，实际 %q", mail.subjects[0])
	}
	if !strings.HasPrefix(mail.subjects[0], "[LTZY-API]") {
		t.Fatalf("邮件主题应带本站品牌前缀，实际 %q", mail.subjects[0])
	}
}

func TestDispatcher_同类事件不同实例互不抑制(t *testing.T) {
	mail := &fakeMailer{}
	repo := &fakeChannels{list: []*model.AlertChannel{
		{ID: 1, Name: "邮箱", Kind: model.AlertChannelEmail, Target: "ops@example.com", Enabled: true},
	}}
	d, _ := newTestDispatcher(t, repo, mail)

	first := sampleAlert()
	first.DedupKey = "channel:12"
	second := sampleAlert()
	second.DedupKey = "channel:34" // 另一个渠道实例
	d.Alert(first)
	d.Alert(second)

	// DedupKey 相同才是同一件事；不同实例必须各发一条，否则"A 挂了"会顺带压掉"B 挂了"。
	if !waitFor(t, func() bool { return mail.count() == 2 }) {
		t.Fatalf("不同渠道实例应各发一条，实际发出 %d 封", mail.count())
	}
}

func TestDispatcher_去重窗口内不重复投递(t *testing.T) {
	mail := &fakeMailer{}
	repo := &fakeChannels{list: []*model.AlertChannel{
		{ID: 1, Name: "邮箱", Kind: model.AlertChannelEmail, Target: "ops@example.com", Enabled: true},
	}}
	reg := metrics.New()
	d := NewDispatcher(repo, mail, map[string]Sender{}, reg)
	d.enabled = alwaysEnabled

	d.now = func() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) }
	d.Alert(sampleAlert())
	if !waitFor(t, func() bool { return mail.count() == 1 }) {
		t.Fatalf("首条应投递成功")
	}

	// 固定时钟不动 → 窗口内必被抑制。
	for i := 0; i < 5; i++ {
		d.Alert(sampleAlert())
	}
	time.Sleep(80 * time.Millisecond)
	if got := mail.count(); got != 1 {
		t.Fatalf("去重窗口内应只发 1 封，实际发出 %d 封（窗口 10 分钟内）", got)
	}
	if suppressed, _ := reg.Value(metricAlertsSuppressed, model.EventChannelUnhealthy); suppressed != 5 {
		t.Fatalf("抑制计数应为 5，实际 %v", suppressed)
	}

	// 推过 10 分钟窗口后应恢复发送。
	d.now = func() time.Time { return time.Date(2026, 3, 1, 0, 11, 0, 0, time.UTC) }
	d.Alert(sampleAlert())
	if !waitFor(t, func() bool { return mail.count() == 2 }) {
		t.Fatalf("超出 10 分钟窗口后应重新投递，实际 %d 封", mail.count())
	}
}

func TestDispatcher_级别决定去重窗口(t *testing.T) {
	if dedupWindow[LevelCritical] >= dedupWindow[LevelWarning] {
		t.Fatalf("critical 窗口应短于 warning，否则「渠道被自动停用」这种最急的事会被 10 分钟窗口压住")
	}
	if dedupWindow[LevelWarning] >= dedupWindow[LevelInfo] {
		t.Fatalf("warning 窗口应短于 info，恢复类通知才不会被刷屏")
	}
}

func TestDispatcher_取通道失败不放弃告警也不影响业务(t *testing.T) {
	repo := &fakeChannels{failErr: context.DeadlineExceeded}
	d, _ := newTestDispatcher(t, repo, &fakeMailer{})

	// 关键性质：Alert 不返回 error、不 panic——触发点不该关心投递结果。
	d.Alert(sampleAlert())
	// 再调一次确保真的走完了这条路。
	if !waitFor(t, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.calls >= 1
	}) {
		t.Fatalf("应尝试读取通道列表")
	}
}

func TestDispatcher_未注册指标注册表也不崩(t *testing.T) {
	repo := &fakeChannels{list: []*model.AlertChannel{
		{ID: 1, Name: "邮箱", Kind: model.AlertChannelEmail, Target: "a@example.com", Enabled: true},
	}}
	mail := &fakeMailer{}
	d := NewDispatcher(repo, mail, map[string]Sender{}, nil)
	d.enabled = alwaysEnabled
	d.Alert(sampleAlert())
	if !waitFor(t, func() bool { return mail.count() == 1 }) {
		t.Fatalf("无注册表时仍应正常投递")
	}
	if sent, _, _ := d.snapshotCounts(); sent != 0 {
		t.Fatalf("无注册表时不应有计数，sent=%v", sent)
	}
}

func TestDispatcher_通道类型未接实现只失败该通道(t *testing.T) {
	mail := &fakeMailer{}
	repo := &fakeChannels{list: []*model.AlertChannel{
		{ID: 1, Name: "未知类型", Kind: "pagerduty", Target: "x", Enabled: true},
		{ID: 2, Name: "邮箱", Kind: model.AlertChannelEmail, Target: "a@example.com", Enabled: true},
	}}
	reg := metrics.New()
	d := NewDispatcher(repo, mail, map[string]Sender{}, reg)
	d.enabled = alwaysEnabled

	d.Alert(sampleAlert())
	if !waitFor(t, func() bool { return mail.count() == 1 }) {
		t.Fatalf("一个通道不支持不应拖累其它通道，实际收到 %d 封", mail.count())
	}
	if failed, _ := reg.Value(metricAlertsFailed, "pagerduty"); failed != 1 {
		t.Fatalf("失败计数应为 1，实际 %v", failed)
	}
}

func TestDispatcher_总开关默认关闭时不投递(t *testing.T) {
	mail := &fakeMailer{}
	repo := &fakeChannels{list: []*model.AlertChannel{
		{ID: 1, Name: "邮箱", Kind: model.AlertChannelEmail, Target: "ops@example.com", Enabled: true},
	}}

	// nil 判定函数 = 未接线，必须按"关闭"处理（安全默认）。
	unwired := NewDispatcher(repo, mail, map[string]Sender{}, metrics.New())
	unwired.Alert(sampleAlert())

	// 显式返回 false 的判定函数同样不投递。
	off := NewDispatcher(repo, mail, map[string]Sender{}, metrics.New())
	off.enabled = func(context.Context) bool { return false }
	off.Alert(sampleAlert())

	time.Sleep(120 * time.Millisecond)
	if got := mail.count(); got != 0 {
		t.Fatalf("总开关关闭时不得投递任何告警，实际发出 %d 封", got)
	}
}

func TestDispatcher_总开关开启时才投递(t *testing.T) {
	mail := &fakeMailer{}
	repo := &fakeChannels{list: []*model.AlertChannel{
		{ID: 1, Name: "邮箱", Kind: model.AlertChannelEmail, Target: "ops@example.com", Enabled: true},
	}}
	d := NewDispatcher(repo, mail, map[string]Sender{}, metrics.New())
	d.enabled = func(context.Context) bool { return true }

	d.Alert(sampleAlert())
	if !waitFor(t, func() bool { return mail.count() == 1 }) {
		t.Fatalf("总开关开启后应正常投递，实际 %d 封", mail.count())
	}
}

func TestSendTo_绕过去重与总开关当场给出成败(t *testing.T) {
	mail := &fakeMailer{}
	repo := &fakeChannels{}
	reg := metrics.New()
	d := NewDispatcher(repo, mail, map[string]Sender{}, reg)
	// 刻意不打开总开关：测试发送是管理员对单条通道的显式动作，不应被总开关拦住。
	d.enabled = func(context.Context) bool { return false }

	ch := &model.AlertChannel{
		ID: 1, Name: "测试", Kind: model.AlertChannelEmail, Target: "test@example.com", Enabled: true,
	}
	// 连续两次测试发送：若测试走去重，管理员第二次会看到"没发出去"的错误信号。
	for i := 0; i < 2; i++ {
		if err := d.SendTo(context.Background(), ch, sampleAlert()); err != nil {
			t.Fatalf("第 %d 次测试发送失败：%v", i+1, err)
		}
	}
	if mail.count() != 2 {
		t.Fatalf("测试发送不走去重，应发出 2 封，实际 %d 封", mail.count())
	}
	if sent, _, _ := d.snapshotCounts(); sent != 0 {
		t.Fatalf("测试发送不计入投递成功指标，sent=%v", sent)
	}
}

func TestSendTo_空通道明确报错(t *testing.T) {
	d, _ := newTestDispatcher(t, &fakeChannels{}, &fakeMailer{})
	if err := d.SendTo(context.Background(), nil, sampleAlert()); err == nil {
		t.Fatalf("空通道应报错")
	}
	// nil 派发器也不能 panic：告警失败绝不允许演变成进程崩溃。
	var nilDispatcher *Dispatcher
	if err := nilDispatcher.SendTo(context.Background(), &model.AlertChannel{}, sampleAlert()); err == nil {
		t.Fatalf("nil 派发器应报错而非崩溃")
	}
}

func TestSendTo_邮件通道未配SMTP时明确报错(t *testing.T) {
	d, _ := newTestDispatcher(t, &fakeChannels{}, nil)
	ch := &model.AlertChannel{Kind: model.AlertChannelEmail, Target: "a@example.com", Enabled: true}
	err := d.SendTo(context.Background(), ch, sampleAlert())
	if err == nil || !strings.Contains(err.Error(), "SMTP") {
		t.Fatalf("应提示 SMTP 未配置，实际 %v", err)
	}
}

// ── 通道请求体契约 ────────────────────────────────────────────────────────

func TestHTTPSender_通用Webhook送出结构化字段(t *testing.T) {
	srv, got := captureServer(t, http.StatusOK)
	sender := NewHTTPSender(model.AlertChannelWebhook)

	if err := sender.Send(context.Background(), srv.URL, sampleAlert()); err != nil {
		t.Fatalf("投递失败：%v", err)
	}
	reqs := got()
	if len(reqs) != 1 {
		t.Fatalf("期望 1 次请求，实际 %d 次", len(reqs))
	}
	if ct := reqs[0].header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type 应为 JSON，实际 %q", ct)
	}
	if ua := reqs[0].header.Get("User-Agent"); ua != "LTZY-API-Alerts/1" {
		t.Fatalf("User-Agent 应为 LTZY-API-Alerts/1，实际 %q", ua)
	}

	var payload map[string]any
	if err := json.Unmarshal(reqs[0].body, &payload); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}
	if payload["source"] != "ltzy-api" {
		t.Fatalf("source 字段应为 ltzy-api，实际 %v", payload["source"])
	}
	if payload["event"] != model.EventChannelUnhealthy {
		t.Fatalf("event 字段应为事件键，实际 %v", payload["event"])
	}
	if payload["level"] != string(LevelWarning) {
		t.Fatalf("level 字段应可被外部系统直接路由，实际 %v", payload["level"])
	}
	fields, _ := payload["fields"].(map[string]any)
	if fields["状态码"] != "503" {
		t.Fatalf("fields 应是标签→值的映射，实际 %v", payload["fields"])
	}
}

func TestHTTPSender_钉钉与企微请求体格式(t *testing.T) {
	cases := []struct {
		kind     string
		wantType string
		wantKey  string
	}{
		{model.AlertChannelDingTalk, "text", "text"},
		{model.AlertChannelWeCom, "markdown", "markdown"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			srv, got := captureServer(t, http.StatusOK)
			if err := NewHTTPSender(tc.kind).Send(context.Background(), srv.URL, sampleAlert()); err != nil {
				t.Fatalf("投递失败：%v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(got()[0].body, &payload); err != nil {
				t.Fatalf("请求体不是合法 JSON：%v", err)
			}
			if payload["msgtype"] != tc.wantType {
				t.Fatalf("msgtype 应为 %s，实际 %v", tc.wantType, payload["msgtype"])
			}
			block, _ := payload[tc.wantKey].(map[string]any)
			content, _ := block["content"].(string)
			if !strings.Contains(content, "OpenAI 主密钥") {
				t.Fatalf("content 应含告警标题，实际 %q", content)
			}
			if tc.kind == model.AlertChannelDingTalk && strings.Contains(content, "**") {
				t.Fatalf("钉钉 text 不支持 markdown 加粗，应送纯文本")
			}
		})
	}
}

func TestHTTPSender_非2xx视为失败且响应体被截断(t *testing.T) {
	huge := strings.Repeat("A", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	err := NewHTTPSender(model.AlertChannelWebhook).Send(context.Background(), srv.URL, sampleAlert())
	if err == nil {
		t.Fatalf("403 应视为投递失败")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("错误信息应含状态码，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "…") || len([]rune(err.Error())) > 400 {
		t.Fatalf("响应体应被截断到约 200 字符，实际长度 %d", len([]rune(err.Error())))
	}
}

func TestHTTPSender_地址不合法不回显原始输入(t *testing.T) {
	err := NewHTTPSender(model.AlertChannelWebhook).
		Send(context.Background(), "ftp://x/\x7f token=secret-token", sampleAlert())
	if err == nil {
		t.Fatalf("非法地址应报错")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("错误信息绝不能回显目标地址（钉钉/企微 URL 自带 token），实际 %v", err)
	}
}

func TestHTTPSender_连接失败也不回显目标地址(t *testing.T) {
	// 真实存在的坑：net/http 的 *url.Error 会把完整 URL 拼进 Error()，
	// 而投递失败正是最常被记日志、最容易被截图的一条路径。
	secret := "https://oapi.dingtalk.com/robot/send?access_token=LEAKY-TOKEN"
	err := NewHTTPSender(model.AlertChannelDingTalk).Send(context.Background(), secret, sampleAlert())
	if err == nil {
		t.Skip("目标域名竟然可解析且返回 2xx，用例前提不成立")
	}
	msg := err.Error()
	if strings.Contains(msg, "LEAKY-TOKEN") {
		t.Fatalf("连接失败的错误信息泄露了 token：%s", msg)
	}
	if strings.Contains(msg, "access_token") {
		t.Fatalf("连接失败的错误信息保留了查询串：%s", msg)
	}
	// 但原因必须还在：只抹地址不抹原因，管理员会看到一句毫无信息量的"失败"
	if !strings.Contains(msg, "投递失败") {
		t.Fatalf("错误信息应说明是投递失败：%s", msg)
	}
}

func TestHTTPSender_命中禁止网段时拒绝且不泄露凭据(t *testing.T) {
	// 云元数据地址（阿里云 ECS 内网元数据服务）：必须被 netguard 预检拦下。
	secret := "http://100.100.100.200/latest/meta-data?access_token=SUPER-SECRET"
	err := NewHTTPSender(model.AlertChannelWeCom).Send(context.Background(), secret, sampleAlert())
	if err == nil {
		t.Fatalf("云元数据地址应被拒绝")
	}
	msg := err.Error()
	if strings.Contains(msg, "SUPER-SECRET") {
		t.Fatalf("错误信息泄露了凭据：%s", msg)
	}
	if !strings.Contains(msg, "禁止访问") {
		t.Fatalf("错误信息应说明命中禁止网段，实际 %s", msg)
	}
}

func TestWebhookSender_经派发器投递成功计入指标(t *testing.T) {
	srv, got := captureServer(t, http.StatusOK)
	repo := &fakeChannels{list: []*model.AlertChannel{
		{ID: 1, Name: "群机器人", Kind: model.AlertChannelWebhook, Target: srv.URL, Enabled: true},
	}}
	reg := metrics.New()
	d := NewDispatcher(repo, &fakeMailer{}, map[string]Sender{
		model.AlertChannelWebhook: webhookSenderFor(srv.URL),
	}, reg)
	d.enabled = alwaysEnabled

	d.Alert(sampleAlert())
	if !waitFor(t, func() bool {
		v, _ := reg.Value(metricAlertsSent, model.AlertChannelWebhook)
		return v == 1
	}) {
		v, _ := reg.Value(metricAlertsSent, model.AlertChannelWebhook)
		t.Fatalf("成功计数应为 1，实际 %v；收到请求 %d 次", v, len(got()))
	}
}

// ── 目标脱敏 ──────────────────────────────────────────────────────────────

func TestMaskedTarget_邮箱掩码保留首字符与域名(t *testing.T) {
	c := &model.AlertChannel{Kind: model.AlertChannelEmail, Target: "operator@example.com"}
	got := c.MaskedTarget()
	if got == "operator@example.com" || strings.Contains(got, "perator") {
		t.Fatalf("邮箱掩码应隐藏本地部分，实际 %q", got)
	}
	if !strings.HasSuffix(got, "@example.com") || !strings.HasPrefix(got, "o") {
		t.Fatalf("应保留首字符与域名，实际 %q", got)
	}
}

func TestMaskedTarget_URL掩码隐藏查询串与凭据(t *testing.T) {
	c := &model.AlertChannel{
		Kind:   model.AlertChannelDingTalk,
		Target: "https://oapi.dingtalk.com/robot/send?access_token=SUPER-SECRET-TOKEN",
	}
	got := c.MaskedTarget()
	if strings.Contains(got, "SUPER-SECRET-TOKEN") || strings.Contains(got, "access_token") {
		t.Fatalf("URL 掩码必须隐藏查询串里的凭据，实际 %q", got)
	}
	if !strings.HasPrefix(got, "https://oapi.dingtalk.com") {
		t.Fatalf("应保留协议与主机，实际 %q", got)
	}
	if !strings.Contains(got, "已隐藏") {
		t.Fatalf("应标注凭据已隐藏，实际 %q", got)
	}
}

// ── 正文渲染 ──────────────────────────────────────────────────────────────

func TestRenderEmail_转义全部用户输入(t *testing.T) {
	alert := Alert{
		Key: model.EventChannelAutoDisabled, Level: LevelCritical,
		Title:  `<img src=x onerror="alert(1)">`,
		Detail: "成功率 12% & 阈值 30%",
		Fields: []Field{{Label: "<b>渠道</b>", Value: "a&b<c"}},
		At:     time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC),
	}
	html := RenderEmail(alert)
	if strings.Contains(html, "<img src=x") || strings.Contains(html, "<b>渠道</b>") {
		t.Fatalf("渠道名等输入可能来自上游返回，必须全量转义：%s", html)
	}
	if !strings.Contains(html, "&lt;img") || !strings.Contains(html, "a&amp;b&lt;c") {
		t.Fatalf("应出现转义后的内容：%s", html)
	}
	if !strings.Contains(html, "#dc2626") {
		t.Fatalf("严重级应使用红色强调条")
	}
	if !strings.Contains(html, "LTZY-API 告警") {
		t.Fatalf("邮件正文应带本站品牌标识：%s", html)
	}
}

func TestRender_三种渲染都不遗漏字段(t *testing.T) {
	alert := sampleAlert()
	for name, out := range map[string]string{
		"纯文本":      RenderPlainText(alert),
		"markdown": RenderMarkdown(alert),
		"邮件":       RenderEmail(alert),
	} {
		for _, want := range []string{alert.Title, alert.Detail, "OpenAI 主密钥", "503", "2026-03-01 10:30:00"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s 渲染缺少 %q", name, want)
			}
		}
	}
}

func TestShortTitle_按字符截断不切坏UTF8(t *testing.T) {
	got := ShortTitle("渠道熔断需要立刻处理", 6)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("应带省略号，实际 %q", got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("截断不得切坏多字节字符，实际 %q", got)
	}
	if ShortTitle("短", 6) != "短" {
		t.Fatalf("未超长时不应加省略号")
	}
}

func TestRenderPlainText_空Detail不产生多余空行(t *testing.T) {
	out := RenderPlainText(Alert{
		Key: model.EventTest, Level: LevelInfo, Title: "测试告警",
		At: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	})
	if strings.Contains(out, "\n\n") {
		t.Fatalf("无 Detail 时不应留空行：%q", out)
	}
}
