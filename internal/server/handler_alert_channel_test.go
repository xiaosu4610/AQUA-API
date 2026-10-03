// 告警通道管理接口的端到端测试。
//
// 意图（Why）：
//
//	这组接口管理的对象是"把站内事件发到站外"的通道，因此要钉住的不只是 CRUD
//	正确性，还有几条安全与可用性契约：
//	  1) 列表里的投递目标必须脱敏（钉钉/企微 URL 自带 access_token，
//	     一次截图或一次日志粘贴就等于把机器人凭据送出去）；
//	  2) 编辑时目标留空表示"沿用原值"（界面上目标本来就是脱敏的，
//	     站长没法把原值抄回来；若要求必填，改个名字就得重贴带 token 的地址）；
//	  3) 告警外发总开关默认关闭，且可显式切换。
//
// 流转（Flow）：
//
//	newAlertChannelFixture → 列表/目录 → CRUD → 测试发送 → 总开关
//
// 扩展（Extend）：
//
//	新增通道类型或事件类型时，目录用例会继续守住"前端不用改版本"。
package server

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/config"
	"github.com/LTZY-ACU/ltzy-api/internal/crypto"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/notify"
	"github.com/LTZY-ACU/ltzy-api/internal/store"
)

// alertChannelFixture 是告警通道用例的运行固件。
type alertChannelFixture struct {
	srv  *Server
	repo model.AlertChannelRepository
	// adminTok 是管理员会话令牌。
	adminTok string
	// userTok 是普通用户令牌，用于验证越权。
	userTok string
	// mailer 记录测试发送的邮件。
	mailer *recordingMailer
}

// recordingMailer 记录测试发送的邮件内容。
type recordingMailer struct {
	sentTo []string
	bodies []string
}

func (r *recordingMailer) Send(_ context.Context, to, _, htmlBody string) error {
	r.sentTo = append(r.sentTo, to)
	r.bodies = append(r.bodies, htmlBody)
	return nil
}

// newAlertChannelFixture 装配一个带告警能力与两种身份会话的服务。
func newAlertChannelFixture(t *testing.T) *alertChannelFixture {
	t.Helper()
	gin.DefaultWriter = io.Discard

	dsn := filepath.Join(t.TempDir(), "alert_channel_test.db")
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	users := store.NewUserRepository(st.DB())
	sessions := store.NewSessionRepository(st.DB())
	alertChannels := store.NewAlertChannelRepository(st.DB())
	mail := &recordingMailer{}

	ctx := context.Background()
	admin := &model.User{
		Username: "alert-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled, Quota: model.QuotaUnlimited,
	}
	if err := users.Create(ctx, admin); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	normal := &model.User{
		Username: "alert-user", PasswordHash: "test-hash",
		Role: model.UserRoleUser, Status: model.UserStatusEnabled, Quota: 1000,
	}
	if err := users.Create(ctx, normal); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	srv := New(Deps{
		Config:        cfg,
		Store:         st,
		Users:         users,
		Sessions:      sessions,
		Settings:      store.NewSettingRepository(st.DB(), st.Dialect()),
		AlertChannels: alertChannels,
		Notifier: notify.NewDispatcher(alertChannels, mail, map[string]notify.Sender{
			model.AlertChannelWebhook:  notify.NewHTTPSender(model.AlertChannelWebhook),
			model.AlertChannelDingTalk: notify.NewHTTPSender(model.AlertChannelDingTalk),
			model.AlertChannelWeCom:    notify.NewHTTPSender(model.AlertChannelWeCom),
		}, nil),
	})

	return &alertChannelFixture{
		srv:      srv,
		repo:     alertChannels,
		adminTok: createAlertChannelSession(t, sessions, admin.ID, "admin"),
		userTok:  createAlertChannelSession(t, sessions, normal.ID, "user"),
		mailer:   mail,
	}
}

