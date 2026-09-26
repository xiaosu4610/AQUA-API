// 调用日志仓储的聚合查询测试。
//
// 意图（Why）：
//
//	仪表盘与用户门户的图表直接依赖聚合结果。这里重点锁定两类容易出错、
//	且出错后不易察觉的行为：
//	  1) 趋势数据的日期区间必须精确为 N 天（多一天会显示"未来"的空柱子）；
//	  2) 没有请求的日期必须补零（否则折线断开，会被误判为服务中断）。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 真实 SQLite 上写入样本日志后校验聚合结果
//
// 扩展（Extend）：
//
//	新增聚合维度（如按渠道）后，仿照本文件补充"有数据 + 无数据"两类用例。
package store

import (
	"context"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestLogRepo 构造基于临时数据库的日志仓储。
func newTestLogRepo(t *testing.T) model.UsageLogRepository {
	t.Helper()
	st := newTestStore(t)
	return NewUsageLogRepository(st.DB(), st.Dialect())
}

// insertLog 写入一条指定时间与状态的日志。
func insertLog(t *testing.T, repo model.UsageLogRepository, at time.Time, statusCode int, tokens int) {
	t.Helper()

	entry := &model.UsageLog{
		UserID:      1,
		ChannelID:   1,
		Model:       "gpt-4o",
		TotalTokens: tokens,
		StatusCode:  statusCode,
		LatencyMS:   100,
		CreatedAt:   at,
	}
	if err := repo.Create(context.Background(), entry); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
}

// TestDailySeries_FillsZeroAndExactRange 验证趋势区间的天数精确且缺失日期补零。
func TestDailySeries_FillsZeroAndExactRange(t *testing.T) {
	repo := newTestLogRepo(t)
	ctx := context.Background()

	today := truncateToDayLocal(time.Now())
	// 仅今天与 3 天前有数据，中间两天应补零
	insertLog(t, repo, today.Add(2*time.Hour), 200, 100)
	insertLog(t, repo, today.AddDate(0, 0, -3).Add(2*time.Hour), 200, 50)
	// 一次失败请求，用于校验成功率
	insertLog(t, repo, today.Add(3*time.Hour), 500, 10)

	since := today.AddDate(0, 0, -6) // 共 7 天（含今天）

	series, err := repo.DailySeries(ctx, model.UsageLogQuery{Since: &since})
	if err != nil {
		t.Fatalf("DailySeries 失败: %v", err)
	}

	// 关键断言 1：恰好 7 个数据点，且最后一个是今天（不能出现"明天"）
	if len(series) != 7 {
		t.Fatalf("数据点数量 = %d，期望 7（区间应为 7 天且不含次日）", len(series))
	}
	if series[0].Date != since.Format("2006-01-02") {
		t.Errorf("首个数据点 = %s，期望 %s", series[0].Date, since.Format("2006-01-02"))
	}
	if series[len(series)-1].Date != today.Format("2006-01-02") {
		t.Errorf("最后一个数据点 = %s，期望今天 %s（不应包含次日）",
			series[len(series)-1].Date, today.Format("2006-01-02"))
	}

	// 关键断言 2：无数据的日期必须补零，而不是被跳过
	//
	// 本用例共写入 3 条日志，但其中两条落在同一天（今天），
	// 因此"有数据的日期"只有 2 天（今天 与 3 天前），其余 5 天应补零。
	zeroCount := 0
	for _, item := range series {
		if item.Requests == 0 {
			zeroCount++
		}
	}
	if zeroCount != 5 {
		t.Errorf("补零天数 = %d，期望 5（7 天中有 2 天有数据）", zeroCount)
	}

	// 有数据的日期计数正确
	if series[len(series)-1].Requests != 2 {
		t.Errorf("今天的请求数 = %d，期望 2", series[len(series)-1].Requests)
	}
}

// TestSummary_CountsAndSuccessRate 验证汇总统计与成功率计算。
func TestSummary_CountsAndSuccessRate(t *testing.T) {
	repo := newTestLogRepo(t)
	ctx := context.Background()

	today := truncateToDayLocal(time.Now())
	insertLog(t, repo, today.Add(time.Hour), 200, 100)
	insertLog(t, repo, today.Add(2*time.Hour), 201, 100)
	insertLog(t, repo, today.Add(3*time.Hour), 500, 0)
	insertLog(t, repo, today.Add(4*time.Hour), 0, 0) // 未产生状态码（连接失败）

	summary, err := repo.Summary(ctx, model.UsageLogQuery{Since: &today})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}

	if summary.Requests != 4 {
		t.Errorf("请求总数 = %d，期望 4", summary.Requests)
	}
	if summary.Success != 2 {
		t.Errorf("成功数 = %d，期望 2（仅 2xx 计入）", summary.Success)
	}
	if summary.Tokens != 200 {
		t.Errorf("token 总量 = %d，期望 200", summary.Tokens)
	}
	// 成功率 = 2/4 = 0.5
	if rate := summary.SuccessRate(); rate < 0.49 || rate > 0.51 {
		t.Errorf("成功率 = %.3f，期望约 0.5", rate)
	}
}

