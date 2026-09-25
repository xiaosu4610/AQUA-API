<?php
/**
 * 模型运行期健康度 + 请求体适配 + 被拒调用留痕。
 *
 * ═══ 这个测试对应的是「大部分请求都在失败」那一类线上问题 ═══
 *
 * 实测的真实数据（修复前一天）：
 *   · deepseek-ai/deepseek-v4.1-flash   20 次调用全部失败，平均 49.9 秒
 *   · z-ai/glm-5.3                       7 次全失败，平均 61.1 秒
 *   · nvidia/nemotron-3-ultra-550b-a55b 36 次只失败 5 次，平均 8.5 秒（健康）
 *   → 整体成功率三成。而剩下的失败里还有两类：
 *     「上游 400 Unsupported parameter(s): dsh_plugin_packages」（客户端私有参数）
 *     「大量 401」其实是旧版把余额不足报成了 401（已修）
 *
 * 所以这里要钉住四件事：
 *   一、坏模型会被自动暂停路由，请求**立刻**失败并给出替代模型（不再白等几分钟）
 *   二、暂停是**自动恢复**的（冷却到期 + 成功一次即清零）
 *   三、上游不认识的客户端私有字段会被剥掉（渠道自己的 extra_body 不能被剥）
 *   四、被本站拒绝的调用要留痕且**不算上游失败**（否则站长永远查不出「谁在用错令牌」）
 *
 * 用法：php dev/test-model-health.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Channel;
use app\common\Db;
use app\common\ModelHealth;
use app\common\Schema;
use app\common\Settings;
use app\common\UsageLog;
use app\common\User;
use app\common\UserToken;

$dbFile = probe_test_temp('model-health-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('health-pass-1234');

echo "一、迁移与表结构\n";

check('schema 版本升到 14', (int) Settings::get('schema_version', 0) === 14, (string) Settings::get('schema_version', '无'));
check('model_health 表已建出', (static function (): bool {
    try {
        Db::selectOne('SELECT COUNT(*) AS c FROM model_health');

        return true;
    } catch (Throwable) {
        return false;
    }
})());

// 版本号在库里存的是 **JSON 字符串**（带引号）。ensure() 里若直接 (int) 强转，
// 会得到 0 → 每次启动都重跑全部迁移 → 日志里一片「已经存在的列/索引」告警。
// 这两条锁住「读法正确」与「重跑安全」两件事
check('schema_version 以 JSON 字符串存储', str_starts_with((string) Settings::raw('schema_version'), '"'), (string) Settings::raw('schema_version'));
Settings::put('schema_version', '13');
Settings::forget();
Schema::ensure();
check('版本号被改小后重跑迁移是安全的（幂等）', (int) Settings::get('schema_version', 0) === 14, (string) Settings::get('schema_version', '无'));

echo "\n二、健康度判定（纯逻辑）\n";

check('默认开启', ModelHealth::enabled());
check('默认连续失败 3 次算坏', ModelHealth::threshold() === 3, (string) ModelHealth::threshold());
check('超时（状态码 0）算模型失败', ModelHealth::isModelFailure(0));
check('503 算模型失败', ModelHealth::isModelFailure(503));
check('404（上游说没这个模型）算模型失败', ModelHealth::isModelFailure(404));
check('400（参数错）不算模型失败', !ModelHealth::isModelFailure(400));
check('401（密钥问题）不算模型失败', !ModelHealth::isModelFailure(401));
check('429（限流）不算模型失败', !ModelHealth::isModelFailure(429));

ModelHealth::markFailure('unit-a', 503, '上游过载');
check('失败 1 次还不算坏', ModelHealth::unavailable('unit-a') === null);
ModelHealth::markFailure('unit-a', 503, '上游过载');
check('失败 2 次还不算坏', ModelHealth::unavailable('unit-a') === null);
ModelHealth::markFailure('unit-a', 503, '上游过载');
$gate = ModelHealth::unavailable('unit-a');
check('失败 3 次 → 判定为不可用', $gate !== null);
check('给出的原因里带了失败次数与最后一次错误',
    $gate !== null && str_contains($gate['reason'], '3 次') && str_contains($gate['reason'], '上游过载'),
    (string) ($gate['reason'] ?? ''));

ModelHealth::markSuccess('unit-a', 1200);
check('成功一次即解除不可用', ModelHealth::unavailable('unit-a') === null);
check('成功会把连击清零', (int) (Db::selectOne('SELECT consecutive_errors AS c FROM model_health WHERE model = ?', ['unit-a'])['c'] ?? -1) === 0);

// 400 之类的「非模型级失败」既不该累加、也不该清零
ModelHealth::markFailure('unit-a', 503, 'x');
ModelHealth::markFailure('unit-a', 503, 'x');
ModelHealth::markFailure('unit-a', 400, '参数不对');
check('非模型级失败不参与计数',
    (int) (Db::selectOne('SELECT consecutive_errors AS c FROM model_health WHERE model = ?', ['unit-a'])['c'] ?? 0) === 2,
    (string) (Db::selectOne('SELECT consecutive_errors AS c FROM model_health WHERE model = ?', ['unit-a'])['c'] ?? 0));

// 冷却到期自动放行
Db::execute('UPDATE model_health SET unavailable_until = ? WHERE model = ?', [time() - 1, 'unit-a']);
ModelHealth::release('');   // release 会清缓存，用来让上面的改动立刻可见
ModelHealth::markFailure('unit-b', 503, 'y');
check('release 之后仍然可用（连击被清零）', ModelHealth::unavailable('unit-a') === null);

echo "\n三、端到端：坏模型不再让用户白等\n";

Settings::put('register.open', false);
Settings::put('billing.require_balance', true);
Settings::forget();

$mock = probe_mock_start(0);
$mockBase = 'http://127.0.0.1:' . $mock['port'];
probe_seed_channel('健康度测试渠道', $mockBase . '/mock', ['ok-1', 'boom-1', 'denied-1'], ['sk-upstream-key']);

$created = User::register('healthy@example.com', 'password123', '127.0.0.1', false, 10.0);
$token = (string) UserToken::create((int) $created['id'], '测试令牌')['plain'];

$app = probe_app_start();
$port = $app['port'];

/** 发一个真实的 JSON 请求（支持自定义方法/请求头） */
$http = static function (string $method, string $path, string $token = '', ?array $json = null): array {
    global $port;

    $ch = curl_init('http://127.0.0.1:' . $port . $path);
    $headers = ['Content-Type: application/json'];
    if ($token !== '') {
        $headers[] = 'Authorization: Bearer ' . $token;
    }
    curl_setopt_array($ch, [
        CURLOPT_CUSTOMREQUEST => strtoupper($method),
        CURLOPT_HTTPHEADER => $headers,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_HEADER => true,
        CURLOPT_TIMEOUT => 60,
    ]);
    if ($json !== null) {
        curl_setopt($ch, CURLOPT_POSTFIELDS, (string) json_encode($json));
    }
    $raw = (string) curl_exec($ch);
    $status = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
    $headerSize = (int) curl_getinfo($ch, CURLINFO_HEADER_SIZE);
    curl_close($ch);

    $head = substr($raw, 0, $headerSize);
    $body = substr($raw, $headerSize);

    return [
        'status' => $status,
        'body' => $body,
        'json' => json_decode($body, true),
        'type' => str_contains(strtolower($head), 'application/json') ? 'json' : 'html',
        'bytes' => strlen($body),
    ];
};