// createAlertChannelSession 建立一条有效会话并返回明文令牌。
func createAlertChannelSession(t *testing.T, sessions model.SessionRepository, userID uint64, tag string) string {
	t.Helper()
	token := "alert-session-" + strconv.FormatUint(userID, 10) + "-" + tag
	if err := sessions.Create(context.Background(), &model.Session{
		UserID:    userID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return token
}

// seedAlertChannel 直接写入一条通道（绕过接口，用于构造前提数据）。
func seedAlertChannel(t *testing.T, fx *alertChannelFixture, item *model.AlertChannel) uint64 {
	t.Helper()
	if err := fx.repo.Create(context.Background(), item); err != nil {
		t.Fatalf("预置告警通道失败: %v", err)
	}
	return item.ID
}

// ── 用例 ──────────────────────────────────────────────────────────────────

func Test告警通道_列表必须脱敏投递目标(t *testing.T) {
	fx := newAlertChannelFixture(t)
	secret := "https://oapi.dingtalk.com/robot/send?access_token=SUPER-SECRET-TOKEN"
	seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "钉钉群", Kind: model.AlertChannelDingTalk, Target: secret, Enabled: true,
	})
	seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "值班邮箱", Kind: model.AlertChannelEmail, Target: "ops@example.com", Enabled: true,
	})

	rec, body := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/alert-channels", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body = %v", rec.Code, body)
	}
	if total, _ := body["total"].(float64); total != 2 {
		t.Fatalf("total = %v，期望 2", body["total"])
	}

	raw := rec.Body.String()
	if strings.Contains(raw, "SUPER-SECRET-TOKEN") {
		t.Fatalf("响应里出现了钉钉 access_token，后台列表必须脱敏：%s", raw)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items 长度 = %d，期望 2", len(items))
	}
	first, _ := items[0].(map[string]any)
	masked, _ := first["target_masked"].(string)
	if masked == "" || strings.Contains(masked, "SUPER-SECRET-TOKEN") {
		t.Fatalf("target_masked = %q，应为非空且已脱敏", masked)
	}
	if kindText, _ := first["kind_text"].(string); kindText == "" {
		t.Error("kind_text 为空，前端无需再维护一份类型中文名映射")
	}
	if events, ok := first["events"].([]any); !ok || len(events) != 0 {
		t.Errorf("events = %v，期望空数组而非 null（前端 .map 会炸）", first["events"])
	}
}

func Test告警通道_目录接口覆盖全部类型与事件(t *testing.T) {
	fx := newAlertChannelFixture(t)
	rec, body := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/alert-channel-kinds", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}

	kinds, _ := body["kinds"].([]any)
	if len(kinds) != 4 {
		t.Fatalf("通道类型数 = %d，期望 4（email/webhook/dingtalk/wecom）", len(kinds))
	}
	events, _ := body["events"].([]any)
	if len(events) != len(model.AllAlertEvents()) {
		t.Fatalf("事件数 = %d，期望与 model.AllAlertEvents() 一致（%d）",
			len(events), len(model.AllAlertEvents()))
	}
	// 目录必须带中文说明，否则前端只能显示英文键名给用户看。
	for _, e := range events {
		item, _ := e.(map[string]any)
		value, _ := item["value"].(string)
		text, _ := item["text"].(string)
		if value == "" || text == "" {
			t.Fatalf("事件目录项缺 value 或 text：%v", item)
		}
		if text == value {
			t.Errorf("事件 %s 缺少中文说明", value)
		}
	}
}

func Test告警通道_普通用户不可访问(t *testing.T) {
	fx := newAlertChannelFixture(t)
	rec, _ := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/alert-channels", fx.userTok, "")
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("普通用户访问后台告警接口应被拒，实际 %d", rec.Code)
	}
	rec, _ = doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/alert-channels", fx.userTok,
		`{"name":"x","kind":"email","target":"a@example.com"}`)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("普通用户创建告警通道应被拒，实际 %d", rec.Code)
	}
}

func Test告警通道_编辑时目标留空沿用原值(t *testing.T) {
	fx := newAlertChannelFixture(t)
	secret := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=SECRET-KEY"
	id := seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "企微群", Kind: model.AlertChannelWeCom, Target: secret,
		Events: model.EventChannelAutoDisabled, Enabled: true,
	})

	// 只改名字，不带 target：界面上目标本来就是脱敏的，站长没法抄回原值
	rec, body := doBearerJSON(t, fx.srv, http.MethodPut,
		"/api/admin/alert-channels/"+strconv.FormatUint(id, 10), fx.adminTok,
		`{"name":"企微值班群","kind":"wecom","events":"`+model.EventChannelAutoDisabled+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body = %v", rec.Code, body)
	}

	got, err := fx.repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("读取通道失败: %v", err)
	}
	if got.Name != "企微值班群" {
		t.Fatalf("名称未更新，实际 %q", got.Name)
	}
	if got.Target != secret {
		t.Fatalf("目标应沿用原值，实际 %q", got.Target)
	}
	// events 与 target 的规则相反：它在界面上完全可见，留空就是"订阅全部"，
	// 因此必须以请求为准——否则站长永远无法清空订阅，只能去数据库改。
	if got.Events != model.EventChannelAutoDisabled {
		t.Fatalf("事件订阅未更新，实际 %q", got.Events)
	}

	// 再提交一次不带 events：应落成"订阅全部"（空），而不是沿用旧订阅
	rec, body = doBearerJSON(t, fx.srv, http.MethodPut,
		"/api/admin/alert-channels/"+strconv.FormatUint(id, 10), fx.adminTok,
		`{"name":"企微值班群","kind":"wecom","events":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body = %v", rec.Code, body)
	}
	got, _ = fx.repo.Get(context.Background(), id)
	if got.Events != "" {
		t.Fatalf("清空订阅应落成空值（=订阅全部），实际 %q", got.Events)
	}
	if got.Target != secret {
		t.Fatalf("清空订阅时目标仍应沿用原值，实际 %q", got.Target)
	}
}

func Test告警通道_修改时可显式换目标(t *testing.T) {
	fx := newAlertChannelFixture(t)
	id := seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "Webhook", Kind: model.AlertChannelWebhook,
		Target: "https://old.example.com/hook", Enabled: true,
	})

	rec, body := doBearerJSON(t, fx.srv, http.MethodPut,
		"/api/admin/alert-channels/"+strconv.FormatUint(id, 10), fx.adminTok,
		`{"name":"Webhook","kind":"webhook","target":"https://new.example.com/hook"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body = %v", rec.Code, body)
	}
	got, _ := fx.repo.Get(context.Background(), id)
	if got.Target != "https://new.example.com/hook" {
		t.Fatalf("目标应被替换，实际 %q", got.Target)
	}
}

