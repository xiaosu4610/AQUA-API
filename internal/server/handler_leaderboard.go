// 本文件实现「用量排行榜」接口：付费榜 / 免费榜。
//
// 意图（Why）：
//
//	门户概览页的图表下方需要一张"谁在用、用了多少"的榜单，
//	让站长与用户一眼看出活跃度分布。拆成付费 / 免费两个榜单：
//	  - 付费用户（有已支付订单）与免费用户的使用强度差异巨大，
//	    混排会让免费榜永远被付费用户占据，失去"免费活跃度"的观察价值；
//	  - 两个榜单共用同一套分数口径，便于横向对比。
//
// 流转（Flow）：
//
//	GET /api/leaderboard?days=30
//	  └─ requireCurrentUser（登录可见；管理员可带 ?all=1 看完整榜）
//	       └─ UsageLogs.Leaderboard(窗口) → 分榜 → 归一化打分 → 排序 → Top N
//
// 扩展（Extend）：
//
//	新增榜单维度（如按分组）时：在 store 的 Leaderboard 聚合里加过滤条件，
//	并在此按维度拆榜即可——分数口径不需要动。
package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/score"
)

// leaderboardDefaultDays 是排行榜默认统计窗口（近 30 天）。
const leaderboardDefaultDays = 30

// leaderboardMaxDays 是排行榜允许的最大统计窗口（近 365 天）。
const leaderboardMaxDays = 365

// leaderboardTopN 是每个榜单默认返回的名次上限（展示 Top 20）。
const leaderboardTopN = 20

// leaderboardEntryDTO 是排行榜中一行的对外表示。
type leaderboardEntryDTO struct {
	Rank     int     `json:"rank"`
	UserID   uint64  `json:"user_id"`
	Username string  `json:"username"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Score    float64 `json:"score"`
	// SuccessRate 是该用户窗口内的请求成功率（0~1），与"综合分数"是两个独立维度：
	// 分数衡量用得多不多（请求数 + Token 各半），成功率衡量用得稳不稳。
	// 早期版本只有分数一列，导致用户无法区分"量大"与"稳定"，
	// 因此成功率独立成列。
	SuccessRate  float64 `json:"success_rate"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	// PeakConcurrency 是窗口内峰值并发请求数（估算口径见 model.LeaderboardEntry）。
	PeakConcurrency int64 `json:"peak_concurrency"`
	// IsMe 标记这一行是否属于当前登录用户（前端据此高亮并标注"我"）。
	IsMe bool `json:"is_me"`
}

// leaderboardSectionDTO 是一个榜单（付费榜 / 免费榜）的整体响应。
type leaderboardSectionDTO struct {
	Items []leaderboardEntryDTO `json:"items"`
	// MyRank 是当前登录用户在该榜中的名次；未上榜时为 0。
	// 单独返回而不是依赖 items 里的 IsMe：用户可能不在前 N 名内，
	// 但榜单页仍要能显示"我排第几"。
	MyRank int `json:"my_rank"`
}

