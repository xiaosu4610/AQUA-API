<?php
/**
 * 用户编号：自助注销 + 连续编号（补位）的测试。
 *
 * 为什么这些必须严格测：补位是**直接搬主键**（UPDATE users SET id = ?）。
 * 它是本项目里少数几个会把「谁是谁」整体重写一遍的动作，
 * 一旦写错就是「A 的余额与令牌跑到 B 名下」——资金事故级别。
 *
 * 覆盖的东西：
 *   一、编号从 1 开始、连续分配
 *   二、注销：令牌删掉、订单与用量保留但解绑
 *   三、空位检测（COUNT vs MAX）
 *   四、执行条件：有在途流量时不许动手
 *   五、补位：users / tokens / payment_orders / usage_logs 四张表一起改
 *   六、补位后新用户接着编号继续（自增序列已重置）
 *   七、代际 +1：重排前的登录态全部失效（HTTP 端到端）
 *   八、后台「立即补位编号」按钮与动作（含 CSRF、流量在跑时拒绝）
 *   九、控制台 BaseURL 复制区块、注销入口、接口文档页
 *
 * 用法：php dev/test-user-id-compact.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Db;
use app\common\Schema;
use app\common\Settings;
use app\common\User;
use app\common\UserToken;

$dbFile = probe_test_temp('uid-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('uid-test-pass-1234');

// 注册相关的开关：本测试关心的是编号，不关心验证码
Settings::put('register.open', true);
Settings::put('register.need_verify', false);
Settings::put('register.verify_email', false);
Settings::put('register.invite_code', '');
Settings::put('register.gift_balance', 0);
Settings::put('register.default_token_quota', 0);
Settings::put('users.auto_compact_ids', true);
Settings::put('users.compact_interval_days', 3);

/**
 * 某条记录的 user_id 是不是真的 NULL。
 *
 * ⚠️ 不能用 `$row['user_id'] ?? 'x'` 来判断：`??` 把 NULL 与「键不存在」
 * 当成同一回事，于是「归属已清空」会被判成失败 —— 这个坑本人先踩过一次。
 */
function uid_is_null(string $where, array $params = []): bool
{
    $row = Db::selectOne("SELECT user_id FROM {$where}", $params);

    return $row !== null && array_key_exists('user_id', $row) && $row['user_id'] === null;
}

/** 取页面上第一条提示条的文字（用来断言「到底被哪个条件挡下了」） */
function uid_alert(int $port, string $path, string $jar): string
{
    $body = probe_http($port, 'GET', $path, [], $jar)['body'];
    if (preg_match('#<div class="alert [a-z]+">(.*?)</div>#s', $body, $m)) {
        return trim(html_entity_decode(strip_tags($m[1])));
    }

    return '';
}

/** 某条记录的 user_id（查不到时返回 -1，便于断言失败时看出问题） */
function uid_of(string $where, array $params = []): int
{
    $row = Db::selectOne("SELECT user_id FROM {$where}", $params);

    return $row === null ? -1 : (int) $row['user_id'];
}

/** 建一份「用户 → 令牌 + 订单 + 用量」的完整关联数据 */
function uid_seed_relations(int $userId, string $tag): void
{
    UserToken::create($userId, $tag . ' 的令牌');

    Db::execute(
        'INSERT INTO payment_orders (order_no, user_id, amount, credit, gateway, pay_type, status, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)',
        ['ORDER-' . $tag, $userId, '10', '10', 'epay_v1', 'alipay', 'paid', time() - 1000]
    );

    Db::execute(
        'INSERT INTO usage_logs (created_at, user_id, token_id, channel_id, channel_name, model, billing_mode,
                                 is_stream, prompt_tokens, completion_tokens, total_tokens, usage_estimated,
                                 upstream_cost, downstream_cost, latency_ms, status, error_message)
         VALUES (?, ?, NULL, NULL, ?, ?, ?, 0, 10, 20, 30, 0, 0, 0, 0, ?, NULL)',
        [time() - 1000, $userId, '测试渠道', 'model-' . $tag, 'token', 'ok']
    );
}

echo "一、编号从 1 开始、连续分配\n";

