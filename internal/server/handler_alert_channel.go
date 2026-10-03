// 本文件实现告警通道的后台管理接口，以及告警外发总开关的缓存读写。
//
// 意图（Why）：
//
//	告警通道是"会主动把站内数据发到站外"的配置，因此这里的每一个动作都比
//	普通设置项多一层考虑：
//	  1) 读：目标一律脱敏（钉钉/企微的 URL 自带 access_token）；
//	  2) 写/删/测试：都在管理后台分组之内（仅管理员，且经审计中间件记录）；
//	     能改告警目标的人，等于能让本站把内部事件发到他指定的任意地址；
//	  3) 测试：真的发一条消息出去，让站长在配错地址时立刻发现，
//	     而不是等到真出事时才发现告警根本没到；
//	  4) 总开关：外发默认关闭（alert_enabled 缺失或非 true 即关闭），
//	     站长在后台显式打开后才会真正投递。
//
// 流转（Flow）：
//
//	GET    /api/admin/alert-channels           列表（脱敏）
//	GET    /api/admin/alert-channel-kinds      通道类型与事件目录
//	POST   /api/admin/alert-channels           新建
//	PUT    /api/admin/alert-channels/:id       修改
//	DELETE /api/admin/alert-channels/:id       删除
//	POST   /api/admin/alert-channels/:id/test  发送测试告警
//	GET    /api/admin/alert-settings           读取总开关
//	POST   /api/admin/alert-settings           切换总开关（切换后失效缓存）
//
// 扩展（Extend）：
//
//	新增通道类型：model 加 kind、notify 加 sender，本文件不用动
//	（表单与下拉由前端从 alert-channel-kinds 拉取）。
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/notify"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
)

// 告警通道相关的错误码（前端据此定位是"配置问题"还是"通道问题"）。
const (
	alertChannelInvalidJSON = "alert_channel.invalid_json"
	alertChannelInvalidID   = "alert_channel.invalid_id"
	alertChannelCreateFail  = "alert_channel.create_failed"
	alertChannelUpdateFail  = "alert_channel.update_failed"
	alertChannelTestFail    = "alert_channel.test_failed"
	alertChannelBadPayload  = "alert_channel.invalid_payload"
	alertChannelUnsupported = "alert_channel.unsupported"
)

// 告警外发总开关相关的常量。
const (
	// alertEnabledSettingKey 是总开关在设置表中的键。
	// 语义：值等于 "true"（忽略大小写与首尾空白）才启用；键缺失或其它值一律视为关闭。
	alertEnabledSettingKey = "alert_enabled"
	// alertEnabledCacheKey 是总开关在 server 侧 TTL 缓存的键。
	alertEnabledCacheKey = "alert:enabled"
)

// alertTestTimeout 是"发送测试告警"的超时。
//
// 比常规投递（8 秒）宽一些：这里等的是人的反应，
// 目标站慢一点不代表配置错了，不该把"慢"报成"错"。
const alertTestTimeout = 15 * time.Second

// alertChannelRequest 是新建/修改的请求体。
type alertChannelRequest struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Events  string `json:"events"`
	Enabled *bool  `json:"enabled"`
}

