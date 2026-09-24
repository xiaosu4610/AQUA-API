<?php
/**
 * 下游用户
 *
 * 与 app/common/Admin.php（超级管理员）是两套完全独立的东西，刻意不合并：
 *
 *   · 管理员：只有一个，**只要密码、不要用户名**，是站点所有者；
 *   · 用户：可以有很多，邮箱 + 密码，是站点的使用者。
 *
 * 合并成一个「用户表 + 角色字段」看起来更省事，但两者一旦共用登录入口，
 * 任何一处权限判断写漏都会让普通用户摸到后台。物理隔离更安全。
 *
 * ═══ 余额变动必须用「带条件的原子 UPDATE」 ═══
 *
 * 扣款不能写成「先读余额 → 判断够不够 → 再写回」：
 * 两个并发请求会同时通过判断，把余额扣成负数 —— 这是真实会发生的，
 * 而且一旦发生就是资金损失。因此这里用
 *   UPDATE users SET balance = balance - ? WHERE id = ? AND balance >= ?
 * 由数据库保证「够才扣」，受影响行数为 0 就表示余额不足。
 */

declare(strict_types=1);

namespace app\common;

final class User
{
    public const STATUS_ENABLED = 1;
    public const STATUS_DISABLED = 0;

    /** 邮箱验证与找回密码的链接有效期（秒） */
    private const VERIFY_TTL = 86400;   // 1 天
    private const RESET_TTL = 3600;     // 1 小时

    /**
     * 按 id 查用户。
     *
     * @return array<string, mixed>|null
     */
    public static function find(int $id): ?array
    {
        return Db::selectOne('SELECT * FROM users WHERE id = ?', [$id]);
    }

    /**
     * 按邮箱查用户。
     *
     * 邮箱**统一转小写再比**：用户下次可能用大写字母登录，
     * 而「邮箱不区分大小写」是所有人的预期，不统一就会出现
     * 「注册成功但登录说用户不存在」。
     *
     * @return array<string, mixed>|null
     */
    public static function findByEmail(string $email): ?array
    {
        return Db::selectOne('SELECT * FROM users WHERE email = ?', [self::normalizeEmail($email)]);
    }

    /** @return array<string, mixed>|null */
    public static function findByVerifyToken(string $token): ?array
    {
        if ($token === '') {
            return null;
        }

        return Db::selectOne('SELECT * FROM users WHERE verify_token = ?', [$token]);
    }

    /** @return array<string, mixed>|null */
    public static function findByResetToken(string $token): ?array
    {
        if ($token === '') {
            return null;
        }

        return Db::selectOne('SELECT * FROM users WHERE reset_token = ?', [$token]);
    }

    public static function normalizeEmail(string $email): string
    {
        return strtolower(trim($email));
    }

    /**
     * 邮箱格式是否合法。
     */
    public static function isValidEmail(string $email): bool
    {
        return filter_var($email, FILTER_VALIDATE_EMAIL) !== false && mb_strlen($email) <= 191;
    }

    /**
     * 注册用户。
     *
     * @param bool $needVerify 是否需要邮箱验证。不需要时直接视为已验证
     * @return array{ok:bool, message:string, id:int, verifyToken:string}
     */
    public static function register(
        string $email,
        string $password,
        string $ip,
        bool $needVerify,
        float $giftBalance = 0.0
    ): array {
        $email = self::normalizeEmail($email);

        if (!self::isValidEmail($email)) {
            return ['ok' => false, 'message' => '邮箱格式不正确', 'id' => 0, 'verifyToken' => ''];
        }

        if (self::findByEmail($email) !== null) {
            // 这里如实告知「邮箱已被注册」。
            // 有人会担心这构成账号枚举，但注册场景下无法回避：
            // 若含糊其辞，正常用户会在「为什么我注册不了」上耗很久。
            return ['ok' => false, 'message' => '该邮箱已被注册', 'id' => 0, 'verifyToken' => ''];
        }

        $now = time();
        $verifyToken = $needVerify ? bin2hex(random_bytes(24)) : '';

        Db::execute(
            'INSERT INTO users
                (email, password_hash, display_name, balance, total_spent, status,
                 email_verified_at, verify_token, register_ip, created_at, updated_at)
             VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)',
            [
                $email,
                password_hash($password, PASSWORD_DEFAULT),
                // 默认显示名取邮箱 @ 前的部分，比空着好看，也不涉及隐私
                mb_substr(strstr($email, '@', true) ?: $email, 0, 64),
                self::dec($giftBalance),
                self::STATUS_ENABLED,
                $needVerify ? null : $now,
                $verifyToken,
                mb_substr($ip, 0, 45),
                $now,
                $now,
            ]
        );

