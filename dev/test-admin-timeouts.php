<?php
/**
 * 后台「请求超时」的测试。
 *
 * 两件必须守住的事：
 *
 * 1. **默认值**：首字节与总时长默认 300 秒。免费上游冷启动动辄几十秒，
 *    卡在 30 秒会把一批本来能用的模型判成「上游无响应」。
 *
 * 2. **专项保存不能顺手动别的配置**：主配置表单把「复选框没出现在请求里」
 *    解释成「用户把它关掉了」，所以拿一个只含超时字段的表单去提交主接口，
 *    会顺手把注册/邮件/支付开关全关掉 —— 这个坑很隐蔽，
 *    站长只想改个超时，结果整站注册被关了。所以超时走独立接口。
 *
 * 用法：php dev/test-admin-timeouts.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Db;
use app\common\Schema;
use app\common\Settings;
use app\common\Timeouts;

$dbFile = probe_test_temp('timeouts-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('timeouts-pass-1234');

echo "一、默认值\n";

$defaults = Timeouts::all();
check('连接超时默认 8 秒', $defaults['gateway.connect_timeout'] === 8, (string) $defaults['gateway.connect_timeout']);
check('测活超时默认 20 秒', $defaults['gateway.probe_timeout'] === 20, (string) $defaults['gateway.probe_timeout']);
check('首字节超时默认 300 秒', $defaults['gateway.ttft_timeout'] === 300, (string) $defaults['gateway.ttft_timeout']);
check('卡住超时默认 60 秒', $defaults['gateway.idle_timeout'] === 60, (string) $defaults['gateway.idle_timeout']);
check('单请求总时长默认 300 秒', $defaults['gateway.total_timeout'] === 300, (string) $defaults['gateway.total_timeout']);

echo "\n二、范围兜底：数据库里被写进荒谬的值时不能把转发打死\n";

Settings::put('gateway.ttft_timeout', 0);
check('首字节被写成 0 → 读取时兜回最小值 1（不会变成「立刻超时」）', Timeouts::ttft() === 1, (string) Timeouts::ttft());

Settings::put('gateway.ttft_timeout', 99999);
check('首字节被写成 99999 → 兜回上限 3600', Timeouts::ttft() === 3600, (string) Timeouts::ttft());

Settings::put('gateway.total_timeout', 0);
check('总时长 0 是合法取值（表示不设上限），不会被兜底改掉', Timeouts::total() === 0, (string) Timeouts::total());

// 恢复默认，后面按默认值测页面
Settings::reset('gateway.ttft_timeout');
Settings::reset('gateway.total_timeout');
Settings::forget();

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('timeouts-cookies', '.txt');
check('登录后台', probe_login($port, $jar, 'timeouts-pass-1234'));
$csrf = probe_csrf($port, '/admin/settings', $jar);
check('取到 CSRF', $csrf !== '');

echo "\n三、页面上的超时卡片\n";

$page = probe_http($port, 'GET', '/admin/settings', [], $jar);
check('配置页可访问', $page['status'] === 200, (string) $page['status']);
check('有「请求超时（秒）」卡片', str_contains($page['body'], '请求超时（秒）'), '没有卡片');
check('卡片说明了推荐 300 秒的理由', str_contains($page['body'], '冷启动'), '没有说明理由');
check('首字节输入框的当前值是 300', str_contains($page['body'], 'name="t[ttft]"'), '没有该字段');
check('卡片带独立的保存按钮', str_contains($page['body'], '保存超时设置'));

echo "\n四、保存自定义值\n";

// 先把几个「不该被顺手改掉」的开关打开，用来验证专项保存的隔离性
Settings::put('register.open', true);
Settings::put('mail.enabled', true);
Settings::put('billing.require_balance', true);
Settings::forget();

$custom = [
    't[connect]' => '5',
    't[probe]' => '15',
    't[ttft]' => '120',
    't[idle]' => '30',
    't[total]' => '240',
];
$save = probe_http($port, 'POST', '/admin/settings/timeouts', $custom + ['_csrf' => $csrf], $jar);
check('保存成功并跳回配置页', $save['status'] === 302 && $save['location'] === '/admin/settings', $save['location']);

Settings::forget();
check('首字节超时已改为 120', Settings::int('gateway.ttft_timeout', 0) === 120, (string) Settings::int('gateway.ttft_timeout', 0));
check('总时长已改为 240', Settings::int('gateway.total_timeout', 0) === 240, (string) Settings::int('gateway.total_timeout', 0));
check('卡住超时已改为 30', Settings::int('gateway.idle_timeout', 0) === 30, (string) Settings::int('gateway.idle_timeout', 0));

$notice = probe_http($port, 'GET', '/admin/settings', [], $jar)['body'];
check('页面提示已保存 5 项', str_contains($notice, '已保存 5 项超时设置'), '提示不对');

echo "\n五、专项保存不能动别的配置（这是它单独存在的原因）\n";

check('注册开关仍然是开', Settings::bool('register.open', false) === true, '被顺手关掉了');
check('邮件开关仍然是开', Settings::bool('mail.enabled', false) === true, '被顺手关掉了');
check('余额门槛仍然是开', Settings::bool('billing.require_balance', false) === true, '被顺手关掉了');

echo "\n六、校验\n";

$noCsrf = probe_http($port, 'POST', '/admin/settings/timeouts', ['t[ttft]' => '60'], $jar);
check('没有 CSRF → 被拒', $noCsrf['status'] === 302 && $noCsrf['location'] === '/admin/settings', $noCsrf['location']);
Settings::forget();
check('被拒时没有改动', Settings::int('gateway.ttft_timeout', 0) === 120, (string) Settings::int('gateway.ttft_timeout', 0));

$bad = probe_http($port, 'POST', '/admin/settings/timeouts', [
    '_csrf' => $csrf,
    't[ttft]' => 'abc',
], $jar);
check('填非数字 → 被拒', $bad['status'] === 302);
check('提示写明要填整数秒', str_contains(probe_http($port, 'GET', '/admin/settings', [], $jar)['body'], '要填整数秒'));

$zero = probe_http($port, 'POST', '/admin/settings/timeouts', [
    '_csrf' => $csrf,
    't[ttft]' => '0',
], $jar);
check('首字节填 0 → 被拒', $zero['status'] === 302);
check('提示写明范围', str_contains(probe_http($port, 'GET', '/admin/settings', [], $jar)['body'], '1-3600 秒之间'));
Settings::forget();
check('被拒时值没有变', Settings::int('gateway.ttft_timeout', 0) === 120, (string) Settings::int('gateway.ttft_timeout', 0));

$tooBig = probe_http($port, 'POST', '/admin/settings/timeouts', [
    '_csrf' => $csrf,
    't[total]' => '99999',
], $jar);
check('总时长填 99999 → 被拒', $tooBig['status'] === 302);
check('提示写明上界', str_contains(probe_http($port, 'GET', '/admin/settings', [], $jar)['body'], '0-3600 秒之间'));

// 组合校验：首字节不能大于总时长，否则那一层超时永远不会生效
$conflict = probe_http($port, 'POST', '/admin/settings/timeouts', [
    '_csrf' => $csrf,
    't[ttft]' => '300',
    't[total]' => '60',
], $jar);
check('首字节大于总时长 → 被拒', $conflict['status'] === 302);
check(
    '提示说清了为什么（总时长先到，首字节永不生效）',
    str_contains(probe_http($port, 'GET', '/admin/settings', [], $jar)['body'], '永远不会生效')
);
Settings::forget();
check('组合冲突时两项都没被写', Settings::int('gateway.ttft_timeout', 0) === 120 && Settings::int('gateway.total_timeout', 0) === 240);

echo "\n七、只认超时这几个键\n";

$sneak = probe_http($port, 'POST', '/admin/settings/timeouts', [
    '_csrf' => $csrf,
    't[site.name]' => '被篡改的站名',
    't[ttft]' => '90',
], $jar);
check('夹带站名 → 照样只保存超时', $sneak['status'] === 302);
Settings::forget();
check('站名没有被改动', Settings::siteName() !== '被篡改的站名', Settings::siteName());
check('超时那一项被正常保存', Settings::int('gateway.ttft_timeout', 0) === 90, (string) Settings::int('gateway.ttft_timeout', 0));

echo "\n八、文档页跟着变\n";

$docs = probe_http($port, 'GET', '/docs');
check('文档页显示当前的首字节超时', str_contains($docs['body'], '90 秒'), '文档没跟上配置');
check('文档页显示总时长上限', str_contains($docs['body'], '240 秒'), '文档没跟上配置');

// 总时长设为 0（不设上限）时，文案不能写成「上限是 0 秒」
Settings::put('gateway.total_timeout', 0);
Settings::forget();
sleep(6);   // 配置有 5 秒进程内缓存，等它过期
check('总时长为 0 时文案写成「不设上限」', str_contains(probe_http($port, 'GET', '/docs')['body'], '不设上限'), '文案不对');

exit(probe_test_report());
