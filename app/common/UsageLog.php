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
     * 后台列表用：按时间倒序分页。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function page(int $limit, int $offset, string $model = ''): array
    {
        $limit = max(1, min(200, $limit));

        if ($model === '') {
            return Db::select(
                'SELECT * FROM usage_logs ORDER BY id DESC LIMIT ' . $limit . ' OFFSET ' . $offset
            );
        }

        return Db::select(
            'SELECT * FROM usage_logs WHERE model LIKE ? ORDER BY id DESC LIMIT ' . $limit . ' OFFSET ' . $offset,
            ['%' . $model . '%']
        );
    }

    /** 分页总数 */
    public static function count(string $model = ''): int
    {
        $row = $model === ''
            ? Db::selectOne('SELECT COUNT(*) AS c FROM usage_logs')
            : Db::selectOne('SELECT COUNT(*) AS c FROM usage_logs WHERE model LIKE ?', ['%' . $model . '%']);

        return (int) ($row['c'] ?? 0);
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
                 WHERE created_at >= ?',
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
