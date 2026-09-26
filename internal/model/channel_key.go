// 本文件定义「渠道密钥池」领域模型与仓储接口。
//
// 意图（Why）：
//
//	一个上游通常有多把密钥（批量申请的免费额度密钥、多人共享的团队密钥）。
//	把它们放进同一个渠道的"池"里轮询使用，能同时得到两个好处：
//	  1) 单渠道即可获得远超单密钥的配额上限（请求在池内轮换）；
//	  2) 某把密钥失效时只摘除这一把，渠道整体仍然可用（故障半径最小）。
//
//	若不这样做而选择"一密钥一渠道"，渠道列表会膨胀到几百行，
//	既无法运维（改一个 base_url 要改几百次），也无法做池内负载均衡。
//
// 安全约定（与 Channel 一致）：
//
//	Key 字段在内存中是明文，仅在进程内流转；落库由 store 层加密，
//	对外输出一律使用 Masked()，绝不返回明文。
//
// 流转（Flow）：
//
//	后台导入：ChannelsView 粘贴多行密钥 → ReplaceAll 落库（加密）
//	转发路径：relay 选中渠道 → ListUsable 取池 → 随机挑一把 → MarkUsed
//	              → 失败 MarkFailure（连续失败达阈值自动摘除）
//	              → 成功 MarkSuccess（清零连续失败计数）
//
// 扩展（Extend）：
//
//	新增密钥维度的策略（如按模型区分密钥）时，在 ChannelKey 加字段并建新迁移，
//	本文件与 store/channel_key_repo.go、前端渠道表单三处同步。
package model

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// KeyAutoRemoveThreshold 是密钥被自动摘除前允许的连续失败次数。
//
// 取值 3 的权衡：
//   - 过小（1 次）：一次网络抖动或上游瞬时 500 就会摘掉好密钥，
//     池子会随时间被"误杀"到没有可用密钥；
//   - 过大：失效密钥（401/额度耗尽）会反复被选中，持续浪费尝试次数与延迟。
//     3 次连续失败已能排除偶发抖动，又能快速隔离真正失效的密钥。
const KeyAutoRemoveThreshold = 3

// ChannelKeyStatus 表示密钥在池中的可用状态。
type ChannelKeyStatus int

const (
	// ChannelKeyStatusEnabled 启用：可参与轮询。
	ChannelKeyStatusEnabled ChannelKeyStatus = 1
	// ChannelKeyStatusDisabled 手动禁用：由管理员主动关闭，自动流程不会恢复。
	ChannelKeyStatusDisabled ChannelKeyStatus = 2
	// ChannelKeyStatusAutoRemoved 自动摘除：连续失败达阈值，可手动恢复。
	ChannelKeyStatusAutoRemoved ChannelKeyStatus = 3
)

