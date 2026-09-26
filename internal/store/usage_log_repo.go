// 本文件是 model.UsageLogRepository 与 model.SettingRepository 的 SQL 实现。
//
// 意图（Why）：
//
//	调用日志层同时承担「写入」与「聚合」两类职责：
//	  - 写入：每次转发完成后落一条记录（高频，必须轻量）；
//	  - 聚合：仪表盘与用户门户所需的汇总、趋势、排行（低频但需正确）。
//	把聚合放在 SQL 层而非取全量再在内存计算，是因为日志会快速增长，
//	全量拉取会带来巨大的内存与网络开销。
//
// 流转（Flow）：
//
//	relay 转发完成 → Create
//	后台/门户 → Summary / DailySeries / TopModels / List
//
// 扩展（Extend）：
//
//	日志量继续增长后：考虑按月分表或引入独立日志库（如 ClickHouse），
//	  本文件的接口签名无需变化，仅替换实现即可。
//	新增统计维度：在 model.UsageLogQuery 加条件，并同步更新 buildUsageWhere。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 日志列表查询的条数约束。
const (
	defaultLogListLimit = 20
	maxLogListLimit     = 100
)

// defaultSeriesDays 是聚合查询未指定起始时间时默认回溯的天数。
const defaultSeriesDays = 7

// usageLogColumns 集中定义查询列，顺序必须与 scanUsageLog 的扫描顺序严格一致。
const usageLogColumns = `id, user_id, token_id, channel_id, model, upstream_model, prompt_tokens, completion_tokens,
	total_tokens, quota, latency_ms, is_stream, status_code, error, request_id, created_at`

// usageLogRepository 是 model.UsageLogRepository 的 SQL 实现，并发安全。
type usageLogRepository struct {
	db *sql.DB
	// dialect：本仓储里「按天分桶」的表达式与数据库方言相关，故需要它。
	// 其余仓储的语句是通用 SQL，不需要方言，构造时也就不传 —— 只让真正有差异的地方拿到方言。
	dialect Dialect
}

// NewUsageLogRepository 创建调用日志仓储。
func NewUsageLogRepository(db *sql.DB, dialect Dialect) model.UsageLogRepository {
	return &usageLogRepository{db: db, dialect: dialect}
}

// Create 写入一条调用日志。
func (r *usageLogRepository) Create(ctx context.Context, log *model.UsageLog) error {
	if err := log.Validate(); err != nil {
		return fmt.Errorf("store: 调用日志非法: %w", err)
	}
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO usage_logs
			(user_id, token_id, channel_id, model, upstream_model, prompt_tokens, completion_tokens, total_tokens,
			 quota, latency_ms, is_stream, status_code, error, request_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		log.UserID, log.TokenID, log.ChannelID, log.Model, log.UpstreamModel,
		log.PromptTokens, log.CompletionTokens, log.TotalTokens,
		log.Quota, log.LatencyMS, boolToInt(log.IsStream), log.StatusCode,
		log.Error, log.RequestID, log.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: 写入调用日志失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取日志 ID 失败: %w", err)
	}
	log.ID = uint64(id)
	return nil
}

// List 按条件返回日志列表（时间倒序，最新在前）。
func (r *usageLogRepository) List(ctx context.Context, q model.UsageLogQuery) ([]*model.UsageLog, error) {
	where, args := buildUsageWhere(q)

	var sb strings.Builder
	sb.WriteString("SELECT " + usageLogColumns + " FROM usage_logs")
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}
	// 按自增主键倒序而非时间倒序：同一秒内写入多条时，时间无法区分先后，
	// 用 id 排序才能保证分页结果稳定（否则翻页可能重复或漏行）。
	sb.WriteString(" ORDER BY id DESC")

	limit := normalizeLimit(q.Limit, defaultLogListLimit, maxLogListLimit)
	offset := normalizeOffset(q.Offset)
	sb.WriteString(" LIMIT ? OFFSET ?")
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询调用日志失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	logs := make([]*model.UsageLog, 0, limit)
	for rows.Next() {
		entry, err := scanUsageLog(rows)
		if err != nil {
			return nil, err
		}
		logs = append(logs, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历日志结果集失败: %w", err)
	}
	return logs, nil
}

// Count 返回符合条件的日志总数。
func (r *usageLogRepository) Count(ctx context.Context, q model.UsageLogQuery) (int, error) {
	where, args := buildUsageWhere(q)

	sb := strings.Builder{}
	sb.WriteString("SELECT COUNT(1) FROM usage_logs")
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}

	var total int
	if err := r.db.QueryRowContext(ctx, sb.String(), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计日志数失败: %w", err)
	}
	return total, nil
}