func Test告警通道_校验失败要给出可读原因(t *testing.T) {
	fx := newAlertChannelFixture(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺少名称", `{"kind":"email","target":"a@example.com"}`, "通道名称"},
		{"类型不支持", `{"name":"x","kind":"telegram","target":"https://x.com"}`, "通道类型"},
		{"邮箱格式错", `{"name":"x","kind":"email","target":"not-an-email"}`, "邮箱"},
		{"URL 无协议", `{"name":"x","kind":"webhook","target":"example.com/hook"}`, "http"},
		// 事件键拼错若放过，会表现为"配置成功但那条告警永远不发"——静默失效，必须拦
		{"事件键拼错", `{"name":"x","kind":"email","target":"a@example.com","events":"channle_unhealthy"}`, "事件类型"},
		{"JSON 坏", `{`, "格式"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/alert-channels", fx.adminTok, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400，body = %v", rec.Code, body)
			}
			errObj, _ := body["error"].(map[string]any)
			msg, _ := errObj["message"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("错误信息 = %q，应包含 %q（前端直接展示给管理员）", msg, tc.want)
			}
		})
	}
	// 一条都不该落库
	items, _, _ := fx.repo.List(context.Background())
	if len(items) != 0 {
		t.Fatalf("校验失败却写入了 %d 条数据", len(items))
	}
}