$chat = static function (string $model, string $token) use ($http): array {
    return $http('POST', '/v1/chat/completions', $token, [
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => 'hi']],
    ]);
};

// 先让 ok-1 成功一次（证明健康模型不受影响，并给 alternatives 提供「近期成功过」的候选）
$ok = $chat('ok-1', $token);
check('健康模型照常 200', $ok['status'] === 200, $ok['status'] . ' ' . mb_substr($ok['body'], 0, 300));
check('成功会写进健康表', (int) (Db::selectOne('SELECT total_calls AS c FROM model_health WHERE model = ?', ['ok-1'])['c'] ?? 0) === 1);

$before = count(probe_mock_requests($mock['logFile']));

// 连续 3 次失败（mock 的 boom- 前缀固定回 500）
$failBodies = [];
for ($i = 1; $i <= 3; $i++) {
    $failed = $chat('boom-1', $token);
    $failBodies[] = $failed['status'] . ':' . (string) ($failed['json']['error']['code'] ?? $failed['status']);
}
check(
    '前 3 次都真的走到了上游（不是「无可用渠道」这类本站问题）',
    !str_contains(implode(' ', $failBodies), 'no_available_channel'),
    implode(' / ', $failBodies)
);

$row = Db::selectOne('SELECT * FROM model_health WHERE model = ?', ['boom-1']);
check('失败被记进健康表（连击 3）', (int) ($row['consecutive_errors'] ?? 0) === 3, (string) ($row['consecutive_errors'] ?? '无'));
check('记录了最后一次上游状态码', (int) ($row['last_status'] ?? 0) === 500, (string) ($row['last_status'] ?? '无'));