// Summary 返回符合条件的用量汇总。
func (r *usageLogRepository) Summary(ctx context.Context, q model.UsageLogQuery) (*model.UsageSummary, error) {
	where, args := buildUsageWhere(q)

	// 用 CASE WHEN 统计成功数，避免为"成功率"再发一次查询
	sb := strings.Builder{}
	sb.WriteString(`SELECT
			COUNT(1),
			COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(quota), 0)
		FROM usage_logs`)
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}

	var summary model.UsageSummary
	if err := r.db.QueryRowContext(ctx, sb.String(), args...).Scan(
		&summary.Requests, &summary.Success, &summary.Tokens, &summary.Quota); err != nil {
		return nil, fmt.Errorf("store: 汇总用量失败: %w", err)
	}
	return &summary, nil
}

// DailySeries 按天聚合用量，并补全没有请求的日期。
//
// 补零的必要性：若某天完全没有请求，SQL 的 GROUP BY 不会产生该日期的行，
// 前端折线图会出现断点——运维看到"线断了"会误判为服务中断，实际上只是没人用。
func (r *usageLogRepository) DailySeries(ctx context.Context, q model.UsageLogQuery) ([]model.DailyUsage, error) {
	since, until := normalizeSeriesRange(q)

	// 强制使用补零后的时间范围，避免与既有的 Since/Until 条件冲突
	rangeQuery := q
	rangeQuery.Since = &since
	rangeQuery.Until = &until
	where, args := buildUsageWhere(rangeQuery)

	// 按本地日期分组：使用者关心的是"我这边几号用了多少"，
	// 若按 UTC 分组，东八区凌晨的用量会被算到前一天。
	// 具体表达式由方言提供（SQLite 用 strftime，其他库各不相同）。
	sb := strings.Builder{}
	sb.WriteString(`SELECT
			` + r.dialect.DayBucket("created_at") + ` AS day,
			COUNT(1),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(quota), 0)
		FROM usage_logs`)
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}
	sb.WriteString(" GROUP BY day ORDER BY day ASC")

	rows, err := r.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: 按天聚合失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 先把查询结果放入 map，再按完整日期区间顺序输出
	byDay := make(map[string]model.DailyUsage)
	for rows.Next() {
		var item model.DailyUsage
		if err := rows.Scan(&item.Date, &item.Requests, &item.Tokens, &item.Quota); err != nil {
			return nil, fmt.Errorf("store: 读取按天聚合结果失败: %w", err)
		}
		byDay[item.Date] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历按天聚合结果失败: %w", err)
	}

	// 补零循环使用半开区间 [since, until)：until 是"次日零点"，
	// 若写成 !day.After(until) 会把次日也输出为一个全零数据点，
	// 前端趋势图末尾会多出一根"未来"的空柱子。
	series := make([]model.DailyUsage, 0, len(byDay)+1)
	for day := since; day.Before(until); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		if item, ok := byDay[key]; ok {
			series = append(series, item)
			continue
		}
		// 该日期无数据，补零占位
		series = append(series, model.DailyUsage{Date: key})
	}
	return series, nil
}

// TopModels 返回用量最高的前 N 个模型（按请求数降序）。
func (r *usageLogRepository) TopModels(ctx context.Context, q model.UsageLogQuery, limit int) ([]model.ModelUsage, error) {
	where, args := buildUsageWhere(q)

	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}

	sb := strings.Builder{}
	sb.WriteString(`SELECT model, COUNT(1), COALESCE(SUM(total_tokens), 0)
		FROM usage_logs`)
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}
	// 空模型名（多为解析失败的请求）不参与排行，避免占据榜单
	sb.WriteString(" GROUP BY model HAVING model != '' ORDER BY COUNT(1) DESC LIMIT ?")
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: 模型排行查询失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]model.ModelUsage, 0, limit)
	for rows.Next() {
		var item model.ModelUsage
		if err := rows.Scan(&item.Model, &item.Requests, &item.Tokens); err != nil {
			return nil, fmt.Errorf("store: 读取模型排行失败: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历模型排行失败: %w", err)
	}
	return result, nil
}

