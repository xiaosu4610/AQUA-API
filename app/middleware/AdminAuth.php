<?php
/**
 * 后台鉴权中间件
 *
 * 作用：拦截所有需要登录的后台页面，未登录一律跳转到登录页。
 *
 * 为什么用中间件而不是在每个控制器方法里判断：
 *   后台会不断增加页面，靠「记得在每个方法里加判断」迟早会漏掉一个，
 *   漏掉的那个就是公开可访问的配置修改接口。中间件挂在路由组上，
 *   新增页面只要在同一组里就自动受保护。
 */

declare(strict_types=1);

namespace app\middleware;

use Webman\Http\Request;
use Webman\Http\Response;
use Webman\MiddlewareInterface;

class AdminAuth implements MiddlewareInterface
{
    /**
     * Session 中标记「已登录」的键。
     * 值固定为 1（单管理员，无需存用户 id）。
     */
    public const SESSION_KEY = 'admin_logged_in';

    public function process(Request $request, callable $handler): Response
    {
        if (!session()->has(self::SESSION_KEY)) {
            // 未登录：跳转登录页。
            // 这里直接构造响应而不用 redirect() 助手，是为了让跳转目标
            // 在本文件内一眼可见，排查重定向问题时不用再去翻助手定义。
            return response('', 302, ['Location' => '/admin/login']);
        }

        return $handler($request);
    }
}
