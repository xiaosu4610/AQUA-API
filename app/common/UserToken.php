<?php
/**
 * 用户令牌（下游调用凭证）
 *
 * ═══ 为什么令牌只存哈希，而渠道 API Key 必须加密 ═══
 *
 * 这两者容易被混为一谈，但需求正好相反：
 *
 *   · **渠道 API Key**（上游）必须**可逆**：转发时要把原文放进请求头，
 *     所以用 AES-256-GCM 加密存储。
 *   · **用户令牌**（下游）必须**不可逆**：它是我们自己签发的，
 *     验证时只需 `hash(收到的令牌) === 库里的哈希`，**永不需要还原原文**。
 *     因此用 SHA-256 哈希存储 —— 库被拖走也无法直接拿去调用。
 *
 * 明文只在创建那一刻返回给用户看一次，之后再也拿不到。
 *
 * ═══ 哈希为什么要加盐/前缀 ═══
 *
 * 这里用「领域前缀 + SHA-256」而不是裸 SHA-256：
 * 前缀（`aqua-user-token:`）保证同一个字符串在别的用途上算出不同哈希，
 * 避免「某天另一个功能也存 SHA-256，两边的值可以互相对照」这类串用。
 * 不用 bcrypt 是因为令牌本身是高熵随机串，不需要抗暴力破解的慢哈希 ——
 * 用慢哈希反而会给每个请求增加几十毫秒的验证成本。
 */

declare(strict_types=1);

namespace app\common;

final class UserToken
{
    public const STATUS_ENABLED = 1;
    public const STATUS_DISABLED = 0;

    /**
     * 令牌前缀。
     * 带上可读前缀是为了让人一眼认出「这是本项目的令牌」，
     * 便于在日志脱敏、GitHub 泄露扫描这类场景里被识别出来。
     */
    public const PREFIX = 'sk-aqua-';

    /** 领域前缀，见文件头说明 */
    private const HASH_DOMAIN = 'aqua-user-token:';

    /**
     * 生成一个新的令牌明文（只在创建时出现这一次）。
     */
    public static function generate(): string
    {
        return self::PREFIX . bin2hex(random_bytes(24));
    }

    public static function hash(string $plain): string
    {
        return hash('sha256', self::HASH_DOMAIN . $plain);
    }

    /**
     * 掩码：`sk-aqua-1a2b…9f3a`。
     * 列表页只显示它，绝不显示完整令牌。
     */
    public static function mask(string $plain): string
    {
        if (mb_strlen($plain) <= 18) {
            return 'sk-…';
        }

        return mb_substr($plain, 0, 12) . '…' . mb_substr($plain, -4);
    }

    /**
     * 创建令牌。
     *
     * @param float       $quotaLimit 额度上限，0 表示不限
     * @param string|null $models     逗号分隔的模型白名单；null/空 表示不限
     * @param int|null    $expiresAt  过期时间戳；null 表示不过期
     * @return array{id:int, plain:string}
     */
    public static function create(
        int $userId,
        string $name,
        float $quotaLimit = 0.0,
        ?string $models = null,
        ?int $expiresAt = null
    ): array {
        $plain = self::generate();
        $now = time();

        Db::execute(
            'INSERT INTO tokens
                (user_id, name, key_hash, key_mask, quota_limit, quota_used, models, expires_at, status, created_at, updated_at)
             VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)',
            [
                $userId,
                mb_substr(trim($name) === '' ? '默认令牌' : trim($name), 0, 64),
                self::hash($plain),
                self::mask($plain),
                self::dec(max(0.0, $quotaLimit)),
                $models === null || trim($models) === '' ? null : mb_substr(trim($models), 0, 1024),
                $expiresAt,
                self::STATUS_ENABLED,
                $now,
                $now,
            ]
        );

