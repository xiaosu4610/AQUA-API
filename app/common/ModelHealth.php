<?php
/**
 * 模型运行期健康度
 *
 * ═══ 这个类是干什么的 ═══
 *
 * 记录**每一个模型在真实调用里的表现**，并在它「明显坏了」的时候
 * 让路由**立刻跳过它**。
 *
 * ═══ 为什么非要有这个东西 ═══
 *
 * 生产实测（某次修复前一天的数据）：
 *
 *   模型                                   近 6h 调用   失败    平均耗时
 *   nvidia/nemotron-3-ultra-550b-a55b          36        5      8.5 秒   ← 健康
 *   deepseek-ai/deepseek-v4.1-flash            20       20     49.9 秒   ← 全失败
 *   z-ai/glm-5.3                                7        7     61.1 秒   ← 全失败
 *   z-ai/glm-5.3-flash                          8        3     83.6 秒
 *
 * 这 36 次调用里 31 次失败，而它们每次都要烧掉半分钟到一分半。
 * 结果是：**整体成功率被压到三成**，用户以为「这个站大部分请求都是坏的」——
 * 这个判断其实是对的，只是原因不是站点整体有问题，而是少数几个模型根本不工作。
 *
 * 光靠人工去翻用量日志发现这件事是不现实的（要等到有人抱怨）。
 * 所以这里做成自动的：连续失败到阈值 → 暂时摘掉 → 冷却后自动放行试探。
 *
 * ═══ 三条设计取舍 ═══
 *
 * 1. **用「连续失败」而不是「失败率」**。
 *    失败率会被大量成功稀释：一个模型 100 次里成功 60 次，看失败率只有 40%，
 *    但那 40 次每次让用户等 60 秒。而「连续失败」抓的正是「根本不工作」这件事。
 *
 * 2. **不是永久下架，而是冷却 + 自动放行**。
 *    上游的故障大多是暂时的（模型过载、临时限流）。永久下架需要人工记着恢复，
 *    而人一定会忘 —— 最后坏的是「那个能用的模型怎么一直没人调」。
 *
 * 3. **只统计「模型级」失败**（超时、5xx、上游说没这个模型）。
 *    参数错误（400）、密钥失效（401/403）、限流（429）都不是「这个模型不能用」——
 *    把它们算进去会把一个健康的模型误摘掉，那比不摘更糟。
 */

declare(strict_types=1);

namespace app\common;

use support\Log;
use Throwable;

final class ModelHealth
{
    /** 进程内缓存的存活秒数。与 Settings 的缓存同量级：够短，不会让「刚恢复」等太久 */
    private const CACHE_TTL = 5;

    /**
     * 当前处于「不可用」状态的模型（进程内缓存）。
     *
     * 为什么缓存整张名单而不是按模型查：判断发生在**每个转发请求**上，
     * 而名单通常只有几条。缓存整张表可以做到「每 5 秒最多一次查询」。
     *
     * @var array<string, array{until:int, reason:string, errors:int}>|null
     */
    private static ?array $unavailableCache = null;

    private static int $cacheAt = 0;

    /** 是否启用（站长可关） */
    public static function enabled(): bool
    {
        return Settings::bool('gateway.model_health_enabled', true);
    }

    /** 连续失败几次判定为「当前不可用」 */
    public static function threshold(): int
    {
        return max(1, Settings::int('gateway.model_fail_threshold', 3));
    }

    /** 判定不可用后自动放行试探的冷却秒数 */
    public static function cooldownSeconds(): int
    {
        return max(30, Settings::int('gateway.model_cooldown_seconds', 600));
    }

    /**
     * 这个失败算不算「模型坏了」。
     *
     * 五种情况算：
     *   · 状态码 0   —— 连接失败 / 超时（连不上的最常见形态）
     *   · 5xx        —— 上游自己说「我不行」（NIM 的 503 ResourceExhausted 就是这类）
     *   · 404        —— 上游说「没有这个模型」。NIM 的
     *                   `Function 'x' not found for account` 正是 404，
     *                   它意味**这个账号/这个模型**根本不可用
     *   · 408        —— 上游自己报的请求超时
     *
     * 其余都不算（400 参数错、401/403 密钥问题、429 限流、
     * 422 校验失败）—— 那些是「这次请求」的问题，不是「这个模型」的问题。
     */
    public static function isModelFailure(int $status): bool
    {
        return $status === 0 || $status === 404 || $status === 408 || $status >= 500;
    }