$midCount = count(probe_mock_requests($mock['logFile']));
$gated = $chat('boom-1', $token);
$afterCount = count(probe_mock_requests($mock['logFile']));

check('达到阈值后立刻返回 503', $gated['status'] === 503, (string) $gated['status']);
check('错误码是 model_temporarily_unavailable', ($gated['json']['error']['code'] ?? '') === 'model_temporarily_unavailable', (string) ($gated['json']['error']['code'] ?? '无'));
check('错误里说清了「当前不可用」', str_contains((string) ($gated['json']['error']['message'] ?? ''), '当前不可用'));
check('错误里给出了可用的替代模型', str_contains((string) ($gated['json']['error']['message'] ?? ''), 'ok-1'), (string) ($gated['json']['error']['message'] ?? ''));
check('这次请求**没有**再发给上游（用户不用白等）', $afterCount === $midCount, "上游请求数 {$midCount} → {$afterCount}");

echo "\n四、被暂停的模型不出现在模型清单里\n";

$models = $http('GET', '/v1/models', $token);
$ids = array_column((array) ($models['json']['data'] ?? []), 'id');
check('清单里还有 ok-1', in_array('ok-1', $ids, true), implode(',', $ids));
check('清单里没有 boom-1（避免用户选了就失败）', !in_array('boom-1', $ids, true), implode(',', $ids));

echo "\n五、后台看得见，也能手动放行\n";

$adminJar = probe_test_temp('model-health-admin', '.txt');
check('登录后台', probe_login($port, $adminJar, 'health-pass-1234'));
$adminCsrf = probe_csrf($port, '/admin/channels', $adminJar);

$channelsPage = probe_http($port, 'GET', '/admin/channels', [], $adminJar)['body'];
check('渠道页有「模型可用性（运行期）」卡片', str_contains($channelsPage, '模型可用性（运行期）'));
check('列出了坏模型 boom-1', str_contains($channelsPage, 'boom-1'));
check('标出「已暂停」', str_contains($channelsPage, '已暂停'));
check('有「全部恢复，立刻重试」按钮', str_contains($channelsPage, '全部恢复，立刻重试'));

$noCsrf = probe_http($port, 'POST', '/admin/channels/health/release', [], $adminJar);
check('没有 CSRF → 被拒', $noCsrf['status'] === 302);
// 用数据库判断「有没有被放行」：进程内的健康度缓存有 5 秒 TTL，
// 跨进程读 API 会读到旧值（这是缓存生效的正常表现，不是 bug）
check('被拒时没有放行', (int) (Db::selectOne('SELECT unavailable_until AS u FROM model_health WHERE model = ?', ['boom-1'])['u'] ?? 0) > time());