$ids = [];
foreach (['a', 'b', 'c', 'd', 'e'] as $tag) {
    $created = User::register("u{$tag}@example.com", 'password123', '127.0.0.1', false);
    check("注册 u{$tag}", $created['ok'], $created['message']);
    $ids[$tag] = $created['id'];
}

check('第一个用户编号是 1（不是 3）', $ids['a'] === 1, '实际 ' . $ids['a']);
check(
    '五个用户编号连续 1..5',
    array_values($ids) === [1, 2, 3, 4, 5],
    implode(',', $ids)
);
check('没有空位时不需要补位', User::needsCompact() === false);

echo "\n二、注销：令牌删掉、订单与用量保留但解绑\n";

uid_seed_relations($ids['b'], 'B');
uid_seed_relations($ids['c'], 'C');
uid_seed_relations($ids['d'], 'D');

check('注销前 b 有 1 把令牌', count(UserToken::allForUser($ids['b'])) === 1);
check('注销不存在的用户返回 false', User::deleteAccount(999999) === false);

check('注销 b 成功', User::deleteAccount($ids['b']) === true);
check('b 的用户行已删除', User::find($ids['b']) === null);
check('b 的邮箱已可重新注册', User::findByEmail('ub@example.com') === null);
check('b 的令牌被一起删掉', count(UserToken::allForUser($ids['b'])) === 0);
check('b 的订单变成「无主」（user_id=0）', uid_of("payment_orders WHERE order_no = 'ORDER-B'") === 0);
check('b 的用量记录保留但归属清空（NULL）', uid_is_null("usage_logs WHERE model = 'model-B'"));
check('b 的用量记录本身还在（不是被删了）', Db::selectOne("SELECT id FROM usage_logs WHERE model = 'model-B'") !== null);

check('注销 d 成功', User::deleteAccount($ids['d']) === true);
check('d 的订单同样变成「无主」', uid_of("payment_orders WHERE order_no = 'ORDER-D'") === 0);

echo "\n三、空位检测\n";

$stat = User::selectCountAndMax();
check('剩余账号 3 个', $stat['total'] === 3, (string) $stat['total']);
check('最大编号仍是 5', $stat['max'] === 5, (string) $stat['max']);
check('检测到需要补位', User::needsCompact() === true);

echo "\n四、执行条件：有在途流量时不动手\n";

Db::execute('INSERT INTO usage_logs (created_at, user_id, model, billing_mode, is_stream, prompt_tokens,
             completion_tokens, total_tokens, usage_estimated, upstream_cost, downstream_cost, latency_ms, status)
             VALUES (?, NULL, ?, ?, 0, 1, 1, 2, 0, 0, 0, 0, ?)',
    [time(), 'just-now', 'token', 'ok']);
check('刚有调用 → 不允许补位', User::canCompactNow() === false);

Db::execute("UPDATE usage_logs SET created_at = ? WHERE model = 'just-now'", [time() - 1000]);
check('流量安静后 → 允许补位', User::canCompactNow() === true);

echo "\n五、补位：四张表一起改\n";

$epochBefore = User::idEpoch();
$before = array_map(static fn (array $r): int => (int) $r['id'], Db::select('SELECT id FROM users ORDER BY id ASC'));
check('补位前编号是 1,3,5', $before === [1, 3, 5], implode(',', $before));

$cEmail = (string) (Db::selectOne('SELECT email FROM users WHERE id = ?', [$ids['c']])['email'] ?? '');
$eEmail = (string) (Db::selectOne('SELECT email FROM users WHERE id = ?', [$ids['e']])['email'] ?? '');

$result = User::compactIds();
check('补位执行成功', $result['changed'] === true);
check('移动了 2 个账号（3→2、5→3）', $result['moved'] === 2, (string) $result['moved']);

$after = array_map(static fn (array $r): int => (int) $r['id'], Db::select('SELECT id FROM users ORDER BY id ASC'));
check('补位后编号是 1,2,3', $after === [1, 2, 3], implode(',', $after));

check('2 号现在是 c（按原顺序）', (string) User::find(2)['email'] === $cEmail, (string) User::find(2)['email']);
check('3 号现在是 e', (string) User::find(3)['email'] === $eEmail, (string) User::find(3)['email']);

