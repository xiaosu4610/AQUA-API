// 本文件实现各类告警通道的投递器，以及告警正文的渲染。
//
// 意图（Why）：
//
//	四种通道的差别只在"请求体长什么样"，投递的共性是：
//	都必须有独立超时、都必须拒绝内网地址、都必须限制响应体大小、
//	都必须把目标地址从错误信息里抹掉。把这些共性收在一处，
//	新增通道类型时只需关心"我的 JSON 长什么样"。
//
// 流转（Flow）：
//
//	Dispatcher.send → sender.Send(target, alert)
//	  → preflightHostGuard（解析 host 并做 netguard 预检）
//	  → buildBody（按通道类型拼请求体）
//	  → doRequest（超时 + netguard 拨号护栏 + 响应体上限）
//	  → 按 2xx 判成功
//
// 扩展（Extend）：
//
//	接飞书 / Telegram / Server 酱：加一个 buildBody 分支与常量，
//	并在 model 认下新的 kind；不需要动 HTTP 客户端与护栏。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/netguard"
)

// 投递相关常量。
const (
	// maxResponseBytes 是响应体读取上限。
	//
	// 为什么必须限制：目标站若返回超大页面，会把内存吃满；
	// 而我们只需要知道"成功没有"，读不读内容都一样。
	maxResponseBytes = 64 << 10
	// requestTimeout 是单次 HTTP 投递的超时。
	requestTimeout = 8 * time.Second
)

// 投递失败的原因（用哨兵错误而非字符串，便于测试断言与上层判断）。
var (
	errUnsupportedChannel = func(kind string) error {
		return fmt.Errorf("notify: 暂不支持的告警通道类型 %s", kind)
	}
	errMailerUnavailable = errors.New("notify: 邮件通道不可用（本站 SMTP 未配置）")
	// errBlockedTarget 是目标地址命中 netguard 黑名单时的错误。
	// 刻意不含任何地址信息——预检拿到的地址仍可能带 token（如查询串），不得回显。
	errBlockedTarget = errors.New("notify: 目标地址属于禁止访问的网段（链路本地/云元数据）")
)

// Sender 投递一条告警到指定目标。
type Sender interface {
	Send(ctx context.Context, target string, alert Alert) error
}

// HTTPSender 是三类 Webhook 通道的共同实现。
//
// 为什么不做成"一个 Sender 带 kind 参数"：那会让调用方可以构造出
// "用 webhook 的方式发钉钉消息体"这种组合，而这种组合在运行时才会失败。
type HTTPSender struct {
	kind string
	// client 带 netguard 护栏：Webhook 目标是站长填的，
	// 但它仍然是一个由外部输入决定的 URL，必须挡住内网与云元数据地址。
	client *http.Client
}

// NewHTTPSender 创建一个 Webhook 类通道投递器。
func NewHTTPSender(kind string) *HTTPSender {
	return &HTTPSender{
		kind: kind,
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
					Control:   netguard.DialControl,
				}).DialContext,
				MaxIdleConns:          4,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		},
	}
}

// Send 投递一条告警。
func (h *HTTPSender) Send(ctx context.Context, target string, alert Alert) error {
	// 预检：先解析 host 做一次地址判定，命中黑名单直接拒绝。
	// 与 Transport 的 DialControl 是两道防线——前者给出明确的"被拦截"语义，
	// 后者在真正建连（已解析 IP）时兜底，挡住 DNS rebinding 之类的绕过。
	if err := h.preflightHostGuard(ctx, target); err != nil {
		return err
	}
	body, err := h.buildBody(alert)
	if err != nil {
		return err
	}
	return h.doRequest(ctx, target, body)
}

