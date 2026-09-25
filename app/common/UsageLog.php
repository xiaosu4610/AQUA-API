<?php
/**
 * 用量日志
 *
 * 每一次上游请求都会在这里留下一条记录。它是「成本可见」的唯一来源 ——
 * 也是整个项目里最不该省的一张表：价格可以事后调，
 * **日志一旦缺失就无法补算**，上个月的毛利永远算不出来了。
 *
 * ═══ 三条设计取舍 ═══
 *
 * 1. **失败也要记**。上游 429、超时、甚至还没发出请求就失败，
 *    同样写一条（status 标记为 error）。只记成功请求的日志，
 *    会让人误以为「这个渠道很稳」，而实际上它可能一半请求都失败了。
 *
 * 2. **估算值要标记出来**（usage_estimated）。
 *    有些上游不返回 usage，只能按文本估算。估算值必须能一眼分辨 ——
 *    否则站长会拿它去对上游客服的账单，然后发现对不上。
 *
 * 3. **写入用「尽力而为」**。日志写入失败绝不能影响转发本身 ——
 *    用户拿到回答才是第一位的。因此这里吞掉异常并记框架日志。
 */

declare(strict_types=1);

namespace app\common;

use Throwable;
use support\Log;

final class UsageLog
{
    /** 请求状态 */
    public const STATUS_OK = 'ok';
    public const STATUS_ERROR = 'error';

    /**
     * 被本站**拒绝**、根本没发往上游的请求（401/402/403）。
     *
     * ═══ 为什么单独一个状态，而不是并入 error ═══
     *
     * 生产上发生过这样一件事：站长看到大量调用失败，怀疑是上游或本站的服务有问题，
     * 查了半天 —— 真相是几个客户端拿着**早就删掉的令牌**在反复重试。
     * 那几百次请求全都在鉴权阶段就被拒了，一次都没到上游。
     *
     * 如果把它们记成 error，两个数字都会失真：
     *   · 「请求数 / 成功率」被这些脏流量污染（用户会以为服务不稳）；
     *   · 「上游失败原因排行」被它们占满，真正需要盯的上游问题反而不显眼。
     *
     * 所以单独一个状态：它既能在后台被看见（「有 N 次调用被拒，用的令牌是 xxx」），
     * 又不会算进「上游失败」的账里。两个问题各自清楚。
     */
    public const STATUS_REJECTED = 'rejected';

    /**
     * 后台报表的口径：**只算真正发往上游的调用**。
     *
     * 被本站拒绝的调用（status=rejected：令牌无效、余额不足、模型无权限）
     * 一次都没到上游。把它们算进「请求数 / 成交额」会让报表失真 ——
     * 站长会以为服务不稳，而真相往往是有人拿着错的令牌在反复重试。
     * 它们由「被拒调用」单列一块（见 rejectedStats），两边互不干扰。
     *
     * ⚠️ 用户自己的控制台**不排除**它们（那里显示的是「我发起过的调用」，
     *    用户需要看到自己被拒的那几次），所以下面的排除只加在后台汇总方法上。
     */
    private const EXCLUDE_REJECTED = "status <> 'rejected'";

    /**
     * 写入一条用量记录。
     *
     * @param array{
     *     model:string, channel_id?:int, channel_name?:string,
     *     user_id?:int, token_id?:int,
     *     prompt_tokens?:int, completion_tokens?:int, usage_estimated?:bool,
     *     upstream_cost?:float, downstream_cost?:float,
     *     billing_mode?:string, is_stream?:bool,
     *     latency_ms?:int, status?:string, error?:string
     * } $data
     */
    public static function record(array $data): void
    {
        $prompt = max(0, (int) ($data['prompt_tokens'] ?? 0));
        $completion = max(0, (int) ($data['completion_tokens'] ?? 0));

        try {
            Db::execute(
                'INSERT INTO usage_logs
                    (created_at, user_id, token_id, channel_id, channel_name, model,
                     billing_mode, is_stream, prompt_tokens, completion_tokens, total_tokens,
                     usage_estimated, upstream_cost, downstream_cost, latency_ms, status, error_message)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
                [
                    time(),
                    $data['user_id'] ?? null,
                    $data['token_id'] ?? null,
                    $data['channel_id'] ?? null,
                    isset($data['channel_name']) ? mb_substr((string) $data['channel_name'], 0, 128) : null,
                    mb_substr((string) ($data['model'] ?? ''), 0, 191),
                    (string) ($data['billing_mode'] ?? Pricing::MODE_TOKEN),
                    !empty($data['is_stream']) ? 1 : 0,
                    $prompt,
                    $completion,
                    $prompt + $completion,
                    !empty($data['usage_estimated']) ? 1 : 0,
                    number_format((float) ($data['upstream_cost'] ?? 0), 10, '.', ''),
                    number_format((float) ($data['downstream_cost'] ?? 0), 10, '.', ''),
                    max(0, (int) ($data['latency_ms'] ?? 0)),
                    (string) ($data['status'] ?? self::STATUS_OK),
                    isset($data['error']) ? mb_substr((string) $data['error'], 0, 450) : null,
                ]
            );
        } catch (Throwable $e) {
            // 记账失败不能拖垮转发 —— 记一条框架日志即可
            Log::error('用量日志写入失败：' . $e->getMessage());
        }
    }