// handleLeaderboard 返回用量排行榜（付费榜 + 免费榜）。
//
// 参数：
//   - days：统计窗口天数（默认 30，上限 365）
//   - all：仅管理员可用；传 1 时返回完整榜单（不限前 N 名）
//
// 归一化说明（与 model.LeaderboardEntry.LeaderboardScore 一致）：
// 每个榜单内部独立取"最大请求数 / 最大 token 数"做分母，
// 分数 = 0.5 × 请求归一 + 0.5 × token 归一，取值 [0, 1]。
func (s *Server) handleLeaderboard(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()

	days := leaderboardDefaultDays
	// 统计窗口上限可在后台「运行上限」页调整（默认 365 天）。
	maxDays := s.limitSettingsCached(ctx).LeaderboardMaxDays
	if raw := c.Query("days"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= maxDays {
			days = parsed
		}
	}
	until := time.Now()
	since := until.AddDate(0, 0, -days)

	// 并行发起两个聚合：排行榜明细 + 全站汇总（总请求/总Token/成功率）。
	// 二者互不依赖，并行后总耗时≈较慢的那个，不因新增汇总而变慢。
	type aggResult struct {
		entries []model.LeaderboardEntry
		summary *model.UsageSummary
		err     error
	}
	aggCh := make(chan aggResult, 1)
	go func() {
		entries, err := s.deps.UsageLogs.Leaderboard(ctx, model.UsageLogQuery{
			Since: &since,
			Until: &until,
		})
		if err != nil {
			aggCh <- aggResult{err: err}
			return
		}
		// 全站汇总：与排行榜同一时间窗、同一数据源，口径天然一致。
		// 成功数用 Summary（它内部按 2xx/3xx 判定），成功率 = 成功/总请求。
		summary, err := s.deps.UsageLogs.Summary(ctx, model.UsageLogQuery{
			Since: &since,
			Until: &until,
		})
		aggCh <- aggResult{entries: entries, summary: summary, err: err}
	}()

	agg := <-aggCh
	if agg.err != nil {
		s.respondInternalError(c, "统计用量排行榜失败")
		return
	}
	entries := agg.entries

	// 管理员可查看完整榜单；普通用户只看前 N 名。
	topN := leaderboardTopN
	if user.IsAdmin() && c.Query("all") == "1" {
		topN = 0 // 0 表示不截断
	}

	paid := make([]model.LeaderboardEntry, 0)
	free := make([]model.LeaderboardEntry, 0)
	for _, entry := range entries {
		if entry.Paid {
			paid = append(paid, entry)
		} else {
			free = append(free, entry)
		}
	}

	paidDTO := s.buildLeaderboardSection(paid, user.ID, topN)
	freeDTO := s.buildLeaderboardSection(free, user.ID, topN)

	c.JSON(http.StatusOK, gin.H{
		"range_days": days,
		// 全站汇总（与排行榜同一时间窗、同一数据源）。
		// 成功率 = 成功请求 / 总请求，SuccessRate() 在无请求时返回 0。
		"totals": gin.H{
			"requests":     agg.summary.Requests,
			"tokens":       agg.summary.Tokens,
			"success_rate": agg.summary.SuccessRate(),
			"users":        len(entries),
		},
		"paid":       paidDTO,
		"free":       freeDTO,
		"updated_at": until.Unix(),
	})
}

// buildLeaderboardSection 对一组成绩评分、排序、截断并计算当前用户名次。
//
// 评分委托给 score.ScoreBoard（动态综合评分算法 v2.3）：
// 锚点由本榜全员数据实时算出的分位数决定，因此任何人的用量变化都会
// 重新标定全榜分数——分数是"相对位置"，不是可累加的累计量。
func (s *Server) buildLeaderboardSection(entries []model.LeaderboardEntry, myUserID uint64, topN int) leaderboardSectionDTO {
	section := leaderboardSectionDTO{
		Items: make([]leaderboardEntryDTO, 0),
	}

	if len(entries) == 0 {
		return section
	}

	// 组装评分输入（ID 传用户名：榜单展示的就是账号 ID）。
	inputs := make([]score.Entry, 0, len(entries))
	for _, entry := range entries {
		inputs = append(inputs, score.Entry{
			ID:       entry.Username,
			Tokens:   float64(entry.Tokens),
			Requests: float64(entry.Requests),
		})
	}

	// 动态评分：全量重算，不做增量更新（增量会让锚点与实际数据脱节）。
	result := score.ScoreBoard(inputs)

	// 分数 → 用户 ID 的反查表（score 包只认 ID 字符串，不知道 user_id）。
	scoreToEntry := make(map[string]model.LeaderboardEntry, len(result.Rows))
	for _, entry := range entries {
		scoreToEntry[entry.Username] = entry
	}

	// 我的名次：无论是否被截断都要能回答"我排第几"。
	for _, row := range result.Rows {
		if entry, ok := scoreToEntry[row.ID]; ok && entry.UserID == myUserID {
			section.MyRank = row.Rank
			break
		}
	}

	// 截断后构建 DTO。
	rows := result.Rows
	if topN > 0 && len(rows) > topN {
		rows = rows[:topN]
	}
	section.Items = make([]leaderboardEntryDTO, 0, len(rows))
	for _, row := range rows {
		entry := scoreToEntry[row.ID]
		section.Items = append(section.Items, leaderboardEntryDTO{
			Rank:            row.Rank,
			UserID:          entry.UserID,
			Username:        entry.Username,
			Requests:        entry.Requests,
			Tokens:          entry.Tokens,
			Score:           row.Total,
			SuccessRate:     entry.SuccessRate(),
			AvgLatencyMS:    entry.AvgLatencyMS,
			PeakConcurrency: entry.PeakConcurrency,
			IsMe:            entry.UserID == myUserID,
		})
	}

	return section
}
