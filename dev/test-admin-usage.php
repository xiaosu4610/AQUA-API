<?php
/**
 * 后台「用量与报表」的测试。
 *
 * 报表页最容易出的错不是崩掉，而是**各处数字对不上**：
 * 总览说 100 条、明细只有 30 条、模型汇总加起来又是另一个数。
 * 一旦出现这种情况，站长会开始怀疑整个统计是不是假的 ——
 * 所以这个测试的重点是「同一时间窗下，各块的数字必须自洽」。
 *
 * 覆盖：
 *   一、聚合口径（区间汇总 / 按天 / 按模型 / 失败原因 / 用户排行）
 *   二、时间窗筛选（今天 / 近 7 天）真的会切掉窗口外的记录
 *   三、模型名筛选
 *   四、分页
 *   五、页面结构（趋势图、明细、口径说明）
 *
 * 用法：php dev/test-admin-usage.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Db;
use app\common\Schema;
use app\common\UsageLog;
use app\common\User;

$dbFile = probe_test_temp('usage-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('usage-test-pass-1234');

$todayStart = strtotime('today') ?: time();

/** 插一条用量记录（时间可控，方便造出「今天/昨天/几天前」的场景） */
function usage_row(int $createdAt, string $model, float $downstream, float $upstream, int $userId, string $status = 'ok', string $error = '', int $latency = 200): void
{
    Db::execute(
        'INSERT INTO usage_logs (created_at, user_id, token_id, channel_id, channel_name, model, billing_mode,
                                 is_stream, prompt_tokens, completion_tokens, total_tokens, usage_estimated,
                                 upstream_cost, downstream_cost, latency_ms, status, error_message)
         VALUES (?, ?, NULL, NULL, ?, ?, ?, 0, 40, 60, 100, 0, ?, ?, ?, ?, ?)',
        [$createdAt, $userId, '测试渠道', $model, 'token', $upstream, $downstream, $latency, $status, $error === '' ? null : $error]
    );
}

$userA = User::register('usage-a@example.com', 'password123', '127.0.0.1', false)['id'];
$userB = User::register('usage-b@example.com', 'password123', '127.0.0.1', false)['id'];

// 今天：55 条 llama（分页会翻到第 2 页）
for ($i = 0; $i < 55; $i++) {
    usage_row($todayStart + 3600 + $i, 'llama-3.1-8b', 0.01, 0.004, (int) $userA);
}
// 昨天：5 条 gpt-4o-mini，其中 2 条失败
for ($i = 0; $i < 3; $i++) {
    usage_row($todayStart - 86400 + 3600 + $i, 'gpt-4o-mini', 0.02, 0.01, (int) $userB);
}
usage_row($todayStart - 86400 + 7200, 'gpt-4o-mini', 0.02, 0.01, (int) $userB, 'error', 'rate limited by upstream');
usage_row($todayStart - 86400 + 7300, 'gpt-4o-mini', 0.02, 0.01, (int) $userB, 'error', 'rate limited by upstream');
// 5 天前：3 条 llama（在「近 7 天」窗口内，在「今天」窗口外）
for ($i = 0; $i < 3; $i++) {
    usage_row($todayStart - 5 * 86400 + 3600 + $i, 'llama-3.1-8b', 0.01, 0.004, (int) $userA);
}

echo "一、聚合口径\n";

$from = $todayStart;
$to = $todayStart + 86400;
$today = UsageLog::statsRange($from, $to);
check('今天的请求数为 55', $today['requests'] === 55, (string) $today['requests']);
check('今天的 token 数为 5500', $today['prompt_tokens'] + $today['completion_tokens'] === 5500, (string) $today['prompt_tokens']);
check('今天的用户消费为 0.55', abs($today['downstream_cost'] - 0.55) < 1e-9, (string) $today['downstream_cost']);
check('今天的上游成本为 0.22', abs($today['upstream_cost'] - 0.22) < 1e-9, (string) $today['upstream_cost']);
check('今天的毛利 = 消费 − 成本', abs($today['profit'] - 0.33) < 1e-9, (string) $today['profit']);
check('今天的平均耗时被算出来', $today['avg_latency_ms'] === 200, (string) $today['avg_latency_ms']);
check('今天没有失败', $today['errors'] === 0, (string) $today['errors']);

