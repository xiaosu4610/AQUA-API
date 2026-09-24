<?php
/**
 * 渠道密钥池
 *
 * 一个渠道可以挂很多把 Key 轮流使用。这是本项目「用大量免费额度
 * 聚合成高并发能力」的核心：例如 399 把 NIM Key × 每把 40 RPM
 * ≈ 每分钟 1.6 万次请求的聚合上限。
 *
 * ═══ 三个关键设计，都是为了解决「多进程」带来的问题 ═══
 *
 * 1. **限流计数器必须放数据库，不能放进程内存**
 *    服务有多个工作进程。如果每个进程各记一份「本分钟用了多少次」，
 *    那么 N 个进程就会各自放行 40 次 —— 实际发出 40N 次请求，
 *    直接超出上游额度。对免费额度来说，超发的代价是 Key 被限流甚至封禁。
 *    所以计数器落在 channel_keys.used_requests 上，全体进程共享。
 *
 * 2. **「判断是否超限」与「占用一个名额」必须是同一条语句**
 *    如果先 SELECT 判断、再 UPDATE 占用，两个进程可能同时通过判断，
 *    这就是典型的竞态。这里用一条带条件的 UPDATE：
 *    只有「当前窗口未用满」时才会改动行，改动行数 = 1 表示抢到名额，
 *    0 表示已被别人抢走 —— 由数据库保证原子性。
 *
 * 3. **轮换按「最久未使用优先」**
 *    如果总是用第一把，第一把会先耗尽额度，其余 Key 闲置。
 *    按 last_used_at 升序取，能把流量摊到所有 Key 上，
 *    既充分利用额度，也避免单把 Key 过高频触发风控。
 *
 * ═══ 关于窗口算法 ═══
 *
 * 用的是**固定窗口**（每分钟一个桶）。它的已知缺点是「跨窗口边界可能瞬时翻倍」，
 * 例如 12:00:59 用满 40 次、12:01:00 又用 40 次，两秒内实际发了 80 次。
 * 换取的是实现简单、单条 SQL 就能完成、跨进程准确。
 * 若将来发现上游对突发特别敏感，再升级为滑动窗口或令牌桶
 * （那时只需改本文件的 acquire()，调用方不受影响）。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;

final class ChannelKey
{
    /** 密钥状态 */
    public const STATUS_ENABLED = 1;
    public const STATUS_DISABLED = 0;

    /**
     * 一次取用时最多尝试几个候选。
     *
     * 取多个是为了应对并发竞争：候选列表是「查询时刻」的快照，
     * 真正占用时可能已被别的进程抢走，这时就试下一个。
     */
    private const ACQUIRE_CANDIDATES = 10;

    /**
     * 批量导入密钥（幂等）。
     *
     * @param array<int, string> $plainKeys 明文 Key 列表（可能含空行/重复）
     * @param int $rpmLimit 每把 Key 的每分钟上限；0 表示沿用渠道或全局默认
     * @return array{added:int, skipped:int, invalid:int} 导入统计
     */
    public static function import(int $channelId, array $plainKeys, int $rpmLimit = 0): array
    {
        $added = 0;
        $skipped = 0;
        $invalid = 0;
        $now = time();

        foreach ($plainKeys as $raw) {
            $key = trim((string) $raw);

            if ($key === '' || strlen($key) < 8) {
                $invalid++;
                continue;
            }

            $hash = self::hashKey($key);

            // 先查是否已存在。这里用「先查后插」而不是「插入后捕获唯一键冲突」，
            // 是为了让正常路径（大量重复导入）不产生异常开销 ——
            // 重复导入几百把 Key 是常见操作，不该靠抛异常来控制流程。
            $exists = Db::selectOne(
                'SELECT id FROM channel_keys WHERE channel_id = ? AND key_hash = ?',
                [$channelId, $hash]
            );

            if ($exists !== null) {
                $skipped++;
                continue;
            }

            try {
                Db::execute(
                    'INSERT INTO channel_keys
                        (channel_id, key_hash, api_key_enc, status, rpm_limit, window_start, used_requests, created_at, updated_at)
                     VALUES (?, ?, ?, ?, ?, 0, 0, ?, ?)',
                    [
                        $channelId,
                        $hash,
                        Crypto::encrypt($key),
                        self::STATUS_ENABLED,
                        $rpmLimit,
                        $now,
                        $now,
                    ]
                );
                $added++;
            } catch (RuntimeException) {
                // 极小概率：两个进程同时导入同一把 Key，唯一约束拦下了一个。
                // 对使用者来说这就是「重复」，计入跳过即可，不是错误。
                $skipped++;
            }
        }

        return ['added' => $added, 'skipped' => $skipped, 'invalid' => $invalid];
    }

    /**
     * 取用一把可用 Key。
     *
     * 返回的数组里带 `plain_key`，调用方用完即弃：
     * **不得写日志、不得返回给前端、不得持久化**。
     *
     * @param int $defaultLimit 渠道或全局的默认每分钟上限
     * @return array<string, mixed>|null null 表示当前无可用的 Key（全部用满或都被停用）
     */
    public static function acquire(int $channelId, int $defaultLimit): ?array
    {
        $window = self::currentWindow();

        $candidates = Db::select(
            'SELECT * FROM channel_keys
             WHERE channel_id = ?
               AND status = 1
               AND (window_start <> ? OR used_requests < CASE WHEN rpm_limit > 0 THEN rpm_limit ELSE ? END)
             ORDER BY last_used_at ASC, id ASC
             LIMIT ' . self::ACQUIRE_CANDIDATES,
            [$channelId, $window, $defaultLimit]
        );

        foreach ($candidates as $candidate) {
            $limit = (int) $candidate['rpm_limit'] > 0
                ? (int) $candidate['rpm_limit']
                : $defaultLimit;

            if (!self::tryAcquire((int) $candidate['id'], $limit)) {
                // 被其它进程抢先占用，试下一个候选
                continue;
            }

            $candidate['plain_key'] = self::plainKey($candidate);
            if ($candidate['plain_key'] === '') {
                // 解密失败（通常是换过 APP_KEY）：停用它并换一把，
                // 否则会一直选中这把「取不出内容」的 Key，导致每次转发都失败
                self::setStatus((int) $candidate['id'], self::STATUS_DISABLED);
                continue;
            }

            return $candidate;
        }

        return null;
    }

    /**
     * 原子地占用一个名额。
     *
     * 这一条 SQL 同时完成两件事：
     *   · 若已进入新的时间窗口 → 计数重置为 1；
     *   · 否则计数 +1，但**仅当还没用满**时才允许改动。
     *
     * 返回值 = 1 表示抢到名额；0 表示窗口已满（或该 Key 被停用）。
     */
    private static function tryAcquire(int $keyId, int $limit): bool
    {
        $window = self::currentWindow();
        $now = time();

        $affected = Db::execute(
            'UPDATE channel_keys
             SET used_requests = CASE WHEN window_start = ? THEN used_requests + 1 ELSE 1 END,
                 window_start = ?,
                 last_used_at = ?,
                 updated_at = ?
             WHERE id = ?
               AND status = 1
               AND (window_start <> ? OR used_requests < ?)',
            [$window, $window, $now, $now, $keyId, $window, $limit]
        );

        return $affected === 1;
    }

    /**
     * 记录一次成功使用（清零失败计数）。
     */
    public static function markSuccess(int $id): void
    {
        Db::execute(
            'UPDATE channel_keys SET fail_count = 0, last_error = NULL, updated_at = ? WHERE id = ?',
            [time(), $id]
        );
    }

    /**
     * 记录一次失败。
     *
     * @param bool $permanent 是否为「永久性失败」（如 401/403：Key 本身无效）。
     *        只有永久性失败才停用 Key。
     *        ⚠️ 这条区分非常重要：若把 429（限流）、5xx、网络抖动也当作永久失败，
     *        上游一次故障就会把几百把 Key 全部停用，而它们其实都是好的 ——
     *        这正是项目文档里「瞬时故障禁止进 retired」那条原则的具体落实。
     */
    public static function markFailure(int $id, string $error, bool $permanent): void
    {
        $now = time();

        if ($permanent) {
            Db::execute(
                'UPDATE channel_keys SET fail_count = fail_count + 1, last_error = ?, status = ?, updated_at = ? WHERE id = ?',
                [mb_substr($error, 0, 450), self::STATUS_DISABLED, $now, $id]
            );

            return;
        }

        Db::execute(
            'UPDATE channel_keys SET fail_count = fail_count + 1, last_error = ?, updated_at = ? WHERE id = ?',
            [mb_substr($error, 0, 450), $now, $id]
        );
    }

    /**
     * 读取某个渠道的全部密钥（不含明文）。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function allForChannel(int $channelId): array
    {
        return Db::select(
            'SELECT * FROM channel_keys WHERE channel_id = ? ORDER BY last_used_at ASC, id ASC',
            [$channelId]
        );
    }

    /**
     * 单个渠道的密钥池统计。
     *
     * @return array{total:int, enabled:int, disabled:int, exhausted:int}
     */
    public static function statsForChannel(int $channelId): array
    {
        $stats = self::statsForChannels()[$channelId] ?? null;

        return $stats ?? ['total' => 0, 'enabled' => 0, 'disabled' => 0, 'exhausted' => 0];
    }

    /**
     * 一次性统计所有渠道的密钥池（供列表页使用）。
     *
     * 做成批量查询而不是「每个渠道查一次」，是因为渠道数量可能不少，
     * 逐个查会产生 N+1 查询 —— 列表页一刷新就是几百次数据库往返。
     *
     * @return array<int, array{total:int, enabled:int, disabled:int, exhausted:int}>
     */
    public static function statsForChannels(): array
    {
        $window = self::currentWindow();

        $rows = Db::select(
            'SELECT channel_id,
                    COUNT(*) AS total,
                    SUM(CASE WHEN status = 1 THEN 1 ELSE 0 END) AS enabled,
                    SUM(CASE WHEN status = 0 THEN 1 ELSE 0 END) AS disabled,
                    SUM(CASE WHEN status = 1 AND window_start = ? AND rpm_limit > 0 AND used_requests >= rpm_limit
                             THEN 1 ELSE 0 END) AS exhausted
             FROM channel_keys
             GROUP BY channel_id',
            [$window]
        );

        $result = [];
        foreach ($rows as $row) {
            $result[(int) $row['channel_id']] = [
                'total' => (int) $row['total'],
                'enabled' => (int) $row['enabled'],
                'disabled' => (int) $row['disabled'],
                'exhausted' => (int) $row['exhausted'],
            ];
        }

        return $result;
    }

    /**
     * 读取单把密钥。
     *
     * @return array<string, mixed>|null
     */
    public static function find(int $id): ?array
    {
        return Db::selectOne('SELECT * FROM channel_keys WHERE id = ?', [$id]);
    }

    /**
     * 启用/停用某把密钥。
     */
    public static function setStatus(int $id, int $status): void
    {
        Db::execute(
            'UPDATE channel_keys SET status = ?, updated_at = ? WHERE id = ?',
            [$status, time(), $id]
        );
    }

    /**
     * 清空某个渠道的限流窗口计数（把「本分钟已用次数」归零）。
     * 用于运维场景：上游恢复后手工解除「已用满」状态。
     */
    public static function resetWindows(int $channelId): void
    {
        Db::execute(
            'UPDATE channel_keys SET window_start = 0, used_requests = 0, updated_at = ? WHERE channel_id = ?',
            [time(), $channelId]
        );
    }

    /**
     * 删除某把密钥。
     */
    public static function delete(int $id): void
    {
        Db::execute('DELETE FROM channel_keys WHERE id = ?', [$id]);
    }

    /**
     * 清空某个渠道的整个密钥池。
     *
     * @return int 删除条数
     */
    public static function deleteForChannel(int $channelId): int
    {
        return Db::execute('DELETE FROM channel_keys WHERE channel_id = ?', [$channelId]);
    }

    /**
     * 取回明文 Key。
     *
     * 这是允许出现明文的两处之一（另一处是 Channel::plainKey）。
     * 调用方必须遵守：只用于构造上游请求，用后即弃。
     */
    public static function plainKey(array $row): string
    {
        $encrypted = (string) ($row['api_key_enc'] ?? '');
        if ($encrypted === '') {
            return '';
        }

        try {
            return Crypto::decrypt($encrypted);
        } catch (RuntimeException $e) {
            // 解密失败通常是 APP_KEY 被换过。这里不抛给上层，
            // 否则整个密钥池列表页都会崩掉；记日志并按「不可用」处理。
            \support\Log::error(
                '密钥池 #' . ($row['id'] ?? '?') . ' 解密失败：' . $e->getMessage()
            );

            return '';
        }
    }

    /**
     * 用于展示的掩码，例如 `nvapi-…9f3a`。
     */
    public static function masked(array $row): string
    {
        return Crypto::mask(self::plainKey($row));
    }

    /**
     * 当前时间窗口的起点（Unix 秒，按分钟对齐）。
     */
    private static function currentWindow(): int
    {
        return (int) (floor(time() / 60) * 60);
    }

    /**
     * 计算用于去重的哈希。
     *
     * 加固定的领域前缀，避免这个哈希在别处被当成通用哈希复用。
     */
    private static function hashKey(string $plain): string
    {
        return hash('sha256', 'aqua-channel-key:' . $plain);
    }
}