    /**
     * 后台明细列表：指定时间窗内按时间倒序分页。
     *
     * 必须带时间窗，而不是「全表倒序分页」：报表页上选了「今天」，
     * 明细却混进昨天的记录，站长一眼就会认为数字算错了 ——
     * 报表和明细必须是同一个口径。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function listInRange(int $fromTs, int $toTs, string $model = '', int $limit = 50, int $offset = 0): array
    {
        $limit = max(1, min(200, $limit));
        $offset = max(0, $offset);

        $sql = 'SELECT * FROM usage_logs WHERE created_at >= ? AND created_at < ? AND ' . self::EXCLUDE_REJECTED;
        $bindings = [$fromTs, $toTs];

        if ($model !== '') {
            $sql .= ' AND model LIKE ?';
            $bindings[] = '%' . $model . '%';
        }

        return Db::select($sql . ' ORDER BY id DESC LIMIT ' . $limit . ' OFFSET ' . $offset, $bindings);
    }

    /** 指定时间窗内的记录条数（分页用） */
    public static function countInRange(int $fromTs, int $toTs, string $model = ''): int
    {
        $sql = 'SELECT COUNT(*) AS c FROM usage_logs WHERE created_at >= ? AND created_at < ? AND ' . self::EXCLUDE_REJECTED;
        $bindings = [$fromTs, $toTs];

        if ($model !== '') {
            $sql .= ' AND model LIKE ?';
            $bindings[] = '%' . $model . '%';
        }

        $row = Db::selectOne($sql, $bindings);

        return (int) ($row['c'] ?? 0);
    }

    /**
     * 某个用户的用量汇总（用户控制台用）。
     *
     * 与 summary() 同样的聚合 SQL，只是多一个 user_id 条件。
     * 不合并成一个方法是因为那样每个调用点都要传一堆可选参数，
     * 反而更容易传错。
     *
     * @return array{requests:int, prompt_tokens:int, completion_tokens:int,
     *               upstream_cost:float, downstream_cost:float, profit:float,
     *               errors:int, estimated:int}
     */
    public static function summaryForUser(int $userId, int $sinceTs): array
    {
        $empty = [
            'requests' => 0, 'prompt_tokens' => 0, 'completion_tokens' => 0,
            'upstream_cost' => 0.0, 'downstream_cost' => 0.0, 'profit' => 0.0,
            'errors' => 0, 'estimated' => 0,
        ];

        try {
            $row = Db::selectOne(
                'SELECT
                    COUNT(*) AS requests,
                    SUM(prompt_tokens) AS prompt_tokens,
                    SUM(completion_tokens) AS completion_tokens,
                    SUM(upstream_cost) AS upstream_cost,
                    SUM(downstream_cost) AS downstream_cost,
                    SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS errors,
                    SUM(usage_estimated) AS estimated
                 FROM usage_logs
                 WHERE user_id = ? AND created_at >= ?',
                [self::STATUS_ERROR, $userId, $sinceTs]
            );
        } catch (Throwable) {
            return $empty;
        }

        if ($row === null || $row['requests'] === null) {
            return $empty;
        }

        $upstream = (float) $row['upstream_cost'];
        $downstream = (float) $row['downstream_cost'];

        return [
            'requests' => (int) $row['requests'],
            'prompt_tokens' => (int) $row['prompt_tokens'],
            'completion_tokens' => (int) $row['completion_tokens'],
            'upstream_cost' => $upstream,
            'downstream_cost' => $downstream,
            'profit' => $downstream - $upstream,
            'errors' => (int) $row['errors'],
            'estimated' => (int) $row['estimated'],
        ];
    }