// alertChannelDTO 是返回给后台的通道表示（目标已脱敏）。
type alertChannelDTO struct {
	ID           uint64   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	KindText     string   `json:"kind_text"`
	TargetMasked string   `json:"target_masked"`
	Events       []string `json:"events"`
	Enabled      bool     `json:"enabled"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

// alertChannelKindText 返回通道类型的中文名。
func alertChannelKindText(kind string) string {
	switch model.NormalizeAlertChannelKind(kind) {
	case model.AlertChannelEmail:
		return "邮件"
	case model.AlertChannelWebhook:
		return "Webhook"
	case model.AlertChannelDingTalk:
		return "钉钉群机器人"
	case model.AlertChannelWeCom:
		return "企业微信机器人"
	default:
		return kind
	}
}

// alertEventText 返回事件键的中文说明。
func alertEventText(event string) string {
	switch event {
	case model.EventChannelUnhealthy:
		return "渠道巡检判定不可用"
	case model.EventChannelRecovered:
		return "渠道从不可用恢复"
	case model.EventChannelAutoDisabled:
		return "渠道被自动停用"
	case model.EventLoginLocked:
		return "用户账号被临时锁定"
	case model.EventTest:
		return "测试消息"
	default:
		return event
	}
}

// toAlertChannelDTO 把领域对象转成脱敏后的返回结构。
func toAlertChannelDTO(c *model.AlertChannel) alertChannelDTO {
	events := make([]string, 0, 4)
	for _, item := range strings.Split(c.Events, ",") {
		if key := strings.TrimSpace(item); key != "" {
			events = append(events, key)
		}
	}
	return alertChannelDTO{
		ID:           c.ID,
		Name:         c.Name,
		Kind:         c.Kind,
		KindText:     alertChannelKindText(c.Kind),
		TargetMasked: c.MaskedTarget(),
		Events:       events,
		Enabled:      c.Enabled,
		CreatedAt:    c.CreatedAt.Unix(),
		UpdatedAt:    c.UpdatedAt.Unix(),
	}
}

// handleListAlertChannels 返回全部告警通道（目标脱敏）。
func (s *Server) handleListAlertChannels(c *gin.Context) {
	if !s.requireAlertChannels(c) {
		return
	}
	items, total, err := s.deps.AlertChannels.List(c.Request.Context())
	if err != nil {
		s.respondInternalError(c, "读取告警通道失败")
		return
	}
	list := make([]alertChannelDTO, 0, len(items))
	for _, item := range items {
		list = append(list, toAlertChannelDTO(item))
	}
	c.JSON(http.StatusOK, gin.H{"items": list, "total": total})
}

// handleAlertChannelKinds 返回可用的通道类型与事件目录，供前端渲染下拉。
//
// 刻意做成接口而不是前端硬编码：新增一种通道时前端无需跟着改版本，
// 漏改的结果是"后台多了一种类型但下拉里选不到"。
func (s *Server) handleAlertChannelKinds(c *gin.Context) {
	kinds := make([]gin.H, 0, 4)
	for _, k := range []string{
		model.AlertChannelEmail, model.AlertChannelWebhook,
		model.AlertChannelDingTalk, model.AlertChannelWeCom,
	} {
		kinds = append(kinds, gin.H{"value": k, "text": alertChannelKindText(k)})
	}
	all := model.AllAlertEvents()
	events := make([]gin.H, 0, len(all))
	for _, e := range all {
		events = append(events, gin.H{"value": e, "text": alertEventText(e)})
	}
	c.JSON(http.StatusOK, gin.H{"kinds": kinds, "events": events})
}

// handleCreateAlertChannel 新建一条告警通道。
func (s *Server) handleCreateAlertChannel(c *gin.Context) {
	if !s.requireAlertChannels(c) {
		return
	}
	item, ok := s.bindAlertChannel(c, nil)
	if !ok {
		return
	}
	if err := s.deps.AlertChannels.Create(c.Request.Context(), item); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, alertChannelCreateFail)
		return
	}
	c.JSON(http.StatusOK, toAlertChannelDTO(item))
}

// handleUpdateAlertChannel 修改一条告警通道。
func (s *Server) handleUpdateAlertChannel(c *gin.Context) {
	if !s.requireAlertChannels(c) {
		return
	}
	id, ok := alertChannelIDFrom(c)
	if !ok {
		return
	}
	// 先确认存在：否则会把"填错 ID"报成"校验失败"，两种问题的处置完全不同。
	existing, err := s.deps.AlertChannels.Get(c.Request.Context(), id)
	if err != nil {
		s.writeAlertChannelError(c, err)
		return
	}
	item, ok := s.bindAlertChannel(c, existing)
	if !ok {
		return
	}
	item.ID = id
	if err := s.deps.AlertChannels.Update(c.Request.Context(), item); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, alertChannelUpdateFail)
		return
	}
	c.JSON(http.StatusOK, toAlertChannelDTO(item))
}

// handleDeleteAlertChannel 删除一条告警通道。
func (s *Server) handleDeleteAlertChannel(c *gin.Context) {
	if !s.requireAlertChannels(c) {
		return
	}
	id, ok := alertChannelIDFrom(c)
	if !ok {
		return
	}
	if err := s.deps.AlertChannels.Delete(c.Request.Context(), id); err != nil {
		s.writeAlertChannelError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleTestAlertChannel 向指定通道发一条测试告警。
//
// 刻意同步返回而不是异步：这里的目的就是让站长当场确认地址可用，
// 异步发送会让"点了没反应"与"地址错了"两种情况无法区分。
func (s *Server) handleTestAlertChannel(c *gin.Context) {
	if !s.requireAlertChannels(c) {
		return
	}
	id, ok := alertChannelIDFrom(c)
	if !ok {
		return
	}
	item, err := s.deps.AlertChannels.Get(c.Request.Context(), id)
	if err != nil {
		s.writeAlertChannelError(c, err)
		return
	}
	if s.deps.Notifier == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "告警派发器未启用",
			oai.TypeServer, alertChannelUnsupported)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), alertTestTimeout)
	defer cancel()
	err = s.deps.Notifier.SendTo(ctx, item, notify.Alert{
		Key:      model.EventTest,
		Level:    notify.LevelInfo,
		Title:    "测试告警：如果你看到这条消息，说明告警通道配置正确",
		Detail:   "这是一条由管理员在后台手动触发的测试消息，可以安全忽略。",
		DedupKey: "test-" + strconv.FormatUint(item.ID, 10),
		Fields: []notify.Field{
			{Label: "通道名称", Value: item.Name},
			{Label: "通道类型", Value: alertChannelKindText(item.Kind)},
			{Label: "投递目标", Value: item.MaskedTarget()},
		},
	})
	if err != nil {
		// 各 sender 已把 URL 从错误里抹掉，这里把原因直接带回给管理员——
		// 告警通道配错是必须当场解决的问题，不该只留在服务端日志里。
		oai.WriteError(c.Writer, http.StatusBadGateway, "测试发送失败："+err.Error(),
			oai.TypeServer, alertChannelTestFail)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "测试告警已发送，请检查目标是否收到"})
}

// bindAlertChannel 解析并校验请求体，返回待保存的领域对象。
//
// 编辑时 target 留空表示"沿用原值"——目标在界面上是脱敏显示的，
// 站长无法把原样抄回来；若要求必填，等于每次改名都要重新粘贴一次带 token 的地址。
//
// events 则相反：它在界面上完全可见可编辑，留空就是"订阅全部"，
// 因此不做沿用（否则站长将无法清空订阅，只能去数据库改）。
func (s *Server) bindAlertChannel(c *gin.Context, existing *model.AlertChannel) (*model.AlertChannel, bool) {
	var req alertChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式不正确",
			oai.TypeInvalidRequest, alertChannelInvalidJSON)
		return nil, false
	}
	item := &model.AlertChannel{
		Name:    strings.TrimSpace(req.Name),
		Kind:    req.Kind,
		Target:  strings.TrimSpace(req.Target),
		Events:  req.Events,
		Enabled: true,
	}
	if existing != nil {
		*item = *existing
		item.Name = strings.TrimSpace(req.Name)
		if kind := model.NormalizeAlertChannelKind(req.Kind); kind != "" {
			item.Kind = kind
		}
		// 顺序要紧：先看请求里有没有新目标，没有才回落到原值。
		// 写成"先 *item = *existing 再判空"会让 item.Target 恒为原值，
		// 于是"换目标"这个最正常的操作被悄悄忽略。
		if target := strings.TrimSpace(req.Target); target != "" {
			item.Target = target
		} else {
			item.Target = existing.Target
		}
		item.Events = req.Events
	}
	if req.Enabled != nil {
		item.Enabled = *req.Enabled
	}
	if err := item.Validate(); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, alertChannelBadPayload)
		return nil, false
	}
	return item, true
}

// alertChannelIDFrom 解析路径里的通道 ID。
func alertChannelIDFrom(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest, "通道编号不合法",
			oai.TypeInvalidRequest, alertChannelInvalidID)
		return 0, false
	}
	return id, true
}

// requireAlertChannels 校验告警能力已装配。
func (s *Server) requireAlertChannels(c *gin.Context) bool {
	if s.deps.AlertChannels == nil {
		oai.WriteError(c.Writer, http.StatusNotFound, "告警功能未启用",
			oai.TypeServer, alertChannelUnsupported)
		return false
	}
	return true
}

// writeAlertChannelError 把仓储错误翻译成合适的响应。
func (s *Server) writeAlertChannelError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrAlertChannelNotFound) {
		oai.WriteError(c.Writer, http.StatusNotFound, "告警通道不存在",
			oai.TypeInvalidRequest, alertChannelInvalidID)
		return
	}
	s.respondInternalError(c, "告警通道操作失败")
}

// ── 告警外发总开关 ────────────────────────────────────────────────────────

// AlertEnabled 返回告警外发是否已开启（供 notify.Dispatcher 作为判定来源）。
//
// 导出是因为装配顺序：派发器要先于 server 构造并注入 Deps，
// 因此由 main 在 server 就绪后把它接到派发器上（见 cmd/ltzy/main.go）。
func (s *Server) AlertEnabled(ctx context.Context) bool {
	return s.alertEnabledCached(ctx)
}

// alertEnabledCached 返回总开关状态，优先命中进程内 TTL 缓存。
//
// 默认值（键不存在 / 非 true / 读库失败）一律为 false——外发是把站内事件送出站的能力，
// 任何不确定都应落到"关闭"，而不是"悄悄打开"。
func (s *Server) alertEnabledCached(ctx context.Context) bool {
	if s == nil || s.alertCache == nil {
		// 测试或非常规装配下的兜底：直接读一次。
		return s.loadAlertEnabled(ctx)
	}
	if cached, ok := s.alertCache.Get(alertEnabledCacheKey); ok {
		if enabled, ok := cached.(bool); ok {
			return enabled
		}
	}
	enabled := s.loadAlertEnabled(ctx)
	s.alertCache.Set(alertEnabledCacheKey, enabled)
	return enabled
}

// loadAlertEnabled 从设置表读取总开关。
//
// 语义：仅当 alert_enabled 的值（去空白、忽略大小写）等于 "true" 才视为启用。
func (s *Server) loadAlertEnabled(ctx context.Context) bool {
	repo := s.settingsRepo()
	if repo == nil {
		return false
	}
	values, err := repo.GetAll(ctx)
	if err != nil {
		slog.Warn("读取告警外发总开关失败，本次按关闭处理", "error", err)
		return false
	}
	return strings.EqualFold(strings.TrimSpace(values[alertEnabledSettingKey]), "true")
}

// invalidateAlertEnabledCache 主动失效总开关缓存，供切换后立即生效使用。
func (s *Server) invalidateAlertEnabledCache() {
	if s == nil || s.alertCache == nil {
		return
	}
	s.alertCache.Invalidate(alertEnabledCacheKey)
}

// handleGetAlertSettings 返回告警外发总开关的当前状态。
func (s *Server) handleGetAlertSettings(c *gin.Context) {
	if s.deps.Settings == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "设置模块未启用",
			oai.TypeServer, oai.CodeInternal)
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": s.alertEnabledCached(c.Request.Context())})
}

// handleUpdateAlertSettings 切换告警外发总开关。
//
// 保存后主动失效缓存，让改动对下一次投递判定立即生效（不必重启、也不必等 TTL）。
func (s *Server) handleUpdateAlertSettings(c *gin.Context) {
	if s.deps.Settings == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "设置模块未启用",
			oai.TypeServer, oai.CodeInternal)
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体需包含 enabled 布尔值",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	value := "false"
	if *req.Enabled {
		value = "true"
	}
	if err := s.deps.Settings.Set(c.Request.Context(), alertEnabledSettingKey, value); err != nil {
		s.respondInternalError(c, "保存告警外发总开关失败")
		return
	}
	s.invalidateAlertEnabledCache()
	c.JSON(http.StatusOK, gin.H{"enabled": *req.Enabled})
}