check('c 的令牌跟到 2 号', count(UserToken::allForUser(2)) === 1);
check('c 的订单跟到 2 号', uid_of("payment_orders WHERE order_no = 'ORDER-C'") === 2);
check('c 的用量记录跟到 2 号', uid_of("usage_logs WHERE model = 'model-C'") === 2);
check('b 的订单仍然是无主（0）', uid_of("payment_orders WHERE order_no = 'ORDER-B'") === 0);
check(
    '没有用量记录指向已不存在的编号',
    (int) (Db::selectOne('SELECT COUNT(*) AS c FROM usage_logs WHERE user_id IS NOT NULL AND user_id > ?', [count($after)])['c'] ?? 0) === 0
);

check('代际 +1', User::idEpoch() === $epochBefore + 1, (string) User::idEpoch());
check('补位时间已记录', Settings::int('users.last_compacted_at', 0) > 0);
check('补位后不再需要补位', User::needsCompact() === false);
check('编号已连续时再补位是空操作', User::compactIds()['changed'] === false);

echo "\n六、补位后新用户接着编号继续\n";

$next = User::register('uf@example.com', 'password123', '127.0.0.1', false);
check('新用户拿到 4 号（不是 6 号）', $next['id'] === 4, '实际 ' . $next['id']);
check('仍然连续', User::needsCompact() === false);

echo "\n七、代际失效：重排前建立的登录态全部作废（HTTP 端到端）\n";

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('uid-user-cookies', '.txt');

$registerCsrf = probe_csrf($port, '/register', $jar);
$httpRegister = probe_http($port, 'POST', '/register', [
    '_csrf' => $registerCsrf,
    'email' => 'httpuser@example.com',
    'password' => 'password123',
    'password2' => 'password123',
], $jar);
check('HTTP 注册成功（拿到注册成功页）', str_contains($httpRegister['body'], '注册成功'));

$httpUser = User::findByEmail('httpuser@example.com');
check('HTTP 用户已入库', $httpUser !== null);
$httpUserId = (int) ($httpUser['id'] ?? 0);
check('HTTP 用户编号接着排在后面', $httpUserId === 5, (string) $httpUserId);

$console = probe_http($port, 'GET', '/console', [], $jar);
check('控制台可访问（已登录）', $console['status'] === 200, (string) $console['status']);

echo "\n八、后台手动补位 + 控制台/文档页内容\n";

$adminJar = probe_test_temp('uid-admin-cookies', '.txt');
check('登录后台', probe_login($port, $adminJar, 'uid-test-pass-1234'));
$adminCsrf = probe_csrf($port, '/admin/users', $adminJar);

$usersPage = probe_http($port, 'GET', '/admin/users', [], $adminJar);
check('用户页有「用户编号维护」区块', str_contains($usersPage['body'], '用户编号维护'));
check('用户页有「立即补位编号」按钮', str_contains($usersPage['body'], '立即补位编号'));
check('没有空位时提示编号连续', str_contains($usersPage['body'], '编号连续'));

check('控制台显示 Base URL 复制区块', str_contains($console['body'], 'Base URL'));
check('控制台显示接入地址（带 /v1）', str_contains($console['body'], '/v1'));
check('控制台有复制按钮', str_contains($console['body'], 'data-copy='));
check('控制台有「注销账号」入口', str_contains($console['body'], '/console/delete'));
check('控制台有调用示例', str_contains($console['body'], 'chat/completions'));

$deletePage = probe_http($port, 'GET', '/console/delete', [], $jar);
check('注销确认页可访问', $deletePage['status'] === 200, (string) $deletePage['status']);
check('注销页要求输入密码', str_contains($deletePage['body'], 'name="password"'));
check('注销页要求二次输入邮箱', str_contains($deletePage['body'], 'name="confirm_email"'));
check('注销页说明了令牌会被一起删除', str_contains($deletePage['body'], '令牌'));

$docs = probe_http($port, 'GET', '/docs');
check('接口文档页可访问（无需登录）', $docs['status'] === 200, (string) $docs['status']);
check('文档页显示接入地址', str_contains($docs['body'], 'chat/completions'));
check('文档页列出接口', str_contains($docs['body'], '/v1/models'));
check('文档页有 curl 示例', str_contains($docs['body'], 'curl'));

