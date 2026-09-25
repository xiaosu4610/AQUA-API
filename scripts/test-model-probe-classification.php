<?php
/**
 * 模型探测内核的分类与重试规则测试（不联网）。
 *
 * 为什么单独测这件事：下架动作是不可逆的（虽然留了备份），
 * 判错一个模型，用户就会看到一个「其实能用」的模型从列表里消失。
 * 所以「什么响应算不可用」必须是可枚举、可回归的，不能靠人工印象。
 *
 * 用法：php scripts/test-model-probe-classification.php
 */

declare(strict_types=1);

require dirname(__DIR__) . '/app/common/ModelProbe.php';

use app\common\ModelProbe;

$failures = [];
$checks = 0;

function check(string $name, bool $ok, string $detail = ''): void
{
    global $failures, $checks;
    $checks++;
    if ($ok) {
        echo "  [通过] {$name}\n";

        return;
    }
    $failures[] = $name . ($detail !== '' ? " —— {$detail}" : '');
    echo "  [失败] {$name}" . ($detail !== '' ? " —— {$detail}" : '') . "\n";
}

/** @param array{http:int,body:string,curl_error:bool} $response */
function resp(int $http, string $body = '', bool $curlError = false): array
{
    return ['http' => $http, 'body' => $body, 'curl_error' => $curlError];
}

echo "一、响应分类\n";

$cases = [
    ['超时/连不上（curl_error）', resp(0, 'Operation timed out', true), ModelProbe::INCONCLUSIVE],
    ['HTTP 0（无状态码）', resp(0), ModelProbe::INCONCLUSIVE],
    ['200 正常', resp(200, '{"choices":[]}'), ModelProbe::OK],
    ['401 未授权', resp(401, '{"error":"invalid api key"}'), ModelProbe::AUTH_ERROR],
    ['403 拒绝', resp(403, '{"error":"forbidden"}'), ModelProbe::AUTH_ERROR],
    ['404 权限型（含 "detail"）', resp(404, '{"detail":"Not found for account"}'), ModelProbe::NO_ACCESS],
    ['404 权限型（含 for account）', resp(404, '{"error":"model not found for account"}'), ModelProbe::NO_ACCESS],
    ['404 权限型（含 Function）', resp(404, '{"error":"Function not found"}'), ModelProbe::NO_ACCESS],
    ['404 普通（未路由）', resp(404, '<html><body>404 Not Found</body></html>'), ModelProbe::UNROUTABLE],
    ['404 空响应体', resp(404, ''), ModelProbe::UNROUTABLE],
    ['429 限流', resp(429, '{"error":"rate limit"}'), ModelProbe::INCONCLUSIVE],
    ['500 上游错误', resp(500, 'internal error'), ModelProbe::INCONCLUSIVE],
    ['503 上游错误', resp(503, 'unavailable'), ModelProbe::INCONCLUSIVE],
    ['400 参数错误', resp(400, '{"error":"bad request"}'), ModelProbe::INCONCLUSIVE],
    ['422 参数错误', resp(422, '{"error":"unprocessable"}'), ModelProbe::INCONCLUSIVE],
];

foreach ($cases as [$name, $response, $expected]) {
    $actual = ModelProbe::classify($response);
    check("{$name} → {$expected}", $actual === $expected, "实际得到 {$actual}");
}

echo "\n二、探测流程（注入假发送器，不联网）\n";

$channel = [
    'base_url' => 'https://upstream.example.com/v1',
    'config' => json_encode([
        'auth_type' => 'bearer',
        'model_map' => ['public-name' => 'vendor-name'],
    ]),
];

$specs = [];
$sender = static function (array $spec) use (&$specs): array {
    $specs[] = $spec;

    return ['http' => 200, 'body' => '{"choices":[]}', 'curl_error' => false];
};
$result = ModelProbe::probe($channel, 'sk-test-key', 'public-name', 20, $sender);
check('200 → 可用', $result['result'] === ModelProbe::OK, $result['result']);
check('模型映射生效（public-name → vendor-name）', $result['upstream_model'] === 'vendor-name', $result['upstream_model']);
check('返回结构中的 model 是调用方传入的名字', $result['model'] === 'public-name', $result['model']);
check('只请求一次，不做无谓重试', count($specs) === 1, '实际 ' . count($specs) . ' 次');
check('请求体里带的是上游模型名', str_contains($specs[0]['body'], '"model":"vendor-name"'), $specs[0]['body']);
check('鉴权头带上了凭据', in_array('Authorization: Bearer sk-test-key', $specs[0]['headers'], true));