    /**
     * 记一次成功。
     *
     * 成功立刻把连击清零并解除不可用 —— 这是「自愈」的落点：
     * 冷却到期后放行的第一次试探如果成功，该模型马上回到正常路由。
     */
    public static function markSuccess(string $model, int $latencyMs = 0): void
    {
        if (!self::enabled() || $model === '') {
            return;
        }

        $row = self::rowOf($model);
        self::write($model, [
            'consecutive_errors' => 0,
            'total_calls' => (int) ($row['total_calls'] ?? 0) + 1,
            'total_errors' => (int) ($row['total_errors'] ?? 0),
            'last_ok_at' => time(),
            'last_error_at' => (int) ($row['last_error_at'] ?? 0),
            'last_status' => 200,
            'last_error' => (string) ($row['last_error'] ?? ''),
            'avg_latency_ms' => self::blend((int) ($row['avg_latency_ms'] ?? 0), (int) ($row['total_calls'] ?? 0), $latencyMs),
            'unavailable_until' => 0,
        ]);

        self::forgetCache();
    }

    /**
     * 记一次失败（**只记最终结果**，不记重试过程中的每一次尝试）。
     *
     * 为什么只记最终结果：一次调用可能换 3 条渠道、4 把 Key 才彻底失败。
     * 若每次都记，一个模型失败一次就被记 4 次，阈值 3 立刻被凑满 ——
     * 那不是「连续失败 3 次」，而是「失败 1 次」。前者才是有意义的信号。
     */
    public static function markFailure(string $model, int $status, string $message, int $latencyMs = 0): void
    {
        if (!self::enabled() || $model === '' || !self::isModelFailure($status)) {
            return;
        }

        $row = self::rowOf($model);
        $consecutive = (int) ($row['consecutive_errors'] ?? 0) + 1;
        $now = time();

        // 达到阈值 → 进入不可用状态（已在不可用中则续期，避免半路又放行）
        $until = (int) ($row['unavailable_until'] ?? 0);
        if ($consecutive >= self::threshold()) {
            $until = $now + self::cooldownSeconds();
        }

        self::write($model, [
            'consecutive_errors' => $consecutive,
            'total_calls' => (int) ($row['total_calls'] ?? 0) + 1,
            'total_errors' => (int) ($row['total_errors'] ?? 0) + 1,
            'last_ok_at' => (int) ($row['last_ok_at'] ?? 0),
            'last_error_at' => $now,
            'last_status' => $status,
            'last_error' => mb_substr($message, 0, 255),
            'avg_latency_ms' => self::blend((int) ($row['avg_latency_ms'] ?? 0), (int) ($row['total_calls'] ?? 0), $latencyMs),
            'unavailable_until' => $until,
        ]);

        self::forgetCache();
    }

    /**
     * 该模型当前是否不可用；不可用时返回原因（否则 null）。
     *
     * @return array{reason:string, until:int, retryInSeconds:int, errors:int}|null
     */
    public static function unavailable(string $model): ?array
    {
        if (!self::enabled() || $model === '') {
            return null;
        }

        $item = self::unavailableMap()[$model] ?? null;

        return $item === null ? null : [
            'reason' => $item['reason'],
            'until' => $item['until'],
            'retryInSeconds' => max(1, $item['until'] - time()),
            'errors' => $item['errors'],
        ];
    }

    /**
     * 给用户一个「现在能用什么」的替代清单。
     *
     * 为什么必须给：只说「这个模型不可用」，用户接下来的动作就是
     * 「那我该用哪个？」—— 让他自己去模型广场里试，等于把问题推回去。
     * 优先推荐**近期真实成功过**的模型（有实据，不是猜）。
     *
     * @return array<int, string>
     */
    public static function alternatives(string $model, int $limit = 3): array
    {
        $unavailable = self::unavailableMap();

        // ① 近期成功过的模型，按最近成功时间倒序 —— 这是最有价值的推荐
        $result = [];
        try {
            $rows = Db::select(
                'SELECT model FROM model_health WHERE last_ok_at > 0 ORDER BY last_ok_at DESC LIMIT 20'
            );
            foreach ($rows as $row) {
                $name = (string) $row['model'];
                if ($name === $model || isset($unavailable[$name])) {
                    continue;
                }
                $result[$name] = true;
            }
        } catch (Throwable) {
            // 健康表还没建好（首次启动时序）时退回到「渠道清单」这条路
        }

        // ② 不够就从启用渠道的模型清单里补（排除已知不可用的）
        if (count($result) < $limit) {
            foreach (Channel::all() as $channel) {
                if ((int) $channel['status'] !== Channel::STATUS_ENABLED) {
                    continue;
                }

                foreach (Channel::modelsOf($channel) as $name) {
                    $name = trim((string) $name);
                    if ($name === '' || $name === $model || str_contains($name, '*')) {
                        continue;
                    }
                    if (isset($unavailable[$name])) {
                        continue;
                    }
                    $result[$name] = true;
                }
            }
        }

        return array_slice(array_keys($result), 0, max(1, $limit));
    }