$home = probe_http($port, 'GET', '/');
check('首页可访问', $home['status'] === 200, (string) $home['status']);
check('首页有 API 文档入口', str_contains($home['body'], '/docs'));
check('首页首屏有接入地址复制', str_contains($home['body'], 'data-copy='));

// ⚠️ 冷启动检查：上面的断言是在「同一个进程已经渲染过很多页面」的前提下做的。
// 常驻内存模型里，模板 include 进来的函数（icon 之类）会**留在进程里**，
// 于是「本页忘了引入图标集」这种错在开发机上完全测不出来 ——
// 只有在全新进程里第一次请求这一页时才 500。生产就是这样炸过一次，
// 所以这里为每个公开页各起一个全新服务，专测「第一个请求」。
echo "\n九、冷启动：每个公开页在全新进程里都能独立渲染\n";

foreach (['/' => '首页', '/docs' => '接口文档', '/models' => '模型广场'] as $path => $label) {
    $cold = probe_app_start();
    $coldPort = $cold['port'];
    $first = probe_http($coldPort, 'GET', $path);
    check(
        "全新进程的第一个请求 {$label}（{$path}）返回 200",
        $first['status'] === 200,
        '实际 ' . $first['status'] . '（很可能是模板少引入了图标集）'
    );
}

echo "\n十、重排后旧登录态作废（后台手动补位）\n";

// HTTP 用户的令牌必须真的建出来了（注册流程会自动建一把默认令牌）
$tokenRow = Db::selectOne('SELECT key_mask FROM tokens WHERE user_id = ?', [$httpUserId]);
check('HTTP 用户有一把默认令牌', $tokenRow !== null);

// 制造空位：删掉 4 号（uf），剩余 1,2,3,5 → 需要补位
check('删掉 4 号制造空位', User::deleteAccount(4) === true);
check('检测到空位', User::needsCompact() === true);

$noCsrf = probe_http($port, 'POST', '/admin/users/compact', [], $adminJar);
check('没有 CSRF → 被拒并跳回用户页', $noCsrf['status'] === 302 && $noCsrf['location'] === '/admin/users', $noCsrf['location']);
check('被拒时代际没有变化', User::idEpoch() === $epochBefore + 1);

$compacted = probe_http($port, 'POST', '/admin/users/compact', ['_csrf' => $adminCsrf], $adminJar);
check('后台补位成功并跳回', $compacted['status'] === 302 && $compacted['location'] === '/admin/users', $compacted['location']);
$noticeAfterCompact = uid_alert($port, '/admin/users', $adminJar);
check('后台提示「编号已重排」', str_contains($noticeAfterCompact, '编号已重排'), '实际提示：' . $noticeAfterCompact);
Settings::forget();
check('代际再次 +1', User::idEpoch() === $epochBefore + 2, (string) User::idEpoch());

$afterConsole = probe_http($port, 'GET', '/console', [], $jar);
check('重排前的用户登录态已失效（跳去登录页）', $afterConsole['status'] === 302 && $afterConsole['location'] === '/login', $afterConsole['location']);

// 重新登录后一切正常：编号变了但账号/令牌不变
$loginCsrf = probe_csrf($port, '/login', $jar);
$relogin = probe_http($port, 'POST', '/login', [
    '_csrf' => $loginCsrf,
    'email' => 'httpuser@example.com',
    'password' => 'password123',
], $jar);
check('用原邮箱密码可以重新登录', $relogin['status'] === 302 && $relogin['location'] === '/console', $relogin['location']);
check('新编号是 4', (int) User::findByEmail('httpuser@example.com')['id'] === 4);

$finalConsole = probe_http($port, 'GET', '/console', [], $jar);
check('重新登录后控制台可用', $finalConsole['status'] === 200, (string) $finalConsole['status']);

echo "\n十、流量在跑时后台会拒绝补位\n";

check('删掉 2 号制造空位', User::deleteAccount(2) === true);
check('需要补位', User::needsCompact() === true);
Db::execute('INSERT INTO usage_logs (created_at, user_id, model, billing_mode, is_stream, prompt_tokens,
             completion_tokens, total_tokens, usage_estimated, upstream_cost, downstream_cost, latency_ms, status)
             VALUES (?, NULL, ?, ?, 0, 1, 1, 2, 0, 0, 0, 0, ?)',
    [time(), 'busy-now', 'token', 'ok']);