    /**
     * 某个用户最近的用量记录。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function recentForUser(int $userId, int $limit = 20): array
    {
        return Db::select(
            'SELECT * FROM usage_logs WHERE user_id = ? ORDER BY id DESC LIMIT ' . max(1, min(100, $limit)),
            [$userId]
        );
    }

    /**
     * 某时间点以来的汇总（仪表盘用）。
     *
     * 用一条聚合 SQL 而不是把记录取回 PHP 再算：
     * 日志表会迅速长到几十万行，取回内存是不可行的。
     *
     * @param int $sinceTs 起始 Unix 时间戳
     * @return array{requests:int, prompt_tokens:int, completion_tokens:int,
     *               upstream_cost:float, downstream_cost:float, profit:float,
     *               errors:int, estimated:int}
     */
    public static function summary(int $sinceTs): array
    {
        $empty = [
            'requests' => 0,
            'prompt_tokens' => 0,
            'completion_tokens' => 0,
            'upstream_cost' => 0.0,
            'downstream_cost' => 0.0,
            'profit' => 0.0,
            'errors' => 0,
            'estimated' => 0,
        ];

        try {
            $row = Db::selectOne(
                'SELECT
                    COUNT(*) AS requests,
                    SUM(prompt_tokens) AS prompt_tokens,
                    SUM(completion_tokens) AS completion_tokens,
                    SUM(upstream_cost) AS upstream_cost,
                    SUM(downstream_cost) AS downstream_cost,
                    SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS errors,
                    SUM(usage_estimated) AS estimated
                 FROM usage_logs
                 WHERE created_at >= ? AND ' . self::EXCLUDE_REJECTED,
                [self::STATUS_ERROR, $sinceTs]
            );
        } catch (Throwable) {
            // 表还没建好（首次启动时序）时不该让仪表盘崩掉
            return $empty;
        }

        if ($row === null || $row['requests'] === null) {
            return $empty;
        }

        $upstream = (float) $row['upstream_cost'];
        $downstream = (float) $row['downstream_cost'];

        return [
            'requests' => (int) $row['requests'],
            'prompt_tokens' => (int) $row['prompt_tokens'],
            'completion_tokens' => (int) $row['completion_tokens'],
            'upstream_cost' => $upstream,
            'downstream_cost' => $downstream,
            'profit' => $downstream - $upstream,
            'errors' => (int) $row['errors'],
            'estimated' => (int) $row['estimated'],
        ];
    }

    /**
     * 一段区间（左闭右开）的汇总，比 summary() 多两项：
     * 平均耗时与流式请求数 —— 报表里最常被问的就是「慢不慢」。
     *
     * 之所以要「区间」而不只是「起点之后」：按天拆报表时，
     * 每一天都要用自己的边界去查，不能拿「起点之后」糊过去。
     *
     * @return array{requests:int, prompt_tokens:int, completion_tokens:int,
     *               upstream_cost:float, downstream_cost:float, profit:float,
     *               errors:int, estimated:int, streams:int, avg_latency_ms:int}
     */
    public static function statsRange(int $fromTs, int $toTs, string $model = ''): array
    {
        $empty = [
            'requests' => 0, 'prompt_tokens' => 0, 'completion_tokens' => 0,
            'upstream_cost' => 0.0, 'downstream_cost' => 0.0, 'profit' => 0.0,
            'errors' => 0, 'estimated' => 0, 'streams' => 0, 'avg_latency_ms' => 0,
        ];

        $sql = 'SELECT
                    COUNT(*) AS requests,
                    SUM(prompt_tokens) AS prompt_tokens,
                    SUM(completion_tokens) AS completion_tokens,
                    SUM(upstream_cost) AS upstream_cost,
                    SUM(downstream_cost) AS downstream_cost,
                    SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS errors,
                    SUM(usage_estimated) AS estimated,
                    SUM(is_stream) AS streams,
                    AVG(latency_ms) AS avg_latency
                FROM usage_logs
                WHERE created_at >= ? AND created_at < ? AND ' . self::EXCLUDE_REJECTED;

        $bindings = [self::STATUS_ERROR, $fromTs, $toTs];
        if ($model !== '') {
            $sql .= ' AND model LIKE ?';
            $bindings[] = '%' . $model . '%';
        }

        try {
            $row = Db::selectOne($sql, $bindings);
        } catch (Throwable) {
            return $empty;
        }

        if ($row === null || $row['requests'] === null) {
            return $empty;
        }

        $upstream = (float) $row['upstream_cost'];
        $downstream = (float) $row['downstream_cost'];

        return [
            'requests' => (int) $row['requests'],
            'prompt_tokens' => (int) $row['prompt_tokens'],
            'completion_tokens' => (int) $row['completion_tokens'],
            'upstream_cost' => $upstream,
            'downstream_cost' => $downstream,
            'profit' => $downstream - $upstream,
            'errors' => (int) $row['errors'],
            'estimated' => (int) $row['estimated'],
            'streams' => (int) $row['streams'],
            'avg_latency_ms' => (int) round((float) ($row['avg_latency'] ?? 0)),
        ];
    }

