<?php
/**
 * 后台 · 用量与报表
 *
 * 路由：
 *   GET /admin/usage    用量看板（趋势 / 模型 / 用户 / 失败原因 / 明细）
 *
 * ═══ 为什么要有这一页 ═══
 *
 * 之前的仪表盘只回答「今天花了多少」，但站长真正天天要回答的是另外几个问题：
 *   · 这几天是涨还是跌？
 *   · 哪个模型在赚钱、哪个在赔钱？
 *   · 谁在用量、谁在刷？
 *   · 失败的都是什么原因？是不是某个上游挂了？
 * 这些都不是「一个数字」能回答的，所以单独做一页报表。
 *
 * ═══ 口径只有一套 ═══
 *
 * 趋势、模型汇总、用户排行、明细，全部由同一个时间窗（$fromTs ~ $toTs）
 * 与同一个模型过滤条件算出来。任何一处用了别的口径，
 * 页面上就会出现「总数对不上明细」这种最伤信任的错。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Csrf;
use app\common\Settings;
use app\common\UsageLog;
use support\Request;
use support\Response;

class AdminUsageController
{
    private const PER_PAGE = 50;

    /**
     * 时间窗预设。
     *
     * 只给「今天 / 近 7 天 / 近 30 天」三档：
     * 报表页最容易失控的地方就是筛选器越加越多，
     * 而实际天天看的只有这几档。
     */
    private const RANGES = [
        'today' => ['label' => '今天', 'days' => 1],
        '7d' => ['label' => '近 7 天', 'days' => 7],
        '30d' => ['label' => '近 30 天', 'days' => 30],
    ];

    /**
     * GET /admin/usage
     */
    public function index(Request $request): Response
    {
        $range = (string) $request->get('range', '7d');
        if (!isset(self::RANGES[$range])) {
            $range = '7d';
        }
        $days = (int) self::RANGES[$range]['days'];

        $model = trim((string) $request->get('model', ''));
        $page = max(1, (int) $request->get('page', 1));

        // 窗口统一在这里算：[$from, $to) —— 左闭右开，避免「今天 00:00:00」
        // 这类边界被算两次或算不到
        $todayStart = strtotime('today') ?: time();
        $fromTs = $todayStart - ($days - 1) * 86400;
        $toTs = $todayStart + 86400;

        $summary = UsageLog::statsRange($fromTs, $toTs, $model);
        $series = UsageLog::dailySeries($days, $model);

        // 趋势图的高度基准：取窗口内最大的请求数，让柱子在视觉上可比
        $peak = 0;
        foreach ($series as $day) {
            $peak = max($peak, (int) $day['requests']);
        }

        $total = UsageLog::countInRange($fromTs, $toTs, $model);
        $pages = max(1, (int) ceil($total / self::PER_PAGE));
        $page = min($page, $pages);

        return view('admin/usage', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'range' => $range,
            'rangeLabel' => self::RANGES[$range]['label'],
            'ranges' => self::RANGES,
            'days' => $days,
            'fromTs' => $fromTs,
            'toTs' => $toTs,
            'fromText' => date('Y-m-d H:i', $fromTs),
            'toText' => date('Y-m-d H:i', $toTs),
            'model' => $model,
            'summary' => $summary,
            'series' => $series,
            'peak' => max(1, $peak),
            'byModel' => UsageLog::byModel($fromTs, $toTs, 30, $model),
            'byUser' => UsageLog::topUsers($fromTs, $toTs, 15, $model),
            'topErrors' => UsageLog::topErrors($fromTs, $toTs, 8, $model),
            'rows' => $this->detailRows($fromTs, $toTs, $model, $page),
            'page' => $page,
            'pages' => $pages,
            'total' => $total,
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'logRetentionDays' => Settings::int('billing.log_retention_days', 0),
        ], '');
    }

    /**
     * 明细行（模板友好形状）。
     *
     * @return array<int, array<string, mixed>>
     */
    private function detailRows(int $fromTs, int $toTs, string $model, int $page): array
    {
        $rows = [];

        foreach (UsageLog::listInRange($fromTs, $toTs, $model, self::PER_PAGE, ($page - 1) * self::PER_PAGE) as $row) {
            $rows[] = [
                'time' => date('m-d H:i:s', (int) $row['created_at']),
                'model' => (string) $row['model'],
                'channel' => (string) ($row['channel_name'] ?: '—'),
                'userId' => (int) ($row['user_id'] ?? 0),
                'tokens' => (int) $row['total_tokens'],
                'estimated' => (int) $row['usage_estimated'] === 1,
                'stream' => (int) $row['is_stream'] === 1,
                'upstreamCost' => (float) $row['upstream_cost'],
                'downstreamCost' => (float) $row['downstream_cost'],
                'latency' => (int) $row['latency_ms'],
                'status' => (string) $row['status'],
                'error' => (string) ($row['error_message'] ?? ''),
            ];
        }

        return $rows;
    }
}
