<?php
/**
 * 定价「一键免费 / 恢复计费」的 HTTP 层测试（真服务、真登录、真提交）。
 *
 * 为什么要盯着它测：这个动作只改「计费模式」一个字段，
 * 但底层 Pricing::update() 是整行覆盖 —— 传漏一个字段就会把价格清零。
 * 「点了设为免费，价格没了」这种事故一旦上线，用户与站长都会一头雾水。
 *
 * 用法：php scripts/test-pricing-free.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Db;
use app\common\Pricing;
use app\common\Schema;

$dbFile = probe_test_temp('pricing-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('pricing-test-pass-1234');

$pricingId = Pricing::create([
    'model' => 'meta/llama-3.1-8b-instruct',
    'billing_mode' => Pricing::MODE_TOKEN,
    'upstream_kind' => 'official',
    'upstream_input_price' => 0.1,
    'upstream_output_price' => 0.2,
    'upstream_call_price' => 0,
    'downstream_input_price' => 1.5,
    'downstream_output_price' => 3.0,
    'downstream_call_price' => 0,
    'price_unit' => 1000000,
    'note' => '测试用定价',
    'status' => Pricing::STATUS_ENABLED,
]);

// 模型广场只列「已启用渠道支持的模型」，所以要先建一条含该模型的渠道，
// 否则广场上根本看不到它 —— 那样断言「标为免费」就等于什么都没验
probe_seed_channel('定价测试渠道', 'https://upstream.example.com/v1', ['meta/llama-3.1-8b-instruct'], ['sk-pricing-test-key']);

$app = probe_app_start();
$port = $app['port'];
$jar = probe_test_temp('pricing-cookies', '.txt');

check('登录后台', probe_login($port, $jar, 'pricing-test-pass-1234'));
$csrf = probe_csrf($port, '/admin/pricing', $jar);
check('取到 CSRF 令牌', $csrf !== '');

$list = probe_http($port, 'GET', '/admin/pricing', [], $jar);
check('定价列表页可访问', $list['status'] === 200, (string) $list['status']);
check('列表里有「设为免费」按钮', str_contains($list['body'], '设为免费'), '页面没有该按钮');

echo "\n一、设为免费\n";

$noCsrf = probe_http($port, 'POST', '/admin/pricing/free', ['id' => (string) $pricingId, 'free' => '1'], $jar);
check('没有 CSRF 令牌 → 被拒（跳回列表）', $noCsrf['status'] === 302 && $noCsrf['location'] === '/admin/pricing', $noCsrf['location']);
check('被拒时没有改动计费模式', (string) Pricing::find($pricingId)['billing_mode'] === Pricing::MODE_TOKEN);

$missing = probe_http($port, 'POST', '/admin/pricing/free', ['_csrf' => $csrf, 'id' => '999999', 'free' => '1'], $jar);
check('不存在的记录 → 被拒', $missing['status'] === 302);
check('页面给出「记录不存在」', str_contains(probe_http($port, 'GET', '/admin/pricing', [], $jar)['body'], '定价记录不存在'));

$free = probe_http($port, 'POST', '/admin/pricing/free', ['_csrf' => $csrf, 'id' => (string) $pricingId, 'free' => '1'], $jar);
check('设为免费 → 跳回列表', $free['status'] === 302 && $free['location'] === '/admin/pricing', $free['location']);

$row = Pricing::find($pricingId);
check('计费模式变为免费', (string) $row['billing_mode'] === Pricing::MODE_FREE, (string) $row['billing_mode']);
check('上游输入价原样保留', (float) $row['upstream_input_price'] === 0.1, (string) $row['upstream_input_price']);
check('下游输入价原样保留', (float) $row['downstream_input_price'] === 1.5, (string) $row['downstream_input_price']);
check('下游输出价原样保留', (float) $row['downstream_output_price'] === 3.0, (string) $row['downstream_output_price']);
check('计价单位原样保留', (int) $row['price_unit'] === 1000000, (string) $row['price_unit']);
check('备注原样保留', (string) $row['note'] === '测试用定价', (string) $row['note']);
check('启用状态原样保留', (int) $row['status'] === Pricing::STATUS_ENABLED);

$afterFree = probe_http($port, 'GET', '/admin/pricing', [], $jar)['body'];
check('列表提示已设为免费', str_contains($afterFree, '已设为免费'));
check('该行按钮变成「恢复计费」', str_contains($afterFree, '恢复计费'));

$market = probe_http($port, 'GET', '/models', [], $jar);
check('模型广场列出了这个模型', str_contains($market['body'], 'meta/llama-3.1-8b-instruct'));
check('模型广场把它标为「站长已设为免费」', str_contains($market['body'], '站长已设为免费'), '广场页没有免费标记');

echo "\n二、恢复计费\n";

$back = probe_http($port, 'POST', '/admin/pricing/free', ['_csrf' => $csrf, 'id' => (string) $pricingId, 'free' => '0'], $jar);
check('恢复计费 → 跳回列表', $back['status'] === 302);
$row = Pricing::find($pricingId);
check('计费模式回到按 Token', (string) $row['billing_mode'] === Pricing::MODE_TOKEN, (string) $row['billing_mode']);
check('价格没有被清零（恢复后仍可计费）', (float) $row['downstream_input_price'] === 1.5 && (float) $row['downstream_output_price'] === 3.0);
check('列表提示已恢复计费', str_contains(probe_http($port, 'GET', '/admin/pricing', [], $jar)['body'], '已恢复按 Token 计费'));

echo "\n三、批量场景：一次任务里多个模型都要能设免费\n";

$secondId = Pricing::create([
    'model' => 'nvidia/nemotron-3-nano-omni-30b-a3b-reasoning',
    'billing_mode' => Pricing::MODE_TOKEN,
    'upstream_kind' => 'official',
    'upstream_input_price' => 0,
    'upstream_output_price' => 0,
    'upstream_call_price' => 0,
    'downstream_input_price' => 0,
    'downstream_output_price' => 0,
    'downstream_call_price' => 0,
    'price_unit' => 1000000,
    'note' => '',
    'status' => Pricing::STATUS_ENABLED,
]);
probe_http($port, 'POST', '/admin/pricing/free', ['_csrf' => $csrf, 'id' => (string) $secondId, 'free' => '1'], $jar);
probe_http($port, 'POST', '/admin/pricing/free', ['_csrf' => $csrf, 'id' => (string) $pricingId, 'free' => '1'], $jar);
$freeCount = (int) (Db::selectOne("SELECT COUNT(*) AS c FROM pricing WHERE billing_mode = 'free'")['c'] ?? 0);
check('两次操作后两行都是免费', $freeCount === 2, '实际 ' . $freeCount . ' 行');

exit(probe_test_report());
