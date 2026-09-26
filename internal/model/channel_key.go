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

// ChannelKey 表示渠道下的一把上游密钥。
type ChannelKey struct {
	ID        uint64           // 主键
	ChannelID uint64           // 归属渠道
	Key       string           // 密钥明文【仅内存】
	Label     string           // 备注（便于人工定位，如"池 #001"）
	Status    ChannelKeyStatus // 可用状态
	FailCount int              // 连续失败次数（成功即清零）
	// LastUsedAt 为最近被选中使用的时间；零值表示从未使用。
	LastUsedAt time.Time
	// LastError 为最近一次失败原因（已脱敏，只记状态码与简短描述）。
	LastError string
	CreatedAt time.Time
}

// IsUsable 判断该密钥当前是否可参与轮询。
func (k *ChannelKey) IsUsable() bool {
	return k.Status == ChannelKeyStatusEnabled
}

// Masked 返回脱敏后的密钥，供界面与日志展示。
//
// 脱敏规则：保留前 8 位与后 4 位。相比渠道密钥多留 2 位前缀，
// 是因为密钥池里往往有几百把同前缀（如 nvapi-）的密钥，
// 只保留 6 位前缀在界面上几乎无法区分。
func (k *ChannelKey) Masked() string {
	const (
		keepPrefix = 8
		keepSuffix = 4
	)
	key := k.Key
	if key == "" {
		return ""
	}
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