// preflightHostGuard 在发起请求前解析目标 host 并做一次 netguard 判定。
//
// 为什么要在 DialControl 之外再做一次：DialControl 只在【建连时】生效，
// 拿到的地址是黑名单命中时会得到一个底层拨号错误；而这里提前判定可以给出
// 更明确、且不带任何地址信息的错误（避免把带 token 的原始 URL 写进日志）。
//
// 取舍：只处理 http/https。其它协议（会被 http.Client 拒绝）与解析失败的域名
// 一律放行，交给后续的 NewRequest/Do 去报错——在这里臆造错误反而会掩盖真实原因。
func (h *HTTPSender) preflightHostGuard(ctx context.Context, target string) error {
	parsed, err := url.Parse(target)
	if err != nil {
		return nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil
	}
	host := parsed.Hostname()
	if host == "" {
		return nil
	}
	// 字面 IP：直接判定，无需解析。
	if ip := net.ParseIP(host); ip != nil {
		if netguard.IsBlocked(ip) {
			return errBlockedTarget
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		// 解析失败保留给真正的 Do 去报错（保留原始原因，避免误导）。
		return nil
	}
	for _, addr := range ips {
		if netguard.IsBlocked(addr.IP) {
			return errBlockedTarget
		}
	}
	return nil
}

// buildBody 按通道类型拼请求体。
func (h *HTTPSender) buildBody(alert Alert) ([]byte, error) {
	switch h.kind {
	case model.AlertChannelDingTalk:
		// 钉钉自定义机器人：text 类型最稳（markdown 需要机器人开启权限）
		return json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": RenderPlainText(alert)},
		})
	case model.AlertChannelWeCom:
		// 企业微信群机器人：markdown 类型支持加粗与彩色，最适合分级告警
		return json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": RenderMarkdown(alert)},
		})
	default:
		// 通用 Webhook：把结构原样送出，方便站长在自己的系统里做路由
		return json.Marshal(map[string]any{
			"source":  "ltzy-api",
			"event":   alert.Key,
			"level":   string(alert.Level),
			"title":   alert.Title,
			"detail":  alert.Detail,
			"fields":  fieldsToMap(alert.Fields),
			"at":      alert.At.Unix(),
			"summary": RenderPlainText(alert),
		})
	}
}

// doRequest 发出 POST 并判成功。
func (h *HTTPSender) doRequest(ctx context.Context, target string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		// URL 本身可能含 token，因此不回显它——只说"地址不合法"。
		return errors.New("notify: 告警地址不合法")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "LTZY-API-Alerts/1")

	resp, err := h.client.Do(req)
	if err != nil {
		return sanitizeTransportError(err, target)
	}
	defer func() { _ = resp.Body.Close() }()

	// 读一点就够判断成功与否；上限防止目标站用超大响应撑爆内存。
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 响应体只截前 200 字符：足够判断"密钥无效"这类可读错误，
		// 又不会把对方的整个错误页塞进我们的日志。
		return fmt.Errorf("notify: 目标返回 %d：%s", resp.StatusCode, snippet(payload))
	}
	return nil
}

// sanitizeTransportError 把传输层错误里的目标地址抹掉后再返回。
//
// 为什么必须这一步：net/http 的 *url.Error 在 Error() 里带上了完整 URL，
// 而钉钉/企微的 URL 自带 access_token。直接把 err 透传出去，
// 等于把这个凭据写进管理员的浏览器、告警日志和任何一次截图里。
//
// 保留的是 url.Error.Err（真正的原因：DNS 失败、连接被拒、超时……），
// 丢掉的是 URL 与操作名——它们对排查"哪个通道坏了"没有帮助，
// 因为调用点本来就知道自己发的是哪个通道。
func sanitizeTransportError(err error, target string) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		msg := urlErr.Err.Error()
		// 双保险：个别错误包装里可能再带一次地址（重定向、代理返回的错误页等）
		if target != "" {
			msg = strings.ReplaceAll(msg, target, targetLabel(target))
		}
		return errors.New("notify: 投递失败：" + msg)
	}
	return errors.New("notify: 投递失败：连接异常")
}

// targetLabel 生成可用于日志的地址标签（只保留 host，丢掉路径与查询串）。
func targetLabel(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return "[告警地址]"
	}
	return u.Host
}

// snippet 截取并压平一段文本用于错误信息。
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 200 {
		return string([]rune(s)[:200]) + "…"
	}
	if s == "" {
		return "（无响应体）"
	}
	return s
}

// fieldsToMap 把键值对转成 map，便于通用 Webhook 消费方直接取用。
func fieldsToMap(fields []Field) map[string]string {
	if len(fields) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[f.Label] = f.Value
	}
	return out
}

// ── 正文渲染 ──────────────────────────────────────────────────────────────

// levelLabel 是各级别在群消息里显示的标记。
func levelLabel(level Level) string {
	switch level {
	case LevelCritical:
		return "【严重】"
	case LevelWarning:
		return "【警告】"
	default:
		return "【通知】"
	}
}

