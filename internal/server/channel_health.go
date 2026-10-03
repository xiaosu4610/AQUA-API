// 本文件实现「按成功率自动禁用渠道」后台任务。
//
// 意图（Why）：
//
//	上游渠道会因密钥失效、余额耗尽、模型下线等原因持续失败。人工盯后台总有疏漏，
//	而一个持续失败的渠道会让大量请求白白消耗重试预算与响应时间。本任务按调用日志
//	（usage_logs）统计各渠道在最近窗口内的成功率，低于阈值即自动置为「自动禁用」，
//	把"人工发现问题 → 手动停用"压缩成"系统自动止损"。
//
// 流转（Flow）：
//
//	main.go 起 goroutine 周期调用 Server.AutoDisableUnhealthyChannels(ctx)
//	  └─ checkAndAutoDisableChannels
//	       ├─ channelHealthStats：列启用中的渠道 → 逐个 UsageLogs.Summary（窗口内成功/总数）
//	       ├─ shouldAutoDisable：纯函数判定（样本量 + 成功率 + 当前状态）
//	       └─ Channels.GetByID → 置 Status=AutoDisabled → Channels.Update（只停用，不删除）
//
// 扩展（Extend）：
//
//	需要"成功率回升后自动恢复"时：在本文件新增一个方向相反的方法，判定条件用
//	ChannelStatusAutoDisabled（恢复只针对自动禁用的渠道，绝不恢复人工停用的）。
//	需要按渠道类型差异化阈值时：在 ChannelHealthConfig 增加维度，并在此读取。
package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/config"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/notify"
)

// defaultChannelHealthWindow 是统计窗口的兜底默认值。
//
// 仅当配置的 WindowMinutes<=0 时使用；正常由 config.Default 提供一致的默认值。
const defaultChannelHealthWindow = 15 * time.Minute

// channelHealthStat 是单个渠道在统计窗口内的健康度快照（只读计算结果）。
type channelHealthStat struct {
	ChannelID   uint64              // 渠道 ID
	Name        string              // 渠道显示名（用于日志留痕）
	Status      model.ChannelStatus // 快照时刻的状态
	Requests    int64               // 窗口内请求总数
	Success     int64               // 窗口内成功数（2xx）
	Failed      int64               // 窗口内失败数（Requests - Success）
	SuccessRate float64             // 成功率（0~1）；无请求时为 0
}

// AutoDisableUnhealthyChannels 是「按成功率自动禁用渠道」后台任务的周期入口。
//
// 用法：由 main.go 起一个 goroutine，按固定间隔（建议 5 分钟）循环调用本方法；
// ctx 取消后本方法立即返回。这里刻意【不含 sleep 循环】——调度交给调用方，
// 便于测试直接调用一次，也便于将来换成统一的定时器组件。
//
// 默认不启用：MinRequests<=0 时直接返回，保证存量部署升级后行为不变。
func (s *Server) AutoDisableUnhealthyChannels(ctx context.Context) {
	if s.deps.Config == nil {
		return
	}
	cfg := s.deps.Config.ChannelHealth
	if cfg.MinRequests <= 0 {
		// 未启用：这是默认状态，不是错误，故不打日志刷屏
		return
	}
	if s.deps.Channels == nil || s.deps.UsageLogs == nil {
		slog.Warn("渠道健康检查跳过：渠道或调用日志仓储未注入")
		return
	}

	disabled, err := s.checkAndAutoDisableChannels(ctx, cfg)
	if err != nil {
		// 禁止静默吞错：检查失败必须留痕，下一轮会重试。
		slog.Warn("渠道健康检查失败", "error", err)
		return
	}
	if disabled > 0 {
		slog.Info("渠道健康检查完成：已自动停用不健康渠道", "disabled", disabled)
	}
}

// channelHealthStats 按渠道聚合最近 window 内的成功/失败数（只读，无副作用）。
//
// 只统计【启用中】的渠道：自动禁用只处理"当前在服务、但正在持续失败"的渠道；
// 已停用的渠道不重复处置（其失败日志也不再计入判定，避免"停用后仍被反复计数"）。
//
// 实现取舍：复用 UsageLogs.Summary 逐渠道查询，而不是在服务层直接写聚合 SQL——
// 仓储接口是访问日志表的唯一入口（分层约定），且后台任务是低频调用、渠道数量有限，
// 逐渠道查询的开销可以接受，换来的是"日志访问全部走仓储"的一致性。
func (s *Server) channelHealthStats(ctx context.Context, window time.Duration) ([]channelHealthStat, error) {
	if window <= 0 {
		window = defaultChannelHealthWindow
	}

	enabled := model.ChannelStatusEnabled
	channels, err := s.deps.Channels.List(ctx, model.ChannelQuery{Status: &enabled})
	if err != nil {
		return nil, fmt.Errorf("server: 查询启用中的渠道失败: %w", err)
	}

	since := time.Now().Add(-window)
	stats := make([]channelHealthStat, 0, len(channels))
	for _, ch := range channels {
		channelID := ch.ID
		summary, err := s.deps.UsageLogs.Summary(ctx, model.UsageLogQuery{
			ChannelID: &channelID,
			Since:     &since,
		})
		if err != nil {
			return nil, fmt.Errorf("server: 统计渠道 %d 的调用成功率失败: %w", ch.ID, err)
		}
		stats = append(stats, channelHealthStat{
			ChannelID:   ch.ID,
			Name:        ch.Name,
			Status:      ch.Status,
			Requests:    summary.Requests,
			Success:     summary.Success,
			Failed:      summary.Requests - summary.Success,
			SuccessRate: summary.SuccessRate(),
		})
	}
	return stats, nil
}

