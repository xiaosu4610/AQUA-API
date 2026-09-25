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

use Throwable;

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
     * ═══ 防暴力破解（v9 补上） ═══
     *
     * 后台（Admin）从一开始就有失败计数与锁定，但用户这边一直没有 ——
     * 等于**用户密码可以被无限次尝试**。而用户账号后面挂的是真金白银的余额
     * 和站长自己买来的上游额度，撞开一个就能把额度跑光。
     * 所以这里补齐：连续失败到阈值即锁定一段时间，锁定期间**连密码都不校验**
     * （否则「锁定」只是个提示，暴力破解照样继续试）。
     *
     * 已知的一个覆盖不到的地方：**邮箱根本不存在时无法计数**（没有对应的用户行可改），
     * 所以攻击者仍然可以拿随机邮箱无限试探接口。但这不会攻破任何具体账号，
     * 且要按 IP 计数就得再建一张表 —— 收益不抵复杂度，明确记为已知取舍。
     *
     * @return array{ok:bool, message:string, user:array<string,mixed>|null}
     */
    public static function attempt(string $email, string $password, ?string $ip = null): array
    {
        $user = self::findByEmail($email);

        // ① 锁定中：直接拒绝
        if ($user !== null) {
            $remaining = self::lockRemainingSeconds($user);

            if ($remaining > 0) {
                return [
                    'ok' => false,
                    'message' => '尝试次数过多，请等待 ' . (int) ceil($remaining / 60) . ' 分钟后再试',
                    'user' => null,
                ];
            }
        }

        // ② 密码不对。注意「邮箱不存在」与「密码错」给**同一句提示**，
        //    否则这个接口就成了一个免费的账号枚举工具
        if ($user === null || !password_verify($password, (string) $user['password_hash'])) {
            if ($user !== null) {
                self::recordLoginFailure($user, $ip);
            }

            return ['ok' => false, 'message' => '邮箱或密码不正确', 'user' => null];
        }

        if ((int) $user['status'] !== self::STATUS_ENABLED) {
            return ['ok' => false, 'message' => '该账号已被停用，请联系站长', 'user' => null];
        }

        if (($user['email_verified_at'] ?? null) === null) {
            return ['ok' => false, 'message' => '邮箱尚未验证，请先点击验证邮件里的链接', 'user' => null];
        }

        // ③ 成功：清零失败计数与锁定，再记录本次登录
        $now = time();
        Db::execute(
            'UPDATE users
             SET failed_attempts = 0, locked_until = NULL,
                 last_login_at = ?, last_login_ip = ?, updated_at = ?
             WHERE id = ?',
            [$now, mb_substr((string) $ip, 0, 45), $now, (int) $user['id']]
        );

        return ['ok' => true, 'message' => '', 'user' => $user];
    }

    /** 锁定剩余秒数（未锁定返回 0） */
    private static function lockRemainingSeconds(array $user): int
    {
        $until = (int) ($user['locked_until'] ?? 0);

        return $until > time() ? $until - time() : 0;
    }

    /**
     * 记录一次登录失败；达到阈值则锁定。
     *
     * ⚠️ 日志里只记邮箱、次数与来源 IP，**绝不记录用户尝试的密码内容** ——
     * 日志文件是运维最容易随手翻看、最容易被整个打包带走的东西。
     */
    private static function recordLoginFailure(array $user, ?string $ip): void
    {
        $failed = (int) ($user['failed_attempts'] ?? 0) + 1;
        $now = time();

        $threshold = max(1, Settings::int('security.user_login_max_attempts', 10));
        $lockSeconds = max(1, Settings::int('security.user_login_lock_minutes', 15)) * 60;

        if ($failed >= $threshold) {
            Db::execute(
                'UPDATE users SET failed_attempts = ?, locked_until = ?, updated_at = ? WHERE id = ?',
                [$failed, $now + $lockSeconds, $now, (int) $user['id']]
            );

            \support\Log::warning(sprintf(
                '用户登录连续失败 %d 次，已锁定 %d 分钟：%s，来源 IP：%s',
                $failed,
                (int) ($lockSeconds / 60),
                (string) $user['email'],
                (string) $ip
            ));

            return;
        }

        Db::execute(
            'UPDATE users SET failed_attempts = ?, updated_at = ? WHERE id = ?',
            [$failed, $now, (int) $user['id']]
        );

        \support\Log::warning(sprintf(
            '用户登录失败（第 %d 次）：%s，来源 IP：%s',
            $failed,
            (string) $user['email'],
            (string) $ip
        ));
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

    // ═══════════════════════════════════════════════════════════
    // 用户编号：自助注销、连续编号
    // ═══════════════════════════════════════════════════════════

    /** 会话中记录「登录时的编号代际」的键 */
    public const SESSION_EPOCH_KEY = 'user_id_epoch';

    /** 引用了用户编号的表 —— 注销与重排都必须一起处理，少一张就会留下指向空号的脏数据 */
    private const USER_ID_TABLES = ['tokens', 'payment_orders', 'usage_logs'];

    /**
     * 当前编号代际。
     *
     * 每次重排（compactIds）都会 +1。会话里记着登录当时的代际，
     * 对不上就视为登录失效 —— 这是「重排」唯一的安全阀：
     * 编号 5 在重排后可能已经是另一个人，若旧会话继续用它，
     * 就会出现「你登录着，看到的却是别人的余额与令牌」这种最严重的事故。
     */
    public static function idEpoch(): int
    {
        return Settings::int('users.id_epoch', 0);
    }

    /**
     * 用户自助注销账号。
     *
     * 处理策略（每一项都有理由）：
     *   · **令牌全删**：留着等于给已注销的账号继续开门
     *   · **订单与用量记录保留**，但把归属清空 —— 这些是账目与审计，
     *     删掉会让「这个月收了多少」再也对不上；清空归属后不再指向任何人
     *   · 用户行本身**真删**（而不是标记停用），这样编号会出现空位，
     *     随后由 compactIds 补齐 —— 用户看到的编号因此保持连续
     */
    public static function deleteAccount(int $id): bool
    {
        if (self::find($id) === null) {
            return false;
        }

        $pdo = Db::pdo();
        $pdo->beginTransaction();
        try {
            Db::execute('DELETE FROM tokens WHERE user_id = ?', [$id]);
            // user_id 列是 NOT NULL，所以用 0 表示「原主人已注销」
            Db::execute('UPDATE payment_orders SET user_id = 0 WHERE user_id = ?', [$id]);
            Db::execute('UPDATE usage_logs SET user_id = NULL WHERE user_id = ?', [$id]);
            Db::execute('DELETE FROM users WHERE id = ?', [$id]);
            $pdo->commit();
        } catch (Throwable $e) {
            if ($pdo->inTransaction()) {
                $pdo->rollBack();
            }
            throw $e;
        }

        return true;
    }

    /** 用户总数与最大编号（判断「有没有空位」的唯一依据） */
    public static function selectCountAndMax(): array
    {
        $row = Db::selectOne('SELECT COUNT(*) AS c, MAX(id) AS m FROM users');

        return [
            'total' => (int) ($row['c'] ?? 0),
            'max' => (int) ($row['m'] ?? 0),
        ];
    }

    /** 编号是否需要补位（有空位才需要，没空位就什么都不做） */
    public static function needsCompact(): bool
    {
        $row = self::selectCountAndMax();

        return $row['total'] > 0 && $row['max'] !== $row['total'];
    }

    /**
     * 现在适合重排编号吗？
     *
     * 为什么需要这个判断：重排会把「编号 5」从 A 变成 B。
     * 若此刻正好有请求在飞（它已经拿到了编号 5，正要去扣费/写用量），
     * 这次扣费就会落到 B 的账上 —— 这是钱的问题，不能靠概率赌。
     *
     * 判据取「最近一次真实调用距今是否超过 5 分钟」：
     * 没有在途流量时才动手。判不出来（从未有过调用）视为安全。
     */
    public static function canCompactNow(int $quietSeconds = 300): bool
    {
        $row = Db::selectOne('SELECT MAX(created_at) AS t FROM usage_logs');
        $last = (int) ($row['t'] ?? 0);

        return $last === 0 || (time() - $last) > $quietSeconds;
    }

    /**
     * 把用户编号重排成连续的 1..N（按现有编号从早到晚）。
     *
     * 为什么要做：注销会留下空位（1、3、5…）。空位本身不致命，
     * 但站长与用户都会拿编号当「第几个用户」看，空位会让人以为系统丢了数据。
     *
     * 安全要点：
     *   · 全程一个事务，失败整体回滚
     *   · **按编号升序**逐个搬：新编号一定 ≤ 旧编号，且升序处理时
     *     每次的目标编号刚好是上一步腾出来的，不会撞主键
     *   · 搬完把自增序列重置到 N，否则下一个新用户又会拿到 N+2 之类的新空位
     *   · 代际 +1，让所有旧会话失效（见 idEpoch 的说明）
     *
     * @return array{changed:bool, total:int, moved:int}
     */
    public static function compactIds(): array
    {
        $ids = array_map(
            static fn (array $r): int => (int) $r['id'],
            Db::select('SELECT id FROM users ORDER BY id ASC')
        );

        $mapping = [];
        $changed = false;
        foreach ($ids as $i => $old) {
            $new = $i + 1;
            $mapping[$old] = $new;
            if ($new !== $old) {
                $changed = true;
            }
        }

        if (!$changed) {
            return ['changed' => false, 'total' => count($ids), 'moved' => 0];
        }

        $pdo = Db::pdo();
        $pdo->beginTransaction();
        try {
            $moved = 0;
            foreach ($mapping as $old => $new) {
                if ($old === $new) {
                    continue;
                }
                foreach (self::USER_ID_TABLES as $table) {
                    Db::execute("UPDATE {$table} SET user_id = ? WHERE user_id = ?", [$new, $old]);
                }
                Db::execute('UPDATE users SET id = ? WHERE id = ?', [$new, $old]);
                $moved++;
            }

            $pdo->commit();
        } catch (Throwable $e) {
            if ($pdo->inTransaction()) {
                $pdo->rollBack();
            }
            throw $e;
        }

        // 自增序列**必须放在事务之外**：MySQL 里 ALTER TABLE 会隐式提交，
        // 写在事务内会让上面的搬移失去「要么全成、要么全不成」的保证
        self::resetIdSequence(count($ids));

        Settings::put('users.id_epoch', self::idEpoch() + 1);
        Settings::put('users.last_compacted_at', time());

        return ['changed' => true, 'total' => count($ids), 'moved' => $moved];
    }

    /**
     * 把用户表的自增计数重置为「当前最大编号」。
     *
     * 不重置的话，下一个新用户会从历史上的最大编号继续往后拿（例如 21、22），
     * 空位又出现了 —— 补位就成了每三天做一次的徒劳动作。
     */
    private static function resetIdSequence(int $max): void
    {
        if (Db::isSqlite()) {
            // SQLite 的自增计数在 sqlite_sequence 里，且只有用过 AUTOINCREMENT 的表才有这一行
            Db::execute("UPDATE sqlite_sequence SET seq = ? WHERE name = 'users'", [$max]);

            return;
        }

        // MySQL：改表定义里的 AUTO_INCREMENT。值必须内联（不能用占位符）
        Db::pdo()->exec('ALTER TABLE users AUTO_INCREMENT = ' . max(1, $max + 1));
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
