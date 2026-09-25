<?php
/**
 * 探测任务执行器端到端测试（本地模拟上游，不碰开发库、不联网）。
 *
 * 为什么需要一个**真的起子进程**的测试：后台检测的关键承诺是
 * 「HTTP 请求立刻返回、探测在独立进程里跑、页面能看着进度往前走」。
 * 这件事只有把子进程真跑起来才能验证 —— 单测任务表只能证明数据存得对，
 * 证明不了「进程会不会卡住不落库」「两个进程会不会把同一份清单跑两遍」。
 *
 * 用法：php scripts/test-model-probe-runner.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\BackgroundProcess;
use app\common\ModelProbeTask;
use app\common\Schema;

$dbFile = probe_test_temp('runner-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();

$mock = probe_mock_start(250);
$logFile = $mock['logFile'];
$mockBase = 'http://127.0.0.1:' . $mock['port'];

function runRunner(int $taskId): array
{
    $process = proc_open(
        [PHP_BINARY, dirname(__DIR__) . '/scripts/run-model-probe-task.php', (string) $taskId],
        [1 => ['pipe', 'w'], 2 => ['pipe', 'w']],
        $pipes,
        dirname(__DIR__)
    );
    if (!is_resource($process)) {
        return ['code' => -1, 'output' => '', 'error' => '无法启动'];
    }
    $output = (string) stream_get_contents($pipes[1]);
    $error = (string) stream_get_contents($pipes[2]);
    fclose($pipes[1]);
    fclose($pipes[2]);

    return ['code' => proc_close($process), 'output' => $output, 'error' => $error];
}

$primaryKey = 'sk-runner-primary-key-9f3a';
$secondKey = 'sk-runner-second-key-4c71';
$models = ['ok-1', 'ok-2', 'denied-1', 'echoauth-1', 'missing-1', 'badreq-1', 'ratelimit-1', 'boom-1'];
$channelId = probe_seed_channel('模拟上游渠道', $mockBase . '/mock', $models, [$primaryKey, $secondKey]);

echo "一、后台任务从 0 跑到完成\n";

throws('任务 ID 非法时拒绝启动子进程', static fn () => BackgroundProcess::startModelProbe(0), '任务 ID 无效');

$taskId = (int) ModelProbeTask::create($channelId, $models)['task']['id'];
$started = microtime(true);
BackgroundProcess::startModelProbe($taskId);

$samples = [];
$seenPartial = false;
$done = probe_test_wait(static function () use ($taskId, &$samples, &$seenPartial): bool {
    $task = ModelProbeTask::find($taskId);
    if ($task === null) {
        return false;
    }
    $samples[] = $task['status'] . ' ' . $task['completed_count'] . '/' . $task['total_count'];
    if ($task['status'] === 'running' && (int) $task['completed_count'] > 0 && (int) $task['completed_count'] < (int) $task['total_count']) {
        $seenPartial = true;
    }

    return in_array($task['status'], ['completed', 'failed', 'aborted'], true);
}, 90000, 150);

$task = ModelProbeTask::find($taskId);
check('子进程把任务跑到了完成', $done && $task !== null && $task['status'] === 'completed', '最终状态：' . (string) ($task['status'] ?? '未知') . '；采样：' . implode(' → ', array_slice($samples, 0, 12)));
check('能观察到「进度在走」的中间态', $seenPartial, '采样：' . implode(' → ', $samples));
check('已完成数等于总数', (int) ($task['completed_count'] ?? 0) === count($models), (string) ($task['completed_count'] ?? 0) . '/' . count($models));
check('可用 2 个', (int) $task['ok_count'] === 2, (string) $task['ok_count']);
check('无权限 2 个（含一个把请求头回显给我们看的）', (int) $task['no_access_count'] === 2, (string) $task['no_access_count']);
check('当前协议不可用 1 个', (int) $task['unroutable_count'] === 1, (string) $task['unroutable_count']);
check('无法判定 3 个（400 / 429 / 500）', (int) $task['inconclusive_count'] === 3, (string) $task['inconclusive_count']);
check('完成后清空当前模型', $task['current_model'] === null);
check('探测耗时与预期量级一致（说明没有空跑整份清单）', microtime(true) - $started < 60);

$rows = ModelProbeTask::results($taskId);
check('结果条数与模型数一致', count($rows) === count($models), (string) count($rows));

$summaries = implode("\n", array_column($rows, 'response_summary'));
$leaked = array_filter([$primaryKey, $secondKey], static fn (string $key): bool => str_contains($summaries, $key));
check('结果摘要里没有完整凭据', $leaked === [], implode(',', $leaked));
$echoRow = null;
foreach ($rows as $row) {
    if ($row['model'] === 'echoauth-1') {
        $echoRow = $row;
    }
}
check('回显了请求头的上游响应里，凭据被替换成占位符', $echoRow !== null && str_contains((string) $echoRow['response_summary'], '[REDACTED]'), (string) ($echoRow['response_summary'] ?? ''));

$mockLines = probe_mock_requests($logFile, '/mock/');
check('每个模型只被请求一次（重试的那两个合计多一次）', count($mockLines) === count($models) + 2, '实际 ' . count($mockLines) . ' 次请求');
$authHeaders = array_unique(array_map(static function (string $line): string {
    $parts = explode(' | ', $line);

    return $parts[2] ?? '';
}, $mockLines));
check('全程只用第 1 把凭据（不轮换，结论才可复现）', $authHeaders === ['Bearer ' . $primaryKey], implode(' / ', $authHeaders));

echo "\n二、鉴权失败时立即中止\n";

$authChannel = probe_seed_channel('鉴权异常渠道', $mockBase . '/authfail', ['x-1', 'x-2', 'x-3'], [$primaryKey]);
$authTaskId = (int) ModelProbeTask::create($authChannel, ['x-1', 'x-2', 'x-3'])['task']['id'];
$result = runRunner($authTaskId);
$authTask = ModelProbeTask::find($authTaskId);
check('进程退出码标明中止', $result['code'] === 2, '退出码 ' . $result['code']);
check('任务被标记为中止', $authTask['status'] === 'aborted', (string) $authTask['status']);
check('中止原因写明 401', str_contains((string) $authTask['error_message'], '401'), (string) $authTask['error_message']);
check('中止后没有留下半截结果', ModelProbeTask::results($authTaskId) === []);
check('第 1 个模型失败后就不再打上游', count(probe_mock_requests($logFile, '/authfail/')) === 1, '实际 ' . count(probe_mock_requests($logFile, '/authfail/')) . ' 次请求');

echo "\n三、同一任务不会被跑两遍\n";

$singleChannel = probe_seed_channel('单模型渠道', $mockBase . '/mock', ['ok-9'], [$primaryKey]);
$singleTaskId = (int) ModelProbeTask::create($singleChannel, ['ok-9'])['task']['id'];
$first = runRunner($singleTaskId);
check('第一次执行成功', $first['code'] === 0, '退出码 ' . $first['code'] . ' 错误输出：' . $first['error']);
$linesAfterFirst = count(probe_mock_requests($logFile, '/mock/'));
$second = runRunner($singleTaskId);
check('第二次执行直接退出（任务已被领走）', $second['code'] === 0, '退出码 ' . $second['code']);
check('第二次执行没有再打上游', count(probe_mock_requests($logFile, '/mock/')) === $linesAfterFirst, '多了 ' . (count(probe_mock_requests($logFile, '/mock/')) - $linesAfterFirst) . ' 次请求');
check('执行器自身输出里没有完整凭据', !str_contains($first['output'] . $first['error'] . $second['output'] . $second['error'], $primaryKey));

exit(probe_test_report());