// 500 → 重试一次 → 200
$specs = [];
$attempt = 0;
$flaky = static function (array $spec) use (&$specs, &$attempt): array {
    $specs[] = $spec;
    $attempt++;

    return $attempt === 1
        ? ['http' => 500, 'body' => 'broken', 'curl_error' => false]
        : ['http' => 200, 'body' => '{"choices":[]}', 'curl_error' => false];
};
$result = ModelProbe::probe($channel, 'sk-test-key', 'public-name', 20, $flaky);
check('500 后重试成功 → 可用', $result['result'] === ModelProbe::OK, $result['result']);
check('重试恰好一次', count($specs) === 2, '实际 ' . count($specs) . ' 次');

// 超时 → 重试一次 → 仍超时 → 保留
$specs = [];
$dead = static function (array $spec) use (&$specs): array {
    $specs[] = $spec;

    return ['http' => 0, 'body' => 'Operation timed out after 20001 ms', 'curl_error' => true];
};
$result = ModelProbe::probe($channel, 'sk-test-key', 'public-name', 20, $dead);
check('持续超时 → 无法判定（保留）', $result['result'] === ModelProbe::INCONCLUSIVE, $result['result']);
check('超时也重试一次', count($specs) === 2, '实际 ' . count($specs) . ' 次');

// 401 不重试
$specs = [];
$denied = static function (array $spec) use (&$specs): array {
    $specs[] = $spec;

    return ['http' => 401, 'body' => '{"error":"invalid api key"}', 'curl_error' => false];
};
$result = ModelProbe::probe($channel, 'sk-test-key', 'public-name', 20, $denied);
check('401 → 鉴权失败', $result['result'] === ModelProbe::AUTH_ERROR, $result['result']);
check('401 不重试（重试也还是 401，只是白打上游）', count($specs) === 1, '实际 ' . count($specs) . ' 次');

// 摘要脱敏
$secret = 'sk-super-secret-value';
$leaky = static fn (array $spec): array => [
    'http' => 401,
    'body' => '{"error":"invalid api key: ' . $secret . '"}',
    'curl_error' => false,
];
$result = ModelProbe::probe($channel, $secret, 'public-name', 20, $leaky);
check('摘要里不出现完整凭据', !str_contains($result['summary'], $secret), $result['summary']);
check('摘要里出现脱敏占位', str_contains($result['summary'], '[REDACTED]'), $result['summary']);

$timeoutSpec = ModelProbe::buildSpec($channel, 'sk-test-key', 'vendor-name', 90);
check('总超时按传入的秒数生效', $timeoutSpec['total_timeout'] === 90, (string) $timeoutSpec['total_timeout']);
check('连接超时被限制在 10 秒内', $timeoutSpec['connect_timeout'] === 10, (string) $timeoutSpec['connect_timeout']);
$querySpec = ModelProbe::buildSpec([
    'base_url' => 'https://upstream.example.com/v1',
    'config' => json_encode(['auth_type' => 'query', 'auth_name' => 'api_key', 'extra_query' => ['x' => '1']]),
], 'sk-q', 'm', 20);
check('query 型鉴权把凭据放进查询串', str_contains($querySpec['url'], 'api_key=sk-q'), $querySpec['url']);
check('原有查询参数不被覆盖', str_contains($querySpec['url'], 'x=1'), $querySpec['url']);
check('query 型不会同时塞进请求头', !str_contains(implode("\n", $querySpec['headers']), 'sk-q'));

echo "\n";
if ($failures === []) {
    echo "全部 {$checks} 项检查通过。\n";
    exit(0);
}

echo "共 {$checks} 项检查，" . count($failures) . " 项失败：\n";
foreach ($failures as $failure) {
    echo "  - {$failure}\n";
}
exit(1);