$weekFrom = $todayStart - 6 * 86400;
$week = UsageLog::statsRange($weekFrom, $todayStart + 86400);
check('近 7 天请求数为 63', $week['requests'] === 63, (string) $week['requests']);
check('近 7 天失败数为 2', $week['errors'] === 2, (string) $week['errors']);

$series = UsageLog::dailySeries(7);
check('按天拆出来正好 7 天', count($series) === 7, (string) count($series));
check('最后一天是今天', $series[6]['date'] === date('Y-m-d', $todayStart), (string) $series[6]['date']);
check('今天那一天统计到 55 条', (int) $series[6]['requests'] === 55, (string) $series[6]['requests']);
check('昨天那一天统计到 5 条', (int) $series[5]['requests'] === 5, (string) $series[5]['requests']);
check('昨天那一天有 2 次失败', (int) $series[5]['errors'] === 2, (string) $series[5]['errors']);
$seriesTotal = array_sum(array_map(static fn (array $d): int => (int) $d['requests'], $series));
check('按天相加等于区间总数（口径一致）', $seriesTotal === 63, (string) $seriesTotal);

$byModel = UsageLog::byModel($weekFrom, $todayStart + 86400);
check('模型汇总按请求数倒序', $byModel[0]['model'] === 'llama-3.1-8b', $byModel[0]['model']);
check('llama 共 58 条', (int) $byModel[0]['requests'] === 58, (string) $byModel[0]['requests']);
check('llama 全部成功，成功率 100%', (int) $byModel[0]['success_rate'] === 100, (string) $byModel[0]['success_rate']);
check('gpt-4o-mini 共 5 条', (int) $byModel[1]['requests'] === 5, (string) $byModel[1]['requests']);
check('gpt-4o-mini 成功率 60%', (int) $byModel[1]['success_rate'] === 60, (string) $byModel[1]['success_rate']);
check('模型汇总的请求数之和等于区间总数', array_sum(array_map(
    static fn (array $m): int => (int) $m['requests'],
    $byModel
)) === 63);

$errors = UsageLog::topErrors($weekFrom, $todayStart + 86400);
check('失败原因被归并成一条', count($errors) === 1, (string) count($errors));
check('失败原因原文被保留', $errors[0]['message'] === 'rate limited by upstream', $errors[0]['message']);
check('该原因出现 2 次', $errors[0]['count'] === 2, (string) $errors[0]['count']);

$byUser = UsageLog::topUsers($weekFrom, $todayStart + 86400, 10);
check('用户排行按请求数倒序', (int) $byUser[0]['userId'] === (int) $userA, (string) $byUser[0]['userId']);
check('用户排行显示邮箱', $byUser[0]['email'] === 'usage-a@example.com', $byUser[0]['email']);
check('用户的请求数与用户维度一致', (int) $byUser[0]['requests'] === 58, (string) $byUser[0]['requests']);
check('用户排行的请求数之和等于区间总数', array_sum(array_map(
    static fn (array $u): int => (int) $u['requests'],
    $byUser
)) === 63);

echo "\n二、明细列表与时间窗一致\n";

$todayRows = UsageLog::listInRange($from, $to, '', 200, 0);
check('今天的明细正好 55 条', count($todayRows) === 55, (string) count($todayRows));
check('明细条数与 countInRange 一致', UsageLog::countInRange($from, $to) === 55, (string) UsageLog::countInRange($from, $to));
$weekRows = UsageLog::listInRange($weekFrom, $todayStart + 86400, '', 200, 0);
check('近 7 天明细 63 条', count($weekRows) === 63, (string) count($weekRows));
check('筛选模型后只剩该模型', UsageLog::countInRange($weekFrom, $todayStart + 86400, 'gpt-4o-mini') === 5, (string) UsageLog::countInRange($weekFrom, $todayStart + 86400, 'gpt-4o-mini'));

echo "\n三、页面\n";

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('usage-cookies', '.txt');
check('登录后台', probe_login($port, $jar, 'usage-test-pass-1234'));