$release = probe_http($port, 'POST', '/admin/channels/health/release', ['_csrf' => $adminCsrf], $adminJar);
check('放行成功并跳回渠道页', $release['status'] === 302 && $release['location'] === '/admin/channels', $release['location']);
check('放行后不再处于暂停状态',
    (int) (Db::selectOne('SELECT unavailable_until AS u FROM model_health WHERE model = ?', ['boom-1'])['u'] ?? -1) === 0,
    (string) (Db::selectOne('SELECT unavailable_until AS u FROM model_health WHERE model = ?', ['boom-1'])['u'] ?? '无'));

$retryCount = count(probe_mock_requests($mock['logFile']));
$retried = $chat('boom-1', $token);
check(
    '放行后确实重新发给了上游试探',
    count(probe_mock_requests($mock['logFile'])) > $retryCount,
    $retried['status'] . ' ' . mb_substr($retried['body'], 0, 200)
);

echo "\n六、请求体适配：剥掉上游不认识的客户端私有字段\n";

$channel = Channel::find(1);
check('标准字段都保留', (static function () use ($channel): bool {
    $out = Channel::sanitizeBody($channel, [
        'model' => 'ok-1',
        'messages' => [['role' => 'user', 'content' => 'hi']],
        'temperature' => 0.7,
        'stream' => true,
        'tools' => [],
    ]);

    return count($out) === 5;
})());

$dirty = Channel::sanitizeBody($channel, [
    'model' => 'ok-1',
    'messages' => [],
    'dsh_plugin_packages' => ['a', 'b'],
    'my_client_version' => '1.2.3',
]);
check('客户端私有字段被剥掉', !isset($dirty['dsh_plugin_packages']) && !isset($dirty['my_client_version']), implode(',', array_keys($dirty)));
check('标准字段不受影响', isset($dirty['model']) && isset($dirty['messages']));

Settings::put('gateway.keep_body_params', 'dsh_plugin_packages');
Settings::forget();
$kept = Channel::sanitizeBody($channel, ['model' => 'ok-1', 'dsh_plugin_packages' => []]);
check('全局「额外保留」清单生效', isset($kept['dsh_plugin_packages']), implode(',', array_keys($kept)));

// 渠道级保留（按生产里真实的存储形态写：csv 字段在保存时已被解析成数组）
Db::execute('UPDATE channels SET config = ? WHERE id = 1', [json_encode(['keep_body' => ['my_client_version']], JSON_UNESCAPED_UNICODE)]);
$channel = Channel::find(1);
$kept2 = Channel::sanitizeBody($channel, ['model' => 'ok-1', 'my_client_version' => '1.2.3']);
check('渠道级「保留请求体字段」生效', isset($kept2['my_client_version']), implode(',', array_keys($kept2)));

Settings::put('gateway.keep_body_params', '');
Settings::put('gateway.strip_unknown_params', false);
Settings::forget();
$raw = Channel::sanitizeBody(Channel::find(1), ['model' => 'ok-1', 'whatever' => 1]);
check('关掉开关后原样透传', isset($raw['whatever']), implode(',', array_keys($raw)));
Settings::put('gateway.strip_unknown_params', true);
Settings::forget();

// buildSpec 的组装顺序：私有字段剥掉，但渠道自己 extra_body 里的字段必须留下
Db::execute('UPDATE channels SET config = ? WHERE id = 1', [json_encode(
    ['extra_body' => ['top_p' => 0.9, 'chat_template_kwargs' => ['x' => 1]]],
    JSON_UNESCAPED_UNICODE
)]);
$spec = Channel::buildSpec(Channel::find(1), 'sk-upstream-key', '/chat/completions', [
    'model' => 'ok-1',
    'messages' => [],
    'dsh_plugin_packages' => ['a'],
], 60);
$sent = json_decode((string) $spec['body'], true);
check('发给上游的请求体里没有客户端私有字段', !isset($sent['dsh_plugin_packages']), (string) $spec['body']);
check('渠道自己声明的 extra_body 字段仍在（不能被误剥）', isset($sent['chat_template_kwargs']) && ($sent['top_p'] ?? 0) === 0.9, (string) $spec['body']);
check('业务字段仍然是请求体里的（model/messages）', isset($sent['model'], $sent['messages']));
Db::execute('UPDATE channels SET config = NULL WHERE id = 1');

