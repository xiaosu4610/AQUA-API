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

use Throwable;

final class Schema
{
    /** 当前期望的表结构版本。新增迁移时递增。 */
    public const VERSION = 5;

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

        // 迁移必须**逐版本递进**、且每一步都可重复执行：
        // 老部署可能停在 v1，新部署从 0 开始，两者都要能安全升到最新。
        if ($current < 1) {
            self::createV1();
        }

        if ($current < 2) {
            self::createV2();
        }

        if ($current < 3) {
            self::createV3();
        }

        if ($current < 4) {
            self::createV4();
        }

        if ($current < 5) {
            self::createV5();
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
     * v2：渠道表（上游连接配置）
     *
     * 设计要点：
     *
     * 1. **上游 Key 加密存储**（api_key_enc）。
     *    这里存的是 app/common/Crypto.php 的输出，绝不是明文 ——
     *    因为 Key 必须能还原原文（转发时要放进 Authorization 头），
     *    所以用可逆加密而不是哈希。
     *
     * 2. **一个渠道可以对应多个模型**（models 字段，JSON 数组）。
     *    这是「渠道池」的基础：同一个上游 Key 往往能调用多个模型，
     *    路由时按「需要哪个模型」来筛可用渠道。将来还会引入
     *    (渠道 × 模型) 的独立健康状态，那时会再加一张索引表。
     *
     * 3. **priority + weight 而不是单一顺序**。
     *    priority 决定「先用谁」（数值大的优先），
     *    weight 决定「同一优先级内按什么比例分摊流量」。
     *    这两者分开，才能同时表达「主备」与「按比例负载」两种诉求。
     *
     * 4. **status 用整数而不是布尔**，为将来的多状态留余地
     *    （例如「正常 / 降级 / 已下线」三态，见项目文档的渠道状态机设计）。
     *
     * 5. **测活结果落库**（last_test_*）。
     *    站长最关心「这个 Key 还能不能用」，测试结果持久化后，
     *    列表页可以直接展示，不必每次都重新探测。
     */
    private static function createV2(): void
    {
        $pdo = Db::pdo();
        $autoId = self::autoId();

        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS channels (
                id           {$autoId},
                name         VARCHAR(128) NOT NULL,
                type         VARCHAR(32)  NOT NULL,
                base_url     VARCHAR(255) NOT NULL,
                api_key_enc  TEXT         NULL,
                models       TEXT         NULL,
                priority     INTEGER      NOT NULL DEFAULT 0,
                weight       INTEGER      NOT NULL DEFAULT 1,
                status       INTEGER      NOT NULL DEFAULT 1,
                rpm_limit    INTEGER      NOT NULL DEFAULT 0,
                last_test_at INTEGER      NULL,
                last_test_ok INTEGER      NULL,
                last_error   VARCHAR(500) NULL,
                created_at   INTEGER      NOT NULL,
                updated_at   INTEGER      NOT NULL
            )
        SQL);
    }