        return ['id' => (int) Db::pdo()->lastInsertId(), 'plain' => $plain];
    }

    /**
     * 按明文令牌查（转发鉴权时使用）。
     *
     * @return array<string, mixed>|null
     */
    public static function findByPlain(string $plain): ?array
    {
        if ($plain === '' || !str_starts_with($plain, self::PREFIX)) {
            return null;
        }

        return Db::selectOne('SELECT * FROM tokens WHERE key_hash = ?', [self::hash($plain)]);
    }

    /** @return array<string, mixed>|null */
    public static function find(int $id): ?array
    {
        return Db::selectOne('SELECT * FROM tokens WHERE id = ?', [$id]);
    }

    /**
     * 某个用户的全部令牌（按创建时间倒序）。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function allForUser(int $userId): array
    {
        return Db::select(
            'SELECT * FROM tokens WHERE user_id = ? ORDER BY id DESC',
            [$userId]
        );
    }

    /** 令牌是否属于该用户（越权操作的第一道防线） */
    public static function belongsTo(int $tokenId, int $userId): bool
    {
        $row = Db::selectOne('SELECT id FROM tokens WHERE id = ? AND user_id = ?', [$tokenId, $userId]);

        return $row !== null;
    }

    public static function setStatus(int $id, int $status): void
    {
        Db::execute('UPDATE tokens SET status = ?, updated_at = ? WHERE id = ?', [$status, time(), $id]);
    }

    public static function delete(int $id): void
    {
        Db::execute('DELETE FROM tokens WHERE id = ?', [$id]);
    }

    public static function deleteForUser(int $userId): void
    {
        Db::execute('DELETE FROM tokens WHERE user_id = ?', [$userId]);
    }

    /**
     * 鉴权：给定令牌明文，判断**这次调用是否允许**。
     *
     * 这是转发引擎唯一需要调用的入口，把「令牌能不能用」「用户能不能用」
     * 「余额够不够」三件事合在一处判断。分在三处写会漏 ——
     * 最容易漏的就是「令牌有效但用户已被停用」这一条：
     * 站长停用了某个用户，如果只校验令牌，那个用户照旧能用。
     *
     * @return array{ok:bool, reason:string, token:array<string,mixed>|null, user:array<string,mixed>|null}
     */
    public static function authorize(string $plain): array
    {
        $fail = static fn (string $reason): array => [
            'ok' => false, 'reason' => $reason, 'token' => null, 'user' => null,
        ];

        $token = self::findByPlain($plain);

        if ($token === null) {
            return $fail('令牌无效');
        }

        $usable = self::usable($token);
        if (!$usable['ok']) {
            return $fail($usable['reason']);
        }

        $user = User::find((int) $token['user_id']);

        if ($user === null) {
            return $fail('令牌所属的用户不存在');
        }

        if ((int) $user['status'] !== User::STATUS_ENABLED) {
            return $fail('账号已被停用，请联系站长');
        }

        // 是否允许「余额为 0 也放行」由站长决定：
        // 商业站必须拒绝（否则等于免费送额度），公益站可以放行
        if (Settings::bool('billing.require_balance', true) && (float) $user['balance'] <= 0) {
            return $fail('余额不足，请先充值');
        }

        return ['ok' => true, 'reason' => '', 'token' => $token, 'user' => $user];
    }

    /**
     * 令牌当前是否可用（状态 + 有效期 + 额度）。
     *
     * 集中在一个方法里判断，是为了避免「转发时只看了状态、
     * 忘了看有效期」这类各处判断不一致的问题。
     *
     * @return array{ok:bool, reason:string}
     */
    public static function usable(array $token): array
    {
        if ((int) $token['status'] !== self::STATUS_ENABLED) {
            return ['ok' => false, 'reason' => '令牌已被停用'];
        }

        if (self::isExpired($token)) {
            return ['ok' => false, 'reason' => '令牌已过期'];
        }

        $limit = (float) $token['quota_limit'];
        if ($limit > 0 && (float) $token['quota_used'] >= $limit) {
            return ['ok' => false, 'reason' => '令牌额度已用完'];
        }

        return ['ok' => true, 'reason' => ''];
    }

    public static function isExpired(array $token): bool
    {
        $expires = $token['expires_at'] ?? null;

        return $expires !== null && $expires !== '' && (int) $expires <= time();
    }

    /**
     * 模型是否在该令牌的白名单内。
     * 未配置白名单（NULL 或空串）表示不限模型。
     */
    public static function allowsModel(array $token, string $model): bool
    {
        $list = trim((string) ($token['models'] ?? ''));
        if ($list === '') {
            return true;
        }

        foreach (preg_split('/[\s,]+/', $list) ?: [] as $item) {
            $item = trim($item);
            if ($item === '') {
                continue;
            }

            // 支持末尾通配：`gpt-4*` 匹配 gpt-4o、gpt-4-turbo
            if (str_ends_with($item, '*')) {
                if (str_starts_with($model, rtrim($item, '*'))) {
                    return true;
                }
                continue;
            }

            if ($item === $model) {
                return true;
            }
        }

        return false;
    }

    /**
     * 扣减令牌额度。
     *
     * 与用户余额同理：条件写在 UPDATE 里，由数据库保证「够才扣」。
     * quota_limit 为 0（不限）时只累加用量，不做上限判断。
     *
     * @return bool 是否扣成功（额度不足返回 false）
     */
    public static function charge(int $id, float $amount): bool
    {
        if ($amount <= 0) {
            return true;
        }

        $affected = Db::execute(
            'UPDATE tokens
             SET quota_used = quota_used + ?, last_used_at = ?, updated_at = ?
             WHERE id = ?
               AND status = ?
               AND (quota_limit <= 0 OR quota_used + ? <= quota_limit)
               AND (expires_at IS NULL OR expires_at > ?)',
            [self::dec($amount), time(), time(), $id, self::STATUS_ENABLED, self::dec($amount), time()]
        );

        return $affected === 1;
    }

    /** 记录一次使用（不扣额度，仅更新时间戳） */
    public static function touch(int $id): void
    {
        Db::execute('UPDATE tokens SET last_used_at = ? WHERE id = ?', [time(), $id]);
    }

    /** 重置某令牌的已用额度 */
    public static function resetQuota(int $id): void
    {
        Db::execute('UPDATE tokens SET quota_used = 0, updated_at = ? WHERE id = ?', [time(), $id]);
    }

    /**
     * 额度的人类可读描述：`已用 1.23 / 10` 或 `已用 1.23（不限）`。
     */
    public static function quotaLabel(array $token): string
    {
        $used = User::money($token['quota_used'] ?? 0);
        $limit = (float) ($token['quota_limit'] ?? 0);

        return $limit > 0
            ? '已用 ' . $used . ' / ' . User::money($limit)
            : '已用 ' . $used . '（不限）';
    }

    private static function dec(float $value): string
    {
        return number_format($value, 10, '.', '');
    }
}
