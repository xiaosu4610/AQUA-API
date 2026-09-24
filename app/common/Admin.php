<?php
/**
 * 管理员账号
 *
 * 本项目的后台是「超级管理员后台」，只有一个管理员，
 * 登录**只需要密码、不需要用户名**，所以这张表只存一行（id = 1）。
 *
 * 安全设计（这块必须在第一版就做对，后台是暴露在公网的入口）：
 *   1. 密码**只存哈希**（password_hash），任何时候都不存明文、不写日志；
 *   2. **失败锁定**：连续失败达到阈值后锁定一段时间。
 *      纯密码登录没有用户名做区分，攻击者可以直接对唯一入口撞库，
 *      没有锁定机制等于把门敞开；
 *   3. 登录成功/失败都记录来源 IP，便于事后追查。
 *
 * 时间字段说明：全部用 Unix 时间戳（整数），不用数据库的 DATETIME /
 * CURRENT_TIMESTAMP —— 原因是数据库的 CURRENT_TIMESTAMP 语义不一致
 * （SQLite 恒为 UTC），与 PHP 侧的本地时区混用会产生极难排查的偏差。
 * 详见 app/common/Schema.php 顶部第 3 条约定。
 */

declare(strict_types=1);

namespace app\common;

use support\Log;

final class Admin
{
    /**
     * 连续失败多少次后锁定。
     *
     * 不再是写死的常量，而是可配置项 —— 站长可以在后台调。
     * 但**读取时机是「每次判定时」而不是「类加载时」**，
     * 因为常驻内存下类只会加载一次，写成常量就永远读不到后台的修改。
     */
    private static function maxFailedAttempts(): int
    {
        $value = Settings::int('security.login_max_attempts', 5);

        // 夹到合理区间：0 会让「锁定」彻底失效（等于关掉防爆破），
        // 过大的值又等于没有保护，都不是站长真正想要的
        return max(1, min(100, $value));
    }

    /** 锁定时长（秒），同样来自可配置项 */
    private static function lockSeconds(): int
    {
        $minutes = Settings::int('security.login_lock_minutes', 15);

        return max(1, min(10080, $minutes)) * 60;
    }

    /**
     * 读取管理员记录（不存在返回 null）。
     *
     * @return array<string, mixed>|null
     */
    public static function find(): ?array
    {
        return Db::selectOne('SELECT * FROM admins WHERE id = 1');
    }

    /**
     * 设置（或重置）管理员密码。
     *
     * 只在两种情况下调用：
     *   · 首次初始化（表里还没有管理员）；
     *   · 后续「重置密码」功能。
     * 日常修改密码应当走「校验旧密码 → 写入新密码」的流程，而不是这个方法。
     */
    public static function setPassword(string $plainPassword): void
    {
        $hash = password_hash($plainPassword, PASSWORD_DEFAULT);
        $now = time();

        $exists = Db::selectOne('SELECT id FROM admins WHERE id = 1');
        if ($exists) {
            Db::execute(
                'UPDATE admins SET password_hash = ?, failed_attempts = 0, locked_until = NULL, updated_at = ? WHERE id = 1',
                [$hash, $now]
            );

            return;
        }

        Db::execute(
            'INSERT INTO admins (id, password_hash, failed_attempts, created_at, updated_at) VALUES (1, ?, 0, ?, ?)',
            [$hash, $now, $now]
        );
    }

    /**
     * 当前是否处于锁定状态。
     *
     * @param array<string, mixed> $admin 管理员记录
     * @return int 剩余锁定秒数；0 表示未锁定
     */
    public static function lockRemainingSeconds(array $admin): int
    {
        $until = $admin['locked_until'] ?? null;
        if ($until === null || $until === '') {
            return 0;
        }

        $remaining = (int) $until - time();

        return $remaining > 0 ? $remaining : 0;
    }

    /**
     * 校验密码。
     *
     * 返回结构化结果而不是抛异常，因为调用方需要区分
     * 「密码错」和「被锁定」—— 两者要给用户完全不同的提示。
     *
     * @return array{ok: bool, message: string, locked_seconds: int}
     */
    public static function attempt(string $plainPassword, string $ip): array
    {
        $admin = self::find();

        // 尚未初始化管理员（.env 里没配 ADMIN_PASSWORD）
        if ($admin === null) {
            return [
                'ok' => false,
                'message' => '后台尚未初始化，请在服务器的 .env 中设置 ADMIN_PASSWORD 后重启服务',
                'locked_seconds' => 0,
            ];
        }

        // 锁定中：直接拒绝，不再校验密码（否则等于锁定可被绕过）
        $locked = self::lockRemainingSeconds($admin);
        if ($locked > 0) {
            return [
                'ok' => false,
                'message' => '尝试次数过多，请等待 ' . (int) ceil($locked / 60) . ' 分钟后再试',
                'locked_seconds' => $locked,
            ];
        }

        if ($plainPassword === '' || !password_verify($plainPassword, (string) $admin['password_hash'])) {
            self::recordFailure($admin, $ip);

            return [
                'ok' => false,
                'message' => '密码错误',
                'locked_seconds' => 0,
            ];
        }

        // 成功：清零失败计数并记录本次登录
        $now = time();
        Db::execute(
            'UPDATE admins SET failed_attempts = 0, locked_until = NULL, last_login_at = ?, last_login_ip = ?, updated_at = ? WHERE id = 1',
            [$now, $ip, $now]
        );

        return ['ok' => true, 'message' => '', 'locked_seconds' => 0];
    }

    /**
     * 记录一次登录失败；达到阈值则锁定。
     *
     * @param array<string, mixed> $admin 当前的管理员记录（用于读取已失败次数）
     */
    private static function recordFailure(array $admin, string $ip): void
    {
        $failed = (int) $admin['failed_attempts'] + 1;
        $now = time();

        if ($failed >= self::maxFailedAttempts()) {
            $lockSeconds = self::lockSeconds();
            $lockUntil = $now + $lockSeconds;
            Db::execute(
                'UPDATE admins SET failed_attempts = ?, locked_until = ?, updated_at = ? WHERE id = 1',
                [$failed, $lockUntil, $now]
            );

            // 锁定属于安全事件，必须留痕。
            // 注意：日志里只记录 IP 与次数，**绝不记录尝试的密码内容**。
            Log::warning(sprintf(
                '后台登录连续失败 %d 次，已锁定 %d 分钟，来源 IP：%s',
                $failed,
                (int) ($lockSeconds / 60),
                $ip
            ));

            return;
        }

        Db::execute(
            'UPDATE admins SET failed_attempts = ?, updated_at = ? WHERE id = 1',
            [$failed, $now]
        );

        Log::warning("后台登录失败（第 {$failed} 次），来源 IP：{$ip}");
    }
}
