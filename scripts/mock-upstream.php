<?php
/**
 * 模拟上游（仅用于测试转发引擎，不参与生产）
 *
 * 为什么需要一个模拟上游：网关的正确性藏在**边界情况**里 ——
 * 上游 429 时会不会换渠道、不返回 usage 时会不会算错钱、
 * 客户端读得慢会不会把内存撑爆、首字节迟迟不来会不会一直挂着。
 * 这些在真实上游上都极难复现，而它们恰恰是最容易写错的地方。
 *
 * 用法：
 *   php scripts/mock-upstream.php --port=9911
 *
 * 然后按模型名触发不同行为（模型名即"剧本"）：
 *   mock-ok          正常流式返回（含 usage）
 *   mock-no-usage    流式但不回 usage —— 验证「估算」路径
 *   mock-json        非流式返回
 *   mock-429         返回 429 —— 验证换渠道重试
 *   mock-500         返回 500 —— 验证换渠道重试
 *   mock-auth        返回 401 —— 验证密钥被永久停用
 *   mock-slow        拖很久才回首字节 —— 验证首字节超时
 *   mock-stall       回了头就再也不发数据 —— 验证空闲超时
 *   mock-big         一次性吐很大一段 —— 验证积压保护
 *   mock-echo        把收到的请求体原样回给你 —— 验证请求是否被正确转发
 *
 * ⚠️ 这是测试工具，不要在生产上跑它，也不要把它暴露到公网。
 */

declare(strict_types=1);

require __DIR__ . '/../vendor/autoload.php';

use Workerman\Connection\TcpConnection;
use Workerman\Protocols\Http\Chunk;
use Workerman\Protocols\Http\Request;
use Workerman\Protocols\Http\Response;
use Workerman\Timer;
use Workerman\Worker;

$args = [];
foreach (array_slice($GLOBALS['argv'], 1) as $arg) {
    if (preg_match('/^--([^=]+)=(.*)$/', $arg, $m)) {
        $args[$m[1]] = $m[2];
    }
}

$port = (int) ($args['port'] ?? 9911);
// Windows 下 Workerman 不支持同一 worker 开多个进程，会退化为 1
$count = max(1, (int) ($args['count'] ?? 4));

$worker = new Worker("http://127.0.0.1:{$port}");
$worker->count = $count;
$worker->name = 'mock-upstream';