// String 返回状态中文名，便于日志与界面展示。
func (s ChannelKeyStatus) String() string {
	switch s {
	case ChannelKeyStatusEnabled:
		return "启用"
	case ChannelKeyStatusDisabled:
		return "手动禁用"
	case ChannelKeyStatusAutoRemoved:
		return "已自动摘除"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

// IsValid 判断状态值是否合法（用于校验外部输入）。
func (s ChannelKeyStatus) IsValid() bool {
	switch s {
	case ChannelKeyStatusEnabled, ChannelKeyStatusDisabled, ChannelKeyStatusAutoRemoved:
		return true
	default:
		return false
	}
}

// CredentialKind 表示凭据类型。
//
// 两类凭据的调度语义完全一致（池内轮询、失败摘除、记录最近使用），
// 差别只在"取出来之后要不要先刷新"：
//   - api_key：长期有效，取出即可用；
//   - oauth  ：access_token 短期有效，过期前需用 refresh_token 刷新。
type CredentialKind string

const (
	// CredentialKindAPIKey 静态 API Key。
	CredentialKindAPIKey CredentialKind = "api_key"
	// CredentialKindOAuth 订阅账号的 OAuth 凭据（含 refresh_token）。
	CredentialKindOAuth CredentialKind = "oauth"
)

// IsValid 判断凭据类型是否合法。
func (k CredentialKind) IsValid() bool {
	return k == CredentialKindAPIKey || k == CredentialKindOAuth
}

// refreshAheadSeconds 是"提前刷新"的窗口。
//
// 为什么要提前：若等到 access_token 真正过期才刷新，那么"过期瞬间进来的请求"
// 会先失败一次（拿 401 才发现要刷新），用户会看到偶发报错。
// 提前 60 秒刷新可以把这类失败窗口完全消除，代价只是偶尔多刷一次。
const refreshAheadSeconds = 60

// ChannelKey 表示渠道下的一条凭据（API Key 或 OAuth 账号）。
type ChannelKey struct {
	ID        uint64           // 主键
	ChannelID uint64           // 归属渠道
	Kind      CredentialKind   // 凭据类型
	Key       string           // api_key 明文【仅内存】；oauth 类型下为空
	Label     string           // 备注（便于人工定位，如"池 #001"）
	Status    ChannelKeyStatus // 可用状态
	FailCount int              // 连续失败次数（成功即清零）

	// 以下字段仅 OAuth 类型使用。
	//
	// RefreshToken / AccessToken 均为明文，仅在内存中流转，落库由仓储加密。
	RefreshToken string
	AccessToken  string
	// ExpiresAt 是 access_token 的过期时间（零值表示未知）。
	ExpiresAt time.Time
	// AccountHint 是账号标识（如邮箱），用于管理员辨认"这是谁的账号"。
	AccountHint string
	// Provider 是关联的 OAuth 提供方名字（对应 oauth_providers.name）。
	Provider string

	// LastUsedAt 为最近被选中使用的时间；零值表示从未使用。
	LastUsedAt time.Time
	// LastError 为最近一次失败原因（已脱敏，只记状态码与简短描述）。
	LastError string
	CreatedAt time.Time
}

// IsUsable 判断该凭据当前是否可参与轮询。
func (k *ChannelKey) IsUsable() bool {
	return k.Status == ChannelKeyStatusEnabled
}

// IsOAuth 判断是否为 OAuth 凭据。
func (k *ChannelKey) IsOAuth() bool {
	return k.Kind == CredentialKindOAuth
}

// NeedsRefresh 判断 OAuth 凭据是否需要在本次使用前刷新。
//
// 判定规则：
//   - 非 OAuth 凭据永不刷新；
//   - access_token 为空 → 必须刷新；
//   - 剩余有效期不足 refreshAheadSeconds → 提前刷新。
func (k *ChannelKey) NeedsRefresh(now time.Time) bool {
	if !k.IsOAuth() {
		return false
	}
	if strings.TrimSpace(k.AccessToken) == "" {
		return true
	}
	if k.ExpiresAt.IsZero() {
		// 过期时间未知：保守起见按"需要刷新"处理，
		// 否则一旦令牌已失效就会持续 401（而池内其他凭据被白白浪费）
		return true
	}
	return now.Add(refreshAheadSeconds * time.Second).After(k.ExpiresAt)
}

// CredentialValue 返回"当前可直接用于上游鉴权"的凭据值。
//
// api_key 型返回 Key；oauth 型返回 AccessToken。
// 这样转发链路无需区分类型，统一取一个字符串即可。
func (k *ChannelKey) CredentialValue() string {
	if k.IsOAuth() {
		return k.AccessToken
	}
	return k.Key
}

// Masked 返回脱敏后的凭据，供界面与日志展示。
//
// 脱敏规则：保留前 8 位与后 4 位。相比渠道密钥多留 2 位前缀，
// 是因为密钥池里往往有几百把同前缀（如 nvapi-）的密钥，
// 只保留 6 位前缀在界面上几乎无法区分。
//
// OAuth 凭据返回"账号标识 + 类型"而不是令牌片段：
// 令牌片段对管理员没有任何辨识价值，而账号标识能立刻告诉他是谁。
func (k *ChannelKey) Masked() string {
	if k.IsOAuth() {
		hint := strings.TrimSpace(k.AccountHint)
		if hint == "" {
			hint = "未命名账号"
		}
		return "[OAuth] " + hint
	}

	key := k.Key
	if key == "" {
		return ""
	}
	const (
		keepPrefix = 8
		keepSuffix = 4
	)
	if len(key) <= keepPrefix+keepSuffix {
		return strings.Repeat("*", len(key))
	}
	return key[:keepPrefix] + "****" + key[len(key)-keepSuffix:]
}

// KeyPoolSummary 是密钥池的概览统计（列表页展示用）。
//
// 为什么需要它：渠道列表若把每个渠道的全部密钥（可能几百把）都查出来，
// 页面数据量会大到不可用。这里只给计数，需要细节时再单独查该渠道的密钥列表。
type KeyPoolSummary struct {
	ChannelID   uint64 `json:"channel_id"`
	Total       int    `json:"total"`
	Enabled     int    `json:"enabled"`
	Disabled    int    `json:"disabled"`
	AutoRemoved int    `json:"auto_removed"`
}

// Available 返回可用密钥数。
func (s KeyPoolSummary) Available() int {
	return s.Enabled
}

// CredentialInput 描述一条待导入的凭据。
//
// 支持两种形态：
//   - API Key：填 APIKey；
//   - OAuth  ：填 RefreshToken（可选同时给 AccessToken 与 ExpiresAt）。
//
// 之所以用"一个结构体装两类"，而不是两个方法：导入路径（后台表单、批量粘贴、
// CLI）对两类凭据的处理流程完全一致，分成两个方法只会让调用点多一层判断。
type CredentialInput struct {
	Kind         CredentialKind // 必填：api_key / oauth
	APIKey       string         // kind=api_key 时必填
	RefreshToken string         // kind=oauth 时必填
	AccessToken  string         // 可选：导入时若已持有访问令牌则一并保存，省一次刷新
	ExpiresAt    time.Time      // 可选：access_token 的过期时间
	AccountHint  string         // 可选：账号标识（邮箱等）
	Provider     string         // kind=oauth 时建议填写：关联的 OAuth 提供方
	Label        string         // 可选：备注
}

// IdentityHash 返回该凭据的去重标识。
//
// 规则：API Key 用密钥本身，OAuth 用 refresh_token——
// 因为 refresh_token 才是账号的长期唯一标识（access_token 每次刷新都变）。
func (c CredentialInput) IdentityHash(sha256Hex func(string) string) string {
	if c.Kind == CredentialKindOAuth {
		return sha256Hex(strings.TrimSpace(c.RefreshToken))
	}
	return sha256Hex(strings.TrimSpace(c.APIKey))
}

// Validate 校验凭据是否可用。
func (c CredentialInput) Validate() error {
	if !c.Kind.IsValid() {
		return fmt.Errorf("凭据类型非法: %q", c.Kind)
	}
	switch c.Kind {
	case CredentialKindAPIKey:
		if strings.TrimSpace(c.APIKey) == "" {
			return errors.New("API Key 不能为空")
		}
	case CredentialKindOAuth:
		if strings.TrimSpace(c.RefreshToken) == "" {
			return errors.New("OAuth 凭据必须提供 refresh_token")
		}
	}
	return nil
}

// ParseCredentialList 解析批量粘贴的 OAuth 凭据文本。
//
// 输入格式（与密钥批量导入保持一致，降低使用者的记忆负担）：
//
//	每行一条：refresh_token [账号标识]
//	支持空格、制表符或逗号分隔；
//	以 # 开头的行视为注释；空行忽略；重复项自动去重。
//
// provider 会写入每条凭据，便于刷新时找到对应的 OAuth 提供方配置。
func ParseCredentialList(raw, provider string) []CredentialInput {
	tokens, labels := ParseKeyList(raw)

	inputs := make([]CredentialInput, 0, len(tokens))
	for i, token := range tokens {
		label := ""
		if i < len(labels) {
			label = labels[i]
		}
		inputs = append(inputs, CredentialInput{
			Kind:         CredentialKindOAuth,
			RefreshToken: token,
			// 备注同时也是账号标识：使用者粘贴时写的通常就是账号邮箱，
			// 直接当作 AccountHint 可以让池列表立刻可读，省去手工再填一遍。
			AccountHint: label,
			Label:       label,
			Provider:    provider,
		})
	}
	return inputs
}

// ChannelKeyRepository 定义密钥池的持久化操作。
//
// 约定：所有方法的实现都必须保证密钥落库加密、读取解密；
// 未找到时返回 ErrChannelKeyNotFound。
type ChannelKeyRepository interface {
	// ReplaceAll 用给定密钥集合整体替换某渠道的密钥池。
	//
	// 语义（幂等）：已存在的密钥（按摘要匹配）保留其状态与统计，只新增缺失的、
	// 删除不在集合中的。因此"编辑渠道时重新粘贴同一批密钥"不会重置统计数据，
	// 也不会产生重复记录。
	//
	// labels 与 keys 一一对应（可为 nil，此时备注留空）。
	// 返回 added（新增数）、removed（删除数）。
	ReplaceAll(ctx context.Context, channelID uint64, keys, labels []string) (added, removed int, err error)

	// ReplaceCredentials 用给定凭据集合整体替换某渠道的凭据池。
	//
	// 与 ReplaceAll 的关系：ReplaceAll 是"纯 API Key 池"的便捷入口，
	// 本方法支持两类凭据混装（API Key + OAuth 订阅账号）。
	// 去重标识：api_key 用密钥摘要，oauth 用 refresh_token 摘要
	// （refresh_token 是 OAuth 凭据的长期唯一标识）。
	ReplaceCredentials(ctx context.Context, channelID uint64, inputs []CredentialInput) (added, removed int, err error)

	// UpdateTokens 回写刷新后的 OAuth 令牌。
	//
	// refreshToken 为空时保留原值：部分平台的刷新响应不回传新的 refresh_token，
	// 此时不应把它清空（清空等于永久丢失该账号）。
	UpdateTokens(ctx context.Context, id uint64, accessToken string, expiresAt time.Time, refreshToken string) error

	// ListByChannel 列出某渠道的全部密钥（含已摘除），按 ID 升序。
	ListByChannel(ctx context.Context, channelID uint64) ([]*ChannelKey, error)

	// ListUsable 列出某渠道中状态为"启用"的密钥，供转发时轮询选择。
	ListUsable(ctx context.Context, channelID uint64) ([]*ChannelKey, error)

	// Summary 批量返回多个渠道的密钥池概览，避免列表页出现 N+1 查询。
	Summary(ctx context.Context, channelIDs []uint64) (map[uint64]KeyPoolSummary, error)

	// DeleteByChannel 删除某渠道的全部密钥（渠道被删除时调用）。
	DeleteByChannel(ctx context.Context, channelID uint64) error

	// MarkUsed 记录密钥被选中使用（用于展示"最近使用时间"）。
	MarkUsed(ctx context.Context, id uint64, at time.Time) error

	// MarkSuccess 记录一次成功：清零连续失败计数。
	MarkSuccess(ctx context.Context, id uint64) error

	// MarkFailure 记录一次失败：累加连续失败计数，达到 KeyAutoRemoveThreshold 时自动摘除。
	MarkFailure(ctx context.Context, id uint64, reason string) error

	// UpdateStatus 手动修改密钥状态（启用 / 禁用 / 重新启用被摘除的密钥）。
	UpdateStatus(ctx context.Context, id uint64, status ChannelKeyStatus) error
}

// ErrChannelKeyNotFound 表示密钥不存在。
var ErrChannelKeyNotFound = errors.New("model: 渠道密钥不存在")

// ParseKeyList 把多行文本解析为密钥列表。
//
// 支持的输入形式（后台"批量粘贴"入口使用）：
//   - 每行一把密钥；
//   - 允许行内带备注，用空白或逗号分隔："nvapi-xxx 池#1" 或 "nvapi-xxx,池#1"；
//   - 忽略空行与以 # 开头的注释行；
//   - 自动去重（保留首次出现的顺序）。
//
// 返回 keys 为密钥正文，labels 与之一一对应（无备注时为空字符串）。
//
// 为什么把解析放在领域层：后台导入、CLI 导入、测试都走同一套解析规则，
// 避免"页面上能导入、脚本里导入格式却不一致"这类割裂。
func ParseKeyList(raw string) (keys []string, labels []string) {
	seen := make(map[string]struct{})

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 分隔符：优先按逗号切分（明确），否则按空白切分（宽松）
		key, label := splitKeyAndLabel(line)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
		labels = append(labels, label)
	}
	return keys, labels
}

// splitKeyAndLabel 从一行文本中拆出密钥与备注。
//
// 规则：逗号优先级高于空白（因为密钥本身不含逗号，而备注常带空格）。
func splitKeyAndLabel(line string) (key, label string) {
	if idx := strings.IndexAny(line, ",，\t"); idx >= 0 {
		return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:])
	}
	if idx := strings.Index(line, " "); idx >= 0 {
		return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:])
	}
	return line, ""
}

// PickKey 从可用密钥中挑一把（随机）。
//
// 为什么用随机而不是"严格轮询"（按 last_used_at 排序取最早）：
//   - 并发场景下"读时间 → 排序 → 写时间"存在竞态，多个请求会选中同一把；
//   - 随机选择在请求量较大时同样接近均匀分布，且无锁、无状态、实现简单。
//
// 池内只有一把时直接返回，省去随机数开销（这是最常见的情况）。
func PickKey(keys []*ChannelKey) *ChannelKey {
	switch len(keys) {
	case 0:
		return nil
	case 1:
		return keys[0]
	}
	return keys[rand.IntN(len(keys))]
}
