<?php
/**
 * 用户控制台「布局重做」的测试。
 *
 * 布局没法靠断言「好看」，但可以把这次的**结构决定**钉住 ——
 * 它们是改版的目的本身，一旦被后人无意改回去，用户就又得滚三屏去拿令牌：
 *
 *   1. 最常做的两件事在**最前面**：状态条（今天的量 + 最近一次成没成）→ 概览 → 令牌
 *   2. 「新建令牌」**不再折在面板里**（这是每个新用户的第一个动作）
 *   3. 「接入信息」折起来（看一次就够用的内容，不该占一整屏）
 *   4. 令牌是**卡片**且带复制按钮（长令牌串在表格里必然折行）
 *   5. 「最近调用」默认 5 行，其余可展开
 *   6. 危险动作（注销）与日常动作分开，且排在最末
 *   7. 旧功能一个不少：停用/启用、清额度、删除、改资料、改密码、BaseURL 复制、示例
 *
 * 用法：php dev/test-console-layout.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Schema;
use app\common\UsageLog;
use app\common\User;
use app\common\UserToken;

$dbFile = probe_test_temp('console-layout-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();

$user = User::register('layout@example.com', 'user-pass-1234', '127.0.0.1', false, 0.0);
check('建出测试用户', $user['ok'] === true, (string) ($user['message'] ?? ''));
$uid = (int) $user['id'];

// 三把令牌，覆盖三种形态：有额度上限（画进度条）、不限额度、已停用
$limited = UserToken::create($uid, '有额度的', 10.0);
UserToken::charge((int) $limited['id'], 2.5);
UserToken::create($uid, '不限额度');
$offed = UserToken::create($uid, '停用的');
UserToken::setStatus((int) $offed['id'], UserToken::STATUS_DISABLED);

// 8 条调用记录（含 1 条失败）：用来验证「最近调用默认只看 5 行」
for ($i = 0; $i < 8; $i++) {
    UsageLog::record([
        'model' => 'demo-model',
        'user_id' => $uid,
        'token_id' => (int) $limited['id'],
        'prompt_tokens' => 5,
        'completion_tokens' => 3,
        'downstream_cost' => 0.01,
        'status' => $i === 0 ? UsageLog::STATUS_ERROR : UsageLog::STATUS_OK,
        'error' => $i === 0 ? '上游返回 429' : '',
    ]);
}

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('console-layout-cookies', '.txt');

$csrf = probe_csrf($port, '/login', $jar);
$login = probe_http($port, 'POST', '/login', [
    '_csrf' => $csrf,
    'email' => 'layout@example.com',
    'password' => 'user-pass-1234',
], $jar);
check('用户登录成功', $login['status'] === 302 && $login['location'] === '/console', $login['location']);

$console = probe_http($port, 'GET', '/console', [], $jar);
$body = $console['body'];
check('控制台可访问', $console['status'] === 200, (string) $console['status']);

echo "\n一、顺序：最常做的排在最前面\n";

$posBar = strpos($body, '今日请求');
$posToken = strpos($body, '我的令牌');
$posAccount = strpos($body, '账号设置');
$posDanger = strpos($body, '危险操作');

check('状态条出现在令牌区之前', $posBar !== false && $posToken !== false && $posBar < $posToken);
check('令牌区在账号区之前', $posToken !== false && $posAccount !== false && $posToken < $posAccount);
check('危险操作排在最末（离日常动作最远）', $posDanger !== false && $posAccount !== false && $posDanger > $posAccount);

echo "\n二、顶部状态条\n";

check('有今日请求数', str_contains($body, '今日请求'));
check('有昨日对比', str_contains($body, '昨日'));
check('有今日消费', str_contains($body, '今日消费'));
check('最近调用显示为「成功」', str_contains($body, '最近调用') && str_contains($body, '>成功<'), '没有最近调用状态');
check('状态条是吸顶的（sticky）', str_contains($body, 'position: sticky'));
check('三个块的跳转入口都在', str_contains($body, 'href="#ov"') && str_contains($body, 'href="#tk"') && str_contains($body, 'href="#ac"'));
check('跳转目标真的存在', str_contains($body, 'id="ov"') && str_contains($body, 'id="tk"') && str_contains($body, 'id="ac"'));

echo "\n三、「新建令牌」不再折在面板里\n";

$createPos = strpos($body, 'action="/console/token/create"');
check('新建令牌表单在页面上', $createPos !== false);

// 判断它是否被某个未闭合的 <details> 包住：
// 取表单之前的那段 HTML，看最后一个 <details 是不是出现在最后一个 </details> 之后
$before = $createPos === false ? '' : substr($body, 0, (int) $createPos);
$lastOpen = strrpos($before, '<details');
$lastClose = strrpos($before, '</details>');
$insideDetails = $lastOpen !== false && $lastOpen > ($lastClose === false ? -1 : $lastClose);
check('新建令牌是常驻卡片（没有折在折叠面板里）', !$insideDetails, '表单被包在 details 里了');
check('新建令牌卡片有标题', str_contains($body, '新建令牌'));

echo "\n四、接入信息折起来\n";

check('接入信息仍是折叠块', str_contains($body, '接入信息（Base URL 与调用示例）'));
check('接入地址还在（只是折起来了）', str_contains($body, 'Base URL') && str_contains($body, '/v1'));
check('调用示例还在', str_contains($body, 'chat/completions'));
check('Base URL 与示例都还能复制', substr_count($body, 'data-copy=') >= 2, '少了复制按钮');

echo "\n五、令牌卡片\n";

check('一把令牌一张卡', substr_count($body, 'class="tk-card"') === 3, '卡片数=' . substr_count($body, 'class="tk-card"'));
check('卡片里有令牌名', str_contains($body, '有额度的') && str_contains($body, '不限额度'));
check('额度写成「已用 / 上限」', str_contains($body, '已用 2.5 / 10'), '额度文案不对');
check('有上限的令牌画了进度条', str_contains($body, 'class="prog"'));
check('进度条按 25% 填（2.5/10）', str_contains($body, 'style="width:25%"'), '比例算错了');
check('不限额度的令牌不画进度条', substr_count($body, 'class="prog"') === 1, '进度条数=' . substr_count($body, 'class="prog"'));
check('已停用的令牌有状态徽标', str_contains($body, '已停用'));
check('每张卡都有「更多操作」折叠', substr_count($body, '更多操作') === 3, '数量不对');

echo "\n六、最近调用默认只看 5 行\n";

check('默认可见的是前 5 条', substr_count($body, 'class="recent-more" hidden') === 3, '隐藏行数=' . substr_count($body, 'class="recent-more" hidden'));
check('有「看全部 8 条」按钮', str_contains($body, '看全部 8 条'));
check('展开按钮带总数（供 JS 还原文案）', str_contains($body, 'data-total="8"'));
check('展开/收起有对应的脚本', str_contains($body, "getElementById('recent-more')"));

echo "\n七、旧功能一个都不少\n";

check('停用/启用入口在', str_contains($body, 'formaction="/console/token/toggle"'));
check('清额度入口在', str_contains($body, 'formaction="/console/token/reset"'));
check('删除入口在', str_contains($body, 'formaction="/console/token/delete"'));
check('删除仍有二次确认', str_contains($body, "confirm('确定删除该令牌"));
check('改资料表单在', str_contains($body, 'action="/console/profile"'));
check('改密码表单在', str_contains($body, 'action="/console/password"'));
check('注销账号入口在', str_contains($body, '/console/delete'));

echo "\n八、手机宽度\n";

check('窄屏下令牌卡片改成单列', str_contains($body, '.tk-grid { grid-template-columns: 1fr; }'));
check('窄屏下状态条不再吸顶（省出屏幕高度）', str_contains($body, '.cbar { position: static; }'));

exit(probe_test_report());