echo "\n六之二、按上游原话学习「它不认的字段」\n";

// 这条措辞是从生产日志里原样抄下来的（NIM 的真实回话）
$realError = '上游返回 HTTP 400：Validation: Unsupported parameter(s): `prompt_cache_key`, `enable_thinking`';
$parsed = Channel::unsupportedParamsFrom($realError);
check('认得出上游点名的两个字段', in_array('prompt_cache_key', $parsed, true) && in_array('enable_thinking', $parsed, true), implode(',', $parsed));
check('不会把普通错误里的词当成字段名', Channel::unsupportedParamsFrom('上游返回 HTTP 500：Internal Server Error') === []);

Settings::put('gateway.auto_strip_params', '');
Settings::forget();
$learned = Channel::learnUnsupportedParams($realError);
check('学到的字段被写进配置', count($learned) === 2, implode(',', $learned));
Settings::forget();
check('配置里能读到', Channel::autoStripFields() === ['prompt_cache_key', 'enable_thinking'], implode(',', Channel::autoStripFields()));
check('再学一次不会重复记（幂等）', Channel::learnUnsupportedParams($realError) === []);

// 关键：prompt_cache_key 本来在白名单里（OpenAI 官方字段），
// 但上游明确说不认识 —— 事实必须压过白名单，否则会一直 400
$strippedByLearning = Channel::sanitizeBody(Channel::find(1), [
    'model' => 'ok-1',
    'messages' => [],
    'prompt_cache_key' => 'abc',
    'temperature' => 0.5,
]);
check('上游点名的字段即使在白名单里也会被剥掉', !isset($strippedByLearning['prompt_cache_key']), implode(',', array_keys($strippedByLearning)));
check('其余标准字段不受影响', isset($strippedByLearning['model'], $strippedByLearning['temperature']));

$spec2 = Channel::buildSpec(Channel::find(1), 'sk-upstream-key', '/chat/completions', [
    'model' => 'ok-1',
    'messages' => [],
    'prompt_cache_key' => 'abc',
    'enable_thinking' => true,
], 60);
$sent2 = json_decode((string) $spec2['body'], true);
check('学到的字段真的不会再发给上游', !isset($sent2['prompt_cache_key']) && !isset($sent2['enable_thinking']), (string) $spec2['body']);

// 这一条是本次踩坑的直接回归：某一家自己的推理开关**默认不该被转发**，
// 因为它不是 OpenAI 标准字段，不支持的上游会整条请求 400
$thinkingSpec = Channel::buildSpec(Channel::find(1), 'sk-upstream-key', '/chat/completions', [
    'model' => 'ok-1',
    'messages' => [],
    'enable_thinking' => true,
    'thinking' => ['type' => 'enabled'],
], 60);
$thinkingSent = json_decode((string) $thinkingSpec['body'], true);
check('非标准的「推理开关」默认不转发（本次线上 400 的根因）', !isset($thinkingSent['enable_thinking'], $thinkingSent['thinking']), (string) $thinkingSpec['body']);

Settings::put('gateway.auto_strip_params', '');
Settings::forget();

echo "\n七、被拒绝的调用要留痕，且不算上游失败\n";

$rejectToken = 'sk-aqua-' . bin2hex(random_bytes(24));
$rejected = $chat('ok-1', $rejectToken);
check('无效令牌 → 401', $rejected['status'] === 401, (string) $rejected['status']);
check('提示说清了「库里没有这把令牌」', str_contains((string) ($rejected['json']['error']['message'] ?? ''), '库里没有这把令牌'), (string) ($rejected['json']['error']['message'] ?? ''));

$masked = $chat('ok-1', 'sk-aqua-1a2b3c4d5e6f…9f3a');
check('把掩码当令牌用 → 提示点出「那是掩码」', str_contains((string) ($masked['json']['error']['message'] ?? ''), '掩码'), (string) ($masked['json']['error']['message'] ?? ''));

