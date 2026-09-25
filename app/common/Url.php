<?php
/**
 * 对外地址推导
 *
 * 只有一件事要做：算出**外部可访问**的站点基址。
 *
 * ═══ 为什么需要单独一个类 ═══
 *
 * 有两处必须拿到「外部地址」，而它们都容易写错：
 *   · 支付回调地址（notify_url）—— 支付网关的服务器要能访问到；
 *   · 邮件里的验证/重置链接 —— 用户点开时要能打开。
 *
 * 这两处的共同陷阱是：**反代场景下 Webman 收到的是 http，
 * 但对外其实是 https**。如果直接取请求的 scheme，回调地址就会变成
 * http://…，要么被网关拒绝，要么让用户看到「不安全」的警告。
 * 正确做法是先看反代透传的 `X-Forwarded-Proto` 头。
 *
 * 抽成一个方法而不是两处各写一遍，是因为这类「环境相关」的判断
 * 一旦不一致，症状会非常隐蔽（一个能收钱、一个收不到），
 * 排查成本远高于多一个文件。
 */

declare(strict_types=1);

namespace app\common;

use support\Request;
use Throwable;

final class Url
{
    /**
     * 站点对外基址，形如 `https://aqua.is3.cc`（不带末尾斜杠）。
     *
     * 优先用后台配置的 `site.domain`：生产环境往往有 CDN、多域名、
     * 或经多层反代，请求头里的 Host 不一定等于对外域名。
     * 没配才回落到当前请求的 Host —— 这样全新安装也能开箱即用。
     */
    public static function base(?Request $request = null): string
    {
        $domain = trim((string) Settings::get('site.domain', ''));

        if ($domain !== '') {
            // 允许站长把域名填成带协议的形式（https://example.com），
            // 那样按协议为准；否则用当前请求的协议
            if (preg_match('#^https?://#i', $domain) === 1) {
                return rtrim($domain, '/');
            }

            return self::scheme($request) . '://' . rtrim($domain, '/');
        }

        return self::scheme($request) . '://' . self::host($request);
    }

    /**
     * OpenAI 兼容接口的接入地址（形如 https://站点/v1）。
     *
     * 为什么收敛到这一个方法：首页、接口文档、用户控制台三处都要显示它，
     * 各写一份的话迟早出现「首页显示一个地址、文档显示另一个」这种
     * 最难排查的不一致 —— 用户会照着一个错的去配。
     */
    public static function apiBase(?Request $request = null): string
    {
        return rtrim(self::base($request), '/') . '/v1';
    }

    /**
     * 判断对外协议。
     *
     * 顺序不能反：只要反代透传了 X-Forwarded-Proto，就以它为准 ——
     * 因为直连 Webman 的那一段永远是 http（Nginx 到 127.0.0.1:8787）。
     * 只有没有该头时（例如本地直接访问、未经过反代）才看请求本身。
     */
    public static function scheme(?Request $request): string
    {
        if ($request === null) {
            return 'http';
        }

        try {
            $proto = strtolower((string) $request->header('x-forwarded-proto', ''));

            if (str_contains($proto, 'https')) {
                return 'https';
            }

            if (str_contains($proto, 'http')) {
                return 'http';
            }

            // 少数反代会用这个头表示「已启用 SSL」
            if (strtolower((string) $request->header('x-forwarded-ssl', '')) === 'on') {
                return 'https';
            }

            return strtolower((string) $request->header('https', '')) === 'on' ? 'https' : 'http';
        } catch (Throwable) {
            return 'http';
        }
    }

    /**
     * 当前请求的 Host（含端口）。拿不到时返回 localhost，
     * 让链接至少是可解析的，而不是拼出一个空域名。
     */
    private static function host(?Request $request): string
    {
        if ($request === null) {
            return 'localhost';
        }

        try {
            $host = (string) $request->host();

            return $host !== '' ? $host : 'localhost';
        } catch (Throwable) {
            return 'localhost';
        }
    }
}
