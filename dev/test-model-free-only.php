<?php
/**
 * 「只上架能真正调用的模型」（probe.free_only）的测试。
 *
 * 要守住的口径（站长与用户都按它理解站点）：
 *   · 上游说「没有权限」= 这不是免费模型 → 不上架、检测后自动下架
 *   · 超时 / 无法判定 = 仍然可用 → **保留**（免费上游的模型只是慢）
 *   · 不认 chat/completions 的模型（向量化、重排）→ 不上架
 *
 * 为什么必须盯住「超时要保留」这一条：把超时当成不可用是最省事、
 * 也最像对的判定 —— 一次全量检测就能把一批慢模型整批删掉，
 * 而它们其实都能用。这种错删是不可逆的体验灾难。
 *
 * 用法：php dev/test-model-free-only.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Channel;
use app\common\Db;
use app\common\ModelProbe;
use app\common\ModelProbeTask;
use app\common\Schema;
use app\common\Settings;

$dbFile = probe_test_temp('freeonly-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('freeonly-pass-1234');

$mock = probe_mock_start(0);
$mockBase = 'http://127.0.0.1:' . $mock['port'];
$logFile = $mock['logFile'];

$channelId = probe_seed_channel('免费模型渠道', $mockBase . '/mock', [], ['sk-freeonly-key']);

/** 渠道当前的模型清单 */
$modelsOf = static function (int $id): array {
    $row = Db::selectOne('SELECT models FROM channels WHERE id = ?', [$id]);

    return Channel::modelsOf(['models' => (string) ($row['models'] ?? '')]);
};

/** 把渠道清单改回指定的内容（重跑各分支用） */
$setModels = static function (int $id, array $models): void {
    Db::execute('UPDATE channels SET models = ? WHERE id = ?', [json_encode($models), $id]);
};

/** 造一份「已完成」的检测任务，结果手工给定 */
$makeTask = static function (int $channelId, array $results): int {
    $created = ModelProbeTask::create($channelId, array_keys($results));
    $taskId = (int) $created['task']['id'];
    ModelProbeTask::start($taskId);
    foreach ($results as $model => $classification) {
        ModelProbeTask::addResult($taskId, [
            'model' => (string) $model,
            'upstream_model' => (string) $model,
            'result' => $classification,
            'http' => $classification === ModelProbe::OK ? 200 : 404,
            'summary' => '测试用',
            'latency_ms' => 100,
        ]);
    }
    ModelProbeTask::finish($taskId);

    return $taskId;
};

echo "一、开关默认开着\n";

check('probe.free_only 默认开启', Settings::bool('probe.free_only', false) === true);
check('渠道初始没有模型清单', $modelsOf($channelId) === []);

echo "\n二、测活会拉取清单并回填（首次没有任何检测记录，不剔任何模型）\n";

$first = Channel::test($channelId);
check('测活判定为可用', $first['ok'] === true, $first['message']);
check('回填了上游拉到的 3 个模型', $modelsOf($channelId) === ['denied-list-1', 'ok-list-1', 'ok-list-2'], implode(',', $modelsOf($channelId)));
check('首次拉取不会凭空剔除模型', !str_contains((string) $first['message'], '跳过'), (string) $first['message']);

echo "\n三、检测结果里哪些算「不能上架」\n";

$taskId = $makeTask($channelId, [
    'ok-list-1' => ModelProbe::OK,
    'denied-list-1' => ModelProbe::NO_ACCESS,
    'ok-list-2' => ModelProbe::INCONCLUSIVE,
]);

$unusable = ModelProbeTask::unusableModels($channelId);
check('只有「无权限」的模型被判定为不能上架', $unusable === ['denied-list-1'], implode(',', $unusable));
check(
    '超时/无法判定的模型不在名单里（免费模型只是慢）',
    !in_array('ok-list-2', $unusable, true)
);

echo "\n四、测活时挑模型：不会拿一个「已知无权限」的模型去验证 Key\n";

Channel::test($channelId);
$requests = probe_mock_requests($logFile, '/mock/chat');
$lastModel = '';
foreach ($requests as $line) {
    $parts = explode(' | ', $line);
    $lastModel = trim((string) ($parts[1] ?? ''));
}
check('最后一次推理请求没有用无权限的模型', $lastModel !== 'denied-list-1', '实际用了：' . $lastModel);

echo "\n五、回填清单时会跳过已知不能用的模型\n";

$setModels($channelId, []);
$second = Channel::test($channelId);
check(
    '回填结果里没有「无权限」的模型',
    !in_array('denied-list-1', $modelsOf($channelId), true),
    implode(',', $modelsOf($channelId))
);
check('保留了未知是否可用的模型', in_array('ok-list-2', $modelsOf($channelId), true), implode(',', $modelsOf($channelId)));
check('提示里说明了跳过了几个', str_contains((string) $second['message'], '跳过 1 个'), (string) $second['message']);

