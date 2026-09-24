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
 *
 * 除了「有没有登录」，这里还负责「登录多久了」：
 *   后台是高权限入口，登录态不能无限期有效。超时后强制重新登录。
 *   这个判定放在中间件里（每个请求都查），因此后台改配置能立刻生效，
 *   不像 config/session.php 那样要重启进程才读得到新值。
 */

declare(strict_types=1);

namespace app\middleware;

use app\common\Settings;
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

    /** Session 中记录登录时刻的键（Unix 时间戳） */
    public const LOGIN_AT_KEY = 'admin_login_at';

    /**
     * 登录态最长有效分钟数，可由后台配置。
     *
     * 之所以要夹上下限：填 0 会让「每次请求都要求重新登录」（把自己锁在门外），
     * 填得过大又等于没有超时保护，两种都不是站长真正想要的。
     */
    private function sessionLimitSeconds(): int
    {
        $minutes = Settings::int('security.admin_session_minutes', 120);

        return max(5, min(10080, $minutes)) * 60;
    }

    public function process(Request $request, callable $handler): Response
    {
        if (!session()->has(self::SESSION_KEY)) {
            return $this->toLogin();
        }

        $loginAt = (int) session()->get(self::LOGIN_AT_KEY, 0);

        // 登录时刻缺失（例如从更早的版本升级上来，那时没记这个字段）
        // 视为「刚登录」并补记，而不是直接踢掉 ——
        // 否则升级后所有人会被强制登出，体验很差且看起来像 bug。
        if ($loginAt <= 0) {
            session()->set(self::LOGIN_AT_KEY, time());

            return $handler($request);
        }

        if (time() - $loginAt > $this->sessionLimitSeconds()) {
            // 超时：清空会话（而不只是删登录标记），
            // 免得残留的临时数据在下次登录后被复用
            session()->flush();

            return $this->toLogin();
        }

        return $handler($request);
    }

    /**
     * 跳转登录页。
     * 直接构造响应而不用 redirect() 助手，是为了让跳转目标在本文件内一眼可见，
     * 排查重定向问题时不用再去翻助手定义。
     */
    private function toLogin(): Response
    {
        return response('', 302, ['Location' => '/admin/login']);
    }
}
