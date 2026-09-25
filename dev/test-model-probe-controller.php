<?php
/**
 * 后台「检测模型」控制器的 HTTP 层测试（真起服务、真登录、真提交表单）。
 *
 * 为什么必须在 HTTP 层测：这一层要守的是「谁能进得来」——
 * 未登录能不能碰、没有 CSRF 令牌能不能提交、跨任务的模型能不能被下架。
 * 这些全都是「中间件 + 会话 + 路由」共同决定的，直接调控制器方法测不出来：
 * 绕过中间件去调方法，中间件坏了也看不出来。
 *
 * 用法：php scripts/test-model-probe-controller.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Channel;
use app\common\Db;
use app\common\ModelProbeTask;
use app\common\Schema;

$dbFile = probe_test_temp('controller-db', '.sqlite');
probe_test_bootstrap($dbFile);

Schema::ensure();
probe_seed_admin('probe-test-pass-1234');

// 任务执行器是独立进程，靠 DB_DSN 继承连到同一个临时库；上游指向模拟上游
$mock = probe_mock_start(0);
$models = ['ok-1', 'ok-2', 'denied-1', 'missing-1', 'badreq-1'];
$channelId = probe_seed_channel(
    '控制器测试渠道',
    'http://127.0.0.1:' . $mock['port'] . '/mock',
    $models,
    ['sk-controller-test-key-1a2b']
);
$noKeyChannel = probe_seed_channel('没有凭据的渠道', 'http://127.0.0.1:' . $mock['port'] . '/mock', ['ok-1'], []);
$noModelChannel = probe_seed_channel('没有模型的渠道', 'http://127.0.0.1:' . $mock['port'] . '/mock', [], ['sk-x']);

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('cookies', '.txt');
$csrfField = '_csrf';

echo "一、未登录一律进不来\n";

foreach ([
    ['GET', '/admin/channels/probe?id=1'],
    ['POST', '/admin/channels/probe'],
    ['GET', '/admin/channels/probe/status?id=1'],
    ['POST', '/admin/channels/probe/apply'],
] as [$method, $path]) {
    $response = probe_http($port, $method, $path);
    check(
        $method . ' ' . $path . ' 未登录 → 跳登录页',
        $response['status'] === 302 && $response['location'] === '/admin/login',
        '实际 ' . $response['status'] . ' → ' . $response['location']
    );
}

echo "\n二、登录与 CSRF\n";

check('能用密码登录后台', probe_login($port, $jar, 'probe-test-pass-1234'));

$list = probe_http($port, 'GET', '/admin/channels', [], $jar);
check('渠道列表页可访问', $list['status'] === 200, '实际 ' . $list['status']);
check(
    '渠道列表上有「检测模型」入口',
    str_contains($list['body'], 'action="/admin/channels/probe"'),
    '页面里没有该表单'
);
$csrf = probe_csrf($port, '/admin/channels', $jar);
check('能取到 CSRF 令牌', $csrf !== '');

$before = (int) (Db::selectOne('SELECT COUNT(*) AS c FROM channel_model_probe_tasks')['c'] ?? 0);

$noCsrf = probe_http($port, 'POST', '/admin/channels/probe', ['id' => (string) $channelId], $jar);
check('没有 CSRF 令牌的提交被拒（跳回列表页）', $noCsrf['status'] === 302 && $noCsrf['location'] === '/admin/channels', '实际 ' . $noCsrf['status']);
check('被拒的提交没有创建任务', (int) (Db::selectOne('SELECT COUNT(*) AS c FROM channel_model_probe_tasks')['c'] ?? 0) === $before);
$flash = probe_http($port, 'GET', '/admin/channels', [], $jar)['body'];
check('页面上给出了「页面已过期」的提示', str_contains($flash, '页面已过期'));

echo "\n三、发起检测的前置校验\n";

$noKey = probe_http($port, 'POST', '/admin/channels/probe', [$csrfField => $csrf, 'id' => (string) $noKeyChannel], $jar);
check('没有凭据的渠道 → 跳回列表', $noKey['status'] === 302 && $noKey['location'] === '/admin/channels', '实际 ' . $noKey['status']);
check('提示要求先添加 Key', str_contains(probe_http($port, 'GET', '/admin/channels', [], $jar)['body'], '没有可用凭据'));

$noModel = probe_http($port, 'POST', '/admin/channels/probe', [$csrfField => $csrf, 'id' => (string) $noModelChannel], $jar);
check('没有模型的渠道被拒', $noModel['status'] === 302);
check('提示要求先添加模型', str_contains(probe_http($port, 'GET', '/admin/channels', [], $jar)['body'], '没有模型清单'));

$missing = probe_http($port, 'POST', '/admin/channels/probe', [$csrfField => $csrf, 'id' => '999999'], $jar);
check('不存在的渠道被拒', $missing['status'] === 302);
check('提示渠道不存在', str_contains(probe_http($port, 'GET', '/admin/channels', [], $jar)['body'], '渠道不存在'));
check(
    '以上被拒的提交都没留下任务',
    (int) (Db::selectOne('SELECT COUNT(*) AS c FROM channel_model_probe_tasks')['c'] ?? 0) === $before
);

echo "\n四、发起检测与重复发起\n";

$created = probe_http($port, 'POST', '/admin/channels/probe', [$csrfField => $csrf, 'id' => (string) $channelId], $jar);
check('发起成功 → 跳任务页', $created['status'] === 302 && str_starts_with($created['location'], '/admin/channels/probe?id='), '实际 ' . $created['status'] . ' → ' . $created['location']);
$taskId = (int) substr($created['location'], (int) strrpos($created['location'], '=') + 1);
check('任务已入库', $taskId > 0 && ModelProbeTask::find($taskId) !== null);
check('任务记录了正确的渠道', (int) ModelProbeTask::find($taskId)['channel_id'] === $channelId);

$again = probe_http($port, 'POST', '/admin/channels/probe', [$csrfField => $csrf, 'id' => (string) $channelId], $jar);
check('同渠道重复发起 → 复用同一个任务', $again['location'] === '/admin/channels/probe?id=' . $taskId, '实际 ' . $again['location']);

echo "\n五、任务页与状态接口\n";

$page = probe_http($port, 'GET', '/admin/channels/probe?id=' . $taskId, [], $jar);
check('任务页可访问', $page['status'] === 200, '实际 ' . $page['status']);
check('任务页显示渠道名', str_contains($page['body'], '控制器测试渠道'));
check('任务页带任务编号', str_contains($page['body'], '任务 #' . $taskId));
check('任务页不含完整凭据', !str_contains($page['body'], 'sk-controller-test-key-1a2b'));

$badPage = probe_http($port, 'GET', '/admin/channels/probe?id=999999', [], $jar);
check('不存在的任务页 → 404', $badPage['status'] === 404, '实际 ' . $badPage['status']);
$badStatus = probe_http($port, 'GET', '/admin/channels/probe/status?id=999999', [], $jar);
check('不存在的任务状态 → 404', $badStatus['status'] === 404, '实际 ' . $badStatus['status']);

$completed = probe_test_wait(static function () use ($taskId): bool {
    $task = ModelProbeTask::find($taskId);

    return $task !== null && in_array((string) $task['status'], ['completed', 'failed', 'aborted'], true);
}, 90000, 250);
check('任务在后台跑到了终态', $completed, '状态：' . (string) (ModelProbeTask::find($taskId)['status'] ?? '未知'));
check('终态是已完成', (string) ModelProbeTask::find($taskId)['status'] === 'completed', (string) ModelProbeTask::find($taskId)['status']);

$status = probe_http($port, 'GET', '/admin/channels/probe/status?id=' . $taskId, [], $jar);
check('状态接口返回 200', $status['status'] === 200, '实际 ' . $status['status']);
check('状态接口禁止缓存（否则页面会看到旧进度）', stripos($status['headers'], 'Cache-Control: no-store') !== false, $status['headers']);
$payload = json_decode($status['body'], true);
check('状态接口返回任务与结果', is_array($payload) && isset($payload['task'], $payload['results']));
check('状态接口不返回备份路径', !array_key_exists('backup_path', (array) ($payload['task'] ?? [])));
check('状态接口不返回操作人', !array_key_exists('applied_by', (array) ($payload['task'] ?? [])));
check('状态接口不含完整凭据', !str_contains($status['body'], 'sk-controller-test-key-1a2b'));

$resultModels = array_column((array) ($payload['results'] ?? []), 'classification', 'model');
check('结果里 200 的模型判为可用', ($resultModels['ok-1'] ?? '') === 'ok');
check('结果里权限型 404 判为无权限', ($resultModels['denied-1'] ?? '') === 'no_access');
check('结果里普通 404 判为当前协议不可用', ($resultModels['missing-1'] ?? '') === 'unroutable');
check('结果里 400 判为无法判定（保留）', ($resultModels['badreq-1'] ?? '') === 'inconclusive');

$donePage = probe_http($port, 'GET', '/admin/channels/probe?id=' . $taskId, [], $jar);
check('完成后页面出现分组结果', str_contains($donePage['body'], '当前协议不可用（1）'), '页面没有分组标题');
check('状态显示为中文', str_contains($donePage['body'], '已完成'), '页面没有中文状态');
check('确定不可用的模型默认被勾选', str_contains($donePage['body'], 'name="models[]" value="denied-1" checked'));
check('无法判定的模型没有勾选框', !str_contains($donePage['body'], 'name="models[]" value="badreq-1"'));

echo "\n六、确认下架\n";

$modelsBefore = Channel::modelsOf(Channel::find($channelId));

$crossTask = probe_http($port, 'POST', '/admin/channels/probe/apply', [
    $csrfField => $csrf,
    'id' => (string) $taskId,
    'models' => ['ok-1'],
], $jar);
check('提交可用模型 → 被拒', $crossTask['status'] === 302 && $crossTask['location'] === '/admin/channels/probe?id=' . $taskId, '实际 ' . $crossTask['location']);
check('被拒的提交没有改动模型清单', Channel::modelsOf(Channel::find($channelId)) === $modelsBefore);
check('页面给出拒绝原因', str_contains(probe_http($port, 'GET', '/admin/channels/probe?id=' . $taskId, [], $jar)['body'], '不允许下架'));

$badId = probe_http($port, 'POST', '/admin/channels/probe/apply', [
    $csrfField => $csrf,
    'id' => '999999',
    'models' => ['denied-1'],
], $jar);
check('对不存在的任务下架 → 被拒', $badId['status'] === 302);
check('被拒后清单仍未改动', Channel::modelsOf(Channel::find($channelId)) === $modelsBefore);

$applied = probe_http($port, 'POST', '/admin/channels/probe/apply', [
    $csrfField => $csrf,
    'id' => (string) $taskId,
    'models' => ['denied-1', 'missing-1'],
], $jar);
check('下架请求被接受', $applied['status'] === 302);
$after = probe_http($port, 'GET', '/admin/channels', [], $jar)['body'];
check('列表页提示下架结果', str_contains($after, '已下架 2 个模型'));

$modelsAfter = Channel::modelsOf(Channel::find($channelId));
check('只移除了选中的两个模型', $modelsAfter === ['ok-1', 'ok-2', 'badreq-1'], implode(',', $modelsAfter));
$taskRow = ModelProbeTask::find($taskId);
check('任务记录了操作人', trim((string) $taskRow['applied_by']) !== '');
check('任务记录了移除数量', (int) $taskRow['removed_count'] === 2);
check('原始清单已备份到磁盘', is_file((string) $taskRow['backup_path']), (string) $taskRow['backup_path']);
if (is_file((string) $taskRow['backup_path'])) {
    $backup = json_decode((string) file_get_contents((string) $taskRow['backup_path']), true);
    check('备份里保存了原始清单', ($backup['original_models'] ?? []) === $modelsBefore, implode(',', (array) ($backup['original_models'] ?? [])));
    @unlink((string) $taskRow['backup_path']);
}

$second = probe_http($port, 'POST', '/admin/channels/probe/apply', [
    $csrfField => $csrf,
    'id' => (string) $taskId,
    'models' => ['ok-1'],
], $jar);
check('同一任务不能下架两次', $second['status'] === 302);
check('重复下架没有再次改动清单', Channel::modelsOf(Channel::find($channelId)) === $modelsAfter);
check('页面提示已经执行过下架', str_contains(probe_http($port, 'GET', '/admin/channels/probe?id=' . $taskId, [], $jar)['body'], '已经执行过下架'));

exit(probe_test_report());