$worker->onMessage = static function (TcpConnection $connection, Request $request): void {
    $path = $request->path();

    if ($path === '/v1/models') {
        $connection->send(new Response(200, ['Content-Type' => 'application/json'], (string) json_encode([
            'object' => 'list',
            'data' => [
                ['id' => 'mock-ok', 'object' => 'model'],
                ['id' => 'mock-json', 'object' => 'model'],
                ['id' => 'mock-no-usage', 'object' => 'model'],
            ],
        ])));

        return;
    }

    if ($path !== '/v1/chat/completions') {
        $connection->send(new Response(404, ['Content-Type' => 'application/json'], '{"error":{"message":"not found"}}'));

        return;
    }

    $body = json_decode($request->rawBody(), true);
    if (!is_array($body)) {
        $connection->send(new Response(400, [], '{"error":{"message":"bad json"}}'));

        return;
    }

    $model = (string) ($body['model'] ?? '');
    $stream = ($body['stream'] ?? false) === true;

    // 也支持「按 API Key 触发不同行为」：这样可以造出
    // 「同一个上游、A 渠道的 Key 被限流、B 渠道的 Key 正常」这种
    // 最贴近现实的换渠道场景
    $authHeader = (string) $request->header('authorization', '');
    $keyTrigger = str_contains($authHeader, 'k-429') ? '429'
        : (str_contains($authHeader, 'k-auth') ? 'auth'
        : (str_contains($authHeader, 'k-500') ? '500' : ''));

    // 需要先返回错误状态的剧本
    $failStatus = match (true) {
        $model === 'mock-429', $keyTrigger === '429' => 429,
        $model === 'mock-500', $keyTrigger === '500' => 500,
        $model === 'mock-auth', $keyTrigger === 'auth' => 401,
        default => 0,
    };

    if ($failStatus > 0) {
        $connection->send(new Response($failStatus, ['Content-Type' => 'application/json'], (string) json_encode([
            'error' => ['message' => "模拟上游错误：{$model}", 'type' => 'mock_error'],
        ], JSON_UNESCAPED_UNICODE)));

        return;
    }

    // 故意拖很久才回：验证首字节超时
    if ($model === 'mock-slow') {
        Timer::add(60, static function () use ($connection) {
            $connection->send(new Response(200, ['Content-Type' => 'application/json'], '{"choices":[]}'));
        }, [], false);

        return;
    }

    // 回一个头就再也不发数据：验证空闲超时
    if ($model === 'mock-stall') {
        $connection->send(new Response(200, [
            'Content-Type' => 'text/event-stream; charset=utf-8',
            'Transfer-Encoding' => 'chunked',
        ], ''));
        $connection->send(new Chunk(": stalled\n\n"));

        return;
    }

    // 把收到的请求体回给你：验证转发过去的请求长什么样
    if ($model === 'mock-echo') {
        $connection->send(new Response(200, ['Content-Type' => 'application/json'], (string) json_encode([
            'received' => $body,
            'headers' => $request->header(),
        ], JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES)));

        return;
    }

    // 非流式
    if (!$stream) {
        $payload = [
            'id' => 'chatcmpl-mock-' . bin2hex(random_bytes(4)),
            'object' => 'chat.completion',
            'created' => time(),
            'model' => $model,
            'choices' => [[
                'index' => 0,
                'message' => ['role' => 'assistant', 'content' => '这是一个非流式的模拟回答。'],
                'finish_reason' => 'stop',
            ]],
            'usage' => ['prompt_tokens' => 12, 'completion_tokens' => 9, 'total_tokens' => 21],
        ];

        if ($model === 'mock-big') {
            $payload['choices'][0]['message']['content'] = str_repeat('很长的回答内容。', 200000);
        }

        $connection->send(new Response(200, ['Content-Type' => 'application/json'], (string) json_encode($payload, JSON_UNESCAPED_UNICODE)));

        return;
    }

    // ── 流式 ──
    $connection->send(new Response(200, [
        'Content-Type' => 'text/event-stream; charset=utf-8',
        'Transfer-Encoding' => 'chunked',
        'Cache-Control' => 'no-cache',
    ], ''));

    // 每块 1.5KB × 2000 块 ≈ 3MB。
    // 想验证「客户端读得慢/不读」的积压保护时，把 100 改成 1000（每块 15KB，
    // 总量约 30MB）—— 只有超过操作系统的收发缓冲，那段逻辑才会真正被走到
    $pieces = $model === 'mock-big'
        ? array_fill(0, 2000, str_repeat('大量内容。', 100))
        : ['你好', '，这是', '一个', '模拟的', '流式回答。'];

    $index = 0;
    $includeUsage = $model !== 'mock-no-usage';

    // mock-big 用更短的间隔猛吐数据，方便在几秒内验证「客户端读得慢」的保护逻辑
    $interval = $model === 'mock-big' ? 0.005 : 0.05;

    $timerId = Timer::add($interval, static function () use (
        &$index,
        $pieces,
        $connection,
        $model,
        $includeUsage,
        &$timerId
    ): void {
        if (!isset($pieces[$index])) {
            Timer::del($timerId);

            // 最后一块：可选的 usage，然后 [DONE]
            if ($includeUsage) {
                $connection->send(new Chunk('data: ' . json_encode([
                    'id' => 'chatcmpl-mock',
                    'object' => 'chat.completion.chunk',
                    'model' => $model,
                    'choices' => [],
                    'usage' => ['prompt_tokens' => 37, 'completion_tokens' => 21, 'total_tokens' => 58],
                ], JSON_UNESCAPED_UNICODE) . "\n\n"));
            }

            $connection->send(new Chunk("data: [DONE]\n\n"));
            $connection->close(new Chunk(''));

            return;
        }

        $connection->send(new Chunk('data: ' . json_encode([
            'id' => 'chatcmpl-mock',
            'object' => 'chat.completion.chunk',
            'created' => time(),
            'model' => $model,
            'choices' => [[
                'index' => 0,
                'delta' => ['content' => $pieces[$index]],
                'finish_reason' => null,
            ]],
        ], JSON_UNESCAPED_UNICODE) . "\n\n"));

        $index++;
    }, [], true);
};

Worker::$logFile = sys_get_temp_dir() . '/mock-upstream.log';
Worker::runAll();