    /**
     * v3：渠道密钥池
     *
     * 一个渠道可以挂很多把 Key，轮流使用 —— 这是本项目支撑
     * 「数百把免费额度 Key 聚合出高并发」的核心表。
     *
     * 设计要点：
     *
     * 1. **key_hash 用于导入去重**。
     *    存 Key 的 SHA-256（不是明文）。用途只有一个：让导入操作**幂等** ——
     *    同一批 Key 重复导入不会产生重复记录。
     *    这不构成泄露风险：Key 本身是高熵随机串，哈希无法反推原文。
     *    约束是 UNIQUE(channel_id, key_hash)，即「同一个渠道内不重复」，
     *    不同渠道允许存在相同的 Key（例如测试渠道与生产渠道共用一把）。
     *
     * 2. **限流计数器落在数据库里**（window_start + used_requests）。
     *    为什么不放进程内存？因为服务有多个工作进程，
     *    每个进程各记一份的话，实际放行量会是限制值的数倍 ——
     *    对「每把 Key 每分钟 40 次」这种硬额度来说，超发就等于烧 Key。
     *    放在库里、并用**单条 UPDATE 完成「判断 + 占用」**，才能跨进程准确。
     *
     * 3. **status 与 fail_count 为健康治理留位**。
     *    将来做「连续失败就隔离、过一段时间再放出来试」时直接用这两列，
     *    不需要再改表。
     */
    private static function createV3(): void
    {
        $pdo = Db::pdo();
        $autoId = self::autoId();

        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS channel_keys (
                id            {$autoId},
                channel_id    INTEGER      NOT NULL,
                key_hash      VARCHAR(64)  NOT NULL,
                api_key_enc   TEXT         NOT NULL,
                status        INTEGER      NOT NULL DEFAULT 1,
                rpm_limit     INTEGER      NOT NULL DEFAULT 0,
                window_start  INTEGER      NOT NULL DEFAULT 0,
                used_requests INTEGER      NOT NULL DEFAULT 0,
                last_used_at  INTEGER      NULL,
                last_error    VARCHAR(500) NULL,
                fail_count    INTEGER      NOT NULL DEFAULT 0,
                created_at    INTEGER      NOT NULL,
                updated_at    INTEGER      NOT NULL,
                CONSTRAINT uq_channel_key UNIQUE (channel_id, key_hash)
            )
        SQL);
    }

    /**
     * v4：渠道级「高级配置」
     *
     * 为什么需要这一列：
     *   不同上游的差异远不止「地址不同」—— 鉴权方式（Bearer / api-key 头 /
     *   查询参数）、额外的查询参数（Azure 的 api-version）、额外的请求体参数、
     *   超时、代理、模型名映射，各家都不一样。
     *   如果为每一种差异都新增一个数据库列，列会无限膨胀；
     *   而如果写死在代码里，加一个新上游就要改代码 —— 这正是本项目
     *   要避免的「接口支持代码化」。
     *
     *   因此：**差异收敛成一个 JSON 列**。
     *   字段清单与默认值声明在 Channel::ADV_FIELDS（单一事实来源），
     *   表单据此渲染、请求构造据此取值，加字段只改那一处。
     *
     * 为什么允许 JSON 而不是更严格的结构：
     *   这是「用户可自由扩展」的逃生口。将来遇到一个谁都没想到的上游怪癖，
     *   站长可以自己填附加请求头/参数解决，不需要等我们发版。
     *
     * 注意：这里存的是**配置**，不是凭据 —— API Key 仍然单独加密存在
     * api_key_enc 里，绝不混进这一列。
     */
    private static function createV4(): void
    {
        self::addColumnIfMissing('channels', 'config', 'TEXT NULL');
    }

    /**
     * v5：密钥池的「冷却恢复」时间点
     *
     * 自治愈需要区分两种停用，而这两者原本都用 status=0 表示、分不出来：
     *
     *   · **永久停用**（凭据确实失效，401/403）—— 等再久也不会变好，
     *     只能人工换 Key。disabled_until 留 NULL 表示这一类。
     *   · **冷却停用**（连续瞬时失败，如上游抖动）—— 上游恢复后这把 Key
     *     其实是好的。设一个到期时间，到点自动放出来再试一次。
     *
     * 不加这列的话，站长每次上游抖一下就得手工把几十把 Key 一个个点回来 ——
     * 而这本可以是自动的。
     */
    private static function createV5(): void
    {
        self::addColumnIfMissing('channel_keys', 'disabled_until', 'INTEGER NULL');
    }

    /**
     * 给已有表加一列（幂等）。
     *
     * 为什么要先探测列是否存在：
     *   迁移要求可重复执行，而 SQLite / MySQL 都**没有**
     *   `ADD COLUMN IF NOT EXISTS` 这个通用语法。
     *   直接 ALTER 在列已存在时会报错，整段迁移就断了。
     *
     * 探测方式按方言分流：
     *   · SQLite  —— PRAGMA table_info
     *   · MySQL   —— information_schema.columns
     * 若该表压根不存在（理论上不会：createV2 已先于本步执行），
     * 则跳过 —— 宁可少加一列，也不要让整个启动流程在这里抛异常。
     */
    private static function addColumnIfMissing(string $table, string $column, string $definition): void
    {
        $pdo = Db::pdo();

        try {
            if (Db::isSqlite()) {
                $exists = false;
                foreach ($pdo->query("PRAGMA table_info({$table})") as $row) {
                    if (($row['name'] ?? '') === $column) {
                        $exists = true;
                        break;
                    }
                }
            } else {
                $row = Db::selectOne(
                    'SELECT COUNT(*) AS c FROM information_schema.columns
                     WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?',
                    [$table, $column]
                );
                $exists = ((int) ($row['c'] ?? 0)) > 0;
            }

            if ($exists) {
                return;
            }
        } catch (Throwable) {
            // 表不存在（全新库）—— 建表语句里已含该列，无需补
            return;
        }

        $pdo->exec("ALTER TABLE {$table} ADD COLUMN {$column} {$definition}");
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