$http('GET', '/v1/models', 'bad-key-not-ours');
check(
    '提示说清「不像本站令牌」',
    str_contains((string) ($http('GET', '/v1/models', 'bad-key-not-ours')['json']['error']['message'] ?? ''), '不像是本站令牌'),
    ''
);

$rejRow = Db::selectOne(
    "SELECT * FROM usage_logs WHERE status = 'rejected' ORDER BY id DESC LIMIT 1"
);
check('留了一条 rejected 记录', $rejRow !== null);
check('记录里写明了是被本站拒绝', $rejRow !== null && str_contains((string) $rejRow['error_message'], '调用被本站拒绝'), (string) ($rejRow['error_message'] ?? '无'));
// 掩码只在「输入长得像令牌」时才有意义：太短的垃圾串如实写「过短的串」
$maskedRow = Db::selectOne(
    "SELECT * FROM usage_logs WHERE status = 'rejected' AND error_message LIKE '%提交的令牌：sk-aqua-%' ORDER BY id DESC LIMIT 1"
);
check('令牌形态像样时，记录里带了掩码供站长对照', $maskedRow !== null, '没有任何一条带上 sk-aqua- 掩码');
check('留痕不产生费用', $rejRow !== null && (float) $rejRow['downstream_cost'] === 0.0);

$rejectedCount = (int) (Db::selectOne("SELECT COUNT(*) AS c FROM usage_logs WHERE status = 'rejected'")['c'] ?? 0);
check('三种被拒形态都留了痕（≥3 条）', $rejectedCount >= 3, (string) $rejectedCount);

$dayStart = strtotime('today') ?: time();
$window = UsageLog::statsRange($dayStart, $dayStart + 86400);
$realErrors = (int) (Db::selectOne(
    "SELECT COUNT(*) AS c FROM usage_logs WHERE status = 'error' AND created_at >= ?",
    [$dayStart]
)['c'] ?? 0);
check('窗口里确实有被拒调用（对照组）', $rejectedCount > 0, (string) $rejectedCount);
check('报表的「失败次数」只算上游失败，不含被拒调用', (int) $window['errors'] === $realErrors, "报表 {$window['errors']} / 实际上游失败 {$realErrors}");
check('被拒调用单独有数', UsageLog::rejectedStats($dayStart, $dayStart + 86400)['count'] === $rejectedCount);

$usagePage = probe_http($port, 'GET', '/admin/usage', [], $adminJar)['body'];
check('用量页把被拒调用单独标出来', str_contains($usagePage, '未发往上游'), '没有标记');
check('用量页说明了这类请求不是上游故障', str_contains($usagePage, '一次都没发往上游'));

echo "\n八、客户端习惯探测的路径与余额端点\n";

$balance = $http('GET', '/user/balance', $token);
check('/user/balance 返回 200', $balance['status'] === 200, (string) $balance['status']);
check('返回 JSON（不再是 HTML 404 页）', $balance['type'] === 'json');
check('带上了余额', isset($balance['json']['balance']) && (float) $balance['json']['balance'] > 0, (string) ($balance['json']['balance'] ?? '无'));
check('兼容字段 data.quota 也在', isset($balance['json']['data']['quota']));

$noAuthBalance = $http('GET', '/user/balance');
check('不带令牌 → 401', $noAuthBalance['status'] === 401, (string) $noAuthBalance['status']);
check('也是 JSON', $noAuthBalance['type'] === 'json');

$unknownApi = $http('GET', '/user/something-else', $token);
check('未知的 /user/* 路径 → 404', $unknownApi['status'] === 404, (string) $unknownApi['status']);
check('404 也是 JSON（原来是 35KB 的 HTML 页）', $unknownApi['type'] === 'json' && $unknownApi['bytes'] < 2000, "type={$unknownApi['type']} bytes={$unknownApi['bytes']}");

$postModels = $http('POST', '/v1/models', $token);
check('POST /v1/models 也能用（有些客户端这么探测）', $postModels['status'] === 200, (string) $postModels['status']);

exit(probe_test_report());