$busy = probe_http($port, 'POST', '/admin/users/compact', ['_csrf' => $adminCsrf], $adminJar);
check('有流量时补位被拒（跳回并提示）', $busy['status'] === 302);
check('被拒时代际未变', User::idEpoch() === $epochBefore + 2, (string) User::idEpoch());
check('被拒时编号仍未补位', User::needsCompact() === true);

$busyPage = probe_http($port, 'GET', '/admin/users', [], $adminJar);
check('页面给出「有调用流量」的提示', str_contains($busyPage['body'], '有调用流量'));

Db::execute("UPDATE usage_logs SET created_at = ? WHERE model = 'busy-now'", [time() - 1000]);
$retry = probe_http($port, 'POST', '/admin/users/compact', ['_csrf' => $adminCsrf], $adminJar);
check('流量安静后重试成功', $retry['status'] === 302);
check('编号已补位', User::needsCompact() === false);
// 配置有 5 秒进程内缓存（Settings::CACHE_TTL），跨进程刚写完的值要主动失效才读得到
Settings::forget();
check('代际 +1', User::idEpoch() === $epochBefore + 3, (string) User::idEpoch());

echo "\n十一、用户自助注销（二次确认，HTTP）\n";

$delJar = probe_test_temp('uid-delete-cookies', '.txt');
$delCsrf = probe_csrf($port, '/register', $delJar);
probe_http($port, 'POST', '/register', [
    '_csrf' => $delCsrf,
    'email' => 'deluser@example.com',
    'password' => 'password123',
    'password2' => 'password123',
], $delJar);

$delUser = User::findByEmail('deluser@example.com');
check('待注销用户已注册', $delUser !== null);
$delId = (int) ($delUser['id'] ?? 0);

$delPageCsrf = probe_csrf($port, '/console/delete', $delJar);
check('注销页能取到 CSRF', $delPageCsrf !== '');

$noCsrfDelete = probe_http($port, 'POST', '/console/delete', ['password' => 'password123', 'confirm_email' => 'deluser@example.com'], $delJar);
check('没有 CSRF → 被拒并留在注销页', $noCsrfDelete['status'] === 302 && $noCsrfDelete['location'] === '/console/delete', $noCsrfDelete['location']);
check('账号没有被删', User::find($delId) !== null);

$wrongPass = probe_http($port, 'POST', '/console/delete', [
    '_csrf' => $delPageCsrf,
    'password' => 'wrong-password',
    'confirm_email' => 'deluser@example.com',
], $delJar);
check('密码错 → 留在注销页', $wrongPass['status'] === 302 && $wrongPass['location'] === '/console/delete');
check('密码错时账号还在', User::find($delId) !== null);

$wrongEmail = probe_http($port, 'POST', '/console/delete', [
    '_csrf' => $delPageCsrf,
    'password' => 'password123',
    'confirm_email' => 'not-my-email@example.com',
], $delJar);
check('邮箱没对上 → 留在注销页', $wrongEmail['status'] === 302 && $wrongEmail['location'] === '/console/delete');
check('邮箱没对上时账号还在', User::find($delId) !== null);

$done = probe_http($port, 'POST', '/console/delete', [
    '_csrf' => $delPageCsrf,
    'password' => 'password123',
    'confirm_email' => 'deluser@example.com',
], $delJar);
check('两次确认都对了 → 注销成功', str_contains($done['body'], '账号已注销'));
check('用户行已删除', User::find($delId) === null);
check('令牌已删除', count(UserToken::allForUser($delId)) === 0);

$afterDelete = probe_http($port, 'GET', '/console', [], $delJar);
check('注销后访问控制台 → 跳去登录页', $afterDelete['status'] === 302 && $afterDelete['location'] === '/login', $afterDelete['location']);

$reloginDeleted = probe_http($port, 'POST', '/login', [
    '_csrf' => probe_csrf($port, '/login', $delJar),
    'email' => 'deluser@example.com',
    'password' => 'password123',
], $delJar);
check('注销后无法再登录', $reloginDeleted['status'] === 200 && str_contains($reloginDeleted['body'], '邮箱或密码不正确'));

exit(probe_test_report());
