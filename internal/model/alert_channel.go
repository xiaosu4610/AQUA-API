// 本文件定义「告警通道」领域模型：投递目标、事件订阅与校验规则。
//
// 意图（Why）：
//
//	渠道熔断、成功率自动停用、登录锁定这些事件若只打到 stdout，等于"站点出事了
//	只有主动翻日志才知道"。告警外发需要一份"把哪类事件发到哪儿"的配置，
//	本文件就是这份配置的领域模型与规则来源：
//	  1) 统一的通道类型与事件键常量（拼错在编译期暴露）；
//	  2) 校验规则（写入口把关，拒绝"配置成功但永远不发"的静默失效）；
//	  3) 脱敏规则（钉钉/企微 URL 自带 access_token，任何回显都视为泄露）。
//
// 流转（Flow）：
//
//	后台表单 → model.AlertChannel.Validate() → store 落库（alert_channels 表）
//	发送时 → store.ListEnabled() → notify.Dispatcher 按 events 过滤与投递
//
// 扩展（Extend）：
//
//	新增通道类型：加 AlertChannelXxx 常量 + 在 AlertChannelKindSupported 与
//	NormalizeAlertChannelKind 认下该取值 + notify 加一个 sender；
//	新增事件：加 EventXxx 常量 + 在 AllAlertEvents 登记（订阅下拉据此渲染）。
package model

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// ErrAlertChannelNotFound 表示告警通道不存在。
var ErrAlertChannelNotFound = errors.New("model: 告警通道不存在")

// 告警通道类型（投递目标形态）。
const (
	// AlertChannelEmail 通过本站 SMTP 发到指定邮箱。
	AlertChannelEmail = "email"
	// AlertChannelWebhook 通用 Webhook：POST 一段 JSON，字段自定。
	AlertChannelWebhook = "webhook"
	// AlertChannelDingTalk 钉钉群机器人（自带 Markdown 消息体）。
	AlertChannelDingTalk = "dingtalk"
	// AlertChannelWeCom 企业微信群机器人（自带 Markdown 消息体）。
	AlertChannelWeCom = "wecom"
)

// 告警事件键。
//
// 为什么用常量而不是到处写字符串：事件键同时出现在"触发点"与"后台订阅配置"两侧，
// 一旦拼错，订阅会静默失效——站长以为订了渠道告警，实际一条都收不到。
// 集中成常量后，拼错会在编译期暴露。
const (
	// EventChannelUnhealthy 渠道巡检判定为不可用。
	EventChannelUnhealthy = "channel.unhealthy"
	// EventChannelRecovered 渠道巡检从不可用恢复。
	EventChannelRecovered = "channel.recovered"
	// EventChannelAutoDisabled 渠道因成功率过低被自动停用。
	EventChannelAutoDisabled = "channel.auto_disabled"
	// EventLoginLocked 用户登录失败次数达标，账号被临时锁定。
	EventLoginLocked = "login.locked"
	// EventTest 管理员点「发送测试告警」时发出，不对应任何真实故障。
	EventTest = "system.test"
)

// AllAlertEvents 返回全部事件键（供后台下拉与"订阅全部"判定）。
func AllAlertEvents() []string {
	return []string{
		EventChannelUnhealthy,
		EventChannelRecovered,
		EventChannelAutoDisabled,
		EventLoginLocked,
		EventTest,
	}
}

// AlertChannelKindSupported 判断通道类型是否受支持。
func AlertChannelKindSupported(kind string) bool {
	switch NormalizeAlertChannelKind(kind) {
	case AlertChannelEmail, AlertChannelWebhook, AlertChannelDingTalk, AlertChannelWeCom:
		return true
	default:
		return false
	}
}

// NormalizeAlertChannelKind 归一通道类型。
//
// 空串返回空串（不猜默认值）：把"没填类型"默默变成某个具体通道，
// 会让站长以为自己配的是 Webhook、实际发去了钉钉。
func NormalizeAlertChannelKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

// AlertChannel 是一条告警投递配置。
type AlertChannel struct {
	ID        uint64
	Name      string // 展示名（后台列表与告警正文里都出现它）
	Kind      string // email / webhook / dingtalk / wecom
	Target    string // 邮箱或 URL（含 token，按密文对待）
	Events    string // 订阅的事件键，逗号分隔；空 = 全部
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SubscribedTo 判断该通道是否订阅了某个事件。
//
// 语义：Events 为空表示"订阅全部"。这一点必须写清——
// 若把空当成"不订阅任何"，新装的站点会表现为"配了通道但一条告警都收不到"，
// 而这种故障表现为"功能没生效"，极难被联想到是空值语义搞反了。
func (c *AlertChannel) SubscribedTo(event string) bool {
	if c == nil || !c.Enabled {
		return false
	}
	list := strings.TrimSpace(c.Events)
	if list == "" {
		return true
	}
	for _, item := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(item), event) {
			return true
		}
	}
	return false
}

