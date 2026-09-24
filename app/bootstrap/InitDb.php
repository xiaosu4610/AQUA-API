<?php
/**
 * 启动引导：确保数据库就绪
 *
 * 在 Webman 启动时执行（见 config/bootstrap.php 的注册），
 * 每个工作进程会各执行一次，因此这里的操作必须**幂等**。
 *
 * 做两件事：
 *   1. 建表（Schema::ensure，内部用 CREATE TABLE IF NOT EXISTS，重复执行无害）
 *   2. 首次初始化管理员密码
 */

declare(strict_types=1);

namespace app\bootstrap;

use app\common\Admin;
use app\common\Schema;
use support\Log;
use Throwable;
use Webman\Bootstrap;
use Workerman\Worker;

class InitDb implements Bootstrap
{
    public static function start(?Worker $worker = null): void
    {
        try {
            Schema::ensure();
            self::ensureAdmin();
        } catch (Throwable $e) {
            // 初始化失败不能让整个进程崩溃 ——
            // 否则一旦数据库配置有误会变成「服务起不来」，
            // 连 /healthz 都无法访问，排查时容易误判为网络问题。
            // 这里记日志并继续启动，具体问题由访问后台时的报错暴露。
            Log::error('数据库初始化失败：' . $e->getMessage());
        }
    }

    /**
     * 首次初始化管理员密码。
     *
     * 密码来源是 .env 的 ADMIN_PASSWORD —— 刻意不写死在代码里，
     * 因为本项目是开源的，任何硬编码进代码的默认密码都等同于公开密码。
     *
     * 这个动作**只在管理员还没创建时执行一次**。
     * 之后修改密码请走后台的「修改密码」功能；
     * 再改 .env 里的 ADMIN_PASSWORD 不会覆盖已存在的密码。
     */
    private static function ensureAdmin(): void
    {
        if (Admin::find() !== null) {
            return;
        }

        $password = (string) (getenv('ADMIN_PASSWORD') ?: '');

        if ($password === '') {
            Log::warning(
                '后台管理员尚未初始化：请在 .env 中设置 ADMIN_PASSWORD，'
                . '然后执行 systemctl restart aqua-api。'
                . '（为避免把默认密码写死进开源代码，此处不提供默认值）'
            );

            return;
        }

        Admin::setPassword($password);
        Log::info('已根据 .env 中的 ADMIN_PASSWORD 完成后台管理员初始化');
    }
}