// RenderPlainText 渲染纯文本正文（钉钉 text 与邮件摘要共用）。
func RenderPlainText(alert Alert) string {
	var sb strings.Builder
	sb.WriteString(levelLabel(alert.Level))
	sb.WriteString(alert.Title)
	if alert.Detail != "" {
		sb.WriteString("\n")
		sb.WriteString(alert.Detail)
	}
	for _, f := range alert.Fields {
		sb.WriteString("\n")
		sb.WriteString(f.Label)
		sb.WriteString("：")
		sb.WriteString(f.Value)
	}
	sb.WriteString("\n时间：")
	sb.WriteString(alert.At.Format("2006-01-02 15:04:05"))
	return sb.String()
}

// RenderMarkdown 渲染企业微信 markdown 正文。
func RenderMarkdown(alert Alert) string {
	var sb strings.Builder
	sb.WriteString("### ")
	sb.WriteString(levelLabel(alert.Level))
	sb.WriteString(alert.Title)
	sb.WriteString("\n")
	if alert.Detail != "" {
		sb.WriteString(alert.Detail)
		sb.WriteString("\n")
	}
	for _, f := range alert.Fields {
		sb.WriteString("> ")
		sb.WriteString(f.Label)
		sb.WriteString("：")
		sb.WriteString(f.Value)
		sb.WriteString("\n")
	}
	sb.WriteString("> 时间：")
	sb.WriteString(alert.At.Format("2006-01-02 15:04:05"))
	return sb.String()
}

// RenderEmail 渲染告警邮件 HTML。
//
// 为什么单独渲染而不是复用纯文本：邮件里字段是表格、天然可读；
// 而群消息要迁就各家机器人的格式限制。所有用户输入一律 HTML 转义——
// 渠道名是站长自己填的，但"渠道名"也可能来自上游返回，
// 不转义就等于让上游内容在邮件客户端里执行脚本。
func RenderEmail(alert Alert) string {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html><html><body style="margin:0;padding:24px;` +
		`background:#f4f6f9;font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;">`)
	sb.WriteString(`<div style="max-width:600px;margin:0 auto;background:#fff;` +
		`border:1px solid #e3e8f0;border-radius:10px;overflow:hidden;">`)

	accent := map[Level]string{
		LevelInfo:     "#1d4ed8",
		LevelWarning:  "#b45309",
		LevelCritical: "#dc2626",
	}[alert.Level]
	if accent == "" {
		accent = "#1d4ed8"
	}

	sb.WriteString(`<div style="height:4px;background:` + accent + `;"></div>`)
	sb.WriteString(`<div style="padding:20px 24px;">`)
	sb.WriteString(`<div style="font-size:12px;color:#8a97ad;letter-spacing:.5px;">LTZY-API 告警</div>`)
	sb.WriteString(`<h1 style="margin:8px 0 4px;font-size:18px;color:#0b1220;">`)
	sb.WriteString(html.EscapeString(levelLabel(alert.Level) + alert.Title))
	sb.WriteString(`</h1>`)
	sb.WriteString(`<div style="font-size:13px;color:#8a97ad;">` +
		html.EscapeString(alert.At.Format("2006-01-02 15:04:05")) + `</div>`)

	if alert.Detail != "" {
		sb.WriteString(`<p style="margin:16px 0 0;font-size:14px;color:#47536a;line-height:1.7;">`)
		sb.WriteString(html.EscapeString(alert.Detail))
		sb.WriteString(`</p>`)
	}

	if len(alert.Fields) > 0 {
		sb.WriteString(`<table style="width:100%;margin-top:16px;border-collapse:collapse;font-size:13px;">`)
		for _, f := range alert.Fields {
			sb.WriteString(`<tr>`)
			sb.WriteString(`<td style="padding:8px 0;color:#8a97ad;width:96px;vertical-align:top;">` +
				html.EscapeString(f.Label) + `</td>`)
			sb.WriteString(`<td style="padding:8px 0;color:#0b1220;">` +
				html.EscapeString(f.Value) + `</td>`)
			sb.WriteString(`</tr>`)
		}
		sb.WriteString(`</table>`)
	}

	sb.WriteString(`<p style="margin:20px 0 0;font-size:12px;color:#8a97ad;">` +
		`这条告警由 LTZY-API 自动发出，请登录后台查看详情。</p>`)
	sb.WriteString(`</div></div></body></html>`)
	return sb.String()
}
