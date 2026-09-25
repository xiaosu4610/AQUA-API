<?php
/**
 * 线路分组 + 计费口径 + 密钥额度的端到端测试。
 *
 * 为什么要把这三件事放在一个文件里测：它们是一条链上的三环 ——
 * 「分组决定谁能调哪条线」→「这条线是不是对用户免费」→「这条线的上游额度还剩多少」。
 * 分开测只能证明每一环自己没写错，证明不了「额度用完 → 整条线自动下架」
 * 这个真正要保证的结果。
 *
 * 用法：php dev/test-groups.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\ChannelKey;
use app\common\Db;
use app\common\Group;
use app\common\Pricing;
use app\common\Schema;
use app\common\User;
use app\common\UserToken;

$dbFile = probe_test_temp('groups-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('groups-test-pass-1234');

$mock = probe_mock_start();
$mockUrl = 'http://127.0.0.1:' . $mock['port'];

echo "一、迁移种子\n";

$groups = Group::overview();
$byCode = [];
foreach ($groups as $g) {
    $byCode[$g['code']] = $g;
}

check('建出了四个内置分组', count($groups) === 4, '实际 ' . count($groups) . ' 个：' . implode(',', array_keys($byCode)));
check('「免费共享线路」存在', isset($byCode['free']));
check('「付费线路」存在', isset($byCode['paid']));
check('「高速稳定专线 · 硅基流动」存在', isset($byCode['siliconflow']));
check('「高速稳定专线 · TierFlow」存在', isset($byCode['tierflow']));
check('免费线路对用户免费', Group::isFreeToUser((int) $byCode['free']['id']) === false, '免费线路的 price_mode 应为 priced（模型自身定价为准）');
check('硅基流动专线对用户临时免费', Group::isFreeToUser((int) $byCode['siliconflow']['id']));
check('硅基流动专线要盯额度（上游花钱）', Group::needsBudgetWatch((int) $byCode['siliconflow']['id']));
check('免费线路不盯额度', Group::needsBudgetWatch((int) $byCode['free']['id']) === false);
check('默认开放的是「免费 + 付费」两条', Group::defaultVisibleCodes() === ['free', 'paid'], implode(',', Group::defaultVisibleCodes()));

$freeId = (int) $byCode['free']['id'];
$siliconId = (int) $byCode['siliconflow']['id'];

echo "\n二、分组增删改的边界\n";

$created = Group::create([
    'code' => 'Bad-Code',
    'label' => '写错的代号',
    'cost_mode' => Group::COST_FREE,
    'price_mode' => Group::PRICE_PRICED,
    'status' => 1,
]);
check('大写代号被拒', !$created['ok'], $created['message']);

$created = Group::create([
    'code' => 'expr',
    'label' => '临时试验线',
    'cost_mode' => Group::COST_FREE,
    'price_mode' => Group::PRICE_PRICED,
    'visible' => 0,
    'default_visible' => 0,
    'sort' => 90,
    'status' => 1,
]);
check('新建分组成功', $created['ok'], $created['message']);

$dup = Group::create([
    'code' => 'expr',
    'label' => '重名',
    'cost_mode' => Group::COST_FREE,
    'price_mode' => Group::PRICE_PRICED,
    'status' => 1,
]);
check('重复代号被拒', !$dup['ok'], $dup['message']);

check('空分组可以删掉', Group::delete($created['id'])['ok']);
check('删掉之后查不到了', Group::find($created['id']) === null);

echo "\n三、计费口径：时段价 + 缓存命中 + 对上免费\n";

// 硅基流动的真实价目：02:00–08:00 打折，其余时段是原价两倍
$pricingId = Pricing::create([
    'model' => 'ok-paid',
    'billing_mode' => Pricing::MODE_TOKEN,
    'upstream_kind' => 'official',
    'group_id' => $siliconId,
    'upstream_input_price' => 3.0,
    'upstream_output_price' => 9.0,
    'upstream_cache_hit_price' => 0.3,
    'downstream_input_price' => 3.0,
    'downstream_output_price' => 9.0,
    'downstream_cache_hit_price' => 0.3,
    'upstream_price_windows' => json_encode([
        ['from' => '02:00', 'to' => '08:00', 'input' => 1.5, 'output' => 4.5, 'cache_hit' => 0.15],
        ['from' => '00:00', 'to' => '24:00', 'input' => 3.0, 'output' => 9.0, 'cache_hit' => 0.3],
    ]),
    'price_unit' => 1000000,
    'status' => Pricing::STATUS_ENABLED,
]);

$row = Pricing::find($pricingId);
check('时段价存下来了', trim((string) $row['upstream_price_windows']) !== '');

// 03:00 落在打折时段：100 万输入 + 100 万输出 = 1.5 + 4.5
$atNight = mktime(3, 0, 0, 9, 25, 2026) ?: time();
$quoteNight = Pricing::quote($row, 1000000, 1000000, 1.0, 0, $atNight);
check('夜间时段价生效（¥6.00）', abs($quoteNight['upstream_cost'] - 6.0) < 0.000001, (string) $quoteNight['upstream_cost']);
check('命中的时段写在结果里', $quoteNight['window'] === '02:00-08:00', $quoteNight['window']);

// 12:00 落在兜底时段：3 + 9
$atNoon = mktime(12, 0, 0, 9, 25, 2026) ?: time();
$quoteNoon = Pricing::quote($row, 1000000, 1000000, 1.0, 0, $atNoon);
check('白天时段价生效（¥12.00）', abs($quoteNoon['upstream_cost'] - 12.0) < 0.000001, (string) $quoteNoon['upstream_cost']);

// 缓存命中：80 万命中 + 20 万未命中。命中按 0.3、未命中按 3.0
$quoteHit = Pricing::quote($row, 1000000, 0, 1.0, 800000, $atNoon);
check('命中部分按命中价算（¥0.84）', abs($quoteHit['upstream_cost'] - 0.84) < 0.000001, (string) $quoteHit['upstream_cost']);
check('结果里记了命中 token 数', (int) $quoteHit['used_cache_hit'] === 800000, (string) $quoteHit['used_cache_hit']);

// 命中数超过输入数（上游偶发脏数据）时不能算出负成本
$quoteBad = Pricing::quote($row, 100000, 0, 1.0, 999999, $atNoon);
check('脏数据下成本不为负', $quoteBad['upstream_cost'] >= 0, (string) $quoteBad['upstream_cost']);

// 临时免费：售价归零、成本照记
$quoteFree = Pricing::quote($row, 1000000, 1000000, 1.0, 0, $atNoon, true);
check('对用户免费 → 售价 0', $quoteFree['downstream_cost'] === 0.0, (string) $quoteFree['downstream_cost']);
check('对用户免费 → 成本照记 12.00', abs($quoteFree['upstream_cost'] - 12.0) < 0.000001, (string) $quoteFree['upstream_cost']);

echo "\n四、渠道与令牌的线路归属\n";

// 免费线路：一条免费渠道
$freeChannel = seedChannelInGroup('免费线路渠道', $mockUrl, ['ok-free'], [$freeId], ['sk-free-key']);
// 专线：一条花钱的渠道，额度只有 0.5 元（后面用来触发「用完自动下架」）
$paidChannel = seedChannelInGroup('专线渠道', $mockUrl, ['ok-paid'], [$siliconId], ['sk-paid-key']);

Db::execute('UPDATE channel_keys SET budget_total = ?, budget_note = ? WHERE channel_id = ?', ['0.5', '测试用额度', $paidChannel]);
Group::forget();

check('专线的密钥额度记上了', (float) ChannelKey::allForChannel($paidChannel)[0]['budget_total'] === 0.5);

$uid = (int) User::register('groups@example.com', 'groups-test-pass-1234', '127.0.0.1', false, 1000.0)['id'];

$tokenFree = UserToken::create($uid, '只走免费线', 0, null, null, 'free');
$tokenPaid = UserToken::create($uid, '只走专线', 0, null, null, 'siliconflow');
$tokenDefault = UserToken::create($uid, '默认线', 0, null, null, null);
$tokenNone = UserToken::create($uid, '不给线路', 0, null, null, 'nonexistent_code');

check('指定线路的令牌存下了代号', (string) UserToken::find($tokenFree['id'])['allow_groups'] === 'free');
check('默认令牌留空（跟着默认可见走）', (string) UserToken::find($tokenDefault['id'])['allow_groups'] === '', '实际「' . (string) UserToken::find($tokenDefault['id'])['allow_groups'] . '」');
check('不存在的代号被丢掉', (string) UserToken::find($tokenNone['id'])['allow_groups'] === '', (string) UserToken::find($tokenNone['id'])['allow_groups']);

// 「勾的就是默认那一组」也应该留空 —— 否则以后新开线路要回头改每一把令牌
$tokenSameAsDefault = UserToken::create($uid, '勾了默认那组', 0, null, null, 'paid,free');
check('勾选结果等于默认值 → 存空串', (string) UserToken::find($tokenSameAsDefault['id'])['allow_groups'] === '', (string) UserToken::find($tokenSameAsDefault['id'])['allow_groups']);

$app = probe_app_start();
$port = $app['port'];

echo "\n五、按线路拦截调用\n";

$r = v1Call($port, (string) $tokenPaid['plain'], 'ok-paid');
check('专线令牌可以调专线模型', $r['status'] === 200, 'HTTP ' . $r['status'] . ' ' . mb_substr($r['body'], 0, 200));

$r = v1Call($port, (string) $tokenFree['plain'], 'ok-paid');
check('免费线路令牌调不到专线模型', $r['status'] === 403, 'HTTP ' . $r['status'] . ' ' . mb_substr($r['body'], 0, 200));
check('说清了是线路权限问题', str_contains($r['body'], '线路'), mb_substr($r['body'], 0, 200));

$r = v1Call($port, (string) $tokenFree['plain'], 'ok-free');
check('免费线路令牌可以调免费模型', $r['status'] === 200, 'HTTP ' . $r['status'] . ' ' . mb_substr($r['body'], 0, 200));

$r = v1Call($port, (string) $tokenDefault['plain'], 'ok-free');
check('默认令牌能调免费模型', $r['status'] === 200, 'HTTP ' . $r['status']);

$r = v1Call($port, (string) $tokenDefault['plain'], 'ok-paid');
check('默认令牌（默认不含专线）调不到专线', $r['status'] === 403, 'HTTP ' . $r['status']);

$r = v1Call($port, (string) $tokenNone['plain'], 'ok-free');
check('指定了不存在的线路 → 按默认线路处理（没被卡死）', $r['status'] === 200, 'HTTP ' . $r['status']);

// 「令牌里填的分组全被删掉了」是另一回事：那种情况下不能悄悄放开成默认线路，
// 否则「我删了一个分组」会变成「所有令牌都能用」这种危险的反向结果
check(
    '令牌里全是已删除的代号 → 视为没有任何线路',
    Group::codesOfToken(['allow_groups' => 'ghost_group']) === [],
    implode(',', Group::codesOfToken(['allow_groups' => 'ghost_group']))
);

echo "\n六、/v1/models 也按线路过滤\n";

$models = static function (int $port, string $plain): string {
    $ch = curl_init('http://127.0.0.1:' . $port . '/v1/models');
    curl_setopt_array($ch, [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_HTTPHEADER => ['Authorization: Bearer ' . $plain],
        CURLOPT_TIMEOUT => 20,
    ]);
    $out = (string) curl_exec($ch);
    curl_close($ch);

    return $out;
};

$listFree = $models($port, (string) $tokenFree['plain']);
check('免费线路令牌的清单里有免费模型', str_contains($listFree, 'ok-free'));
check('免费线路令牌的清单里没有专线模型', !str_contains($listFree, 'ok-paid'), '专线模型不该出现');

$listPaid = $models($port, (string) $tokenPaid['plain']);
check('专线令牌的清单里有专线模型', str_contains($listPaid, 'ok-paid'));

echo "\n七、上游额度用完 → 密钥自动停用 + 整条线下架\n";

check('专线当前可用', Group::isLive($siliconId));
check('专线当前不在模型广场（还没放开可见）', !str_contains(probe_http($port, 'GET', '/models')['body'], 'ok-paid'));

// 按真实用量记账，把 0.5 元额度花超
ChannelKey::addCost((int) ChannelKey::allForChannel($paidChannel)[0]['id'], 0.6);
Group::forget();

// 服务进程里 Group 的缓存活着 5 秒（CACHE_TTL），而这里是另一个进程改的数据 ——
// 不等它过期就发请求，测到的会是「改之前」的状态。这不是测试的技巧，
// 正是生产上「改完分组要等几秒才生效」的真实表现
sleep(6);

$key = ChannelKey::allForChannel($paidChannel)[0];
check('密钥被自动停用', (int) $key['status'] === ChannelKey::STATUS_DISABLED, 'status=' . $key['status']);
check('标记了「因额度耗尽被停用」', (int) $key['budget_disabled'] === 1);
check('密钥数据没有被删除', $key !== null && (int) $key['channel_id'] === $paidChannel);
check('专线整条变成不可用', Group::isLive($siliconId) === false);

$r = v1Call($port, (string) $tokenPaid['plain'], 'ok-paid');
check('专线不可用时调用被拒（403）', $r['status'] === 403, 'HTTP ' . $r['status'] . ' ' . mb_substr($r['body'], 0, 200));
check('说的是「线路已下线 / 额度用完」', str_contains($r['body'], '额度') || str_contains($r['body'], '下线'), mb_substr($r['body'], 0, 200));

echo "\n八、补录额度 → 自动恢复\n";

$add = ChannelKey::addBudget((int) $key['id'], 2.0, '补 2 元');
Group::forget();

check('补录成功', $add['ok'], $add['message']);
$key = ChannelKey::allForChannel($paidChannel)[0];
check('密钥自动恢复启用', (int) $key['status'] === ChannelKey::STATUS_ENABLED);
check('自动停用标记被清掉', (int) $key['budget_disabled'] === 0);
check('额度是追加而不是覆盖（0.5 + 2.0 + 0.6 已用）', abs((float) $key['budget_total'] - 2.5) < 0.000001, (string) $key['budget_total']);
check('专线恢复可用', Group::isLive($siliconId));

$rows = ChannelKey::budgetRows();
$mine = null;
foreach ($rows as $b) {
    if ((int) $b['id'] === (int) $key['id']) {
        $mine = $b;
    }
}
check('额度看板里有这条密钥', $mine !== null);
check(
    '看板算出了剩余额度（剩余 = 总额 - 已用）',
    $mine !== null && abs((float) $mine['remain'] - ((float) $mine['total'] - (float) $mine['used'])) < 0.000001,
    $mine === null ? '' : (string) $mine['remain']
);
// 刚才那两次真实调用（5 输入 + 3 输出 tokens）已经被记进已用额度 ——
// 这正是「成本按真实用量累积」的证据，顺带证明它不只是个写死的数字
check('真实调用把成本记到了密钥上', $mine !== null && (float) $mine['used'] > 0.6, $mine === null ? '' : (string) $mine['used']);

echo "\n九、放开专线可见 → 广场分板块 + 显示临时免费\n";

Db::execute('UPDATE line_groups SET visible = 1 WHERE id = ?', [$siliconId]);
Db::execute('UPDATE line_groups SET default_visible = 1 WHERE id = ?', [$siliconId]);
Group::forget();
sleep(6); // 等服务站进程的分组缓存过期，理由同上

$market = probe_http($port, 'GET', '/models')['body'];
check('广场列出了专线模型', str_contains($market, 'ok-paid'));
check('广场分板块展示（出现线路名）', str_contains($market, '高速稳定专线 · 硅基流动'));
check('专线模型标为「本线路临时免费」', str_contains($market, '本线路临时免费'), '没有临时免费标记');

$after = Group::defaultVisibleCodes();
check('新开线路后默认开放自动包含它', in_array('siliconflow', $after, true), implode(',', $after));

$newToken = UserToken::create($uid, '放开之后建的', 0, null, null, null);
check('新令牌仍然留空（跟着默认走）', (string) UserToken::find($newToken['id'])['allow_groups'] === '');
$r = v1Call($port, (string) $newToken['plain'], 'ok-paid');
check('新令牌能调专线（因为默认开放了它）', $r['status'] === 200, 'HTTP ' . $r['status'] . ' ' . mb_substr($r['body'], 0, 200));

echo "\n十、后台分组页\n";

$jar = probe_test_temp('groups-cookies', '.txt');
check('登录后台', probe_login($port, $jar, 'groups-test-pass-1234'));
$page = probe_http($port, 'GET', '/admin/groups', [], $jar);
check('分组管理页可访问', $page['status'] === 200, 'HTTP ' . $page['status']);
check('页面列出了四条线路', substr_count($page['body'], '高速稳定专线') >= 2, '没看到专线');
check('页面显示额度看板', str_contains($page['body'], '额度') && str_contains($page['body'], '补录'));

exit(probe_test_report());

/**
 * 走一次真实的 /v1/chat/completions（JSON 请求体 + Bearer 令牌）。
 *
 * 不能复用测试台的 probe_http：它按表单编码发请求体，
 * 而转发入口只认 JSON —— 用表单发过去会被当成「请求体不合法」，
 * 于是测出来的 403 到底是不是「线路权限」被拒就分不清了。
 *
 * @return array{status:int, body:string}
 */
