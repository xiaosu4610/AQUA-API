// 本文件定义「调用日志」领域模型，它是用量统计与后续计费的数据基础。
//
// 意图（Why）：
//
//	网关必须能回答运营中最常见的几个问题：
//	  「今天用了多少」「哪个模型最热」「谁用得最多」「失败率多高」。
//	这些问题的答案只能来自逐条记录的调用日志，因此每条转发（无论成功失败）
//	都应落一条日志，且记录足够完整（用户、令牌、渠道、模型、耗时、状态码）。
//
// 流转（Flow）：
//
//	relay 转发完成 → 组装 UsageLog → UsageLogRepository.Create
//	  └─ 后台仪表盘：Summary / DailySeries / TopModels 聚合查询
//	  └─ 用户门户：按 UserID 过滤的同一组聚合查询
//
// 扩展（Extend）：
//
//	接入计费后：新增 quota 的精算逻辑（当前由调用方按倍率估算后写入），
//	  并考虑把日志库独立出来（日志写入量远大于业务表）。
//	新增统计维度（如按渠道、按令牌）：在 UsageLogQuery 加条件并在仓储层实现聚合。
package model

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 领域错误。
var (
	// ErrUsageLogNotFound 表示未找到指定日志（通常无需对外暴露）。
	ErrUsageLogNotFound = errors.New("model: 调用日志不存在")
)

// LogStatus 是日志查询中使用的状态语义值。
//
// 为什么不直接用 HTTP 状态码：客户端筛选时想表达的是「成功/失败」这类语义，
// 而不是具体是 400 还是 500。仓储层负责把语义翻译为 SQL 条件。
const (
	// LogStatusSuccess 表示按「成功」筛选（2xx）。
	LogStatusSuccess = "success"
	// LogStatusError 表示按「失败」筛选（非 2xx，或存在错误信息）。
	LogStatusError = "error"
)

// UsageLog 表示一次模型接口调用记录。
//
// 字段设计说明：
//   - ChannelID 允许为 0（表示尚未选定渠道就失败，如无可用渠道）；
//   - Error 只记录"面向运维的简短原因"，禁止写入上游返回的原始报错全文
//     （可能包含上游地址或密钥片段）；
//   - RequestID 用于与客户端日志对账，排查"客户端说失败、服务端说成功"这类问题。
type UsageLog struct {
	ID               uint64    // 主键
	UserID           uint64    // 调用者用户 ID（0 表示未认证或系统调用）
	TokenID          uint64    // 使用的访问令牌 ID
	ChannelID        uint64    // 命中的上游渠道 ID
	Model            string    // 请求的模型名（对外模型名）
	UpstreamModel    string    // 实际发给上游的模型名（经渠道映射改写）；空串表示与 Model 相同
	PromptTokens     int       // 输入 token 数
	CompletionTokens int       // 输出 token 数
	TotalTokens      int       // 总 token 数
	Quota            int64     // 本次消耗额度（内部单位）
	LatencyMS        int       // 总耗时（毫秒）
	IsStream         bool      // 是否流式请求
	StatusCode       int       // 回写给客户端的状态码
	Error            string    // 失败原因（已脱敏、简短）
	RequestID        string    // 请求标识
	CreatedAt        time.Time // 记录时间
}

// Validate 校验日志的必要字段。
//
// 说明：日志的校验刻意宽松——它是可观测性数据，宁可记录一条字段不全的日志，
// 也不要因为校验失败而丢失"某次调用发生过"这一事实。
func (l *UsageLog) Validate() error {
	if l.StatusCode < 0 || l.StatusCode > 599 {
		return fmt.Errorf("状态码非法: %d", l.StatusCode)
	}
	if l.LatencyMS < 0 {
		return fmt.Errorf("耗时不能为负数: %d", l.LatencyMS)
	}
	if l.PromptTokens < 0 || l.CompletionTokens < 0 {
		return errors.New("token 数不能为负数")
	}
	return nil
}

// IsSuccess 判断本次调用是否成功（以回写给客户端的状态码为准）。
func (l *UsageLog) IsSuccess() bool {
	return l.StatusCode >= 200 && l.StatusCode < 300
}

// UsageLogQuery 描述调用日志的查询与聚合条件。
//
// 同一结构体同时用于列表查询与聚合统计，避免两套条件出现语义漂移
// （例如"列表按用户过滤、统计忘了过滤"这类难以发现的偏差）。
type UsageLogQuery struct {
	UserID    *uint64    // 按用户过滤；nil 表示不过滤
	TokenID   *uint64    // 按令牌过滤
	ChannelID *uint64    // 按渠道过滤
	Model     string     // 按模型精确匹配；空表示不过滤
	Status    string     // LogStatusSuccess / LogStatusError / 空=不过滤
	Since     *time.Time // 起始时间（含）；nil 表示不限
	Until     *time.Time // 结束时间（含）；nil 表示不限
	Limit     int        // 条数上限（仅列表查询使用）
	Offset    int        // 偏移量（仅列表查询使用）
}

// UsageSummary 是某条件下的用量汇总。
type UsageSummary struct {
	Requests int64 // 请求总数
	Success  int64 // 成功数（2xx）
	Tokens   int64 // token 总量
	Quota    int64 // 额度总量
}

// SuccessRate 返回成功率（0~1）；无请求时返回 0。
func (s UsageSummary) SuccessRate() float64 {
	if s.Requests <= 0 {
		return 0
	}
	return float64(s.Success) / float64(s.Requests)
}

// DailyUsage 是单日用量，用于趋势图。
type DailyUsage struct {
	Date     string // 日期，格式 2006-01-02
	Requests int64  // 请求数
	Tokens   int64  // token 数
	Quota    int64  // 额度
}

// ModelUsage 是单个模型的用量，用于排行榜。
type ModelUsage struct {
	Model    string // 模型名
	Requests int64  // 请求数
	Tokens   int64  // token 数
}

// UsageLogRepository 定义调用日志的持久化与聚合操作。
type UsageLogRepository interface {
	// Create 写入一条调用日志。
	Create(ctx context.Context, log *UsageLog) error

	// List 按条件返回日志列表，按时间倒序（最新在前）。
	List(ctx context.Context, q UsageLogQuery) ([]*UsageLog, error)

	// Count 返回符合条件的日志总数，用于分页。
	Count(ctx context.Context, q UsageLogQuery) (int, error)

	// Summary 返回符合条件的用量汇总。
	Summary(ctx context.Context, q UsageLogQuery) (*UsageSummary, error)

	// DailySeries 按天聚合，返回按日期升序的趋势数据。
	//
	// 实现要求：必须补全"没有请求的日期"（补 0），否则前端折线图会出现断点，
	// 让人误以为那些天服务中断了。补零逻辑放在实现层，避免每个调用方各自处理。
	DailySeries(ctx context.Context, q UsageLogQuery) ([]DailyUsage, error)

	// TopModels 返回用量最高的前 N 个模型，按请求数降序。
	TopModels(ctx context.Context, q UsageLogQuery, limit int) ([]ModelUsage, error)
}
