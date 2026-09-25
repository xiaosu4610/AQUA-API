<?php
/**
 * 全局异常处理
 *
 * 框架默认的行为有两个不适合我们：
 *   1）非 JSON 请求遇到未预期的异常时，只回一句 "Server internal error"，
 *      或者（调试模式下）把整段堆栈原样吐给访客 —— 前者让访客毫无头绪，
 *      后者是明确的信息泄露；
 *   2）JSON 请求与浏览器请求的响应形态混在一起判断。
 *
 * 这里做的事情只有一件：**把「未预期的异常」渲染成我们自己设计的错误页，
 * 并且保留正确的 HTTP 状态码**。状态码很重要 ——
 * 客户端与监控靠它区分「该重试」还是「该报错」，
 * 一律返回 200 会让上游的故障被我们自己的监控漏掉。
 *
 * 两条边界：
 *   · 异常自己带 render() 的，优先交给它 —— 404 就是靠这条路走 app/view/404.html；
 *   · /v1/* 一律回 JSON，因为各家 SDK 不会解析 HTML 错误页，
 *     收到 HTML 只会报「无法解析响应」，反而把真正的原因盖掉。
 *
 * 顺带保留框架的「不记录」清单（BusinessException 不写日志），
 * 否则 404 会把日志刷满。
 */

declare(strict_types=1);

namespace app\exception;

use app\middleware\SecurityHeaders;
use Throwable;
use Webman\Http\Request;
use Webman\Http\Response;

class Handler extends \support\exception\Handler
{
    /**
     * 渲染异常。
     *
     * 判断顺序很重要：**先定「回 JSON 还是回 HTML」，再决定怎么渲染**。
     * 反过来的话，PageNotFoundException 会抢先把 /v1/* 的请求渲染成 HTML
     * （它自己不判断路径），而 API 调用方拿到 HTML 只会报「无法解析响应」。
     */
    public function render(Request $request, Throwable $exception): Response
    {
        // 异常码不合法（0、负数、或误用成业务码）时统一按 500 处理
        $status = (int) $exception->getCode();
        if ($status < 400 || $status > 599) {
            $status = 500;
        }

        // 安全响应头必须由这里补一次：**未匹配到路由的请求不会经过全局中间件**
        // （路由分发阶段就抛异常了），所以 404/500 这些响应默认是「裸的」。
        // 而错误页恰恰是扫描者最常撞的一批页面。共用中间件那份清单，不另写一份
        $headers = SecurityHeaders::headersFor(SecurityHeaders::normalizePath($request));

        if ($this->wantsJson($request)) {
            return $this->jsonResponse($status, $exception, $headers);
        }

        // HTML 路径：先让异常自带渲染 —— 404 就是靠这一步去渲染 app/view/404.html
        if (method_exists($exception, 'render') && ($response = $exception->render($request))) {
            return $response->withHeaders($headers);
        }

        return $this->htmlResponse($status, $exception, $headers);
    }

    /**
     * 调用方期望收到 JSON 吗。
     */
    private function wantsJson(Request $request): bool
    {
        if ($request->expectsJson()) {
            return true;
        }

        // /v1/* 是给程序调用的（OpenAI 兼容接口）。
        // 这些客户端通常不带 Accept: application/json，也不带 X-Requested-With，
        // 所以 expectJson() 判断不出来，必须按路径显式兜住
        return str_starts_with((string) $request->path(), '/v1/');
    }

    /**
     * JSON 响应：沿用 OpenAI 的错误信封，让各家 SDK 能按既有逻辑解析。
     *
     * @param array<string, string> $headers 安全响应头（与全局中间件同一份清单）
     */
    private function jsonResponse(int $status, Throwable $exception, array $headers): Response
    {
        // 生产环境不回显异常原文 —— 里面可能有路径、SQL、上游响应
        $message = $this->debug ? $exception->getMessage() : $this->publicMessage($status);

        $body = (string) json_encode([
            'error' => [
                'message' => $message,
                'type' => $status >= 500 ? 'api_error' : 'invalid_request_error',
                'param' => null,
                'code' => null,
            ],
        ], JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);

        return response($body, $status, $headers + ['Content-Type' => 'application/json; charset=utf-8']);
    }

    /**
     * HTML 响应：渲染共用的错误页。
     *
     * @param array<string, string> $headers 安全响应头（与全局中间件同一份清单）
     */
    private function htmlResponse(int $status, Throwable $exception, array $headers): Response
    {
        $html = raw_view('_error', [
            'code' => $status,
            // 调试详情只在 APP_DEBUG 打开时给出，并且由模板转义后放进折叠区
            'debugDetail' => $this->debug ? (string) $exception : '',
        ], '')->rawBody();

        return response($html, $status, $headers + ['Content-Type' => 'text/html; charset=utf-8']);
    }

    /**
     * 给调用方看的一句话说明（英文 API 惯例，但这里用中文更贴合本项目）。
     */
    private function publicMessage(int $status): string
    {
        return match (true) {
            $status === 400 => '请求格式有误',
            $status === 401 => '需要提供有效的访问令牌',
            $status === 403 => '没有访问权限',
            $status === 404 => '请求的路径不存在',
            $status === 405 => '请求方式不被支持',
            $status === 429 => '请求过于频繁，请稍后重试',
            $status >= 500 => '服务器内部错误',
            default => '请求未能完成',
        };
    }
}
