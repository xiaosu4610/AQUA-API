<?php
/**
 * 表结构定义与初始化
 *
 * 三条必须遵守的约定：
 *
 * 1. **幂等**：本文件里的每一条 DDL 都必须能重复执行而不报错。
 *    因为服务有多个工作进程，它们会各自调用一次 ensure()，
 *    谁先谁后不可预期。用 `CREATE TABLE IF NOT EXISTS` 天然满足这一点。
 *
 * 2. **跨库可移植**：默认库是 SQLite，但用户可以换成 MySQL，
 *    所以 DDL 不能使用任一方的专有语法。具体避免：
 *      · 不用 SQLite 的 `AUTOINCREMENT`，也不用 MySQL 的 `AUTO_INCREMENT`
 *        （需要自增列时统一走 autoId() 按方言生成）
 *      · 不用 MySQL 的排序规则声明（如 utf8mb4_0900_ai_ci）
 *      · 字符串主键必须显式给长度（MySQL 的索引对长度有限制，SQLite 无所谓）
 *
 * 3. **时间一律用 INTEGER 存 Unix 时间戳，不用 DATETIME**。
 *    原因是一个实际踩到过的坑：
 *    SQLite 的 `CURRENT_TIMESTAMP` 恒为 **UTC**，而 PHP 端按 .env 设置的
 *    Asia/Shanghai 显示，两者差了 8 小时 —— 于是「上次登录时间」看起来就是错的。
 *    更麻烦的是锁定窗口、计费周期这类逻辑，一旦时区混用，
 *    会出现「明明锁了却还能登」「账单时间对不上」这种极难排查的问题。
 *    改用 Unix 时间戳后：存储无时区歧义、跨库一致、比较与计算都简单，
 *    只在**展示时**才格式化成人类可读的本地时间。
 *
 * 版本管理：当前版本号存在 options 表的 schema_version 键里，
 * 后续加表/改表时按版本号递增执行，避免每次启动都跑全量 DDL。
 */

declare(strict_types=1);

namespace app\common;

final class Schema
{
    /** 当前期望的表结构版本。新增迁移时递增。 */
    public const VERSION = 1;

    /**
     * 确保表结构存在且为最新版本。
     *
     * 由启动引导（app/bootstrap/InitDb.php）调用，每个工作进程启动时执行一次。
     */
    public static function ensure(): void
    {
        $current = (int) (Settings::raw('schema_version') ?? 0);

        if ($current >= self::VERSION) {
            return;
        }

        // v1：基础表
        if ($current < 1) {
            self::createV1();
        }

        Settings::put('schema_version', (string) self::VERSION);
    }

    /**
     * v1：配置表 + 管理员表
     */
    private static function createV1(): void
    {
        $pdo = Db::pdo();

        // ── options：配置中心 ─────────────────────────────────
        // 本项目的一个核心设计：**尽量把可调项放数据库**，
        // 这样管理后台可以随时修改并立即生效，不需要改代码、不需要重启。
        //
        // 值统一以 JSON 编码后存入 opt_value，
        // 好处是能保留类型（布尔、整数、数组），读出来不用猜是字符串还是数字。
        //
        // 注意：数据库连接串、APP_KEY 这类「引导配置」不能放这里 ——
        // 因为读它本身就需要先连上数据库，属于先有鸡还是先有蛋。
        // 这类配置留在 .env（见 .env.example 的说明）。
        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS options (
                opt_key    VARCHAR(191) NOT NULL,
                opt_value  TEXT         NULL,
                updated_at INTEGER      NOT NULL,
                PRIMARY KEY (opt_key)
            )
        SQL);

        // ── admins：超级管理员 ────────────────────────────────
        // 本项目的后台是「超级管理员后台」，登录**只需要密码、不需要用户名**，
        // 因此这张表只保留一行（id = 1），不设用户名/邮箱等字段。
        //
        // password_hash 存的是 password_hash() 的结果，绝不存明文。
        // failed_attempts / locked_until 用于防暴力破解 —— 后台暴露在公网，
        // 没有失败锁定的话，纯密码登录很容易被撞库。
        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS admins (
                id              INTEGER      NOT NULL,
                password_hash   VARCHAR(255) NOT NULL,
                last_login_at   INTEGER      NULL,
                last_login_ip   VARCHAR(45)  NULL,
                failed_attempts INTEGER      NOT NULL DEFAULT 0,
                locked_until    INTEGER      NULL,
                created_at      INTEGER      NOT NULL,
                updated_at      INTEGER      NOT NULL,
                PRIMARY KEY (id)
            )
        SQL);
    }

    /**
     * 生成自增主键列定义（按当前数据库方言）。
     *
     * 目前 v1 的表都用不到自增（options 用字符串主键、admins 固定 id=1），
     * 但后续的渠道、令牌、日志等表一定会用到，所以先把方言差异收敛在这一个方法里，
     * 避免以后再到处写 if (SQLite) ... else ...。
     */
    public static function autoId(): string
    {
        return Db::isSqlite()
            ? 'INTEGER PRIMARY KEY AUTOINCREMENT'
            : 'BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY';
    }
}