        return [
            'ok' => true,
            'message' => '',
            'id' => (int) Db::pdo()->lastInsertId(),
            'verifyToken' => $verifyToken,
        ];
    }

    /**
     * 校验邮箱 + 密码，返回结果。
     *
     * 失败原因刻意区分「密码错」与「邮箱未验证」——
     * 后者不是攻击行为，含糊其辞只会让用户反复重试密码。
     *
     * @return array{ok:bool, message:string, user:array<string,mixed>|null}
     */
    public static function attempt(string $email, string $password, ?string $ip = null): array
    {
        $user = self::findByEmail($email);

        if ($user === null || !password_verify($password, (string) $user['password_hash'])) {
            return ['ok' => false, 'message' => '邮箱或密码不正确', 'user' => null];
        }

        if ((int) $user['status'] !== self::STATUS_ENABLED) {
            return ['ok' => false, 'message' => '该账号已被停用，请联系站长', 'user' => null];
        }

        if (($user['email_verified_at'] ?? null) === null) {
            return ['ok' => false, 'message' => '邮箱尚未验证，请先点击验证邮件里的链接', 'user' => null];
        }

        $now = time();
        Db::execute(
            'UPDATE users SET last_login_at = ?, last_login_ip = ?, updated_at = ? WHERE id = ?',
            [$now, mb_substr((string) $ip, 0, 45), $now, (int) $user['id']]
        );

        return ['ok' => true, 'message' => '', 'user' => $user];
    }

    /** 邮箱是否已验证 */
    public static function isVerified(array $user): bool
    {
        return ($user['email_verified_at'] ?? null) !== null;
    }

    /**
     * 标记邮箱已验证，并清掉验证 token（一次性凭据用完即废）。
     */
    public static function markVerified(int $id): void
    {
        Db::execute(
            'UPDATE users SET email_verified_at = ?, verify_token = NULL, updated_at = ? WHERE id = ?',
            [time(), time(), $id]
        );
    }

    /** 重新签发验证 token（用于「重发验证邮件」） */
    public static function issueVerifyToken(int $id): string
    {
        $token = bin2hex(random_bytes(24));

        Db::execute(
            'UPDATE users SET verify_token = ?, updated_at = ? WHERE id = ?',
            [$token, time(), $id]
        );

        return $token;
    }

    /**
     * 签发找回密码 token。
     *
     * 带有效期：找回链接通常出现在邮件里、可能被转发或长期留在邮箱中，
     * 不过期等于一把永久钥匙。
     */
    public static function issueResetToken(int $id): string
    {
        $token = bin2hex(random_bytes(24));

        Db::execute(
            'UPDATE users SET reset_token = ?, reset_expires_at = ?, updated_at = ? WHERE id = ?',
            [$token, time() + self::RESET_TTL, time(), $id]
        );

        return $token;
    }

    /** 找回 token 是否仍有效 */
    public static function resetTokenValid(array $user): bool
    {
        $expires = (int) ($user['reset_expires_at'] ?? 0);

        return $expires > time();
    }

    /**
     * 修改密码并清掉找回 token（防止同一个链接被重复使用）。
     */
    public static function setPassword(int $id, string $plain): void
    {
        Db::execute(
            'UPDATE users SET password_hash = ?, reset_token = NULL, reset_expires_at = NULL, updated_at = ? WHERE id = ?',
            [password_hash($plain, PASSWORD_DEFAULT), time(), $id]
        );
    }

    public static function updateProfile(int $id, string $displayName): void
    {
        Db::execute(
            'UPDATE users SET display_name = ?, updated_at = ? WHERE id = ?',
            [mb_substr(trim($displayName), 0, 64), time(), $id]
        );
    }

    public static function setStatus(int $id, int $status): void
    {
        Db::execute(
            'UPDATE users SET status = ?, updated_at = ? WHERE id = ?',
            [$status, time(), $id]
        );
    }

    /**
     * 加钱（充值到账、注册赠送、站长手工调整）。
     *
     * 原子自增，不做「先读后写」—— 支付回调可能并发到达。
     */
    public static function credit(int $id, float $amount): void
    {
        if ($amount == 0.0) {
            return;
        }

        Db::execute(
            'UPDATE users SET balance = balance + ?, updated_at = ? WHERE id = ?',
            [self::dec($amount), time(), $id]
        );
    }

    /**
     * 扣钱：余额足够才扣，返回是否扣成功。
     *
     * 这是本项目**唯一允许把余额扣成负数的地方**的防线 ——
     * 条件写在 UPDATE 里，由数据库保证原子性（详见文件头说明）。
     */
    public static function tryDebit(int $id, float $amount): bool
    {
        if ($amount <= 0) {
            return true;
        }

        $affected = Db::execute(
            'UPDATE users
             SET balance = balance - ?, total_spent = total_spent + ?, updated_at = ?
             WHERE id = ? AND balance >= ?',
            [self::dec($amount), self::dec($amount), time(), $id, self::dec($amount)]
        );

        return $affected === 1;
    }

    /**
     * 分页（后台用户列表用）。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function page(int $limit, int $offset): array
    {
        return Db::select(
            'SELECT * FROM users ORDER BY id DESC LIMIT ' . max(1, $limit) . ' OFFSET ' . max(0, $offset)
        );
    }

    public static function count(): int
    {
        $row = Db::selectOne('SELECT COUNT(*) AS c FROM users');

        return (int) ($row['c'] ?? 0);
    }

    /** 余额字符串（供模板安全输出） */
    public static function money(mixed $value): string
    {
        $float = (float) $value;

        return $float == 0.0
            ? '0'
            : rtrim(rtrim(number_format($float, 8, '.', ''), '0'), '.');
    }

    /**
     * 小数字符串（落库用）。
     *
     * 用 number_format 而不是 (string) 强转：后者对很小的数
     * 会输出科学计数法（1.0E-7），数据库的 DECIMAL 不认这种写法。
     */
    private static function dec(float $value): string
    {
        return number_format($value, 10, '.', '');
    }
}
