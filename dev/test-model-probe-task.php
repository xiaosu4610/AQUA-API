<?php
/**
 * 探测任务存储层测试（跑在临时 SQLite 上，不碰开发库、不联网）。
 *
 * 为什么值得单独测：下架是不可逆动作，它的正确性依赖一串状态流转 ——
 * 「任务在跑」「逐项计数」「完成后才能下架」「只下架确定不可用的那些」。
 * 这些流转如果只在页面上手点一遍，改一次代码就可能悄悄坏掉，
 * 而人眼很难发现（页面上数字只是变小了而已）。
 *
 * 用法：php scripts/test-model-probe-task.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Channel;
use app\common\Db;
use app\common\ModelProbe;
use app\common\ModelProbeTask;
use app\common\Schema;
use app\common\Settings;

$dbFile = probe_test_temp('task-db', '.sqlite');
probe_test_bootstrap($dbFile);

// 本文件验证的是「站长手动勾选哪些模型下架」这条路径，
// 所以先关掉「只上架能真正调用的模型」——它会把无权限的模型**自动**一并下架，
// 那是另一条路径，由 dev/test-model-free-only.php 专门覆盖。
Settings::put('probe.free_only', false);

function insertChannel(string $name, array $models): int
{
    Db::execute(
        'INSERT INTO channels (name, type, base_url, models, status, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)',
        [$name, 'openai', 'https://upstream.example.com/v1', json_encode($models), time(), time()]
    );

    return (int) Db::pdo()->lastInsertId();
}

function modelsOfChannel(int $id): array
{
    $row = Db::selectOne('SELECT models FROM channels WHERE id = ?', [$id]);

    return $row === null ? [] : (json_decode((string) $row['models'], true) ?: []);
}

/** @return array{model:string,upstream_model:string,result:string,http:int,summary:string} */
function result(string $model, string $classification, int $http = 200, string $summary = 'ok'): array
{
    return [
        'model' => $model,
        'upstream_model' => $model,
        'result' => $classification,
        'http' => $http,
        'summary' => $summary,
    ];
}

Schema::ensure();

$models = ['a', 'b', 'c', 'd', 'e'];
$channelId = insertChannel('测试渠道 A', $models);

echo "一、创建与互斥\n";

$created = ModelProbeTask::create($channelId, $models);
$taskId = (int) $created['task']['id'];
check('首次创建成功', $created['created'] === true);
check('初始状态是待执行', $created['task']['status'] === 'pending', (string) $created['task']['status']);
check('总数等于清单长度', (int) $created['task']['total_count'] === 5, (string) $created['task']['total_count']);
check('已完成数从 0 开始', (int) $created['task']['completed_count'] === 0);

$again = ModelProbeTask::create($channelId, $models);
check('同渠道再次发起 → 复用运行中的任务', $again['created'] === false && (int) $again['task']['id'] === $taskId);

$channelB = insertChannel('测试渠道 B', ['x']);
$channelC = insertChannel('测试渠道 C', ['y']);
ModelProbeTask::create($channelB, ['x']);
throws('全站已有 2 个任务在跑时第 3 个被拒', static fn () => ModelProbeTask::create($channelC, ['y']), '2 个检测任务');

check('第一次领取任务成功', ModelProbeTask::start($taskId) === true);
check('同一任务不能被领第二次（防重复跑整份清单）', ModelProbeTask::start($taskId) === false);
$running = ModelProbeTask::find($taskId);
check('领取后状态变为运行中', $running !== null && $running['status'] === 'running', (string) ($running['status'] ?? ''));
check('记录开始时间', (int) ($running['started_at'] ?? 0) > 0);

echo "\n二、逐项落库与计数\n";

ModelProbeTask::setCurrent($taskId, 'a');
check('当前模型被记录', (string) ModelProbeTask::find($taskId)['current_model'] === 'a');

ModelProbeTask::addResult($taskId, result('a', ModelProbe::NO_ACCESS, 404, '{"error":"Function not found for account"}'));
ModelProbeTask::addResult($taskId, result('b', ModelProbe::UNROUTABLE, 404, '404 page not found'));
ModelProbeTask::addResult($taskId, result('c', ModelProbe::OK));
ModelProbeTask::addResult($taskId, result('d', ModelProbe::INCONCLUSIVE, 0, 'Operation timed out'));
ModelProbeTask::addResult($taskId, result('e', ModelProbe::NO_ACCESS, 404, str_repeat('x', 900)));

$after = ModelProbeTask::find($taskId);
check('已完成数累计到 5', (int) $after['completed_count'] === 5, (string) $after['completed_count']);
check('无权限计数为 2', (int) $after['no_access_count'] === 2, (string) $after['no_access_count']);
check('不可用计数为 1', (int) $after['unroutable_count'] === 1, (string) $after['unroutable_count']);
check('可用计数为 1', (int) $after['ok_count'] === 1, (string) $after['ok_count']);
check('无法判定计数为 1', (int) $after['inconclusive_count'] === 1, (string) $after['inconclusive_count']);
check('心跳被刷新', (int) $after['heartbeat_at'] >= (int) $after['started_at']);

