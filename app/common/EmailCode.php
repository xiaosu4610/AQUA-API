<?php
/**
 * 邮箱验证码（发码、限流、校验）。
 *
 * 为什么要有这个东西：注册时要求「证明这个邮箱是你的」。
 * 之前用的是「点邮件里的链接」，但那条路对用户其实更绕 ——
 * 要切到邮箱、点链接、再切回来，中途还常被邮件客户端拦在浏览器外。
 * 验证码是「在同一个页面上填 6 个数字」，成功率明显更高。
 *
 * ═══ 三条安全约束（缺一条都会被滥用）═══
 *
 * 1) **只存哈希**。数据库里是 HMAC-SHA256，不是 6 位数字本身。
 *    与用户密码同一原则：拿到库也不等于拿到能通过验证的字符串。
 *
 * 2) **限流**。没有限流的话，任何人都可以拿别人的邮箱地址
 *    反复触发发信 —— 既骚扰对方，也把我们的发信通道信誉烧掉。
 *    所以：同一邮箱 60 秒内只能发一次、每小时最多 5 次；
 *    同一 IP 每小时最多 20 次。
 *
 * 3) **限次**。验证码允许试错，但同一条码最多试 5 次，
 *    超过就作废（否则 6 位数字在一台机器上几分钟就能穷举完）。
 */

declare(strict_types=1);

namespace app\common;

final class EmailCode
{
    /** 注册用途。以后要加「换邮箱」「二次验证」直接加常量即可 */
    public const PURPOSE_REGISTER = 'register';

    /** 验证码有效期（秒）。10 分钟：够用户切到邮箱看一眼，又不至于长期有效 */
    private const TTL = 600;

    /** 同一邮箱两次发码的最小间隔（秒） */
    private const RESEND_INTERVAL = 60;

    /** 同一邮箱每小时最多发几次 */
    private const MAX_PER_EMAIL_HOUR = 5;

    /** 同一 IP 每小时最多发几次（防止换邮箱批量刷） */
    private const MAX_PER_IP_HOUR = 20;

    /** 同一条验证码最多允许试错次数 */
    private const MAX_ATTEMPTS = 5;

    /**
     * 发一条新验证码。
     *
     * 返回里带**明文验证码**（调用方负责发信），数据库里只落哈希 ——
     * 明文只在这一刻存在于内存中，不写日志、不入库。
     *
     * @return array{ok:bool, message:string, code:string, retryAfter:int}
     */
    public static function issue(string $email, string $purpose, string $ip): array
    {
        $email = mb_strtolower(trim($email));
        $now = time();

        $last = Db::selectOne(
            'SELECT created_at FROM email_codes WHERE email = ? AND purpose = ? ORDER BY id DESC LIMIT 1',
            [$email, $purpose]
        );
        if ($last !== null) {
            $wait = self::RESEND_INTERVAL - ($now - (int) $last['created_at']);
            if ($wait > 0) {
                return ['ok' => false, 'message' => "请等待 {$wait} 秒后再获取", 'code' => '', 'retryAfter' => $wait];
            }
        }

        $hourAgo = $now - 3600;
        $byEmail = (int) (Db::selectOne(
            'SELECT COUNT(*) AS c FROM email_codes WHERE email = ? AND purpose = ? AND created_at > ?',
            [$email, $purpose, $hourAgo]
        )['c'] ?? 0);
        if ($byEmail >= self::MAX_PER_EMAIL_HOUR) {
            return [
                'ok' => false,
                'message' => '这个邮箱今天获取验证码的次数过多，请稍后再试',
                'code' => '',
                'retryAfter' => 0,
            ];
        }

        $byIp = (int) (Db::selectOne(
            'SELECT COUNT(*) AS c FROM email_codes WHERE ip = ? AND created_at > ?',
            [$ip, $hourAgo]
        )['c'] ?? 0);
        if ($byIp >= self::MAX_PER_IP_HOUR) {
            return [
                'ok' => false,
                'message' => '当前网络的获取次数过多，请稍后再试',
                'code' => '',
                'retryAfter' => 0,
            ];
        }

        // 六位数字验证码。用 random_int 而不是 rand：后者是可预测的伪随机，
        // 攻击者拿到几条历史码就能推出后面的
        $code = str_pad((string) random_int(0, 999999), 6, '0', STR_PAD_LEFT);

        Db::execute(
            'INSERT INTO email_codes (email, purpose, code_hash, ip, attempts, expires_at, used_at, created_at)
             VALUES (?, ?, ?, ?, 0, ?, NULL, ?)',
            [$email, $purpose, self::hash($email, $purpose, $code), $ip, $now + self::TTL, $now]
        );

        self::cleanup($now);

        return ['ok' => true, 'message' => '', 'code' => $code, 'retryAfter' => self::RESEND_INTERVAL];
    }

    /**
     * 校验验证码。
     *
     * 成功即作废该码（一次性）；失败会累计次数，超限自动作废。
     *
     * @return array{ok:bool, message:string}
     */
    public static function verify(string $email, string $purpose, string $code): array
    {
        $email = mb_strtolower(trim($email));
        $code = trim($code);
        $now = time();

        if ($code === '') {
            return ['ok' => false, 'message' => '请先获取邮箱验证码'];
        }

        $row = Db::selectOne(
            'SELECT id, code_hash, attempts FROM email_codes
             WHERE email = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?
             ORDER BY id DESC LIMIT 1',
            [$email, $purpose, $now]
        );

        if ($row === null) {
            return ['ok' => false, 'message' => '验证码已过期，请重新获取'];
        }

        $attempts = (int) $row['attempts'] + 1;
        Db::execute('UPDATE email_codes SET attempts = ? WHERE id = ?', [$attempts, (int) $row['id']]);

        if ($attempts > self::MAX_ATTEMPTS) {
            // 作废：继续留着只会被继续穷举
            Db::execute('UPDATE email_codes SET used_at = ? WHERE id = ?', [$now, (int) $row['id']]);

            return ['ok' => false, 'message' => '验证码错误次数过多，请重新获取'];
        }

        // 定长时间比较，避免通过响应耗时逐位试出验证码
        if (!hash_equals((string) $row['code_hash'], self::hash($email, $purpose, $code))) {
            $left = self::MAX_ATTEMPTS - $attempts;

            return ['ok' => false, 'message' => "验证码不正确（还可以试 {$left} 次）"];
        }

        Db::execute('UPDATE email_codes SET used_at = ? WHERE id = ?', [$now, (int) $row['id']]);

        return ['ok' => true, 'message' => ''];
    }

    /**
     * 验证码哈希。
     *
     * 用 HMAC 而不是裸 SHA-256：6 位数字总共只有 100 万种可能，
     * 裸哈希被拖库后可以瞬间反查出来。加一把只存在于本机的密钥
     * （APP_KEY）之后，攻击者没有密钥就算拿到哈希也算不出原码。
     */
    private static function hash(string $email, string $purpose, string $code): string
    {
        $secret = trim((string) (getenv('APP_KEY') ?: ''));

        return hash_hmac('sha256', $purpose . '|' . $email . '|' . $code, $secret);
    }

    /** 清掉过期很久的记录，避免表无限增长（每次发码顺手做一次，无需定时任务） */
    private static function cleanup(int $now): void
    {
        Db::execute('DELETE FROM email_codes WHERE created_at < ?', [$now - 86400]);
    }
}