function v1Call(int $port, string $plain, string $model, bool $stream = false): array
{
    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => '你好']],
        'stream' => $stream,
    ]);

    $ch = curl_init('http://127.0.0.1:' . $port . '/v1/chat/completions');
    curl_setopt_array($ch, [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_HEADER => true,
        CURLOPT_POST => true,
        CURLOPT_POSTFIELDS => $body,
        CURLOPT_HTTPHEADER => [
            'Content-Type: application/json',
            'Authorization: Bearer ' . $plain,
        ],
        CURLOPT_TIMEOUT => 30,
    ]);
    $raw = (string) curl_exec($ch);
    curl_close($ch);

    $split = strpos($raw, "\r\n\r\n");
    $head = $split === false ? $raw : substr($raw, 0, $split);
    $payload = $split === false ? '' : substr($raw, $split + 4);

    $status = 0;
    foreach (explode("\r\n", $head) as $line) {
        if (preg_match('#^HTTP/\S+\s+(\d{3})#', $line, $m)) {
            $status = (int) $m[1];
        }
    }

    return ['status' => $status, 'body' => $payload];
}

/**
 * 建一条带线路分组的渠道（探针测试台里的同名函数不带 group_id）。
 *
 * @param array<int, string> $models
 * @param array<int, int>    $groupIds
 * @param array<int, string> $keys
 */
function seedChannelInGroup(string $name, string $baseUrl, array $models, array $groupIds, array $keys): int
{
    Db::execute(
        'INSERT INTO channels (name, type, base_url, models, group_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?, ?)',
        [$name, 'openai', $baseUrl, json_encode($models), $groupIds[0] ?? 0, time(), time()]
    );
    $channelId = (int) Db::pdo()->lastInsertId();

    foreach ($keys as $key) {
        Db::execute(
            'INSERT INTO channel_keys (channel_id, key_hash, api_key_enc, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)',
            [
                $channelId,
                hash('sha256', $key),
                \app\common\Crypto::encrypt($key),
                ChannelKey::STATUS_ENABLED,
                time(),
                time(),
            ]
        );
    }

    return $channelId;
}