    /**
     * 后台用的清单：**只列有情况的模型**（坏过、被摘掉的），
     * 全健康的模型不列 —— 一个几十行的「一切正常」表格没人会看。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function troubleList(): array
    {
        try {
            $rows = Db::select(
                'SELECT * FROM model_health
                 WHERE unavailable_until > ? OR consecutive_errors > 0
                 ORDER BY unavailable_until DESC, consecutive_errors DESC, last_error_at DESC
                 LIMIT 100',
                [time()]
            );
        } catch (Throwable) {
            return [];
        }

        $now = time();

        return array_map(static function (array $row) use ($now): array {
            $until = (int) $row['unavailable_until'];
            $calls = (int) $row['total_calls'];
            $errors = (int) $row['total_errors'];

            return [
                'model' => (string) $row['model'],
                'state' => $until > $now ? 'unavailable' : 'degraded',
                'stateLabel' => $until > $now ? '已暂停路由' : '异常但仍在路由',
                'consecutiveErrors' => (int) $row['consecutive_errors'],
                'calls' => $calls,
                'errors' => $errors,
                'errorRate' => $calls > 0 ? (int) round($errors * 100 / $calls) : 0,
                'avgLatencyMs' => (int) $row['avg_latency_ms'],
                'lastOkAt' => (int) $row['last_ok_at'],
                'lastErrorAt' => (int) $row['last_error_at'],
                'lastStatus' => (int) $row['last_status'],
                'lastError' => (string) ($row['last_error'] ?? ''),
                'retryInSeconds' => $until > $now ? $until - $now : 0,
            ];
        }, $rows);
    }

    /** 后台卡片上的汇总：暂停 / 异常 各几个 */
    public static function summary(): array
    {
        $list = self::troubleList();
        $paused = 0;
        foreach ($list as $item) {
            if ($item['state'] === 'unavailable') {
                $paused++;
            }
        }

        return ['paused' => $paused, 'degraded' => count($list) - $paused, 'total' => count($list)];
    }

    /**
     * 清掉健康记录（让模型立刻重新参与路由）。
     *
     * 两种用法：
     *   · 传模型名 → 只清这一个（「我确认它好了，放它回来」）
     *   · 不传     → 全清（换上游、批量恢复时用）
     *
     * ⚠️ 只解禁、不删证据：把连续失败清零、解除不可用，
     * 但**保留累计调用数与失败数**，后台仍看得到「它历史上坏过」。
     *
     * @return int 影响条数
     */
    public static function release(string $model = ''): int
    {
        self::forgetCache();

        if ($model !== '') {
            return Db::execute(
                'UPDATE model_health SET consecutive_errors = 0, unavailable_until = 0, updated_at = ? WHERE model = ?',
                [time(), $model]
            );
        }

        return Db::execute(
            'UPDATE model_health SET consecutive_errors = 0, unavailable_until = 0, updated_at = ? WHERE unavailable_until > 0 OR consecutive_errors > 0',
            [time()]
        );
    }

    // ═══════════════════════════════════════════════════════════
    // 内部
    // ═══════════════════════════════════════════════════════════

    /**
     * 当前不可用的模型名单（带进程内缓存）。
     *
     * @return array<string, array{until:int, reason:string, errors:int}>
     */
    private static function unavailableMap(): array
    {
        $now = time();

        if (self::$unavailableCache !== null && ($now - self::$cacheAt) < self::CACHE_TTL) {
            return self::$unavailableCache;
        }

        $map = [];
        try {
            $rows = Db::select(
                'SELECT model, unavailable_until, consecutive_errors, last_error, last_status
                 FROM model_health WHERE unavailable_until > ?',
                [$now]
            );
            foreach ($rows as $row) {
                $map[(string) $row['model']] = [
                    'until' => (int) $row['unavailable_until'],
                    'errors' => (int) $row['consecutive_errors'],
                    'reason' => self::reasonText(
                        (int) $row['consecutive_errors'],
                        (int) $row['last_status'],
                        (string) ($row['last_error'] ?? '')
                    ),
                ];
            }
        } catch (Throwable) {
            // 表还没建好：按「都可用」处理。健康度是**优化**，
            // 不该因为自己的表出问题而让转发整条链路不可用
            $map = [];
        }

        self::$unavailableCache = $map;
        self::$cacheAt = $now;

        return $map;
    }