func Test告警通道_编号不合法与不存在要区分(t *testing.T) {
	fx := newAlertChannelFixture(t)

	rec, body := doBearerJSON(t, fx.srv, http.MethodPut, "/api/admin/alert-channels/abc", fx.adminTok,
		`{"name":"x","kind":"email"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非数字编号应返回 400，实际 %d，body = %v", rec.Code, body)
	}

	// 存在的语义：编号合法但库里没有 → 404，而不是"校验失败"
	rec, _ = doBearerJSON(t, fx.srv, http.MethodDelete, "/api/admin/alert-channels/999999", fx.adminTok, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的通道应返回 404，实际 %d", rec.Code)
	}
}

func Test告警通道_测试发送当场给出成败(t *testing.T) {
	fx := newAlertChannelFixture(t)
	id := seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "值班邮箱", Kind: model.AlertChannelEmail, Target: "ops@example.com", Enabled: true,
	})

	rec, body := doBearerJSON(t, fx.srv, http.MethodPost,
		"/api/admin/alert-channels/"+strconv.FormatUint(id, 10)+"/test", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body = %v", rec.Code, body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("ok = %v，期望 true", body["ok"])
	}
	if len(fx.mailer.sentTo) != 1 || fx.mailer.sentTo[0] != "ops@example.com" {
		t.Fatalf("应恰好向 ops@example.com 发出 1 封，实际 %v", fx.mailer.sentTo)
	}
	// 测试消息本身也走脱敏后的目标，绝不能把原值拼进正文
	if strings.Contains(strings.Join(fx.mailer.bodies, "\n"), "ops@example.com") {
		t.Error("测试正文不应回显完整投递目标（邮箱本身也应脱敏）")
	}
	if !strings.Contains(fx.mailer.bodies[0], "LTZY-API 告警") {
		t.Errorf("测试邮件正文缺少告警标识，实际 %.120s", fx.mailer.bodies[0])
	}
}

func Test告警通道_测试发送失败要带原因且不回显URL(t *testing.T) {
	fx := newAlertChannelFixture(t)
	// 用一个必然失败的地址：TLD 为 .invalid，DNS 解析不到
	id := seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "坏地址", Kind: model.AlertChannelWebhook,
		Target: "https://aq-alert-target.invalid/hook?access_token=LEAKY", Enabled: true,
	})

	rec, body := doBearerJSON(t, fx.srv, http.MethodPost,
		"/api/admin/alert-channels/"+strconv.FormatUint(id, 10)+"/test", fx.adminTok, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("目标不可达应返回 502，实际 %d，body = %v", rec.Code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "测试发送失败") {
		t.Errorf("错误信息 = %q，应说明是测试发送失败", msg)
	}
	if strings.Contains(msg, "LEAKY") {
		t.Errorf("错误信息回显了目标里的 token：%q", msg)
	}
}

func Test告警通道_测试发送不走去重(t *testing.T) {
	fx := newAlertChannelFixture(t)
	id := seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "值班邮箱", Kind: model.AlertChannelEmail, Target: "ops@example.com", Enabled: true,
	})
	// 连续两次：若测试走去重，管理员第二次会看到"没发出去"，误判成通道坏了
	for i := 0; i < 2; i++ {
		rec, _ := doBearerJSON(t, fx.srv, http.MethodPost,
			"/api/admin/alert-channels/"+strconv.FormatUint(id, 10)+"/test", fx.adminTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次测试发送失败，状态码 %d", i+1, rec.Code)
		}
	}
	if len(fx.mailer.sentTo) != 2 {
		t.Fatalf("测试发送不走去重，应发出 2 封，实际 %d 封", len(fx.mailer.sentTo))
	}
}

func Test告警通道_未装配时返回明确提示(t *testing.T) {
	fx := newAlertChannelFixture(t)
	// 模拟"告警模块未装配"的部署形态
	fx.srv.deps.AlertChannels = nil

	rec, body := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/alert-channels", fx.adminTok, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未装配时应返回 404，实际 %d，body = %v", rec.Code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); !strings.Contains(code, "alert_channel") {
		t.Errorf("错误码 = %q，应能定位到告警通道模块", code)
	}
}

func Test告警通道_停用后不参与投递(t *testing.T) {
	fx := newAlertChannelFixture(t)
	id := seedAlertChannel(t, fx, &model.AlertChannel{
		Name: "临时关闭的群", Kind: model.AlertChannelEmail, Target: "off@example.com", Enabled: false,
	})

	// 列表里能看到（可以随时再启用），但 ListEnabled 不能把它算进去，
	// 否则真告警会发到已停用的通道。
	enabled, err := fx.repo.ListEnabled(context.Background())
	if err != nil {
		t.Fatalf("读取已启用通道失败: %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("已停用的通道不应出现在 ListEnabled 中，实际 %d 条", len(enabled))
	}
	if all, _, _ := fx.repo.List(context.Background()); len(all) != 1 {
		t.Fatalf("列表接口仍应返回停用通道（可再启用），实际 %d 条", len(all))
	}
	_ = id
}

func Test告警外发开关_默认关闭且可切换(t *testing.T) {
	fx := newAlertChannelFixture(t)

	// 默认：设置表里没有 alert_enabled → 必须为关闭。
	rec, body := doBearerJSON(t, fx.srv, http.MethodGet, "/api/admin/alert-settings", fx.adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body = %v", rec.Code, body)
	}
	if enabled, _ := body["enabled"].(bool); enabled {
		t.Fatalf("总开关默认应为关闭，实际 %v", body["enabled"])
	}
	if fx.srv.AlertEnabled(context.Background()) {
		t.Fatalf("AlertEnabled 默认应为 false")
	}

	// 显式打开
	rec, body = doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/alert-settings", fx.adminTok,
		`{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("切换状态码 = %d，期望 200，body = %v", rec.Code, body)
	}
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Fatalf("切换后应返回 enabled=true，实际 %v", body["enabled"])
	}
	// 切换后缓存应已失效：立即读取即为新值
	if !fx.srv.AlertEnabled(context.Background()) {
		t.Fatalf("切换后 AlertEnabled 应立即为 true（缓存需主动失效）")
	}

	// 再关闭
	rec, _ = doBearerJSON(t, fx.srv, http.MethodPost, "/api/admin/alert-settings", fx.adminTok,
		`{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭状态码 = %d，期望 200", rec.Code)
	}
	if fx.srv.AlertEnabled(context.Background()) {
		t.Fatalf("关闭后 AlertEnabled 应立即为 false")
	}
}