echo "\n六、下架时自动带上「无权限」的模型（不必逐条勾选）\n";

$setModels($channelId, ['denied-list-1', 'ok-list-1', 'ok-list-2']);
$applied = ModelProbeTask::apply($taskId, []);
check('自动补入的无权限模型有 1 个', $applied['auto_added'] === ['denied-list-1'], implode(',', $applied['auto_added']));
check('实际移除了 1 个', $applied['removed'] === 1, (string) $applied['removed']);
check('清单里已没有无权限模型', $modelsOf($channelId) === ['ok-list-1', 'ok-list-2'], implode(',', $modelsOf($channelId)));
check('无法判定的模型被保留', in_array('ok-list-2', $modelsOf($channelId), true));

echo "\n七、关掉开关：不再自动剔除，一切交回站长\n";

Settings::put('probe.free_only', false);

$channel2 = probe_seed_channel('关掉开关的渠道', $mockBase . '/mock', [], ['sk-freeonly-key2']);
$task2 = $makeTask($channel2, [
    'ok-list-1' => ModelProbe::OK,
    'denied-list-1' => ModelProbe::NO_ACCESS,
]);

$second = Channel::test($channel2);
check('关掉后回填包含全部模型', $modelsOf($channel2) === ['denied-list-1', 'ok-list-1', 'ok-list-2'], implode(',', $modelsOf($channel2)));
check('提示里不再提「跳过」', !str_contains((string) $second['message'], '跳过'), (string) $second['message']);

$applied2 = ModelProbeTask::apply($task2, []);
check('不勾选时不会自动下架任何模型', $applied2['removed'] === 0 && $applied2['auto_added'] === [], (string) $applied2['removed']);
check('清单原样保留', $modelsOf($channel2) === ['denied-list-1', 'ok-list-1', 'ok-list-2'], implode(',', $modelsOf($channel2)));

echo "\n八、同一个任务不能重复下架\n";

$task3 = $makeTask($channel2, ['denied-list-1' => ModelProbe::NO_ACCESS]);
ModelProbeTask::apply($task3, ['denied-list-1']);
try {
    ModelProbeTask::apply($task3, ['denied-list-1']);
    check('同一个任务不能重复下架', false, '第二次居然成功了');
} catch (Throwable $e) {
    check('同一个任务不能重复下架', str_contains($e->getMessage(), '已经执行过下架'), $e->getMessage());
}

echo "\n九、后台页面把口径讲清楚\n";

Settings::put('probe.free_only', true);
$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('freeonly-cookies', '.txt');
probe_login($port, $jar, 'freeonly-pass-1234');

$channel3 = probe_seed_channel('页面用渠道', $mockBase . '/mock', ['denied-list-1', 'ok-list-1', 'ok-list-2'], ['sk-freeonly-key3']);
$task4 = $makeTask($channel3, [
    'ok-list-1' => ModelProbe::OK,
    'denied-list-1' => ModelProbe::NO_ACCESS,
    'ok-list-2' => ModelProbe::INCONCLUSIVE,
]);

$page = probe_http($port, 'GET', '/admin/channels/probe?id=' . $task4, [], $jar);
check('任务页可访问', $page['status'] === 200, (string) $page['status']);
check('页面上写着「非免费模型」的说法', str_contains($page['body'], '非免费模型'), '页面没有把无权限解释成非免费模型');
check('页面提示未勾选也会一起下架', str_contains($page['body'], '即使不勾选也会一起下架'));
check('页面说明超时的模型不会被下架', str_contains($page['body'], '只是超时或无法判定'));

$settingsPage = probe_http($port, 'GET', '/admin/settings?group=probe', [], $jar);
check('配置页有「模型上架」分组', str_contains($settingsPage['body'], '模型上架'));
$probeCsrf = probe_csrf($port, '/admin/channels', $jar);
$apply = probe_http($port, 'POST', '/admin/channels/probe/apply', [
    '_csrf' => $probeCsrf,
    'id' => (string) $task4,
], $jar);
check('不勾选直接提交也能下架', $apply['status'] === 302, (string) $apply['status']);
check('渠道清单已剔除无权限模型', $modelsOf($channel3) === ['ok-list-1', 'ok-list-2'], implode(',', $modelsOf($channel3)));
$after = probe_http($port, 'GET', '/admin/channels/probe?id=' . $task4, [], $jar);
check('页面提示自动下架了几个', str_contains($after['body'], '自动一并下架'), '没有说明自动下架');

exit(probe_test_report());
