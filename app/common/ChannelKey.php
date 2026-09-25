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
use Throwable;
use support\Log;

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

        // 先把「冷却到期」的 Key 放出来。
        // 放在取用之前而不是起一个定时任务，是因为定时任务在多进程下
        // 要么重复执行、要么需要额外的调度机制；而这里反正是要查库的，
        // 顺手一条 UPDATE 就完成了自治愈，成本几乎为零。
        self::reviveCooled($channelId);

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
     * 记录一次失败，并按配置决定是否停用这把 Key。
     *
     * 两条不同的处置路径，对应两类完全不同的故障：
     *
     *   1. **永久性失败**（401/403：Key 本身无效/无权限）
     *      → **立即停用**，且不设恢复时间。等再久也不会变好，只能人工换 Key。
     *      不设阈值的理由：这是确定性结论，不是概率问题。
     *
     *   2. **瞬时失败**（429 限流、5xx、网络抖动）
     *      → 只累加计数；连续失败达到 `key_pool.fail_threshold` 且开启了
     *        `key_pool.auto_disable` 时，停用 `key_pool.cooldown_seconds` 秒。
     *        到期由 reviveCooled() 自动放出来再试。
     *
     * ⚠️ 把第 2 类当成第 1 类处理是本项目明确禁止的做法：
     *    上游抖动一次就会把几百把本来好好的 Key 全部永久停用，
     *    而站长完全不知道发生了什么。这条原则写进了项目文档。
     *
     * @param bool $permanent 是否为「永久性失败」（如 401/403：Key 本身无效）
     */
    public static function markFailure(int $id, string $error, bool $permanent): void
    {
        $now = time();
        $message = mb_substr($error, 0, 450);

        if ($permanent) {
            Db::execute(
                'UPDATE channel_keys
                 SET fail_count = fail_count + 1, last_error = ?, status = ?, disabled_until = NULL, updated_at = ?
                 WHERE id = ?',
                [$message, self::STATUS_DISABLED, $now, $id]
            );

            return;
        }

        // 瞬时失败：先累加计数
        Db::execute(
            'UPDATE channel_keys SET fail_count = fail_count + 1, last_error = ?, updated_at = ? WHERE id = ?',
            [$message, $now, $id]
        );

        if (!Settings::bool('key_pool.auto_disable', true)) {
            return;
        }

        $threshold = max(1, Settings::int('key_pool.fail_threshold', 5));

        // 用带条件的 UPDATE 一次性完成「判断是否达到阈值 + 停用」，
        // 而不是先 SELECT 再判断再 UPDATE ——
        // 后者在并发下会漏掉或重复处置（与 tryAcquire 同样的原子性理由）
        $cooldown = max(0, Settings::int('key_pool.cooldown_seconds', 600));

        Db::execute(
            'UPDATE channel_keys
             SET status = ?, disabled_until = ?, updated_at = ?
             WHERE id = ? AND status = ? AND fail_count >= ?',
            [
                self::STATUS_DISABLED,
                $cooldown > 0 ? $now + $cooldown : null,
                $now,
                $id,
                self::STATUS_ENABLED,
                $threshold,
            ]
        );
    }

    /**
     * 把「冷却到期」的 Key 重新启用。
     *
     * 只碰 disabled_until 非空且已到期的行：
     *   · 永久停用的 Key（disabled_until 为 NULL）不会被误放出来；
     *   · 站长手工停用的 Key（disabled_until 也是 NULL）同样不受影响。
     *
     * 启用时清零 fail_count，让它重新获得完整的失败预算 ——
     * 否则一把老 Key 刚放出来就因为历史计数再次被停用，冷却就成了摆设。
     */
    public static function reviveCooled(?int $channelId = null): int
    {
        $now = time();

        $sql = 'UPDATE channel_keys
                SET status = ?, disabled_until = NULL, fail_count = 0, updated_at = ?
                WHERE status = ? AND disabled_until IS NOT NULL AND disabled_until <= ?';

        $bindings = [self::STATUS_ENABLED, $now, self::STATUS_DISABLED, $now];

        if ($channelId !== null) {
            $sql .= ' AND channel_id = ?';
            $bindings[] = $channelId;
        }

        return Db::execute($sql, $bindings);
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
     * @return array{total:int, enabled:int, disabled:int, exhausted:int, cooling:int}
     */
    public static function statsForChannel(int $channelId): array
    {
        $stats = self::statsForChannels()[$channelId] ?? null;

        return $stats ?? ['total' => 0, 'enabled' => 0, 'disabled' => 0, 'exhausted' => 0, 'cooling' => 0];
    }

    /**
     * 一次性统计所有渠道的密钥池（供列表页使用）。
     *
     * 做成批量查询而不是「每个渠道查一次」，是因为渠道数量可能不少，
     * 逐个查会产生 N+1 查询 —— 列表页一刷新就是几百次数据库往返。
     *
     * disabled 与 cooling 的关系：cooling 是 disabled 的子集。
     * 分开统计是为了让站长能分辨「这把 Key 是真的坏了」还是
     * 「只是被冷却了、过一会儿会自动回来」—— 两者的处置动作完全不同。
     *
     * @return array<int, array{total:int, enabled:int, disabled:int, exhausted:int, cooling:int}>
     */
    public static function statsForChannels(): array
    {
        $window = self::currentWindow();

        $rows = Db::select(
            'SELECT channel_id,
                    COUNT(*) AS total,
                    SUM(CASE WHEN status = 1 THEN 1 ELSE 0 END) AS enabled,
                    SUM(CASE WHEN status = 0 THEN 1 ELSE 0 END) AS disabled,
                    SUM(CASE WHEN status = 0 AND disabled_until IS NOT NULL THEN 1 ELSE 0 END) AS cooling,
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
                'cooling' => (int) $row['cooling'],
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
     *
     * 两个细节：
     *   · **启用时清掉 disabled_until**。这是站长的明确意志，
     *     必须压过自动冷却 —— 否则会出现「刚点启用，冷却时间一到又被按回去」
     *     的诡异现象。
     *   · **停用时也清掉 disabled_until**，让它变成「永久停用」，
     *     不会被 reviveCooled() 自动放出来。
     */
    public static function setStatus(int $id, int $status): void
    {
        Db::execute(
            'UPDATE channel_keys SET status = ?, disabled_until = NULL, updated_at = ? WHERE id = ?',
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

    // ═══════════════════════════════════════════════════════════
    // 额度账本（上游给的余额 + 本地记账）
    // ═══════════════════════════════════════════════════════════

    /** 剩余额度低于总额的这个比例就标黄提醒（0.2 = 剩两成） */
    public const BUDGET_WARN_RATIO = 0.2;

    /**
     * 额度耗尽后自动停用的原因标记（写进 last_error，后台据此显示原因）。
     *
     * 为什么要把原因写进 last_error 而不是新加一列：后台密钥列表本来就要显示
     * 「上一行错误」，把原因放在那里，站长在一屏里就能同时看到
     * 「这把为什么停了」与「什么时候停的」。
     */
    public const BUDGET_EXHAUSTED_NOTE = '额度已用完（本地记账），已自动停用；补录额度后会自动恢复';

    /**
     * 记一笔上游成本到某把密钥的账上，额度用完就自动停用。
     *
     * ═══ 为什么必须本地记账 ═══
     *
     * 两家上游（硅基流动 / TierFlow）都**没有**「按 API Key 读余额」的接口，
     * 所以「还剩多少钱」只能靠：人工录入初始额度 + 每次调用按真实 usage 累加。
     * 这不是一个漂亮的方案，但它是唯一能提前预警的方案 ——
     * 否则只能等上游开始拒绝请求（41x）才知道钱花完了。
     *
     * ═══ 停用而不是删除 ═══
     *
     * 停用只是让它不参与密钥池消费，**密钥本身、历史用量、账目全都保留**。
     * 站长的原话是「只是单纯让密钥不进入密钥池消费」——
     * 所以补录额度后应当能原样恢复，而不是要重新导入一把新 Key。
     */
    public static function addCost(int $id, float $cost): void
    {
        if ($id <= 0 || $cost <= 0) {
            return;
        }

        try {
            Db::execute(
                'UPDATE channel_keys SET budget_used = budget_used + ?, updated_at = ? WHERE id = ?',
                [number_format($cost, 10, '.', ''), time(), $id]
            );
        } catch (Throwable $e) {
            Log::error('密钥额度累加失败：key_id=' . $id . ' —— ' . $e->getMessage());

            return;
        }

        $row = self::find($id);

        if ($row === null || !self::exhausted($row) || (int) $row['status'] !== self::STATUS_ENABLED) {
            return;
        }

        // 停用（disabled_until 置空 = 永久停用，不会被冷却恢复逻辑自动放出来）
        Db::execute(
            'UPDATE channel_keys SET status = ?, budget_disabled = 1, disabled_until = NULL, last_error = ?, updated_at = ?
             WHERE id = ?',
            [self::STATUS_DISABLED, self::BUDGET_EXHAUSTED_NOTE, time(), $id]
        );

        Log::warning(sprintf(
            '密钥池 #%d 额度已用完（已用 %s / 总额 %s），已自动停用并退出密钥池（数据保留）',
            $id,
            $row['budget_used'] ?? '?',
            $row['budget_total'] ?? '?'
        ));
    }

    /**
     * 这把密钥的额度是否已经用完。
     *
     * 总额为 0 表示「没设额度」= 不限额、永不算耗尽 ——
     * 现有那些不限额度的密钥（NIM 免费线路）不能因为这一功能被停掉。
     *
     * @param array<string, mixed> $row
     */
    public static function exhausted(array $row): bool
    {
        $total = (float) ($row['budget_total'] ?? 0);

        if ($total <= 0) {
            return false;
        }

        return (float) ($row['budget_used'] ?? 0) >= $total;
    }

    /**
     * 录入 / 追加额度。
     *
     * 「追加」的语义（而不是「覆盖」）：上游给的是新的一笔钱，
     * 把 budget_total 加上去，已用部分不变 —— 这样账目连续，也和真实一致。
     * 录完如果额度已经够用，且这把密钥是「因耗尽被自动停用」的，就自动恢复启用。
     *
     * @return array{ok:bool, message:string}
     */
    public static function addBudget(int $id, float $amount, string $note = ''): array
    {
        $row = self::find($id);

        if ($row === null) {
            return ['ok' => false, 'message' => '密钥不存在'];
        }

        if ($amount <= 0) {
            return ['ok' => false, 'message' => '追加的额度必须大于 0'];
        }

        $total = (float) $row['budget_total'] + $amount;
        $used = (float) $row['budget_used'];
        $note = mb_substr(trim($note), 0, 255);

        $prefix = trim((string) ($row['budget_note'] ?? ''));
        $merged = $prefix === ''
            ? $note
            : ($note === '' ? $prefix : $prefix . ' / ' . $note);

        Db::execute(
            'UPDATE channel_keys SET budget_total = ?, budget_note = ?, updated_at = ? WHERE id = ?',
            [number_format($total, 10, '.', ''), $merged, time(), $id]
        );

        // 因耗尽被停用、而现在额度又够了 → 自动恢复
        if ((int) ($row['budget_disabled'] ?? 0) === 1 && $used < $total) {
            Db::execute(
                'UPDATE channel_keys SET status = ?, budget_disabled = 0, last_error = NULL, updated_at = ? WHERE id = ?',
                [self::STATUS_ENABLED, time(), $id]
            );

            return [
                'ok' => true,
                'message' => sprintf(
                    '已追加 %s 元额度（现有总额 %s 元，已用 %s 元），这把密钥已自动恢复启用',
                    $amount,
                    $total,
                    $used
                ),
            ];
        }

        return [
            'ok' => true,
            'message' => sprintf(
                '已追加 %s 元额度（现有总额 %s 元，已用 %s 元，剩余 %s 元）',
                $amount,
                $total,
                $used,
                max(0, $total - $used)
            ),
        ];
    }

    /**
     * 记下上游口径的读数（TierFlow 的 /v1/dashboard/billing/usage）。
     *
     * 这个数字**不是余额**，而是「上游自己记的累计用量」—— 它的价值在于**对账**：
     * 和我们本地记账的数字并排放着，一眼能看出计价口径有没有错。
     * 上游没这个接口的（硅基流动）就一直是 0，页面据此不显示这一列。
     */
    public static function setReported(int $id, float $reported): void
    {
        Db::execute(
            'UPDATE channel_keys SET budget_reported = ?, budget_reported_at = ?, updated_at = ? WHERE id = ?',
            [number_format($reported, 10, '.', ''), time(), time(), $id]
        );
    }

    /**
     * 额度看板：所有设了额度的密钥，带剩余、消耗速率与预计可用天数。
     *
     * 消耗速率按**最近 7 天**的实际成本折算成「元/天」。为什么不按全部历史：
     * 全历史会把「刚上线时没人用」的时段算进去，速率被严重拉低，
     * 于是「还能用 30 天」这句话在用户突然变多时会变成谎话。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function budgetRows(): array
    {
        try {
            // 注意：这里必须取 api_key_enc（掩码是从它算出来的）。
            // 早先误写成一个不存在的 key_mask 列，结果整个查询报错、
            // 被下面的 catch 吞掉 —— 页面上表现为「额度看板是空的」，
            // 看起来像「还没设过额度」，排查起来极费时间。测试已覆盖这一点。
            $rows = Db::select(
                'SELECT k.id, k.channel_id, k.budget_total, k.budget_used, k.budget_note,
                        k.budget_disabled, k.budget_reported, k.budget_reported_at,
                        k.status, k.api_key_enc, k.last_used_at,
                        c.name AS channel_name, c.group_id AS group_id
                 FROM channel_keys k
                 LEFT JOIN channels c ON c.id = k.channel_id
                 WHERE k.budget_total > 0
                 ORDER BY k.budget_used DESC'
            );
        } catch (Throwable) {
            return [];
        }

        $from = time() - 7 * 86400;
        $out = [];

        foreach ($rows as $row) {
            $id = (int) $row['id'];
            $total = (float) $row['budget_total'];
            $used = (float) $row['budget_used'];
            $remain = max(0.0, $total - $used);

            $weekCost = UsageLog::costByKey($id, $from, time());
            $perDay = $weekCost / 7;

            $days = null;
            if ($perDay > 0) {
                $days = $remain / $perDay;
            }

            $out[] = [
                'id' => $id,
                'channelId' => (int) $row['channel_id'],
                'channelName' => (string) ($row['channel_name'] ?? ''),
                'groupId' => (int) ($row['group_id'] ?? 0),
                'groupLabel' => Group::labelOfId((int) ($row['group_id'] ?? 0)),
                'mask' => self::masked($row),
                'total' => $total,
                'used' => $used,
                'remain' => $remain,
                'ratio' => $total > 0 ? min(1.0, $used / $total) : 0.0,
                'perDay' => $perDay,
                'days' => $days,
                'reported' => (float) $row['budget_reported'],
                'reportedAt' => (int) $row['budget_reported_at'],
                // 本地记账 vs 上游读数：差异超过 1% 就值得看一眼，
                // 所以这里直接把差额算好，不让站长自己做减法
                'diff' => (float) $row['budget_reported'] > 0
                    ? (float) $row['budget_reported'] - $used
                    : null,
                'note' => (string) ($row['budget_note'] ?? ''),
                'enabled' => (int) $row['status'] === self::STATUS_ENABLED,
                'autoDisabled' => (int) $row['budget_disabled'] === 1,
                'exhausted' => self::exhausted($row),
                'warn' => $total > 0 && ($remain / $total) <= self::BUDGET_WARN_RATIO,
                'lastUsedAt' => (int) ($row['last_used_at'] ?? 0),
            ];
        }

        return $out;
    }

    /**
     * 清空某个渠道的限流窗口计数之外的额度重算：按用量日志重算已用额度。
     *
     * 用在两种场景：① 换过计价口径后想让账目归零重算；
     * ② 上游账单与本地记账有明显差距、站长手工对账后要一个干净起点。
     *
     * @return float 重算出来的已用额度
     */
    public static function recalcUsed(int $id): float
    {
        $row = self::find($id);

        if ($row === null) {
            return 0.0;
        }

        $sum = Db::selectOne(
            'SELECT COALESCE(SUM(upstream_cost), 0) AS c FROM usage_logs WHERE channel_key_id = ?',
            [$id]
        );

        $used = (float) ($sum['c'] ?? 0);

        Db::execute(
            'UPDATE channel_keys SET budget_used = ?, updated_at = ? WHERE id = ?',
            [number_format($used, 10, '.', ''), time(), $id]
        );

        return $used;
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
