<?php
/**
 * 「检测可用模型」的端到端与安全回归（真服务、真子进程、本地模拟上游）。
 *
 * 这个脚本回答三个问题：
 *   1. **判定是否可靠**：十种上游响应形态（200/权限型404/普通404/401/403/429/500/超时/400/422）
 *      是否各自走到预期的结论。判错一个就会误删一个能用的模型，或留下一个永远调不通的模型。
 *   2. **凭据是否守住**：页面、状态接口、任务表、执行器输出、运行日志里
 *      是否出现过完整 Key。这是本功能唯一可能放大泄露面的地方（它要拿着 Key 到处打上游）。
 *   3. **有没有伤到别的页面**：首页、模型广场、渠道列表、`/v1/*`、错误页是否照常。
 *
 * 用法：php scripts/test-model-probe-e2e.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Channel;
use app\common\Db;
use app\common\ModelProbeTask;
use app\common\Schema;

$dbFile = probe_test_temp('e2e-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('e2e-test-pass-1234');

// 唯一指纹：用来在页面/日志/数据库里搜「有没有泄露」
$secretKey = 'sk-e2e-secret-8f31c7d0a5e2';
$secondKey = 'sk-e2e-backup-4a91';

$mock = probe_mock_start(0);
$mockBase = 'http://127.0.0.1:' . $mock['port'];
$logFile = $mock['logFile'];

$allModels = ['ok-1', 'ok-2', 'denied-1', 'echoauth-1', 'missing-1', 'badreq-1', 'unprocessable-1', 'ratelimit-1', 'boom-1'];
$channelId = probe_seed_channel('全响应类型渠道', $mockBase . '/mock', $allModels, [$secretKey, $secondKey]);
$authChannel = probe_seed_channel('鉴权 401 渠道', $mockBase . '/authfail', ['a-1', 'a-2', 'a-3'], [$secretKey]);
$forbiddenChannel = probe_seed_channel('鉴权 403 渠道', $mockBase . '/authfail403', ['b-1', 'b-2'], [$secretKey]);
// 慢上游：渠道级超时设成 5 秒，好在可接受的时间内复现「超时 → 保留」
$slowChannel = probe_seed_channel('慢上游渠道', $mockBase . '/mock', ['ok-9', 'slow-1'], [$secretKey]);
Db::execute('UPDATE channels SET config = ? WHERE id = ?', [json_encode(['total_timeout' => 5]), $slowChannel]);

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('e2e-cookies', '.txt');
$csrf = '';

echo "一、登录并跑一次完整检测\n";

check('登录后台', probe_login($port, $jar, 'e2e-test-pass-1234'));
$csrf = probe_csrf($port, '/admin/channels', $jar);
check('取到 CSRF 令牌', $csrf !== '');

$created = probe_http($port, 'POST', '/admin/channels/probe', ['_csrf' => $csrf, 'id' => (string) $channelId], $jar);
$taskId = (int) substr((string) $created['location'], (int) strrpos((string) $created['location'], '=') + 1);
check('发起检测 → 跳到任务页', $created['status'] === 302 && $taskId > 0, $created['status'] . ' → ' . $created['location']);

$done = probe_test_wait(static function () use ($taskId): bool {
    $task = ModelProbeTask::find($taskId);

    return $task !== null && in_array((string) $task['status'], ['completed', 'failed', 'aborted'], true);
}, 90000, 250);
$task = ModelProbeTask::find($taskId);
check('任务完成', $done && (string) $task['status'] === 'completed', (string) ($task['status'] ?? '未知'));

$groups = [];
foreach (ModelProbeTask::results($taskId) as $row) {
    $groups[(string) $row['classification']][] = (string) $row['model'];
}
$expect = [
    'ok' => ['ok-1', 'ok-2'],
    'no_access' => ['denied-1', 'echoauth-1'],
    'unroutable' => ['missing-1'],
    'inconclusive' => ['badreq-1', 'unprocessable-1', 'ratelimit-1', 'boom-1'],
];
foreach ($expect as $classification => $models) {
    $actual = $groups[$classification] ?? [];
    sort($actual);
    sort($models);
    check(
        "十种响应各自归类正确：{$classification} = " . implode(',', $models),
        $actual === $models,
        '实际 ' . implode(',', $actual)
    );
}

echo "\n二、刷新与重复发起\n";

$page = probe_http($port, 'GET', '/admin/channels/probe?id=' . $taskId, [], $jar);
check('刷新任务页仍是同一任务', $page['status'] === 200 && str_contains($page['body'], '任务 #' . $taskId));
$status = probe_http($port, 'GET', '/admin/channels/probe/status?id=' . $taskId, [], $jar);
$payload = json_decode($status['body'], true);
check('状态接口返回同一任务的完成计数', (int) ($payload['task']['completed_count'] ?? 0) === count($allModels), (string) ($payload['task']['completed_count'] ?? 0));

// ① 上一次已经跑完：允许再查一遍（清单可能变了，站长需要复查）
$second = probe_http($port, 'POST', '/admin/channels/probe', ['_csrf' => $csrf, 'id' => (string) $channelId], $jar);
$secondId = (int) substr((string) $second['location'], (int) strrpos((string) $second['location'], '=') + 1);
check('已完成的任务不阻塞再次检测', $secondId > 0 && $secondId !== $taskId, $second['location']);

// ② 正在跑：复用，不重复启动（否则同一份清单会被并行打两遍，上游配额白烧）
$third = probe_http($port, 'POST', '/admin/channels/probe', ['_csrf' => $csrf, 'id' => (string) $channelId], $jar);
check('任务在跑时再次发起 → 复用同一任务', $third['location'] === '/admin/channels/probe?id=' . $secondId, (string) $third['location']);
check(
    '该渠道没有多出第三个任务',
    (int) (Db::selectOne('SELECT COUNT(*) AS c FROM channel_model_probe_tasks WHERE channel_id = ?', [$channelId])['c'] ?? 0) === 2
);
probe_test_wait(static function () use ($secondId): bool {
    $row = ModelProbeTask::find($secondId);

    return $row !== null && in_array((string) $row['status'], ['completed', 'failed', 'aborted'], true);
}, 60000, 200);

echo "\n三、鉴权失败立即中止\n";

foreach ([['401', $authChannel, 'a-1'], ['403', $forbiddenChannel, 'b-1']] as [$label, $channel, $firstModel]) {
    $response = probe_http($port, 'POST', '/admin/channels/probe', ['_csrf' => $csrf, 'id' => (string) $channel], $jar);
    $id = (int) substr((string) $response['location'], (int) strrpos((string) $response['location'], '=') + 1);
    probe_test_wait(static function () use ($id): bool {
        $row = ModelProbeTask::find($id);

        return $row !== null && in_array((string) $row['status'], ['completed', 'failed', 'aborted'], true);
    }, 30000, 200);
    $row = ModelProbeTask::find($id);
    check("{$label} → 任务中止", (string) $row['status'] === 'aborted', (string) $row['status']);
    check("{$label} → 中止原因写明状态码", str_contains((string) $row['error_message'], $label), (string) $row['error_message']);
    check("{$label} → 只打了第一个模型就停", count(probe_mock_requests($logFile, $label === '401' ? '/authfail/' : '/authfail403/')) === 1, '实际 ' . count(probe_mock_requests($logFile, $label === '401' ? '/authfail/' : '/authfail403/')) . ' 次');
    $abortedPage = probe_http($port, 'GET', '/admin/channels/probe?id=' . $id, [], $jar);
    check("{$label} → 页面给出「改 Key / 改鉴权配置」的下一步", str_contains($abortedPage['body'], '鉴权失败，检测已中止'));
}

echo "\n四、超时判为「无法判定」而不是「不可用」\n";

$slowCreated = probe_http($port, 'POST', '/admin/channels/probe', ['_csrf' => $csrf, 'id' => (string) $slowChannel], $jar);
$slowTaskId = (int) substr((string) $slowCreated['location'], (int) strrpos((string) $slowCreated['location'], '=') + 1);
$slowDone = probe_test_wait(static function () use ($slowTaskId): bool {
    $row = ModelProbeTask::find($slowTaskId);

    return $row !== null && in_array((string) $row['status'], ['completed', 'failed', 'aborted'], true);
}, 120000, 500);
$slowRows = [];
foreach (ModelProbeTask::results($slowTaskId) as $row) {
    $slowRows[(string) $row['model']] = (string) $row['classification'];
}
check('慢渠道任务完成', $slowDone, (string) (ModelProbeTask::find($slowTaskId)['status'] ?? '未知'));
check('正常响应的模型判为可用', ($slowRows['ok-9'] ?? '') === 'ok', (string) ($slowRows['ok-9'] ?? ''));
check('超时的模型判为无法判定（保留，不下架）', ($slowRows['slow-1'] ?? '') === 'inconclusive', (string) ($slowRows['slow-1'] ?? ''));

echo "\n五、确认下架只动选中的确定不可用项\n";

$before = Channel::modelsOf(Channel::find($channelId));
$rejected = probe_http($port, 'POST', '/admin/channels/probe/apply', [
    '_csrf' => $csrf,
    'id' => (string) $taskId,
    'models' => ['denied-1', 'badreq-1'],
], $jar);
check('提交「无法判定」的模型 → 被拒', $rejected['status'] === 302);
check('被拒后清单原封不动', Channel::modelsOf(Channel::find($channelId)) === $before);

$applied = probe_http($port, 'POST', '/admin/channels/probe/apply', [
    '_csrf' => $csrf,
    'id' => (string) $taskId,
    'models' => ['denied-1', 'echoauth-1', 'missing-1'],
], $jar);
$after = Channel::modelsOf(Channel::find($channelId));
check('下架请求被接受', $applied['status'] === 302);
check(
    '只剩可用与无法判定的模型',
    $after === ['ok-1', 'ok-2', 'badreq-1', 'unprocessable-1', 'ratelimit-1', 'boom-1'],
    implode(',', $after)
);
$backupPath = (string) ModelProbeTask::find($taskId)['backup_path'];
check('生成了原始清单备份', is_file($backupPath), $backupPath);
if (is_file($backupPath)) {
    $backup = json_decode((string) file_get_contents($backupPath), true);
    check('备份内容与下架前一致', ($backup['original_models'] ?? []) === $before, implode(',', (array) ($backup['original_models'] ?? [])));
    check('备份里也写明了被移除的模型', ($backup['removed'] ?? []) === ['denied-1', 'echoauth-1', 'missing-1'], implode(',', (array) ($backup['removed'] ?? [])));
    @unlink($backupPath);
}

echo "\n六、凭据不落在任何能被看到的地方\n";

$pagesToScan = [
    '/admin/channels',
    '/admin/channels/probe?id=' . $taskId,
    '/admin/channels/probe/status?id=' . $taskId,
    '/admin/channels/probe?id=' . $authChannel,
];
$leakedInPages = [];
foreach ($pagesToScan as $path) {
    $body = probe_http($port, 'GET', $path, [], $jar)['body'];
    if (str_contains($body, $secretKey) || str_contains($body, $secondKey)) {
        $leakedInPages[] = $path;
    }
}
check('页面与状态接口都不含完整凭据', $leakedInPages === [], implode(',', $leakedInPages));

$storedText = json_encode(ModelProbeTask::results($taskId), JSON_UNESCAPED_UNICODE)
    . json_encode(ModelProbeTask::find($taskId), JSON_UNESCAPED_UNICODE);
check('任务表与结果表里没有完整凭据', !str_contains($storedText, $secretKey) && !str_contains($storedText, $secondKey));

$logHits = [];
foreach (glob(dirname(__DIR__) . '/runtime/logs/*.log') ?: [] as $log) {
    $content = (string) @file_get_contents($log);
    if (str_contains($content, $secretKey) || str_contains($content, $secondKey)) {
        $logHits[] = basename($log);
    }
}
check('运行日志里没有完整凭据', $logHits === [], implode(',', $logHits));

// 上游确实收到了凭据（否则「没泄露」只是因为它压根没发出去）
$authSeen = array_unique(array_map(static function (string $line): string {
    $parts = explode(' | ', $line);

    return $parts[2] ?? '';
}, probe_mock_requests($logFile, '/mock/')));
check('上游确实收到了第 1 把凭据（证明上面的「没泄露」是真结论）', $authSeen === ['Bearer ' . $secretKey], implode(' / ', $authSeen));

echo "\n七、回归：其他页面没被影响\n";

$regressions = [
    ['/', 200, '首页'],
    ['/models', 200, '模型广场'],
    ['/healthz', 200, '健康检查'],
    ['/admin/channels', 200, '渠道列表'],
    ['/admin', 200, '仪表盘'],
];
foreach ($regressions as [$path, $expectStatus, $label]) {
    $response = probe_http($port, 'GET', $path, [], $jar);
    check($label . ' 正常', $response['status'] === $expectStatus, '实际 ' . $response['status']);
}

$v1 = probe_http($port, 'GET', '/v1/models');
check('/v1/models 未带令牌 → 401 JSON', $v1['status'] === 401, '实际 ' . $v1['status']);
check('/v1/models 返回 JSON', str_contains($v1['headers'], 'application/json'), $v1['headers']);

$notFound = probe_http($port, 'GET', '/no-such-page');
check('错误页返回 404', $notFound['status'] === 404, '实际 ' . $notFound['status']);
check('错误页带安全响应头（未匹配路由也不漏）', str_contains($notFound['headers'], 'X-Content-Type-Options: nosniff'), $notFound['headers']);
check('错误页不回显异常细节', !str_contains($notFound['body'], 'Stack trace') && !str_contains($notFound['body'], 'Exception'));

exit(probe_test_report());
