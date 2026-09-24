<?php
/**
 * CSRF 令牌
 *
 * 后台表单（登录、登出、以及后续所有改配置的页面）都要带这个令牌。
 * 原因是浏览器会自动携带 Cookie，攻击者只要诱导已登录的管理员访问一个恶意页面，
 * 就能借用其身份提交请求 —— 后台是改配置、加渠道的地方，被 CSRF 后果很严重。
 *
 * 实现方式：令牌存在 Session 里，每次生成后复用；
 * 校验用 hash_equals()（定长时间比较），避免通过响应时间差猜测令牌。
 */

declare(strict_types=1);

namespace app\common;

final class Csrf
{
    /** Session 中存放令牌的键名 */
    private const SESSION_KEY = '_csrf_token';

    /**
     * 获取当前令牌；不存在则生成一个。
     */
    public static function token(): string
    {
        $session = session();
        $token = $session->get(self::SESSION_KEY);

        if (!is_string($token) || $token === '') {
            $token = bin2hex(random_bytes(32));
            $session->set(self::SESSION_KEY, $token);
        }

        return $token;
    }

    /**
     * 校验提交上来的令牌是否有效。
     */
    public static function check(mixed $submitted): bool
    {
        $expected = session()->get(self::SESSION_KEY);

        if (!is_string($expected) || $expected === '' || !is_string($submitted)) {
            return false;
        }

        return hash_equals($expected, $submitted);
    }
}
