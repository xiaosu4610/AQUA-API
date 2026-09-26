// 本文件定义对外响应的数据结构（DTO）与领域模型的转换函数。
//
// 意图（Why）：
//
//	领域模型与对外接口是两个不同的关注点，必须分开：
//	  1) 安全：领域模型里有口令哈希、渠道明文密钥、令牌明文等敏感字段，
//	     若直接把模型序列化为 JSON，一次疏忽就会把密钥泄露给前端；
//	  2) 稳定：接口字段是契约（前端依赖它），不应随内部重构而被动变化。
//	因此所有响应都经过显式转换，且敏感字段在此处统一脱敏。
//
// 流转（Flow）：
//
//	handler → toXxxDTO(领域模型) → JSON 响应
//
// 扩展（Extend）：
//
//	新增接口字段时：在此结构体补充字段与转换逻辑，
//	并在 docs/06-前后端接口契约.md 同步更新（契约与实现必须一致）。
//	新增敏感字段时：务必只输出脱敏形式（如 xxx_masked），绝不输出原文。
package server

import (
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// pagedResponse 是列表接口的统一响应结构。
//
// 使用泛型而非 interface{}：让每个列表接口的 items 类型在编译期确定，
// 避免前端拿到结构不一致的数据。
type pagedResponse[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Size  int `json:"size"`
}

// newPagedResponse 构造分页响应。items 为 nil 时替换为空切片，
// 保证 JSON 中是 [] 而不是 null（前端可直接 .map，无需判空）。
func newPagedResponse[T any](items []T, total, page, size int) pagedResponse[T] {
	if items == nil {
		items = make([]T, 0)
	}
	return pagedResponse[T]{Items: items, Total: total, Page: page, Size: size}
}

// unixOrZero 把时间转为 Unix 秒；零值时间返回 0。
//
// 约定：接口层统一用 0 表示"未设置/永不"（如永不过期、从未测活），
// 避免前端处理 Go 零值时间那个奇怪的公元 1 年时间戳。
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// ---------------------------------------------------------------------------
// 用户
// ---------------------------------------------------------------------------

// userDTO 是对外的用户信息。
//
// 安全约束：绝不包含 password_hash。
type userDTO struct {
	ID             uint64 `json:"id"`
	Username       string `json:"username"`
	Email          string `json:"email"`
	Role           int    `json:"role"`
	RoleText       string `json:"role_text"`
	Status         int    `json:"status"`
	StatusText     string `json:"status_text"`
	Quota          int64  `json:"quota"`
	UsedQuota      int64  `json:"used_quota"`
	RemainingQuota int64  `json:"remaining_quota"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// toUserDTO 把用户模型转为对外 DTO。
func toUserDTO(u *model.User) userDTO {
	if u == nil {
		return userDTO{}
	}
	return userDTO{
		ID:             u.ID,
		Username:       u.Username,
		Email:          u.Email,
		Role:           int(u.Role),
		RoleText:       u.Role.String(),
		Status:         int(u.Status),
		StatusText:     u.Status.String(),
		Quota:          u.Quota,
		UsedQuota:      u.UsedQuota,
		RemainingQuota: u.RemainingQuota(),
		CreatedAt:      unixOrZero(u.CreatedAt),
		UpdatedAt:      unixOrZero(u.UpdatedAt),
	}
}

// ---------------------------------------------------------------------------
// 渠道
// ---------------------------------------------------------------------------

// channelDTO 是对外的渠道信息。
//
// 安全约束：只输出掩码后的密钥（masked_key），绝不输出明文。
type channelDTO struct {
	ID         uint64   `json:"id"`
	Name       string   `json:"name"`
	Type       int      `json:"type"`
	BaseURL    string   `json:"base_url"`
	MaskedKey  string   `json:"masked_key"`
	Models     []string `json:"models"`
	Group      string   `json:"group"`
	Priority   int      `json:"priority"`
	Weight     int      `json:"weight"`
	Status     int      `json:"status"`
	StatusText string   `json:"status_text"`
	LastTestAt int64    `json:"last_test_at"`
	LastTestOK bool     `json:"last_test_ok"`
	CreatedAt  int64    `json:"created_at"`
	UpdatedAt  int64    `json:"updated_at"`
	// KeyPool 是密钥池概览（渠道可挂多把上游密钥并轮询使用）。
	// 零值表示该渠道没有配置密钥池，走"单密钥"模式。
	KeyPool keyPoolDTO `json:"key_pool"`
}

// keyPoolDTO 是密钥池的概览统计。
//
// 只给计数不给明细：一个渠道可能挂 500 把密钥，
// 若列表接口把每把密钥都带上，页面数据量会大到不可用。
// 需要明细时再单独请求该渠道的密钥列表。
type keyPoolDTO struct {
	Total       int `json:"total"`
	Enabled     int `json:"enabled"`
	Disabled    int `json:"disabled"`
	AutoRemoved int `json:"auto_removed"`
}

// channelKeyDTO 是凭据池中单条凭据的对外表示。
//
// 安全约束：只输出掩码（masked_key），明文永不返回给前端。
type channelKeyDTO struct {
	ID         uint64 `json:"id"`
	Kind       string `json:"kind"`
	KindText   string `json:"kind_text"`
	Label      string `json:"label"`
	MaskedKey  string `json:"masked_key"`
	Status     int    `json:"status"`
	StatusText string `json:"status_text"`
	FailCount  int    `json:"fail_count"`
	LastUsedAt int64  `json:"last_used_at"`
	LastError  string `json:"last_error"`
	CreatedAt  int64  `json:"created_at"`
}

// toChannelKeyDTO 把凭据模型转为对外 DTO。
func toChannelKeyDTO(key *model.ChannelKey) channelKeyDTO {
	if key == nil {
		return channelKeyDTO{}
	}
	kindText := "API Key"
	if key.IsOAuth() {
		kindText = "订阅账号"
	}
	return channelKeyDTO{
		ID:         key.ID,
		Kind:       string(key.Kind),
		KindText:   kindText,
		Label:      key.Label,
		MaskedKey:  key.Masked(),
		Status:     int(key.Status),
		StatusText: key.Status.String(),
		FailCount:  key.FailCount,
		LastUsedAt: unixOrZero(key.LastUsedAt),
		LastError:  key.LastError,
		CreatedAt:  unixOrZero(key.CreatedAt),
	}
}

// toChannelKeyDTOList 批量转换密钥。
func toChannelKeyDTOList(keys []*model.ChannelKey) []channelKeyDTO {
	result := make([]channelKeyDTO, 0, len(keys))
	for _, key := range keys {
		result = append(result, toChannelKeyDTO(key))
	}
	return result
}

// applyKeyPool 把密钥池概览填充到渠道 DTO 上。
func applyKeyPool(dto *channelDTO, summary model.KeyPoolSummary) {
	dto.KeyPool = keyPoolDTO{
		Total:       summary.Total,
		Enabled:     summary.Enabled,
		Disabled:    summary.Disabled,
		AutoRemoved: summary.AutoRemoved,
	}
}

// toChannelDTO 把渠道模型转为对外 DTO。
func toChannelDTO(ch *model.Channel) channelDTO {
	if ch == nil {
		return channelDTO{}
	}
	models := ch.Models
	if models == nil {
		models = make([]string, 0)
	}
	return channelDTO{
		ID:         ch.ID,
		Name:       ch.Name,
		Type:       ch.Type,
		BaseURL:    ch.BaseURL,
		MaskedKey:  ch.MaskedAPIKey(),
		Models:     models,
		Group:      ch.Group,
		Priority:   ch.Priority,
		Weight:     ch.Weight,
		Status:     int(ch.Status),
		StatusText: ch.Status.String(),
		LastTestAt: unixOrZero(ch.LastTestAt),
		LastTestOK: ch.LastTestOK,
		CreatedAt:  unixOrZero(ch.CreatedAt),
		UpdatedAt:  unixOrZero(ch.UpdatedAt),
	}
}

// toChannelDTOList 批量转换渠道。
func toChannelDTOList(channels []*model.Channel) []channelDTO {
	result := make([]channelDTO, 0, len(channels))
	for _, ch := range channels {
		result = append(result, toChannelDTO(ch))
	}
	return result
}

// ---------------------------------------------------------------------------
// 访问令牌
// ---------------------------------------------------------------------------

// tokenDTO 是对外的访问令牌信息。
//
// 安全约束：只输出 masked_key；明文仅在创建时通过一次性字段返回。
type tokenDTO struct {
	ID             uint64   `json:"id"`
	Name           string   `json:"name"`
	MaskedKey      string   `json:"masked_key"`
	Status         int      `json:"status"`
	StatusText     string   `json:"status_text"`
	ExpiresAt      int64    `json:"expires_at"`
	RemainQuota    int64    `json:"remain_quota"`
	UnlimitedQuota bool     `json:"unlimited_quota"`
	UsedQuota      int64    `json:"used_quota"`
	Models         []string `json:"models"`
	CreatedAt      int64    `json:"created_at"`
	UpdatedAt      int64    `json:"updated_at"`
	LastUsedAt     int64    `json:"last_used_at"`

	// 以下字段用于管理端展示归属信息（用户门户中为空，前端会自动忽略）。
	UserID   uint64 `json:"user_id"`
	Username string `json:"username"`

	// Key 是令牌明文，【仅创建时】填充一次，其余场景因 omitempty 而不会出现在响应中。
	//
	// 设计说明：数据库只保存摘要与密文，明文无法二次取回；
	// 因此这是使用者唯一一次看到明文的机会，前端必须显著提示立即保存。
	Key string `json:"key,omitempty"`
}

// toTokenDTO 把令牌模型转为对外 DTO。
//
// 参数 status 使用"结合时间与额度后的实际状态"，因为对使用者而言
// "已过期"比数据库里记录的"启用"更有意义。
func toTokenDTO(t *model.Token, status model.TokenStatus) tokenDTO {
	if t == nil {
		return tokenDTO{}
	}
	models := t.Models
	if models == nil {
		models = make([]string, 0)
	}
	return tokenDTO{
		ID:             t.ID,
		Name:           t.Name,
		MaskedKey:      t.MaskedKey(),
		Status:         int(status),
		StatusText:     status.String(),
		ExpiresAt:      unixOrZero(t.ExpiresAt),
		RemainQuota:    t.RemainQuota,
		UnlimitedQuota: t.UnlimitedQuota,
		UsedQuota:      t.UsedQuota,
		Models:         models,
		CreatedAt:      unixOrZero(t.CreatedAt),
		UpdatedAt:      unixOrZero(t.UpdatedAt),
		LastUsedAt:     unixOrZero(t.LastUsedAt),
		UserID:         t.OwnerID,
	}
}

// ---------------------------------------------------------------------------
// 调用日志
// ---------------------------------------------------------------------------

// usageLogDTO 是对外的调用日志信息。
type usageLogDTO struct {
	ID               uint64 `json:"id"`
	UserID           uint64 `json:"user_id"`
	Username         string `json:"username"`
	TokenName        string `json:"token_name"`
	ChannelID        uint64 `json:"channel_id"`
	ChannelName      string `json:"channel_name"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	Quota            int64  `json:"quota"`
	LatencyMS        int    `json:"latency_ms"`
	IsStream         bool   `json:"is_stream"`
	StatusCode       int    `json:"status_code"`
	Error            string `json:"error"`
	CreatedAt        int64  `json:"created_at"`
}

// toUsageLogDTO 把日志模型转为对外 DTO。
//
// 参数 usernames/channelNames 为"ID → 名称"映射，由调用方批量预取。
//
// 为什么用映射而非在日志表里存名称：名称会变（渠道改名），
// 日志应记录当时的 ID 事实；展示时的名称解析放到查询侧。
// 批量预取而非逐条查询，是为了避免 N+1 查询（一页 20 条就是 40 次额外查询）。
func toUsageLogDTO(log *model.UsageLog, usernames map[uint64]string, channelNames map[uint64]string) usageLogDTO {
	if log == nil {
		return usageLogDTO{}
	}
	return usageLogDTO{
		ID:               log.ID,
		UserID:           log.UserID,
		Username:         usernames[log.UserID],
		ChannelID:        log.ChannelID,
		ChannelName:      channelNames[log.ChannelID],
		Model:            log.Model,
		PromptTokens:     log.PromptTokens,
		CompletionTokens: log.CompletionTokens,
		TotalTokens:      log.TotalTokens,
		Quota:            log.Quota,
		LatencyMS:        log.LatencyMS,
		IsStream:         log.IsStream,
		StatusCode:       log.StatusCode,
		Error:            log.Error,
		CreatedAt:        unixOrZero(log.CreatedAt),
	}
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

// dailyUsageDTO 是趋势图上的单个数据点。
type dailyUsageDTO struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
	Quota    int64  `json:"quota"`
}

// toDailyUsageDTOList 批量转换趋势数据。
func toDailyUsageDTOList(items []model.DailyUsage) []dailyUsageDTO {
	result := make([]dailyUsageDTO, 0, len(items))
	for _, item := range items {
		result = append(result, dailyUsageDTO{
			Date:     item.Date,
			Requests: item.Requests,
			Tokens:   item.Tokens,
			Quota:    item.Quota,
		})
	}
	return result
}

// modelUsageDTO 是模型排行中的单项。
type modelUsageDTO struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// toModelUsageDTOList 批量转换模型排行。
func toModelUsageDTOList(items []model.ModelUsage) []modelUsageDTO {
	result := make([]modelUsageDTO, 0, len(items))
	for _, item := range items {
		result = append(result, modelUsageDTO{
			Model:    item.Model,
			Requests: item.Requests,
			Tokens:   item.Tokens,
		})
	}
	return result
}