// buildUsageWhere 构造日志查询的 WHERE 子句与参数。
func buildUsageWhere(q model.UsageLogQuery) (string, []any) {
	var (
		conditions []string
		args       []any
	)

	if q.UserID != nil {
		conditions = append(conditions, "user_id = ?")
		args = append(args, *q.UserID)
	}
	if q.TokenID != nil {
		conditions = append(conditions, "token_id = ?")
		args = append(args, *q.TokenID)
	}
	if q.ChannelID != nil {
		conditions = append(conditions, "channel_id = ?")
		args = append(args, *q.ChannelID)
	}
	if modelName := strings.TrimSpace(q.Model); modelName != "" {
		conditions = append(conditions, "model = ?")
		args = append(args, modelName)
	}

	switch q.Status {
	case model.LogStatusSuccess:
		conditions = append(conditions, "(status_code >= 200 AND status_code < 300)")
	case model.LogStatusError:
		// 把"非 2xx"统一视为失败；注意 0（未产生状态码）也属于失败
		conditions = append(conditions, "(status_code < 200 OR status_code >= 300)")
	}

	if q.Since != nil {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, q.Since.Unix())
	}
	if q.Until != nil {
		// 注意用 <（严格小于）并在调用方保证 Until 为"次日零点"，
		// 这样可精确覆盖整天，避免"结束当天 00:00 之后的记录被漏掉"。
		conditions = append(conditions, "created_at < ?")
		args = append(args, q.Until.Unix())
	}

	return strings.Join(conditions, " AND "), args
}

// normalizeSeriesRange 归一化趋势查询的时间范围。
//
// 约定：Until 为"次日零点"（半开区间），保证包含结束当天全天。
func normalizeSeriesRange(q model.UsageLogQuery) (time.Time, time.Time) {
	now := time.Now()

	until := now
	if q.Until != nil {
		until = *q.Until
	}

	since := until.AddDate(0, 0, -defaultSeriesDays+1)
	if q.Since != nil {
		since = *q.Since
	}

	// 归一到"天"的边界，保证补零循环覆盖完整日期
	since = truncateToDay(since)
	until = truncateToDay(until.AddDate(0, 0, 1))

	return since, until
}

// truncateToDay 把时间截断到当天零点（本地时区）。
func truncateToDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// scanUsageLog 把一行数据映射为日志对象。
func scanUsageLog(sc rowScanner) (*model.UsageLog, error) {
	var (
		id               uint64
		userID           uint64
		tokenID          uint64
		channelID        uint64
		modelName        string
		upstreamModel    string
		promptTokens     int
		completionTokens int
		totalTokens      int
		quota            int64
		latencyMS        int
		isStream         int
		statusCode       int
		errMsg           string
		requestID        string
		createdAt        int64
	)

	if err := sc.Scan(&id, &userID, &tokenID, &channelID, &modelName, &upstreamModel,
		&promptTokens, &completionTokens, &totalTokens, &quota, &latencyMS,
		&isStream, &statusCode, &errMsg, &requestID, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取调用日志字段失败: %w", err)
	}

	return &model.UsageLog{
		ID:               id,
		UserID:           userID,
		TokenID:          tokenID,
		ChannelID:        channelID,
		Model:            modelName,
		UpstreamModel:    upstreamModel,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		Quota:            quota,
		LatencyMS:        latencyMS,
		IsStream:         isStream != 0,
		StatusCode:       statusCode,
		Error:            errMsg,
		RequestID:        requestID,
		CreatedAt:        time.Unix(createdAt, 0),
	}, nil
}

// ---------------------------------------------------------------------------
// 系统设置仓储
// ---------------------------------------------------------------------------

// settingRepository 是 model.SettingRepository 的 SQL 实现。
type settingRepository struct {
	db *sql.DB
	// dialect：设置表的 UPSERT 语法各方言不同（ON CONFLICT / ON DUPLICATE KEY），故需要它。
	dialect Dialect
}

// NewSettingRepository 创建设置仓储。
func NewSettingRepository(db *sql.DB, dialect Dialect) model.SettingRepository {
	return &settingRepository{db: db, dialect: dialect}
}

// Get 读取单个设置；不存在时返回空字符串与 nil。
func (r *settingRepository) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 未配置不是错误：调用方会回退到默认值
			return "", nil
		}
		return "", fmt.Errorf("store: 读取设置 %s 失败: %w", key, err)
	}
	return value, nil
}

// GetAll 读取全部设置。
func (r *settingRepository) GetAll(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, fmt.Errorf("store: 读取全部设置失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	values := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: 读取设置项失败: %w", err)
		}
		values[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历设置失败: %w", err)
	}
	return values, nil
}

// Set 写入单个设置（存在则覆盖）。
func (r *settingRepository) Set(ctx context.Context, key, value string) error {
	return r.SetMany(ctx, map[string]string{key: value})
}

// SetMany 在单个事务内批量写入设置。
func (r *settingRepository) SetMany(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启设置事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	upsert := r.dialect.UpsertSettingSQL()
	for key, value := range values {
		// UPSERT：一条语句同时覆盖「插入」与「已存在则更新」，
		// 避免"先查再写"带来的并发窗口（两个请求同时插同一个键会有一个失败）。
		if _, err := tx.ExecContext(ctx, upsert, key, value, now); err != nil {
			return fmt.Errorf("store: 写入设置 %s 失败: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交设置失败: %w", err)
	}
	return nil
}