    /**
     * 说给用户听的原因。必须是一句人话 ——
     * 用户看到的是这句话，而不是我们的字段名。
     */
    private static function reasonText(int $errors, int $status, string $lastError): string
    {
        $why = match (true) {
            $status === 0 => '连不上上游（超时或网络不通）',
            $status === 404 => '上游说没有这个模型（该账号无权使用）',
            $status >= 500 => "上游自己报错（HTTP {$status}）",
            default => '持续失败',
        };

        $text = "最近 {$errors} 次调用全部失败：{$why}";
        if ($lastError !== '') {
            $text .= '（' . mb_substr($lastError, 0, 60) . '）';
        }

        return $text;
    }

    /** @return array<string, mixed>|null */
    private static function rowOf(string $model): ?array
    {
        try {
            return Db::selectOne('SELECT * FROM model_health WHERE model = ?', [$model]);
        } catch (Throwable) {
            return null;
        }
    }

    /**
     * 加权平均耗时：新值按「历史次数」的权重并入。
     * 直接取最后一次会让「偶尔一次卡 300 秒」把整体说得很难看。
     */
    private static function blend(int $avg, int $pastCalls, int $latencyMs): int
    {
        if ($latencyMs <= 0) {
            return $avg;
        }

        if ($pastCalls <= 0) {
            return $latencyMs;
        }

        return (int) round(($avg * $pastCalls + $latencyMs) / ($pastCalls + 1));
    }

    /**
     * 写入一行（跨库通用的原子 upsert）。
     *
     * ⚠️ 两个刻意的写法选择：
     *
     * 1. **值在 PHP 里算好再写，不用 SQL 表达式做累加**。
     *    一旦在 upsert 的 SET 里引用旧值（`x = x + 1`），
     *    就要维护两套 SQL，早晚会有一边写错。
     *
     * 2. **SET 子句不用 `VALUES(col)` / `excluded.col`**，
     *    而是把新值再绑定一遍。那两个写法两家数据库各支持一半
     *    （SQLite 3.24+ 用的是 `excluded.`，MySQL 8.0.20 起把 `VALUES()` 标记为废弃），
     *    只有「同样的值绑两次」是两边都认的 —— 与 Settings::put() 里同一套思路。
     *
     * @param array<string, mixed> $values
     */
    private static function write(string $model, array $values): void
    {
        $columns = ['model', 'consecutive_errors', 'total_calls', 'total_errors', 'last_ok_at',
            'last_error_at', 'last_status', 'last_error', 'avg_latency_ms', 'unavailable_until', 'updated_at'];

        $row = [
            $model,
            (int) $values['consecutive_errors'],
            (int) $values['total_calls'],
            (int) $values['total_errors'],
            (int) $values['last_ok_at'],
            (int) $values['last_error_at'],
            (int) $values['last_status'],
            (string) $values['last_error'] === '' ? null : (string) $values['last_error'],
            (int) $values['avg_latency_ms'],
            (int) $values['unavailable_until'],
            time(),
        ];

        // SET 子句覆盖除主键外的所有列，值再绑一遍
        $updates = implode(', ', array_map(
            static fn (string $column): string => $column . ' = ?',
            array_slice($columns, 1)
        ));

        $sql = 'INSERT INTO model_health (' . implode(', ', $columns) . ') VALUES ('
            . implode(', ', array_fill(0, count($columns), '?')) . ') '
            . (Db::isSqlite() ? 'ON CONFLICT(model) DO UPDATE SET ' : 'ON DUPLICATE KEY UPDATE ')
            . $updates;

        try {
            Db::execute($sql, array_merge($row, array_slice($row, 1)));
        } catch (Throwable $e) {
            // 健康度是「锦上添花」：写不进去也绝不能让转发本身失败
            Log::warning('模型健康度写入失败：' . $e->getMessage());
        }
    }

    private static function forgetCache(): void
    {
        self::$unavailableCache = null;
        self::$cacheAt = 0;
    }
}
