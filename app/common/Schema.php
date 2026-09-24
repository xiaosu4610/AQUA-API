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
    public const VERSION = 7;

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

        if ($current < 6) {
            self::createV6();
        }

        if ($current < 7) {
            self::createV7();
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
     * v6：模型定价 + 用量日志（成本计算的落点）
     *
     * ═══ 为什么上游成本和下游售价要分开存 ═══
     *
     * 「不是所有上游和所有下游都是免费的，也不是所有计费都跟官方一样」——
     * 这句话正是这张表的设计依据：
     *   · 上游（我们的支出）可能是官方原价、三方中转的折扣价、
     *     自建推理的零边际成本、订阅套餐的固定月费；
     *   · 下游（我们向用户收的）可能是上游成本乘一个倍率，
     *     也可能是完全独立的一套价格。
     * 把两者塞进一个「价格」字段，就没法回答「这个模型到底赚不赚钱」。
     *
     * ═══ 为什么要有 billing_mode ═══
     *
     * 因为**计费口径本身就不一样**，这不是同一套公式换个参数能表达的：
     *   · token        按输入/输出 token 分别计价（绝大多数对话模型）
     *   · call         按次计价（图像/视频/语音这类，与 token 无关）
     *   · subscription 订阅套餐（编程套餐、包月），边际成本为 0，
     *                  但仍要记 token 用于统计与限额
     *   · free         免费额度
     *
     * ═══ 为什么金额不用 FLOAT ═══
     *
     * 用 DECIMAL(20,10)：SQLite 与 MySQL 都支持这个写法，
     * 且能避免二进制浮点累加误差（0.1+0.2 这类问题在计费上是事故）。
     * PDO 读出来是字符串，在 PHP 侧转 float 计算、四舍五入后再落库。
     */
    private static function createV6(): void
    {
        $pdo = Db::pdo();
        $autoId = self::autoId();

        // ── pricing：模型定价（上游成本 + 下游售价）───────────
        // model 唯一：一个模型一条定价。这与「渠道」是两个维度 ——
        // 同一个模型可能同时接了好几家上游，成本取哪家由站长按主要线路填，
        // 逐渠道的差异则通过渠道高级配置里的模型名映射与上游种类区分。
        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS pricing (
                id                      {$autoId},
                model                   VARCHAR(191) NOT NULL,
                billing_mode            VARCHAR(16)  NOT NULL DEFAULT 'token',
                upstream_kind           VARCHAR(16)  NOT NULL DEFAULT 'official',
                upstream_input_price    DECIMAL(20,10) NOT NULL DEFAULT 0,
                upstream_output_price   DECIMAL(20,10) NOT NULL DEFAULT 0,
                upstream_call_price     DECIMAL(20,10) NOT NULL DEFAULT 0,
                downstream_input_price  DECIMAL(20,10) NOT NULL DEFAULT 0,
                downstream_output_price DECIMAL(20,10) NOT NULL DEFAULT 0,
                downstream_call_price   DECIMAL(20,10) NOT NULL DEFAULT 0,
                price_unit              INTEGER      NOT NULL DEFAULT 1000000,
                note                    VARCHAR(255) NULL,
                status                  INTEGER      NOT NULL DEFAULT 1,
                created_at              INTEGER      NOT NULL,
                updated_at              INTEGER      NOT NULL,
                CONSTRAINT uq_pricing_model UNIQUE (model)
            )
        SQL);

        // ── usage_logs：每次请求一条用量记录 ──────────────────
        // 这是「成本可见」的唯一来源。宁可多记字段，
        // 也不要事后发现「上个月的毛利算不出来」——
        // 日志类数据一旦缺失就无法补算。
        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS usage_logs (
                id                {$autoId},
                created_at        INTEGER      NOT NULL,
                user_id           INTEGER      NULL,
                token_id          INTEGER      NULL,
                channel_id        INTEGER      NULL,
                channel_name      VARCHAR(128) NULL,
                model             VARCHAR(191) NOT NULL,
                billing_mode      VARCHAR(16)  NOT NULL DEFAULT 'token',
                is_stream         INTEGER      NOT NULL DEFAULT 0,
                prompt_tokens     INTEGER      NOT NULL DEFAULT 0,
                completion_tokens INTEGER      NOT NULL DEFAULT 0,
                total_tokens      INTEGER      NOT NULL DEFAULT 0,
                usage_estimated   INTEGER      NOT NULL DEFAULT 0,
                upstream_cost     DECIMAL(20,10) NOT NULL DEFAULT 0,
                downstream_cost   DECIMAL(20,10) NOT NULL DEFAULT 0,
                latency_ms        INTEGER      NOT NULL DEFAULT 0,
                status            VARCHAR(16)  NOT NULL DEFAULT 'ok',
                error_message     VARCHAR(500) NULL
            )
        SQL);

        // 索引单独建（不能写进 CREATE TABLE：MySQL 支持内联 INDEX、SQLite 不支持）
        self::createIndexIfMissing('usage_logs', 'idx_usage_created', ['created_at']);
        self::createIndexIfMissing('usage_logs', 'idx_usage_model', ['model']);
        self::createIndexIfMissing('usage_logs', 'idx_usage_channel', ['channel_id']);
        self::createIndexIfMissing('usage_logs', 'idx_usage_user', ['user_id']);
    }

    /**
     * v7：下游用户、令牌与支付订单
     *
     * 前面几版都在做「上游」——渠道、密钥池、定价。
     * 这一版开始做「下游」：谁能用、用什么凭证用、钱怎么进来。
     * 没有这三张表，`usage_logs` 里的 user_id / token_id 永远是空的，
     * 也就谈不上「下游成本」的归属。
     *
     * ═══ 三张表的分工 ═══
     *
     *   users           —— 谁在用（余额、状态、邮箱验证）
     *   tokens          —— 用什么凭证调用（可多个、可限额、可设有效期）
     *   payment_orders  —— 钱怎么进来（充值订单，含回调原文便于对账）
     *
     * ═══ 几个刻意的取舍 ═══
     *
     * 1. **余额用 DECIMAL(20,10)**，与定价表同一套精度。
     *    钱绝不用浮点累加：0.1 + 0.2 这类误差在余额上就是事故。
     *
     * 2. **令牌只存哈希 + 掩码**（key_hash / key_mask），不存明文。
     *    与渠道 API Key 必须可逆不同：令牌是我们自己签发的，
     *    验证时只需比对哈希，**永不需要还原原文** ——
     *    所以用哈希而不是加密，泄露面更小（库被拖走也无法直接使用）。
     *
     * 3. **订单里保留回调原文**（notify_raw）。
     *    支付回调是对账时唯一的一手证据。上游说「我通知过了」、
     *    我们这边却没到账，靠的就是这个字段。截断保存，够用即可。
     *
     * 4. **邮箱验证 / 找回密码用同一个 token 字段**，不做成两张表。
     *    两者都是「一次性、有期限的凭据」，语义相同，分开存只会多一处维护。
     */
    private static function createV7(): void
    {
        $pdo = Db::pdo();
        $autoId = self::autoId();

        // ── users：下游用户 ──────────────────────────────────
        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS users (
                id                {$autoId},
                email             VARCHAR(191) NOT NULL,
                password_hash     VARCHAR(255) NOT NULL,
                display_name      VARCHAR(64)  NOT NULL DEFAULT '',
                balance           DECIMAL(20,10) NOT NULL DEFAULT 0,
                total_spent       DECIMAL(20,10) NOT NULL DEFAULT 0,
                status            INTEGER      NOT NULL DEFAULT 1,
                email_verified_at INTEGER      NULL,
                verify_token      VARCHAR(64)  NULL,
                reset_token       VARCHAR(64)  NULL,
                reset_expires_at  INTEGER      NULL,
                last_login_at     INTEGER      NULL,
                last_login_ip     VARCHAR(45)  NULL,
                register_ip       VARCHAR(45)  NULL,
                created_at        INTEGER      NOT NULL,
                updated_at        INTEGER      NOT NULL,
                CONSTRAINT uq_users_email UNIQUE (email)
            )
        SQL);

        // ── tokens：调用凭证 ─────────────────────────────────
        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS tokens (
                id           {$autoId},
                user_id      INTEGER      NOT NULL,
                name         VARCHAR(64)  NOT NULL,
                key_hash     VARCHAR(64)  NOT NULL,
                key_mask     VARCHAR(32)  NOT NULL,
                quota_limit  DECIMAL(20,10) NOT NULL DEFAULT 0,
                quota_used   DECIMAL(20,10) NOT NULL DEFAULT 0,
                models       VARCHAR(1024) NULL,
                expires_at   INTEGER      NULL,
                status       INTEGER      NOT NULL DEFAULT 1,
                last_used_at INTEGER      NULL,
                created_at   INTEGER      NOT NULL,
                updated_at   INTEGER      NOT NULL,
                CONSTRAINT uq_tokens_hash UNIQUE (key_hash)
            )
        SQL);

        // ── payment_orders：充值订单 ─────────────────────────
        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS payment_orders (
                id         {$autoId},
                order_no   VARCHAR(64)  NOT NULL,
                user_id    INTEGER      NOT NULL,
                amount     DECIMAL(20,10) NOT NULL,
                credit     DECIMAL(20,10) NOT NULL,
                gateway    VARCHAR(16)  NOT NULL,
                pay_type   VARCHAR(16)  NOT NULL DEFAULT 'alipay',
                trade_no   VARCHAR(64)  NULL,
                status     VARCHAR(16)  NOT NULL DEFAULT 'pending',
                notify_raw VARCHAR(2000) NULL,
                created_at INTEGER      NOT NULL,
                paid_at    INTEGER      NULL,
                CONSTRAINT uq_orders_no UNIQUE (order_no)
            )
        SQL);

        self::createIndexIfMissing('tokens', 'idx_tokens_user', ['user_id']);
        self::createIndexIfMissing('payment_orders', 'idx_orders_user', ['user_id']);
        self::createIndexIfMissing('payment_orders', 'idx_orders_status', ['status']);
        self::createIndexIfMissing('users', 'idx_users_verify', ['verify_token']);
        self::createIndexIfMissing('users', 'idx_users_reset', ['reset_token']);
    }

    /**
     * 建索引（幂等）。
     *
     * 为什么不用 `CREATE INDEX IF NOT EXISTS`：
     * MySQL **不支持**这个语法（SQLite 支持），所以必须自己探一次。
     * 与 addColumnIfMissing 同理，这是跨库可移植的代价，
     * 代价集中在 Schema 这一个文件里，值得。
     *
     * @param array<int, string> $columns
     */
    private static function createIndexIfMissing(string $table, string $index, array $columns): void
    {
        $pdo = Db::pdo();

        try {
            if (Db::isSqlite()) {
                $exists = false;
                foreach ($pdo->query("PRAGMA index_list({$table})") as $row) {
                    if (($row['name'] ?? '') === $index) {
                        $exists = true;
                        break;
                    }
                }
            } else {
                $row = Db::selectOne(
                    'SELECT COUNT(*) AS c FROM information_schema.statistics
                     WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?',
                    [$table, $index]
                );
                $exists = ((int) ($row['c'] ?? 0)) > 0;
            }

            if ($exists) {
                return;
            }
        } catch (Throwable) {
            return;
        }

        $pdo->exec(sprintf(
            'CREATE INDEX %s ON %s (%s)',
            $index,
            $table,
            implode(', ', $columns)
        ));
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
