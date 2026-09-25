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
 * 明文在创建那一刻一定返回给用户看一次；之后还能不能再看到，
 * 取决于站长的开关 `security.token_reveal`（见 revealEnabled 的说明）。
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

use support\Log;
use Throwable;

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
     * 生成一个新的令牌明文（创建时返回给用户，之后能否再看取决于回显开关）。
     */
    public static function generate(): string
    {
        return self::PREFIX . bin2hex(random_bytes(24));
    }

    /**
     * 是否允许「随时复制令牌」。
     *
     * 开着时：新建令牌会额外存一份**可逆**副本（AES-256-GCM），控制台上可以直接复制；
     * 关掉时：只存哈希，令牌除了创建那一刻再也拿不到（回到原来的口径）。
     *
     * ⚠️ 这里同时要求 APP_KEY 已配置：没有 APP_KEY 就没法加密，
     * 此时**不能**假装开着（否则用户会看到一堆「点了没反应」的复制按钮），
     * 而是如实降级成「不保存副本」。
     */
    public static function revealEnabled(): bool
    {
        return Settings::bool('security.token_reveal', true) && Crypto::isConfigured();
    }

    /**
     * 取某个令牌的可回显明文。
     *
     * 取不到（开关关着、老令牌、APP_KEY 换过导致解不开）一律返回空串 ——
     * 调用方据此显示「无法回显」的说明，而不是抛错让整页打不开。
     */
    public static function plainOf(array $token): string
    {
        $enc = trim((string) ($token['key_enc'] ?? ''));
        if ($enc === '' || !self::revealEnabled()) {
            return '';
        }

        try {
            return Crypto::decrypt($enc);
        } catch (Throwable $e) {
            // 解密失败最常见的原因是 APP_KEY 被换过。记一条日志便于排查，
            // 但不影响页面：令牌本身（哈希）仍然能用
            Log::warning('令牌副本解密失败（可能换过 APP_KEY）：token_id=' . (int) ($token['id'] ?? 0)
                . ' —— ' . $e->getMessage());

            return '';
        }
    }

    /**
     * 清掉所有已保存的令牌副本（把「可回显」这件事彻底收回）。
     *
     * 关掉开关时用它：开关只管「以后还存不存」，
     * 已经存下来的明文不会凭空消失，需要站长显式清一次。
     *
     * @return int 被清除的条数
     */
    public static function purgePlaintext(): int
    {
        return Db::execute('UPDATE tokens SET key_enc = NULL WHERE key_enc IS NOT NULL');
    }

    /**
     * 还有多少把令牌保存着可回显副本（后台显示用）。
     */
    public static function countRevealable(): int
    {
        $row = Db::selectOne('SELECT COUNT(*) AS c FROM tokens WHERE key_enc IS NOT NULL');

        return (int) ($row['c'] ?? 0);
    }

    public static function hash(string $plain): string
    {
        return hash('sha256', self::HASH_DOMAIN . $plain);
    }

    /**
     * 掩码：`sk-aqua-1a2b…9f3a`。
     *
     * 没有可回显副本时列表页显示它（有副本则显示完整令牌 + 复制按钮）。
     * 无论哪种情况，日志与后台渠道密钥的展示一律用掩码。
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

        // 回显开关开着时额外存一份可逆副本（只用于展示，鉴权仍然走哈希）
        $keyEnc = null;
        if (self::revealEnabled()) {
            $keyEnc = Crypto::encrypt($plain);
        }

        Db::execute(
            'INSERT INTO tokens
                (user_id, name, key_hash, key_mask, key_enc, quota_limit, quota_used, models, expires_at, status, created_at, updated_at)
             VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)',
            [
                $userId,
                mb_substr(trim($name) === '' ? '默认令牌' : trim($name), 0, 64),
                self::hash($plain),
                self::mask($plain),
                $keyEnc,
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
     * ═══ 为什么失败原因要带上状态码 ═══
     *
     * 早先的实现把所有失败都回成 401 + `invalid_api_key`，
     * 于是「余额不足」也会显示成「API Key 错误」——
     * 用户拿着一个好端端的令牌反复检查、重发、怀疑是本站的问题，
     * 而真正要做的只是充值。这不是文案问题，是把人引到了错误的方向。
     * 现在按 OpenAI 的惯例区分：
     *   · 401 invalid_api_key      令牌本身有问题（缺失/无效/停用/过期）
     *   · 402 insufficient_balance 余额不足（令牌是好的）
     *   · 403 insufficient_quota   令牌额度用完 / 账号被停用
     *
     * @return array{ok:bool, reason:string, token:array<string,mixed>|null,
     *               user:array<string,mixed>|null, status:int, code:string, type:string}
     */
    public static function authorize(string $plain): array
    {
        $fail = static fn (
            string $reason,
            int $status = 401,
            string $code = 'invalid_api_key',
            string $type = 'invalid_request_error'
        ): array => [
            'ok' => false, 'reason' => $reason, 'token' => null, 'user' => null,
            'status' => $status, 'code' => $code, 'type' => $type,
        ];

        $token = self::findByPlain($plain);

        if ($token === null) {
            return $fail('令牌无效：请确认用的是本站令牌（以 sk-aqua- 开头），且复制时没有多余空格或换行');
        }

        $usable = self::usable($token);
        if (!$usable['ok']) {
            // 额度用尽属于「配额」而不是「密钥坏了」：换一把令牌或清额度就能解决
            $isQuota = $usable['reason'] === '令牌额度已用完';

            return $fail(
                $usable['reason'],
                $isQuota ? 403 : 401,
                $isQuota ? 'insufficient_quota' : 'invalid_api_key',
                $isQuota ? 'insufficient_quota' : 'invalid_request_error'
            );
        }

        $user = User::find((int) $token['user_id']);

        if ($user === null) {
            return $fail('令牌所属的用户不存在（账号可能已注销）');
        }

        if ((int) $user['status'] !== User::STATUS_ENABLED) {
            return $fail('账号已被停用，请联系站长', 403, 'account_disabled');
        }

        // ⚠️ 余额检查**不在这里**，而在调用方（OpenAiController::chatCompletions）。
        //
        // 原因：余额门槛只该拦住「要花钱的调用」。鉴权阶段还不知道用户要调哪个模型，
        // 若在这里一刀切，就会出现「全都调用免费模型、余额为 0、于是谁都调不通」——
        // 生产上就是这么炸的（27 个模型全是免费上游，28 个用户余额 0）。
        // 判定「这个模型收不收费」需要定价信息，因此挪到看得到模型的地方。

        return [
            'ok' => true, 'reason' => '', 'token' => $token, 'user' => $user,
            'status' => 200, 'code' => '', 'type' => '',
        ];
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
