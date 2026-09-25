<?php
/**
 * 全局安全响应头
 *
 * 这些头部是「浏览器侧」的防线，加一次全局生效，不需要每个页面各写一遍。
 * 逐个说明为什么是这个取值 —— 这几个值里有几个是**刻意不选更严的档**，
 * 改之前请先读完理由，否则会把正常功能一起拦掉。
 *
 * ═══ 一个必须知道的边界 ═══
 *
 * **流式转发（/v1/chat/completions）不走这里。**
 * 转发引擎在确认上游 2xx 之后会**自己构造并直接写响应头**（见 RelayEngine::commit），
 * 因为它必须先握有完整控制权才能做到「状态码等上游确认后才发」。
 * 所以那类响应的头部由引擎自己负责，本中间件加不上。
 * 这是可接受的：那些是给程序读的 API 响应，不渲染成页面，浏览器侧的
 * 点击劫持 / 收录 / 嗅探风险都不适用。
 */
declare(strict_types=1);

namespace app\middleware;

use Webman\Http\Request;
use Webman\Http\Response;
use Webman\MiddlewareInterface;

class SecurityHeaders implements MiddlewareInterface
{
    public function process(Request $request, callable $handler): Response
    {
        /** @var Response $response */
        $response = $handler($request);

        return $response->withHeaders(self::headersFor(self::normalizePath($request)));
    }

    /**
     * 该加哪些安全响应头。
     *
     * ⚠️ 这个方法刻意是 **public static** 的：异常处理器也要用同一份。
     *
     * 原因是 Webman 的一个边界 —— **未匹配到路由的请求不会经过全局中间件**
     * （路由分发阶段就抛了 PageNotFoundException）。所以「404 页面」
     * 拿不到这些头。既然错误页恰恰是扫描者最常看到的一批页面，
     * 就不能让它是半吊子的：让两处共用同一个来源，而不是各写一份。
     *
     * @return array<string, string>
     */
    public static function headersFor(string $path): array
    {
        $headers = [
            // 禁止浏览器「猜」内容类型。
            // 不加这条时，某些浏览器会把一个纯文本响应按 HTML 解析，
            // 于是响应体里的 <script> 就真的执行了 —— 这是 XSS 的一条旁路
            'X-Content-Type-Options' => 'nosniff',

            // 禁止别的站点用 iframe 把本站嵌进去（防点击劫持）。
            // 用 SAMEORIGIN 而不是 DENY：本站自己的页面之间可能需要内嵌
            'X-Frame-Options' => 'SAMEORIGIN',

            // ★ 这一档是**刻意选的，不要改成 no-referrer**。
            //
            // 本站有对外跳转（友情链接、项目仓库、备案查询）。
            // no-referrer 会让对方的来源统计**全部丢失** —— 等于把正常访问
            // 也一起拦了，别人还以为我们没给他带流量。
            // strict-origin-when-cross-origin 只送「来源域」、不送完整路径：
            // 对方统计得到「有人从我站过来」，而我们的具体页面地址不外泄。
            'Referrer-Policy' => 'strict-origin-when-cross-origin',

            // 明确关掉本站用不到的高危能力，减少被利用面
            'Permissions-Policy' => 'geolocation=(), microphone=(), camera=()',
        ];

        // 只让首页可被搜索引擎收录，其余一律 noindex。
        //
        // 后台尤其重要：路径不公开也得加这条 —— 爬虫会撞路径、外链会泄露地址，
        // 一旦被收录，「后台入口」就等于挂在搜索结果里
        if ($path !== '/') {
            $headers['X-Robots-Tag'] = 'noindex, nofollow, noarchive';
        }

        return $headers;
    }

    /**
     * 把路径归一成「以 / 开头且无结尾斜杠」的形式。
     *
     * Webman 的 path() 不带前导斜杠，且不同版本行为略有差别，
     * 所以统一在这里规范化一次，避免比较时踩坑（例如 '/admin' 与 'admin' 判成不同）。
     */
    public static function normalizePath(Request $request): string
    {
        $path = '/' . trim($request->path(), '/');

        return $path === '//' ? '/' : $path;
    }
}