// shouldAutoDisable 判断某渠道健康度是否满足自动停用条件（纯函数，便于单测）。
//
// 三条安全约束（务必保留）：
//  1. 必须有最小样本数：样本太少时成功率没有统计意义（凌晨整窗可能只有一两次请求），
//     少于阈值一律不判定，避免低峰误杀；
//  2. 只处理【启用中】的渠道：已停用的（人工禁用或自动禁用）不重复处置；
//  3. 阈值比较用严格小于：成功率恰好等于阈值视为达标，不停用。
func shouldAutoDisable(stat channelHealthStat, cfg config.ChannelHealthConfig) bool {
	if cfg.MinRequests <= 0 {
		// 功能关闭
		return false
	}
	if stat.Status != model.ChannelStatusEnabled {
		// 已停用（人工或自动）不重复处置；也避免与管理员的人工操作打架
		return false
	}
	if stat.Requests < int64(cfg.MinRequests) {
		// 样本不足，不判定
		return false
	}
	return stat.SuccessRate < cfg.SuccessRate
}

// checkAndAutoDisableChannels 检查各渠道健康度，低于阈值则置为「自动禁用」。
//
// 返回本轮实际停用的渠道数量，便于调用方决定是否打印汇总日志。
// 停用动作只写 Status 字段，不删除渠道、不动其它配置。
func (s *Server) checkAndAutoDisableChannels(ctx context.Context, cfg config.ChannelHealthConfig) (int, error) {
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	if window <= 0 {
		window = defaultChannelHealthWindow
	}

	stats, err := s.channelHealthStats(ctx, window)
	if err != nil {
		return 0, err
	}

	disabled := 0
	for _, stat := range stats {
		if !shouldAutoDisable(stat, cfg) {
			continue
		}

		// 重新载入完整渠道（Update 需要整行配置），并再次确认状态——
		// 从统计到处置之间有窗口，期间管理员可能已手动停用，避免覆盖其意图。
		ch, err := s.deps.Channels.GetByID(ctx, stat.ChannelID)
		if err != nil {
			return disabled, fmt.Errorf("server: 载入渠道 %d 失败: %w", stat.ChannelID, err)
		}
		if ch.Status != model.ChannelStatusEnabled {
			continue
		}

		ch.Status = model.ChannelStatusAutoDisabled
		if err := s.deps.Channels.Update(ctx, ch); err != nil {
			return disabled, fmt.Errorf("server: 自动停用渠道 %d 失败: %w", stat.ChannelID, err)
		}
		disabled++

		// 留痕：含渠道 id/名称/成功率/样本数/阈值。
		// 用「自动禁用」状态本身区分人工停用（Channels 状态枚举已区分两者），
		// 不额外硬加"停用原因"字段——状态 + 本条 Warn 日志足以还原事实。
		slog.Warn("渠道成功率低于阈值，已自动停用",
			"channel_id", stat.ChannelID,
			"channel_name", stat.Name,
			"success_rate", stat.SuccessRate,
			"requests", stat.Requests,
			"success", stat.Success,
			"failed", stat.Failed,
			"threshold", cfg.SuccessRate,
			"min_requests", cfg.MinRequests,
			"window_minutes", int(window.Minutes()),
		)

		// 外发告警：自动停用意味着这条渠道正在把用户请求打失败，是"立刻要处理"级别。
		// 只留一条 Warn 日志等于要求站长主动翻日志才能发现——而这件事发生在凌晨时，
		// 往往要等到早上用户投诉才被发现。Notifier 为 nil（未装配告警）时 Alert 自行忽略。
		// DedupKey 取渠道维度：同一时刻多个渠道被停用必须各发一条，不能互相抑制。
		s.deps.Notifier.Alert(notify.Alert{
			Key:   model.EventChannelAutoDisabled,
			Level: notify.LevelCritical,
			Title: fmt.Sprintf("渠道「%s」已被自动停用", stat.Name),
			Detail: fmt.Sprintf("最近 %d 分钟内调用成功率 %.1f%%，低于阈值 %.1f%%，已自动停用该渠道。",
				int(window.Minutes()), stat.SuccessRate*100, cfg.SuccessRate*100),
			DedupKey: fmt.Sprintf("channel/%d", stat.ChannelID),
			Fields: []notify.Field{
				{Label: "渠道", Value: fmt.Sprintf("%s（#%d）", stat.Name, stat.ChannelID)},
				{Label: "成功率", Value: fmt.Sprintf("%.1f%%（成功 %d / 共 %d）", stat.SuccessRate*100, stat.Success, stat.Requests)},
				{Label: "阈值", Value: fmt.Sprintf("%.1f%%（样本数下限 %d）", cfg.SuccessRate*100, cfg.MinRequests)},
			},
		})
	}
	return disabled, nil
}