$rows = ModelProbeTask::results($taskId);
check('结果条数与清单一致', count($rows) === 5, (string) count($rows));
check('结果按探测顺序返回', array_column($rows, 'model') === $models, implode(',', array_column($rows, 'model')));
$longest = 0;
foreach ($rows as $row) {
    $longest = max($longest, mb_strlen((string) $row['response_summary']));
}
check('超长上游响应被截断到 500 字以内', $longest <= 500, '最长 ' . $longest . ' 字');

$columns = array_column(Db::select('PRAGMA table_info(channel_model_probe_tasks)'), 'name');
$resultColumns = array_column(Db::select('PRAGMA table_info(channel_model_probe_results)'), 'name');
$credentialish = array_filter(
    array_merge($columns, $resultColumns),
    static fn (string $name): bool => str_contains($name, 'key')
);
check('任务表与结果表都不存在凭据列', $credentialish === [], implode(',', $credentialish));

ModelProbeTask::finish($taskId);
$finished = ModelProbeTask::find($taskId);
check('完成后状态为已完成', $finished['status'] === 'completed', (string) $finished['status']);
check('记录完成时间', (int) $finished['finished_at'] > 0);
check('完成后清空当前模型', $finished['current_model'] === null);

echo "\n三、中止与失败\n";

$abortedId = (int) ModelProbeTask::create($channelB, ['x'])['task']['id'];
ModelProbeTask::abort($abortedId, 403);
$aborted = ModelProbeTask::find($abortedId);
check('鉴权失败 → 任务被标记为中⽌', $aborted['status'] === 'aborted', (string) $aborted['status']);
check('中止原因写明状态码', str_contains((string) $aborted['error_message'], '403'), (string) $aborted['error_message']);

$failedId = (int) ModelProbeTask::create($channelC, ['y'])['task']['id'];
ModelProbeTask::fail($failedId, '模拟崩溃');
$failed = ModelProbeTask::find($failedId);
check('异常 → 任务被标记为失败', $failed['status'] === 'failed', (string) $failed['status']);
check('失败原因被保留', $failed['error_message'] === '模拟崩溃');

echo "\n四、失联任务回收\n";

$staleId = (int) ModelProbeTask::create($channelC, ['y'])['task']['id'];
ModelProbeTask::start($staleId);
Db::execute('UPDATE channel_model_probe_tasks SET heartbeat_at = ? WHERE id = ?', [time() - ModelProbeTask::STALE_SECONDS - 60, $staleId]);
check('回收了失联任务', ModelProbeTask::failStale() >= 1);
check('失联任务被标记为失败', ModelProbeTask::find($staleId)['status'] === 'failed');
check('回收后同渠道可以重新发起', ModelProbeTask::create($channelC, ['y'])['created'] === true);

echo "\n五、确认下架\n";

throws('不存在的任务不能下架', static fn () => ModelProbeTask::apply(999999, []), '尚未完成');

$pendingId = (int) ModelProbeTask::create($channelC, ['y'])['task']['id'];
throws('仍在待执行的任务不能下架', static fn () => ModelProbeTask::apply($pendingId, ['y']), '尚未完成');

throws('提交可用模型会被拒（越权下架）', static fn () => ModelProbeTask::apply($taskId, ['c']), '不允许下架');
throws('提交不属于结果的模型会被拒', static fn () => ModelProbeTask::apply($taskId, ['zzz']), '不允许下架');

// 模拟「任务跑完之后，站长又在渠道里手工删掉了一个模型」：
// 下架必须按**当前**清单求差集，而不是拿任务发起时的快照覆盖。
Db::execute('UPDATE channels SET models = ? WHERE id = ?', [json_encode(['b', 'c', 'd', 'e']), $channelId]);

$applied = ModelProbeTask::apply($taskId, ['a', 'b']);
check('只移除了当前清单里真实存在的模型', $applied['removed'] === 1, (string) $applied['removed']);
check('下架前数量取当前清单（不是历史快照）', $applied['before'] === 4, (string) $applied['before']);
check('下架后数量正确', $applied['after'] === 3, (string) $applied['after']);
check('保留的是未选中的模型', modelsOfChannel($channelId) === ['c', 'd', 'e'], implode(',', modelsOfChannel($channelId)));
check('生成了备份文件', is_file($applied['backup']));
$backup = json_decode((string) file_get_contents($applied['backup']), true);
check('备份里保存的是下架前的当前清单', ($backup['original_models'] ?? []) === ['b', 'c', 'd', 'e']);
check('备份里写明被移除的模型', ($backup['removed'] ?? []) === ['b']);

$audit = ModelProbeTask::find($taskId);
check('记录下架时间', (int) $audit['applied_at'] > 0);
check('记录下架数量', (int) $audit['removed_count'] === 1);
check('记录备份路径', (string) $audit['backup_path'] === $applied['backup']);
throws('同一任务不能下架两次', static fn () => ModelProbeTask::apply($taskId, ['b']), '已经执行过下架');

@unlink($applied['backup']);

exit(probe_test_report());
