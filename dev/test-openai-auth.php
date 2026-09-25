<?php
/**
 * 对外接口（/v1）的鉴权与调用闭环测试 —— 用标准 OpenAI SDK 的调用姿势打真实 HTTP。
 *
 * ═══ 为什么要单独测这一条链路 ═══
 *
 * 用户报的问题永远是同一句话：「401，API Key 错误」。
 * 但**能导致调用失败的原因有八九种**，而且用户要采取的动作完全不同：
 * 该换令牌、该充值、该找站长解封、还是本站自己的问题。
 * 早先的实现把所有失败都回成 401 + invalid_api_key，
 * 于是「余额不足」看起来也像「密钥错了」，用户就会拿着好密钥反复排查 ——
 * 生产上 28 个用户全部 0 余额，看起来就像「本站密钥系统坏了」。
 *
 * 这个测试把每一种失败都钉在一个明确的状态码上，顺带把**成功路径**也走通，
 * 确保「说清了原因」之后仍然真的能调通。
 *
 * 覆盖：
 *   一、401：缺 Key / 错 Key / 停用 / 过期 / 用户已注销
 *   二、402：余额不足（令牌是好的，这一条最容易和 401 混）
 *   三、403：额度用尽 / 账号停用 / 模型不在白名单
 *   四、成功路径：非流式 + 流式 + 计费扣款
 *   五、/v1/models 按令牌白名单过滤
 *   六、x-api-key 头也认
 *
 * 用法：php dev/test-openai-auth.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Db;
use app\common\Pricing;
use app\common\Schema;
use app\common\Settings;
use app\common\User;
use app\common\UserToken;

$dbFile = probe_test_temp('openai-auth-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('openai-auth-pass-1234');

// 注册相关全部关掉，本测试自己造数据，避免被验证码/真实性检查干扰
Settings::put('register.open', true);
Settings::put('register.need_verify', false);
Settings::put('register.verify_email', false);
Settings::put('register.gift_balance', 0);
Settings::put('register.default_token_quota', 0);
Settings::put('billing.require_balance', true);

$mock = probe_mock_start(0);
$mockBase = 'http://127.0.0.1:' . $mock['port'];
probe_seed_channel('鉴权测试渠道', $mockBase . '/mock', ['ok-1', 'ok-2'], ['sk-upstream-key']);

// ok-2 配上价格：用来验证「扣款路径」真的会走（免费模型不会扣）
Pricing::create([
    'model' => 'ok-2',
    'billing_mode' => Pricing::MODE_TOKEN,
    'upstream_kind' => 'official',
    'upstream_input_price' => 0.1,
    'upstream_output_price' => 0.2,
    'upstream_call_price' => 0,
    'downstream_input_price' => 3.0,
    'downstream_output_price' => 6.0,
    'downstream_call_price' => 0,
    'price_unit' => 1000000,
    'note' => '鉴权测试用',
    'status' => Pricing::STATUS_ENABLED,
]);

/** 造一个用户并给他的令牌，返回 [userId, plain] */
$makeUser = static function (string $email, float $balance, bool $enabled = true): array {
    $created = User::register($email, 'password123', '127.0.0.1', false);
    $userId = (int) $created['id'];
    if ($balance > 0) {
        User::credit($userId, $balance);
    }
    if (!$enabled) {
        User::setStatus($userId, User::STATUS_DISABLED);
    }

    return [$userId, (string) UserToken::create($userId, '测试令牌')['plain']];
};

[$richId, $richToken] = $makeUser('rich@example.com', 10.0);
[$poorId, $poorToken] = $makeUser('poor@example.com', 0.0);
[$bannedId, $bannedToken] = $makeUser('banned@example.com', 10.0, false);
[$goneId, $goneToken] = $makeUser('gone@example.com', 10.0);

$disabledToken = (string) UserToken::create($richId, '停用的')['plain'];
UserToken::setStatus((int) Db::selectOne('SELECT id FROM tokens WHERE key_hash = ?', [UserToken::hash($disabledToken)])['id'], UserToken::STATUS_DISABLED);

$expiredToken = (string) UserToken::create($richId, '过期的')['plain'];
Db::execute('UPDATE tokens SET expires_at = ? WHERE key_hash = ?', [time() - 60, UserToken::hash($expiredToken)]);

$usedUpToken = (string) UserToken::create($richId, '额度用尽', 5.0)['plain'];
Db::execute('UPDATE tokens SET quota_used = 5 WHERE key_hash = ?', [UserToken::hash($usedUpToken)]);

$whitelistToken = (string) UserToken::create($richId, '只允许 ok-2', 0.0, 'ok-2')['plain'];