    /**
     * 按天拆分的报表数据（含今天，倒序返回最近 $days 天）。
     *
     * ⚠️ 刻意在 PHP 里算好每天的边界，再逐天查，而不是用数据库的
     * `strftime` / `DATE(FROM_UNIXTIME())` 分组：
     * 两家数据库的日期函数不通用，而且它们各自按**数据库会话的时区**解释，
     * 页面上用 PHP 的 date() 显示时间 —— 两边时区不一致时，
     * 「凌晨那几小时的请求」会被算到前一天，报表与明细对不上。
     * 一天的边界由 PHP 决定，两家数据库都只能按时间戳区间去查，不会错。
     *
     * 代价是每天一次查询（30 天 = 30 次）。走 created_at 索引，代价可忽略。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function dailySeries(int $days, string $model = ''): array
    {
        $days = max(1, min(90, $days));
        $today = strtotime('today') ?: time();
        $series = [];

        for ($i = $days - 1; $i >= 0; $i--) {
            $from = $today - $i * 86400;
            $to = $from + 86400;
            $row = self::statsRange($from, $to, $model);
            $row['date'] = date('Y-m-d', $from);
            $row['label'] = date('m-d', $from);
            $row['weekday'] = ['日', '一', '二', '三', '四', '五', '六'][(int) date('w', $from)];
            $series[] = $row;
        }

        return $series;
    }

    /**
     * 按模型聚合（报表用）。
     *
     * 报表页上的每个数字都吃同一个筛选条件（含模型名过滤）：
     * 页面上有一块没跟着筛，就会出现「总数与明细对不上」——
     * 那是报表最不能有的毛病。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function byModel(int $fromTs, int $toTs, int $limit = 30, string $model = ''): array
    {
        $bindings = [self::STATUS_ERROR, $fromTs, $toTs];
        $filter = '';
        if ($model !== '') {
            $filter = ' AND model LIKE ?';
            $bindings[] = '%' . $model . '%';
        }

        $rows = Db::select(
            'SELECT model,
                    COUNT(*) AS requests,
                    SUM(prompt_tokens) AS prompt_tokens,
                    SUM(completion_tokens) AS completion_tokens,
                    SUM(total_tokens) AS total_tokens,
                    SUM(upstream_cost) AS upstream_cost,
                    SUM(downstream_cost) AS downstream_cost,
                    SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS errors,
                    SUM(usage_estimated) AS estimated,
                    AVG(latency_ms) AS avg_latency
             FROM usage_logs
             WHERE created_at >= ? AND created_at < ?' . $filter . ' AND ' . self::EXCLUDE_REJECTED . '
             GROUP BY model
             ORDER BY requests DESC LIMIT ' . max(1, min(200, $limit)),
            $bindings
        );

        return array_map(static function (array $row): array {
            $upstream = (float) $row['upstream_cost'];
            $downstream = (float) $row['downstream_cost'];
            $requests = (int) $row['requests'];

            return [
                'model' => (string) $row['model'],
                'requests' => $requests,
                'prompt_tokens' => (int) $row['prompt_tokens'],
                'completion_tokens' => (int) $row['completion_tokens'],
                'total_tokens' => (int) $row['total_tokens'],
                'upstream_cost' => $upstream,
                'downstream_cost' => $downstream,
                'profit' => $downstream - $upstream,
                'errors' => (int) $row['errors'],
                'estimated' => (int) $row['estimated'],
                'success_rate' => $requests > 0 ? (int) round(($requests - (int) $row['errors']) * 100 / $requests) : 100,
                'avg_latency_ms' => (int) round((float) ($row['avg_latency'] ?? 0)),
            ];
        }, $rows);
    }

    /**
     * 失败请求的原因排行（报表用）。
     *
     * 为什么按 error_message 分组而不是按状态码：上游把状态码藏在
     * 各种包装里，真正能指向问题的是那句原文（例如 「monthly quota exceeded」）。
     *
     * @param bool $includeRejected 是否把「被本站拒绝、没发往上游」的请求也算进来。
     *        默认不算 —— 它们不是上游故障；但排查「为什么用户都说用不了」时
     *        必须一起看，否则会漏掉「一大批人在用错的令牌」这个最常见的真相
     * @return array<int, array{message:string, count:int}>
     */
    public static function topErrors(int $fromTs, int $toTs, int $limit = 10, string $model = '', bool $includeRejected = false): array
    {
        $statuses = $includeRejected
            ? [self::STATUS_ERROR, self::STATUS_REJECTED]
            : [self::STATUS_ERROR];
        $placeholders = implode(', ', array_fill(0, count($statuses), '?'));

        $bindings = $statuses;
        $bindings[] = $fromTs;
        $bindings[] = $toTs;

        $filter = '';
        if ($model !== '') {
            $filter = ' AND model LIKE ?';
            $bindings[] = '%' . $model . '%';
        }

        $rows = Db::select(
            'SELECT error_message AS message, COUNT(*) AS c, MAX(status) AS st
             FROM usage_logs
             WHERE status IN (' . $placeholders . ') AND created_at >= ? AND created_at < ? AND error_message IS NOT NULL
                   AND error_message <> \'\'' . $filter . '
             GROUP BY error_message ORDER BY c DESC LIMIT ' . max(1, min(50, $limit)),
            $bindings
        );

        return array_map(
            static fn (array $row): array => [
                'message' => (string) $row['message'],
                'count' => (int) $row['c'],
                'rejected' => ($row['st'] ?? '') === self::STATUS_REJECTED,
            ],
            $rows
        );
    }

