<?php
/**
 * 测试客户端：发出请求后就**停止读取**（仅用于验证转发引擎的积压保护）
 *
 * 为什么需要它：慢客户端是真实存在的（手机弱网、脚本忘了消费响应体）。
 * 如果服务端不管不顾地往连接里堆数据，内存会被瞬间吃光 ——
 * 这是网关最容易出的「内存爆炸」故障。这个脚本就是把它复现出来。
 *
 * 用法：php scripts/test-hungry-client.php <令牌> [秒数]
 */

declare(strict_types=1);

$token = $GLOBALS['argv'][1] ?? '';
$holdSeconds = (int) ($GLOBALS['argv'][2] ?? 60);

$body = json_encode([
    'model' => 'mock-big',
    'stream' => true,
    'messages' => [['role' => 'user', 'content' => 'x']],
], JSON_UNESCAPED_UNICODE);

$socket = stream_socket_client('tcp://127.0.0.1:8787', $errno, $errstr, 5);
if ($socket === false) {
    fwrite(STDERR, "连接失败：$errstr\n");
    exit(1);
}

fwrite($socket, "POST /v1/chat/completions HTTP/1.1\r\n"
    . "Host: 127.0.0.1:8787\r\n"
    . "Authorization: Bearer {$token}\r\n"
    . "Content-Type: application/json\r\n"
    . 'Content-Length: ' . strlen($body) . "\r\n"
    . "Connection: keep-alive\r\n\r\n"
    . $body);

// 只读一小段响应头，确认服务端已开始转发 —— 之后就不读了
usleep(300000);
$head = (string) fread($socket, 512);
echo "已读到响应头（前 80 字节）：" . substr(str_replace(["\r", "\n"], '|', $head), 0, 80) . "\n";
echo "现在停止读取 {$holdSeconds} 秒，观察服务端会不会撑爆内存……\n";

sleep($holdSeconds);

// 服务端若触发了积压保护，此时连接已被关闭
stream_set_blocking($socket, false);
$rest = (string) fread($socket, 65536);
echo '停止读取结束后又能读到 ' . strlen($rest) . " 字节（若为 0，通常表示服务端已中止该连接）\n";

fclose($socket);