// 所属用户不在了、但令牌还留着（例如有人直接改库、或历史遗留数据）：
// 这里**不能**用 User::deleteAccount()——那会把令牌一起删掉，
// 于是根本走不到「用户不存在」这个分支，测出来的其实是「令牌无效」
Db::execute('DELETE FROM users WHERE id = ?', [$goneId]);

$app = probe_app_start();
$port = $app['port'];

/** 调一次 /v1/chat/completions，返回 [状态码, 解析后的 JSON] */
$chat = static function (string $token, string $model = 'ok-1', bool $stream = false, string $header = 'authorization') use ($port): array {
    $headers = [];
    $post = ['model' => $model, 'messages' => [['role' => 'user', 'content' => '你好']], 'stream' => $stream];

    $ch = curl_init('http://127.0.0.1:' . $port . '/v1/chat/completions');
    $curlHeaders = ['Content-Type: application/json'];
    if ($token !== '') {
        $curlHeaders[] = $header === 'authorization' ? 'Authorization: Bearer ' . $token : $header . ': ' . $token;
    }
    curl_setopt_array($ch, [
        CURLOPT_POST => true,
        CURLOPT_POSTFIELDS => (string) json_encode($post),
        CURLOPT_HTTPHEADER => $curlHeaders,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT => 60,
    ]);
    $body = (string) curl_exec($ch);
    $status = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
    curl_close($ch);

    return [$status, json_decode($body, true), $body];
};

echo "一、401：令牌本身的问题\n";

[$status, $json] = $chat('');
check('不带 Key → 401', $status === 401, (string) $status);
check('错误码是 missing_api_key', ($json['error']['code'] ?? '') === 'missing_api_key', (string) ($json['error']['code'] ?? '无'));

[$status, $json] = $chat('sk-aqua-' . bin2hex(random_bytes(24)));
check('不存在的令牌 → 401', $status === 401, (string) $status);
check('错误码是 invalid_api_key', ($json['error']['code'] ?? '') === 'invalid_api_key', (string) ($json['error']['code'] ?? '无'));
check('提示里说清了「要以 sk-aqua- 开头」', str_contains((string) ($json['error']['message'] ?? ''), 'sk-aqua-'), (string) ($json['error']['message'] ?? ''));

[$status, $json] = $chat('完全不是本站令牌');
check('格式不对的 Key → 401', $status === 401, (string) $status);

[$status, $json] = $chat($disabledToken);
check('已停用的令牌 → 401', $status === 401, (string) $status);
check('提示里写明「令牌已被停用」', str_contains((string) ($json['error']['message'] ?? ''), '停用'), (string) ($json['error']['message'] ?? ''));

[$status, $json] = $chat($expiredToken);
check('已过期的令牌 → 401', $status === 401, (string) $status);
check('提示里写明「已过期」', str_contains((string) ($json['error']['message'] ?? ''), '过期'), (string) ($json['error']['message'] ?? ''));

[$status, $json] = $chat($goneToken);
check('账号已注销 → 401', $status === 401, (string) $status);
check('提示里写明「用户不存在」', str_contains((string) ($json['error']['message'] ?? ''), '用户不存在'), (string) ($json['error']['message'] ?? ''));

echo "\n二、402：令牌是好的，只是余额不够（最容易与 401 混淆的一条）\n";

[$status, $json] = $chat($poorToken, 'ok-2');
check('余额为 0 且开关开着、调收费模型 → 402（不是 401）', $status === 402, (string) $status);
check('错误码是 insufficient_balance', ($json['error']['code'] ?? '') === 'insufficient_balance', (string) ($json['error']['code'] ?? '无'));
check('类型是 insufficient_quota（客户端据此判断不该重试）', ($json['error']['type'] ?? '') === 'insufficient_quota', (string) ($json['error']['type'] ?? '无'));
check('提示里点名了是哪个模型收费', str_contains((string) ($json['error']['message'] ?? ''), 'ok-2'), (string) ($json['error']['message'] ?? ''));
check('提示里说明免费模型不受余额限制', str_contains((string) ($json['error']['message'] ?? ''), '免费模型不受余额限制'), (string) ($json['error']['message'] ?? ''));

// 关键回归：余额为 0 的用户调**免费/未定价**模型必须放行。
// 否则「上游全是免费模型 + 用户余额全是 0」就等于谁都调不通（生产实况）
[$status, $json] = $chat($poorToken, 'ok-1');
check('余额为 0 也能调免费（未定价）模型 → 200', $status === 200, (string) $status);