$page = probe_http($port, 'GET', '/admin/usage', [], $jar);
check('报表页可访问', $page['status'] === 200, (string) $page['status']);
check('有「用量与报表」标题', str_contains($page['body'], '用量与报表'));
check('有总览', str_contains($page['body'], '总览'));
check('有每日趋势', str_contains($page['body'], '每日趋势'));
check('有模型汇总', str_contains($page['body'], '模型汇总'));
check('有用户用量排行', str_contains($page['body'], '用户用量排行'));
check('有失败原因排行', str_contains($page['body'], '失败原因排行'));
check('显示两个模型名', str_contains($page['body'], 'llama-3.1-8b') && str_contains($page['body'], 'gpt-4o-mini'));
check('显示失败原因原文', str_contains($page['body'], 'rate limited by upstream'));
check('显示成功率', str_contains($page['body'], '60%'));
check('显示毛利口径', str_contains($page['body'], '毛利'));
check('显示日志保留天数说明', str_contains($page['body'], '用量日志保留天数'));

$bars = substr_count($page['body'], '<div class="bar');
check('近 7 天画出 7 根柱子', $bars === 7, '实际 ' . $bars . ' 根');

$todayPage = probe_http($port, 'GET', '/admin/usage?range=today', [], $jar);
check('切到「今天」仍是 200', $todayPage['status'] === 200, (string) $todayPage['status']);
check('「今天」不再出现昨天的模型', !str_contains($todayPage['body'], 'gpt-4o-mini'));
check('「今天」仍有今天调用过的模型', str_contains($todayPage['body'], 'llama-3.1-8b'));
$todayBars = substr_count($todayPage['body'], '<div class="bar');
check('「今天」只画 1 根柱子', $todayBars === 1, '实际 ' . $todayBars . ' 根');
check('「今天」明细共 55 条', str_contains($todayPage['body'], '55 条'), '页面没有显示 55 条');

$modelPage = probe_http($port, 'GET', '/admin/usage?model=gpt-4o-mini', [], $jar);
check('按模型筛选后仍是 200', $modelPage['status'] === 200, (string) $modelPage['status']);
check('筛选结果只含 5 条', str_contains($modelPage['body'], '5 条'), '页面没有显示 5 条');
check('筛选后没有另一个模型', !str_contains($modelPage['body'], 'llama-3.1-8b'));
check('筛选后的用户排行同步收窄（口径一致）', !str_contains($modelPage['body'], 'usage-a@example.com'), '用户排行没跟着筛选');

$llamaPage = probe_http($port, 'GET', '/admin/usage?model=llama', [], $jar);
check('筛选 llama 后只剩它的 58 条', str_contains($llamaPage['body'], '58 条'), '没有显示 58 条');
check('筛选 llama 后看不到别的模型的失败原因', !str_contains($llamaPage['body'], 'rate limited by upstream'));

$badRange = probe_http($port, 'GET', '/admin/usage?range=不存在', [], $jar);
check('非法时间档位被当成默认值而不是报错', $badRange['status'] === 200, (string) $badRange['status']);

echo "\n四、分页\n";

$page1 = probe_http($port, 'GET', '/admin/usage?range=today', [], $jar);
check('第一页显示分页信息', str_contains($page1['body'], '第 1 / 2 页'), '没有分页信息');
$page2 = probe_http($port, 'GET', '/admin/usage?range=today&page=2', [], $jar);
check('第二页可访问', $page2['status'] === 200, (string) $page2['status']);
check('第二页显示第 2 页', str_contains($page2['body'], '第 2 / 2 页'));
$beyond = probe_http($port, 'GET', '/admin/usage?range=today&page=99', [], $jar);
check('页码超过总数被夹到最后一页', $beyond['status'] === 200 && str_contains($beyond['body'], '第 2 / 2 页'));

$otherPage = probe_http($port, 'GET', '/admin/usage?range=today&model=llama-3.1-8b&page=2', [], $jar);
check('翻页不会丢掉筛选条件', $otherPage['status'] === 200 && str_contains($otherPage['body'], 'llama-3.1-8b'));

echo "\n五、入口\n";

$dashboard = probe_http($port, 'GET', '/admin', [], $jar);
check('后台导航里有「用量报表」入口', str_contains($dashboard['body'], '/admin/usage'), '导航没有入口');
$noLogin = probe_http($port, 'GET', '/admin/usage');
check('未登录访问会被挡去登录页', $noLogin['status'] === 302, (string) $noLogin['status']);

exit(probe_test_report());