// Validate 校验通道配置。
func (c *AlertChannel) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("请填写通道名称")
	}
	kind := NormalizeAlertChannelKind(c.Kind)
	if !AlertChannelKindSupported(kind) {
		return errors.New("不支持的告警通道类型：" + kind)
	}
	c.Kind = kind

	target := strings.TrimSpace(c.Target)
	if target == "" {
		return errors.New("请填写投递目标")
	}
	if kind == AlertChannelEmail {
		if !strings.Contains(target, "@") || strings.HasPrefix(target, "@") || strings.HasSuffix(target, "@") {
			return errors.New("邮箱地址格式不正确")
		}
	} else if !strings.HasPrefix(target, "https://") && !strings.HasPrefix(target, "http://") {
		return errors.New("Webhook 地址必须以 http:// 或 https:// 开头")
	}
	c.Target = target

	// 事件键逐个校验：存下拼错的事件键不会报错，只会让那条告警永远不发，
	// 属于"配置看起来成功、实际没生效"的静默失效，必须在入口就拦住。
	normalized := make([]string, 0, 4)
	for _, item := range strings.Split(c.Events, ",") {
		key := strings.TrimSpace(item)
		if key == "" {
			continue
		}
		if !alertEventKnown(key) {
			return errors.New("存在不认识的事件类型：" + key)
		}
		normalized = append(normalized, key)
	}
	c.Events = strings.Join(normalized, ",")
	return nil
}

// alertEventKnown 判断事件键是否在目录内。
func alertEventKnown(event string) bool {
	for _, known := range AllAlertEvents() {
		if known == event {
			return true
		}
	}
	return false
}

// MaskedTarget 返回可安全展示的投递目标。
//
// 钉钉/企微的 URL 里带 access_token，直接回显等于把机器人凭据发到浏览器——
// 后台列表、审计日志、任何一次截图都可能成为泄露途径。
func (c *AlertChannel) MaskedTarget() string {
	if c == nil {
		return ""
	}
	if c.Kind == AlertChannelEmail {
		return maskEmailTarget(c.Target)
	}
	return maskURLTarget(c.Target)
}

// maskEmailTarget 掩码邮箱：保留首字符与域名，隐藏中间。
//
// 为什么要掩码而不是干脆不返回：站长需要在列表里确认"这封信发到了哪个地址"，
// 完全隐藏就得逐条点开详情，配置多时无法核对。
func maskEmailTarget(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at <= 0 {
		return "***"
	}
	local, domain := addr[:at], addr[at:]
	runes := []rune(local)
	if len(runes) <= 1 {
		return "***" + domain
	}
	return string(runes[0]) + "***" + domain
}

// maskURLTarget 掩码 URL：保留协议、主机与路径，隐藏查询串与片段。
//
// 钉钉/企微的 token 在查询串里，通用 Webhook 也常把签名放在 fragment，
// 因此这两处一律整体隐藏，而不是"只隐藏部分字符"——
// 部分隐藏对 token 而言等于没隐藏（前缀泄露即可拼接口调用）。
func maskURLTarget(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "（地址格式异常）"
	}
	host := parsed.Host
	if host == "" {
		host = "（地址格式异常）"
	}
	path := parsed.Path
	// 路径里也可能带 token（如 Slack 风格 /services/xxx），故只保留末段。
	if idx := strings.LastIndex(strings.Trim(path, "/"), "/"); idx >= 0 {
		path = "…/" + strings.Trim(path, "/")[idx+1:]
	} else if path != "" {
		path = "/…"
	}
	return parsed.Scheme + "://" + host + path + "（凭据已隐藏）"
}

// AlertChannelRepository 是告警通道配置的持久化接口。
type AlertChannelRepository interface {
	// Create 写入一条通道配置。
	Create(ctx context.Context, c *AlertChannel) error

	// Update 按 ID 覆盖一条通道配置。
	// 为什么不给"部分更新"：这类配置字段少且后台表单是一次性提交的，
	// 部分更新会让"清空某个字段"变得难以表达。
	Update(ctx context.Context, c *AlertChannel) error

	// Delete 按 ID 删除；不存在时返回 ErrAlertChannelNotFound。
	Delete(ctx context.Context, id uint64) error

	// Get 按 ID 读取单条。
	Get(ctx context.Context, id uint64) (*AlertChannel, error)

	// ListEnabled 返回全部【已启用】的通道（发送时用）。
	//
	// 与 List 分开而不是复用加过滤：发送路径在热路径之外但要求极简，
	// 少一次筛选条件就少一处"忘了加 enabled 判断"的漏发/误发风险。
	ListEnabled(ctx context.Context) ([]*AlertChannel, error)

	// List 返回全部通道（后台列表用），同时返回总数。
	List(ctx context.Context) ([]*AlertChannel, int, error)
}