// 关掉开关后收费模型也应当能用（免费站场景）
//
// ⚠️ 配置有 5 秒进程内缓存（Settings::CACHE_TTL），而这里改的是**另一个进程**（HTTP 服务）
// 的配置 —— 不等缓存过期，测到的还是旧值，会得到一个假的「失败」
Settings::put('billing.require_balance', false);
sleep(6);
[$status] = $chat($poorToken, 'ok-2');
check('把「余额为 0 时拒绝调用」关掉后 → 收费模型也放行', $status === 200, (string) $status);
Settings::put('billing.require_balance', true);

echo "\n三、403：配额与权限问题\n";

[$status, $json] = $chat($usedUpToken);
check('令牌额度用尽 → 403', $status === 403, (string) $status);
check('错误码是 insufficient_quota', ($json['error']['code'] ?? '') === 'insufficient_quota', (string) ($json['error']['code'] ?? '无'));

[$status, $json] = $chat($bannedToken);
check('账号被停用 → 403', $status === 403, (string) $status);
check('错误码是 account_disabled', ($json['error']['code'] ?? '') === 'account_disabled', (string) ($json['error']['code'] ?? '无'));

[$status, $json] = $chat($whitelistToken, 'ok-1');
check('模型不在令牌白名单 → 403', $status === 403, (string) $status);
check('错误码是 model_not_allowed', ($json['error']['code'] ?? '') === 'model_not_allowed', (string) ($json['error']['code'] ?? '无'));
check('提示里带上了令牌名与模型名', str_contains((string) ($json['error']['message'] ?? ''), 'ok-1'), (string) ($json['error']['message'] ?? ''));

echo "\n四、成功路径：真调通（非流式 + 流式 + 扣款）\n";

$balanceBefore = (float) User::find($richId)['balance'];

[$status, $json, $raw] = $chat($richToken, 'ok-2');
check('正常调用 → 200', $status === 200, (string) $status);
check('返回 OpenAI 结构（choices[0].message.content）', ($json['choices'][0]['message']['content'] ?? '') === 'hi', mb_substr($raw, 0, 120));

$balanceAfter = (float) User::find($richId)['balance'];
check('余额被扣了（ok-2 配了价格）', $balanceAfter < $balanceBefore, $balanceBefore . ' → ' . $balanceAfter);
$log = Db::selectOne('SELECT * FROM usage_logs WHERE user_id = ? ORDER BY id DESC LIMIT 1', [$richId]);
check('写了用量日志', $log !== null);
check('日志里有真实 token 数（上游返回了 usage）', (int) ($log['total_tokens'] ?? 0) === 8, (string) ($log['total_tokens'] ?? 0));
check('日志状态是成功', (string) ($log['status'] ?? '') === 'ok', (string) ($log['status'] ?? ''));
check('令牌的 last_used_at 被刷新', (int) (Db::selectOne('SELECT last_used_at FROM tokens WHERE key_hash = ?', [UserToken::hash($richToken)])['last_used_at'] ?? 0) > 0);

[$status, $json, $raw] = $chat($richToken, 'ok-1', true);
check('流式调用 → 200', $status === 200, (string) $status);
check('响应是 SSE（含 data: 与 [DONE]）', str_contains($raw, 'data:') && str_contains($raw, '[DONE]'), mb_substr($raw, 0, 200));
check('SSE 里带出了内容片段', str_contains($raw, 'hi'), mb_substr($raw, 0, 200));

echo "\n五、/v1/models 也认令牌并按白名单过滤\n";

$modelsCall = static function (string $token) use ($port): array {
    $ch = curl_init('http://127.0.0.1:' . $port . '/v1/models');
    curl_setopt_array($ch, [
        CURLOPT_HTTPHEADER => ['Authorization: Bearer ' . $token],
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT => 30,
    ]);
    $body = (string) curl_exec($ch);
    $status = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
    curl_close($ch);

    return [$status, json_decode($body, true)];
};

[$status, $json] = $modelsCall($richToken);
check('/v1/models → 200', $status === 200, (string) $status);
$ids = array_column((array) ($json['data'] ?? []), 'id');
sort($ids);
check('列出两个模型', $ids === ['ok-1', 'ok-2'], implode(',', $ids));

[$status, $json] = $modelsCall($whitelistToken);
$ids = array_column((array) ($json['data'] ?? []), 'id');
check('白名单令牌只看到 ok-2', $ids === ['ok-2'], implode(',', $ids));

[$status, $json] = $modelsCall('');
check('/v1/models 不带令牌 → 401', $status === 401, (string) $status);

echo "\n六、x-api-key 头也认（Anthropic / 部分客户端的习惯）\n";

[$status, $json] = $chat($richToken, 'ok-1', false, 'x-api-key');
check('用 x-api-key 头 → 200', $status === 200, (string) $status);

exit(probe_test_report());