    /**
     * 指定时间窗内「被本站拒绝」的调用条数与原因分布（报表用）。
     *
     * 这是回答「为什么用户说调不通」的第一手材料：
     * 数字一旦上去，就说明有客户端在用过期的 / 错的令牌反复重试。
     *
     * @return array{count:int, reasons:array<int, array{message:string, count:int}>}
     */
    public static function rejectedStats(int $fromTs, int $toTs, int $limit = 5): array
    {
        try {
            $row = Db::selectOne(
                'SELECT COUNT(*) AS c FROM usage_logs WHERE status = ? AND created_at >= ? AND created_at < ?',
                [self::STATUS_REJECTED, $fromTs, $toTs]
            );
            $count = (int) ($row['c'] ?? 0);
        } catch (Throwable) {
            return ['count' => 0, 'reasons' => []];
        }

        return ['count' => $count, 'reasons' => self::topErrors($fromTs, $toTs, $limit, '', true)];
    }

    /**
     * 下游用户用量排行（报表用）。
     *
     * 已注销的用户在 usage_logs 里的归属是 NULL，会并入「（已注销）」一行 ——
     * 账要对得上，就不能让这部分金额凭空消失。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function topUsers(int $fromTs, int $toTs, int $limit = 20, string $model = ''): array
    {
        $bindings = [self::STATUS_ERROR, $fromTs, $toTs];
        $filter = '';
        if ($model !== '') {
            $filter = ' AND model LIKE ?';
            $bindings[] = '%' . $model . '%';
        }

        $rows = Db::select(
            'SELECT user_id, COUNT(*) AS requests, SUM(total_tokens) AS total_tokens,
                    SUM(upstream_cost) AS upstream_cost, SUM(downstream_cost) AS downstream_cost,
                    SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS errors
             FROM usage_logs
             WHERE created_at >= ? AND created_at < ?' . $filter . ' AND ' . self::EXCLUDE_REJECTED . '
             GROUP BY user_id ORDER BY requests DESC LIMIT ' . max(1, min(100, $limit)),
            $bindings
        );

        $result = [];
        foreach ($rows as $row) {
            $userId = (int) ($row['user_id'] ?? 0);
            $user = $userId > 0 ? User::find($userId) : null;

            $result[] = [
                'userId' => $userId,
                'email' => $user === null
                    ? ($userId > 0 ? "（已不存在的编号 {$userId}）" : '（已注销的用户）')
                    : (string) $user['email'],
                'requests' => (int) $row['requests'],
                'total_tokens' => (int) $row['total_tokens'],
                'upstream_cost' => (float) $row['upstream_cost'],
                'downstream_cost' => (float) $row['downstream_cost'],
                'profit' => (float) $row['downstream_cost'] - (float) $row['upstream_cost'],
                'errors' => (int) $row['errors'],
            ];
        }

        return $result;
    }

    /**
     * 清理过期日志。
     *
     * 日志表是唯一会无限增长的表，必须有回收手段，
     * 否则几个月后单表几百万行、备份和查询都会变慢。
     *
     * @return int 删除条数
     */
    public static function purgeBefore(int $beforeTs): int
    {
        return Db::execute('DELETE FROM usage_logs WHERE created_at < ?', [$beforeTs]);
    }
}
