<?php
/**
 * 渠道模型探测任务持久化。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;
use Throwable;

final class ModelProbeTask
{
    public const RUNNING = ['pending', 'running'];
    public const STALE_SECONDS = 180;

    /** @return array{task:array<string,mixed>,created:bool} */
    public static function create(int $channelId, array $models): array
    {
        self::failStale();
        $existing = self::runningForChannel($channelId);
        if ($existing !== null) {
            return ['task' => $existing, 'created' => false];
        }

        $running = Db::selectOne(
            "SELECT COUNT(*) AS c FROM channel_model_probe_tasks WHERE status IN ('pending', 'running')"
        );
        if ((int) ($running['c'] ?? 0) >= 2) {
            throw new RuntimeException('全站已有 2 个检测任务在运行，请稍后再试');
        }

        $now = time();
        try {
            Db::execute(
                'INSERT INTO channel_model_probe_tasks
                 (channel_id, status, total_count, completed_count, ok_count, no_access_count,
                  unroutable_count, inconclusive_count, created_at, heartbeat_at)
                 VALUES (?, ?, ?, 0, 0, 0, 0, 0, ?, ?)',
                [$channelId, 'pending', count($models), $now, $now]
            );
        } catch (Throwable $e) {
            $existing = self::runningForChannel($channelId);
            if ($existing !== null) {
                return ['task' => $existing, 'created' => false];
            }
            throw $e;
        }

        return ['task' => self::find((int) Db::pdo()->lastInsertId()), 'created' => true];
    }

    public static function find(int $id): ?array
    {
        return Db::selectOne('SELECT * FROM channel_model_probe_tasks WHERE id = ?', [$id]);
    }

    public static function runningForChannel(int $channelId): ?array
    {
        return Db::selectOne(
            "SELECT * FROM channel_model_probe_tasks
             WHERE channel_id = ? AND status IN ('pending', 'running') ORDER BY id DESC LIMIT 1",
            [$channelId]
        );
    }

    /**
     * 把任务从 pending 领起。
     *
     * 返回 false 表示「没领到」—— 只有 `status = 'pending'` 才能被领走，
     * 所以同一个任务即使被两个进程同时启动，也只有一个能继续跑。
     * 这一点必须靠 SQL 的条件来保证，不能靠「先查一次再改」：
     * 两个进程同时查到 pending、同时改成 running，就会各自跑一遍整份清单，
     * 结果计数翻倍、上游也被打两遍。
     */
    public static function start(int $id): bool
    {
        $now = time();

        return Db::execute(
            "UPDATE channel_model_probe_tasks SET status = 'running', started_at = ?, heartbeat_at = ?
             WHERE id = ? AND status = 'pending'",
            [$now, $now, $id]
        ) > 0;
    }

    public static function setCurrent(int $id, string $model): void
    {
        Db::execute(
            'UPDATE channel_model_probe_tasks SET current_model = ?, heartbeat_at = ? WHERE id = ?',
            [mb_substr($model, 0, 191), time(), $id]
        );
    }

    /** @param array{model:string,upstream_model:string,result:string,http:int,summary:string,latency_ms?:int} $result */
    public static function addResult(int $taskId, array $result): void
    {
        $pdo = Db::pdo();
        $pdo->beginTransaction();
        try {
            Db::execute(
                'INSERT INTO channel_model_probe_results
                 (task_id, model, upstream_model, classification, http_status, response_summary, latency_ms, completed_at)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?)',
                [
                    $taskId,
                    mb_substr($result['model'], 0, 191),
                    mb_substr($result['upstream_model'], 0, 191),
                    $result['result'],
                    $result['http'],
                    mb_substr($result['summary'], 0, 500),
                    // 探测内核会给，历史调用方可能不给 —— 缺了就记 0（页面显示「暂无数据」）
                    max(0, (int) ($result['latency_ms'] ?? 0)),
                    time(),
                ]
            );
            $column = match ($result['result']) {
                ModelProbe::OK => 'ok_count',
                ModelProbe::NO_ACCESS => 'no_access_count',
                ModelProbe::UNROUTABLE => 'unroutable_count',
                default => 'inconclusive_count',
            };
            Db::execute(
                "UPDATE channel_model_probe_tasks
                 SET completed_count = completed_count + 1, {$column} = {$column} + 1, heartbeat_at = ?
                 WHERE id = ?",
                [time(), $taskId]
            );
            $pdo->commit();
        } catch (Throwable $e) {
            if ($pdo->inTransaction()) {
                $pdo->rollBack();
            }
            throw $e;
        }
    }

    public static function finish(int $id): void
    {
        Db::execute(
            "UPDATE channel_model_probe_tasks
             SET status = 'completed', current_model = NULL, heartbeat_at = ?, finished_at = ? WHERE id = ?",
            [time(), time(), $id]
        );
    }

    public static function abort(int $id, int $http): void
    {
        Db::execute(
            "UPDATE channel_model_probe_tasks
             SET status = 'aborted', error_message = ?, current_model = NULL, heartbeat_at = ?, finished_at = ?
             WHERE id = ? AND status IN ('pending', 'running')",
            ["上游返回 HTTP {$http}，鉴权失败，任务已中止", time(), time(), $id]
        );
    }

    /**
     * 标记任务失败。
     *
     * 条件里的 `status IN ('pending', 'running')` 是必须的：探测进程崩溃时会走这里，
     * 但如果它崩溃得太晚（任务其实已经完成），无条件覆盖会把一份**成功的结果**
     * 改写成失败 —— 页面上明明有四类计数，状态却写着失败，人只会以为数据坏了。
     */
    public static function fail(int $id, string $message): void
    {
        Db::execute(
            "UPDATE channel_model_probe_tasks
             SET status = 'failed', error_message = ?, current_model = NULL, heartbeat_at = ?, finished_at = ?
             WHERE id = ? AND status IN ('pending', 'running')",
            [mb_substr($message, 0, 500), time(), time(), $id]
        );
    }

    public static function failStale(): int
    {
        $before = time() - self::STALE_SECONDS;

        return Db::execute(
            "UPDATE channel_model_probe_tasks
             SET status = 'failed', error_message = '后台任务失联，请重新发起', finished_at = ?
             WHERE status IN ('pending', 'running') AND heartbeat_at < ?",
            [time(), $before]
        );
    }

    /** @return array<int, array<string,mixed>> */
    public static function results(int $taskId): array
    {
        return Db::select(
            'SELECT model, upstream_model, classification, http_status, response_summary, completed_at
             FROM channel_model_probe_results WHERE task_id = ? ORDER BY id',
            [$taskId]
        );
    }

    /**
     * @param array<int, string> $selected
     * @return array{removed:int,before:int,after:int,backup:string}
     */
    public static function apply(int $taskId, array $selected, string $operator = 'admin'): array
    {
        $task = self::find($taskId);
        if ($task === null || (string) $task['status'] !== 'completed') {
            throw new RuntimeException('任务尚未完成，不能下架模型');
        }
        if (!empty($task['applied_at'])) {
            throw new RuntimeException('该任务已经执行过下架');
        }

        $allowedRows = Db::select(
            "SELECT model FROM channel_model_probe_results
             WHERE task_id = ? AND classification IN ('no_access', 'unroutable')",
            [$taskId]
        );
        $allowed = array_column($allowedRows, 'model');
        $selected = array_values(array_unique(array_filter(array_map('strval', $selected))));
        foreach ($selected as $model) {
            if (!in_array($model, $allowed, true)) {
                throw new RuntimeException('提交包含不允许下架的模型');
            }
        }

        $channel = Channel::find((int) $task['channel_id']);
        if ($channel === null) {
            throw new RuntimeException('渠道不存在');
        }
        $original = Channel::modelsOf($channel);
        $kept = array_values(array_filter($original, static fn (string $m): bool => !in_array($m, $selected, true)));
        $removed = count($original) - count($kept);

        $runtime = runtime_path();
        $backup = $runtime . DIRECTORY_SEPARATOR . 'probe-models-backup-'
            . (int) $task['channel_id'] . '-' . $taskId . '-' . date('Ymd-His') . '.json';
        $payload = [
            'channel_id' => (int) $task['channel_id'],
            'task_id' => $taskId,
            'created_at' => time(),
            'original_models' => $original,
            'removed' => array_values(array_intersect($original, $selected)),
            'kept' => $kept,
        ];
        if (file_put_contents($backup, json_encode($payload, JSON_PRETTY_PRINT | JSON_UNESCAPED_UNICODE)) === false) {
            throw new RuntimeException('无法写入原始模型清单备份，已取消下架');
        }

        $pdo = Db::pdo();
        $pdo->beginTransaction();
        try {
            Db::execute(
                'UPDATE channels SET models = ?, updated_at = ? WHERE id = ?',
                [json_encode($kept, JSON_UNESCAPED_UNICODE), time(), (int) $task['channel_id']]
            );
            Db::execute(
                'UPDATE channel_model_probe_tasks
                 SET applied_at = ?, applied_by = ?, removed_count = ?, backup_path = ? WHERE id = ?',
                [time(), mb_substr($operator, 0, 64), $removed, $backup, $taskId]
            );
            $pdo->commit();
        } catch (Throwable $e) {
            if ($pdo->inTransaction()) {
                $pdo->rollBack();
            }
            throw $e;
        }

        return ['removed' => $removed, 'before' => count($original), 'after' => count($kept), 'backup' => $backup];
    }
}