// TestSummary_EmptyResult 验证无数据时汇总返回零值而不报错。
func TestSummary_EmptyResult(t *testing.T) {
	repo := newTestLogRepo(t)

	summary, err := repo.Summary(context.Background(), model.UsageLogQuery{})
	if err != nil {
		t.Fatalf("空库汇总不应报错: %v", err)
	}
	if summary.Requests != 0 || summary.Tokens != 0 || summary.SuccessRate() != 0 {
		t.Errorf("空库应返回全零汇总，实际 %+v", summary)
	}
}

// TestList_StatusFilter 验证按成功/失败筛选。
func TestList_StatusFilter(t *testing.T) {
	repo := newTestLogRepo(t)
	ctx := context.Background()

	today := truncateToDayLocal(time.Now())
	insertLog(t, repo, today.Add(time.Hour), 200, 10)
	insertLog(t, repo, today.Add(2*time.Hour), 429, 0)

	success, err := repo.List(ctx, model.UsageLogQuery{Status: model.LogStatusSuccess})
	if err != nil {
		t.Fatalf("查询成功日志失败: %v", err)
	}
	if len(success) != 1 || success[0].StatusCode != 200 {
		t.Errorf("成功筛选结果异常：%d 条", len(success))
	}

	failed, err := repo.List(ctx, model.UsageLogQuery{Status: model.LogStatusError})
	if err != nil {
		t.Fatalf("查询失败日志失败: %v", err)
	}
	if len(failed) != 1 || failed[0].StatusCode != 429 {
		t.Errorf("失败筛选结果异常：%d 条", len(failed))
	}
}

// TestList_OrderedByIDDesc 验证日志按自增 ID 倒序返回。
//
// 必要性：同一秒内可能写入多条日志，按时间排序无法区分先后，
// 会导致分页翻页时出现重复或漏行。
func TestList_OrderedByIDDesc(t *testing.T) {
	repo := newTestLogRepo(t)
	ctx := context.Background()

	// 三条日志使用完全相同的时间戳
	sameTime := time.Now()
	for i := 0; i < 3; i++ {
		insertLog(t, repo, sameTime, 200, 10)
	}

	logs, err := repo.List(ctx, model.UsageLogQuery{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("日志数 = %d，期望 3", len(logs))
	}
	for i := 1; i < len(logs); i++ {
		if logs[i-1].ID < logs[i].ID {
			t.Errorf("未按 ID 倒序：%d 在 %d 之前", logs[i-1].ID, logs[i].ID)
		}
	}
}

// TestTopModels_ExcludesEmptyModel 验证模型排行排除空模型名。
//
// 空模型名多来自解析失败的请求，若参与排行会占据榜单却没有参考价值。
func TestTopModels_ExcludesEmptyModel(t *testing.T) {
	repo := newTestLogRepo(t)
	ctx := context.Background()

	today := truncateToDayLocal(time.Now())
	// 模拟解析失败的请求：模型名为空
	emptyModel := &model.UsageLog{
		StatusCode: 503,
		Model:      "",
		CreatedAt:  today.Add(time.Hour),
	}
	if err := repo.Create(ctx, emptyModel); err != nil {
		t.Fatalf("写入空模型日志失败: %v", err)
	}

	entry := &model.UsageLog{Model: "claude-3", StatusCode: 200, CreatedAt: today.Add(2 * time.Hour)}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}

	items, err := repo.TopModels(ctx, model.UsageLogQuery{}, 5)
	if err != nil {
		t.Fatalf("TopModels 失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("排行项数 = %d，期望 1（空模型应被排除）", len(items))
	}
	if items[0].Model != "claude-3" {
		t.Errorf("排行首项 = %q，期望 claude-3", items[0].Model)
	}
}

// TestSettingRepository_SetAndGet 验证设置读写与未配置时的回退语义。
func TestSettingRepository_SetAndGet(t *testing.T) {
	st := newTestStore(t)
	repo := NewSettingRepository(st.DB(), st.Dialect())
	ctx := context.Background()

	// 未配置时应返回空串且不报错（调用方据此回退默认值）
	value, err := repo.Get(ctx, model.SettingKeySiteName)
	if err != nil {
		t.Fatalf("读取未配置项不应报错: %v", err)
	}
	if value != "" {
		t.Errorf("未配置项 = %q，期望空串", value)
	}

	// 批量写入（模拟保存整个设置页）
	if err := repo.SetMany(ctx, map[string]string{
		model.SettingKeySiteName:            "我的中转站",
		model.SettingKeyRegistrationEnabled: "false",
	}); err != nil {
		t.Fatalf("批量写入失败: %v", err)
	}

	// 合并读取默认值
	settings, err := model.LoadSiteSettings(ctx, repo)
	if err != nil {
		t.Fatalf("加载设置失败: %v", err)
	}
	if settings.SiteName != "我的中转站" {
		t.Errorf("站点名 = %q，期望已更新", settings.SiteName)
	}
	if settings.RegistrationEnabled {
		t.Error("注册开关应为关闭")
	}
	// 未写入的项应保留默认值
	if settings.DefaultGroup != "default" {
		t.Errorf("默认分组 = %q，期望回退默认值 default", settings.DefaultGroup)
	}

	// 覆盖写入
	if err := repo.Set(ctx, model.SettingKeySiteName, "改名后"); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	if updated, _ := repo.Get(ctx, model.SettingKeySiteName); updated != "改名后" {
		t.Errorf("覆盖后 = %q，期望 改名后", updated)
	}
}

// truncateToDayLocal 把时间截断到当天零点（本地时区），与实现侧保持一致。
func truncateToDayLocal(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
