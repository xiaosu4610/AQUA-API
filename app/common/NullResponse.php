<?php
/**
 * 「什么都不发」的响应占位
 *
 * ═══ 为什么需要这个奇怪的东西 ═══
 *
 * Webman 的流程是「控制器返回响应 → 框架把响应写到连接上」。而流式转发
 * 必须**自己决定什么时候写响应头**：
 *
 *   · 上游迟迟不响应时，客户端应当继续等待，而不是先收到一个 200；
 *   · 上游返回 401/429/5xx 时，客户端应当收到**真实状态码**，
 *     这样才能正确重试或提示用户；
 *   · 只有确认上游 2xx 之后，才该把 `200 + text/event-stream` 发出去。
 *
 * 如果让控制器先返回一个 200，上面三点就都做不到了 ——
 * HTTP 状态码一旦发出就改不了，客户端会以为请求成功。
 *
 * ═══ 它为什么要继承 Response，而不是随便一个对象 ═══
 *
 * 因为框架在用**类型判断**决定「这条连接接下来怎么办」：
 *
 *   App::send() 里，只有当 `Connection: close` 没有出现时才会走
 *   `$connection->send()`；否则走 `$connection->close()` —— 那会在
 *   引擎开始转发之前就把连接关掉。
 *   （唯一的例外判断是「响应是 Response 且带 Transfer-Encoding: chunked」）
 *
 * 所以本类必须：
 *   ① 继承 support\Response —— 让上述判断成立；
 *   ② 带上 Transfer-Encoding: chunked —— 这样即使是显式要求
 *      `Connection: close` 的客户端，框架也会选择「发送」而不是「立刻关闭」；
 *      反正我们马上要用 chunked 自己写响应，这个头本来就该有；
 *   ③ 覆写 __toString 返回空串 —— 框架实际写出的字节数就是 0，
 *      TcpConnection::send('') 会直接返回不做任何写入。
 *
 * 三条缺一不可，所以才写成一个有名字的类，而不是在控制器里玩花活：
 * 下一个读代码的人必须能看懂「这里刻意什么都不发，以及为什么」。
 */

declare(strict_types=1);

namespace app\common;

use support\Response;

final class NullResponse extends Response
{
    public function __construct()
    {
        parent::__construct(200, ['Transfer-Encoding' => 'chunked'], '');
    }

    /**
     * 空串 —— 框架因此不会写出任何字节。
     */
    public function __toString(): string
    {
        return '';
    }
}
