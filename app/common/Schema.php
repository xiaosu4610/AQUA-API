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
    public const VERSION = 16;

    /**
     * 确保表结构存在且为最新版本。
     *
     * 由启动引导（app/bootstrap/InitDb.php）调用，每个工作进程启动时执行一次。
     */
    public static function ensure(): void
    {
        // ⚠️ 这里必须 **raw() + json_decode**，不能直接 `(int) Settings::raw(...)`。
        //
        // 库里存的是一个 JSON 字符串（值是 `"14"`，带引号），
        // 直接强转成 int 会得到 **0** —— 那样 `$current < self::VERSION` 永远成立，
        // 于是**每个工作进程每次启动都会把全部迁移重跑一遍**。
        // 症状不会表现为「功能坏了」，而是启动日志里一片
        // 「Duplicate column name / Duplicate key name / Duplicate entry」告警
        // （生产上真的出现过，当时被当成历史噪音放过去了）。
        //
        // 也不用 Settings::get()：它读的是进程内缓存，
        // 而第一次迁移时 options 表可能刚建出来、缓存还是空的。
        $current = (int) json_decode((string) Settings::raw('schema_version'), true);

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

        if ($current < 8) {
            self::createV8();
        }

        if ($current < 9) {
            self::createV9();
        }

        if ($current < 10) {
            self::createV10();
        }

        if ($current < 11) {
            self::createV11();
        }

        if ($current < 12) {
            self::createV12();
        }

        if ($current < 13) {
            self::createV13();
        }

        if ($current < 14) {
            self::createV14();
        }

        // ⚠️ 这段原来叫 v15、表名用的是 `groups`。`groups` 是 **MySQL 8 的保留字**，
        // 于是 MySQL 部署上 CREATE TABLE / SELECT / INSERT 全部语法错误，
        // 整段迁移在第一步就中断 —— 表与列一个都没建成，而版本号停在 14。
        // 更糟的是「看起来没事」：服务照常启动（InitDb 把异常记进日志就继续了），
        // 直到新代码去写 `tokens.groups` 这类不存在的列，才以「新建令牌失败」的形式爆出来。
        // 现在表名改成 `line_groups`，并把版本推到 16：
        // 所有停在 15 之前的库（包括那次失败的 MySQL 库）都会补跑这一遍。
        // 旧的 `groups` 表刻意不删（迁移只加不删），它已不再被任何代码引用。
        if ($current < 16) {
            self::createV16();
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
     * v8：渠道级熔断状态
     *
     * 解决的问题：某条上游整体挂了（比如对方机房故障）时，
     * 每个进来的请求都会先去它那儿撞一次墙 —— 白白消耗一次连接超时
     * （最坏 8 秒）、把用户晾在那儿，然后才轮到下一个渠道。
     * 几百个请求同时撞上去，还会把上游彻底压垮。
     *
     * 做法：连续失败到阈值就把该渠道短路一段时间（breaker_until），
     * 期间不再选它；到点自动放出来再试（与密钥池的冷却同一个思路）。
     *
     * 注意这是**渠道级**而不是密钥级：
     *   · 密钥级看的是「这把 Key 还行不行」（401/403 立刻停）；
     *   · 渠道级看的是「这条线路整体通不通」（连不上、超时、5xx 连击）。
     *   两者粒度不同，混在一起会导致「上游抖动一次就把整个渠道禁掉」。
     */
    private static function createV8(): void
    {
        self::addColumnIfMissing('channels', 'fail_streak', 'INTEGER NOT NULL DEFAULT 0');
        self::addColumnIfMissing('channels', 'breaker_until', 'INTEGER NULL');
    }

    /**
     * v9：给下游用户也加上「登录失败锁定」
     *
     * 解决的问题：后台（admins 表）从 v1 起就有 failed_attempts / locked_until，
     * 但**下游用户一直没有任何防暴力破解机制** —— 也就是说**用户这一侧的密码
     * 是可以被无限次尝试的**。
     *
     * 这个缺口比它看起来严重：用户账号后面挂的是真金白银的余额（还有站长自掏腰包
     * 买来的上游额度）。撞开一个账号就能把余额跑光，而日志里只会留下一串
     * 「登录失败」，看不出这是一次持续攻击。
     *
     * 为什么不在 v7 建表时直接写上这两列：
     *   迁移必须逐版本递进，而 v7 已经在生产上跑过了。补列只能靠新版本，
     *   这样老部署升级与新装部署走的是同一条路径，行为一致。
     */
    private static function createV9(): void
    {
        self::addColumnIfMissing('users', 'failed_attempts', 'INTEGER NOT NULL DEFAULT 0');
        self::addColumnIfMissing('users', 'locked_until', 'INTEGER NULL');
    }

    /**
     * v11：给探测结果加「这次请求花了多少毫秒」。
     *
     * 为什么要单独存耗时，而不是只看「可用/不可用」：
     *   像 NVIDIA NIM 这种免费上游，一大半模型其实**能调通**，
     *   只是首包要十几秒。用户真正想知道的是「哪个模型反应快、哪个慢」——
     *   只给一个「可用」会让人踩到最慢的那个然后以为站点有问题。
     *
     * 取值是**单次 HTTP 往返的实际耗时**（含重试则以最后一次为准），
     * 不是流式首字节耗时（探测用的是非流式请求，本来就等价于首字节）。
     * 老数据补 0，页面据此显示「暂无数据」而不是假装它是 0 毫秒。
     */
    private static function createV11(): void
    {
        self::addColumnIfMissing('channel_model_probe_results', 'latency_ms', 'INTEGER NOT NULL DEFAULT 0');
    }

    /**
     * v16：分组（`line_groups`）+ 计费口径 + 密钥额度
     *
     * （这段内容原本是 v15，因为表名撞上 MySQL 保留字而整体失败，见 ensure() 里的说明。）
     *
     * ═══ 为什么要有「分组」═══
     *
     * 同一个站点会接多条上游线路，它们的性质完全不同：
     *   · 现有 NIM 免费线路：不花钱，给所有用户用
     *   · 新接的专线（硅基流动 / TierFlow）：**站长在花钱**，临时免费给用户体验
     *   · 将来的付费线路：站长花钱、用户也花钱
     * 这三类必须能分开展示、分开计价、分开盯额度 —— 否则「哪条线在烧钱」永远看不清。
     *
     * 所以分组的定位是：**「一条（或一组）上游线路 + 它提供的模型 + 它的计价方式」的集合**。
     * 落地方式刻意选了「挂在渠道上」而不是另建一张分组-模型表：
     *   本站已经有渠道清单与定价表，再建一张就会**三处描述同一件事**，迟早对不上。
     *
     * ═══ cost_mode 与 price_mode 为什么必须分开 ═══
     *
     * 「临时免费」这件事用单个字段表达不了：专线是**站长在花钱、用户不花钱**。
     * 一个字段无论怎么设计都会把其中一半说错，于是分成两个维度：
     *   · cost_mode  上游要不要花钱（运维视角：这条线要盯额度）
     *   · price_mode 对用户收不收费（用户视角：模型广场上显不显示价格）
     *
     * ═══ 一并加进来的字段，以及它们各自的理由 ═══
     *
     *   · `pricing.*_cache_hit_price`   缓存命中是**独立计费维度**（命中价通常是输入价的 1/10），
     *                                   不分出来的话成本会算高一大截
     *   · `pricing.*_price_windows`     **时段价**：上游有谷时半价（如 02:00-08:00），
     *                                   不建模就永远算不准夜间成本
     *   · `channel_keys.budget_*`       上游余额是**按密钥**给的（16 元 / 74 元各一把），
     *                                   且上游不提供余额接口 → 只能本地记账
     *   · `usage_logs.cached_tokens`    记账要能被审计：事后要能拿这条记录重算一遍成本
     *   · `tokens.groups`               令牌可访问的分组（默认「所有默认可见的分组」）
     */
    private static function createV16(): void
    {
        $pdo = Db::pdo();
        $autoId = self::autoId();

        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS line_groups (
                id              {$autoId},
                code            VARCHAR(32)  NOT NULL,
                label           VARCHAR(64)  NOT NULL,
                description     VARCHAR(255) NULL,
                cost_mode       VARCHAR(16)  NOT NULL DEFAULT 'free',
                price_mode      VARCHAR(16)  NOT NULL DEFAULT 'priced',
                visible         INTEGER      NOT NULL DEFAULT 1,
                default_visible INTEGER      NOT NULL DEFAULT 1,
                sort            INTEGER      NOT NULL DEFAULT 0,
                status          INTEGER      NOT NULL DEFAULT 1,
                created_at      INTEGER      NOT NULL,
                updated_at      INTEGER      NOT NULL,
                CONSTRAINT uq_groups_code UNIQUE (code)
            )
        SQL);

        // 渠道 / 定价 / 令牌 各挂一个分组维度
        self::addColumnIfMissing('channels', 'group_id', 'INTEGER NOT NULL DEFAULT 0');
        self::addColumnIfMissing('pricing', 'group_id', 'INTEGER NOT NULL DEFAULT 0');
        self::addColumnIfMissing('tokens', 'allow_groups', 'VARCHAR(191) NULL');

        // 计费的两个新维度（缓存命中 + 时段价）
        self::addColumnIfMissing('pricing', 'upstream_cache_hit_price', 'DECIMAL(20,10) NOT NULL DEFAULT 0');
        self::addColumnIfMissing('pricing', 'downstream_cache_hit_price', 'DECIMAL(20,10) NOT NULL DEFAULT 0');
        self::addColumnIfMissing('pricing', 'upstream_price_windows', 'TEXT NULL');
        self::addColumnIfMissing('pricing', 'downstream_price_windows', 'TEXT NULL');

        // 密钥额度（上游给的余额，本地记账）
        self::addColumnIfMissing('channel_keys', 'budget_total', 'DECIMAL(20,10) NOT NULL DEFAULT 0');
        self::addColumnIfMissing('channel_keys', 'budget_used', 'DECIMAL(20,10) NOT NULL DEFAULT 0');
        self::addColumnIfMissing('channel_keys', 'budget_note', 'VARCHAR(255) NULL');
        // 1 = 这把密钥是**因为额度耗尽被自动停用**的（补录额度后可以自动恢复）。
        // 为什么要单独一个标记，而不是靠「budget_used >= budget_total」现场判断：
        // 站长手动停用的密钥必须永远保持停用，不能被「额度还够」自动放出来 ——
        // 两者混在一起就会出现「我明明关了它，它自己又跑起来了」这种失控感
        self::addColumnIfMissing('channel_keys', 'budget_disabled', 'INTEGER NOT NULL DEFAULT 0');
        // 上游口径的读数（目前只有 TierFlow 有：/v1/dashboard/billing/usage 的 total_usage）
        self::addColumnIfMissing('channel_keys', 'budget_reported', 'DECIMAL(20,10) NOT NULL DEFAULT 0');
        self::addColumnIfMissing('channel_keys', 'budget_reported_at', 'INTEGER NOT NULL DEFAULT 0');

        // 记账可审计：把缓存命中 tokens 与「这次用的是哪把密钥」都记下来，
        // 事后能拿这条记录重算一遍成本，也能算出每把密钥各自的消耗速率
        self::addColumnIfMissing('usage_logs', 'cached_tokens', 'INTEGER NOT NULL DEFAULT 0');
        self::addColumnIfMissing('usage_logs', 'channel_key_id', 'INTEGER NULL');

        // ── 四个初始分组 ──
        // 注意两个专线分组刻意是 `visible=0, default_visible=0`：
        // 代码先上，口径先建好，但**不对外露出**——什么时候放开由站长一句话决定
        self::seedGroup('free', '免费共享线路', '现有共享线路，所有用户可用', 'free', 'priced', 1, 1, 10);
        self::seedGroup('paid', '付费线路', '按定价收费的线路（暂未接入渠道）', 'paid', 'priced', 1, 1, 20);
        self::seedGroup('siliconflow', '高速稳定专线 · 硅基流动', '硅基流动专线，临时免费体验，额度用尽后自动下线', 'paid', 'free', 0, 0, 30);
        self::seedGroup('tierflow', '高速稳定专线 · TierFlow', 'TierFlow 专线，临时免费体验，额度用尽后自动下线', 'paid', 'free', 0, 0, 40);

        // 现有渠道与定价全部归入「免费共享线路」——
        // 这样 group_id 永远指向一个真实分组，不必到处写「0 表示免费」这种隐形约定
        $freeId = (int) (Db::selectOne("SELECT id FROM line_groups WHERE code = 'free'")['id'] ?? 0);
        if ($freeId > 0) {
            Db::execute('UPDATE channels SET group_id = ? WHERE group_id = 0', [$freeId]);
            Db::execute('UPDATE pricing SET group_id = ? WHERE group_id = 0', [$freeId]);
        }
    }

    /**
     * 插入一个初始分组（已存在则不动）。
     *
     * 为什么用「先查再插 + 吞掉重复键异常」而不是 INSERT OR IGNORE：
     * 两个数据库的「忽略重复」写法不同（SQLite 是 OR IGNORE、MySQL 是 IGNORE），
     * 而这里要的语义只是「没有就建」，本地并发（多进程同时启动）撞键时吞掉即可。
     */
    private static function seedGroup(
        string $code,
        string $label,
        string $description,
        string $costMode,
        string $priceMode,
        int $visible,
        int $defaultVisible,
        int $sort
    ): void {
        if (Db::selectOne('SELECT id FROM line_groups WHERE code = ?', [$code]) !== null) {
            return;
        }

        $now = time();

        try {
            Db::execute(
                'INSERT INTO line_groups (code, label, description, cost_mode, price_mode, visible, default_visible, sort, status, created_at, updated_at)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)',
                [$code, $label, $description, $costMode, $priceMode, $visible, $defaultVisible, $sort, $now, $now]
            );
        } catch (Throwable) {
            // 另一个进程刚好抢先建好了：忽略
        }
    }

    /**
     * v14：模型运行期健康度（`model_health`）
     *
     * ═══ 为什么需要它 ═══
     *
     * 生产上「大部分请求都在失败」，而失败高度集中在少数几个模型上 ——
     * 实测某次：一个模型 20 次调用**全部**超时，每次要烧 50 秒；
     * 另一个 7 次全失败，平均 61 秒。用户那边等了两三分钟，
     * 最后拿到一句「上游超时」。这些模型是谁、失败多少次，
     * 只能靠人去翻用量日志才知道，而请求已经被白白浪费掉了。
     *
     * 所以给每个模型记一份**运行期**健康度：连续失败到阈值就暂时摘掉它，
     * 让请求**立刻**拿到「这个模型上游当前不可用，请换一个」，
     * 而不是陪着它一起等超时。冷却期一到自动放行一次，上游恢复了能自动回来。
     *
     * 为什么不复用 channel_model_probe_results：
     *   那是**探测任务**的结论（人工触发、一次性的），而这里是
     *   **每一次真实调用**滚动出来的结论，两者的更新频率与生命周期完全不同。
     *
     * 字段说明（都不是「统计好看」用的，每一个都有明确用途）：
     *   · consecutive_errors —— 判定可用性的**唯一依据**。用「连续」而不是
     *     「失败率」：失败率会被大量成功稀释，而我们要抓的是「根本不工作」
     *   · unavailable_until —— 冷却截止时间，到点自动放行（自愈的关键）
     *   · last_error —— 前台要把它念给用户听（「为什么不可用」）
     *   · avg_latency_ms —— 后台判断「慢得没法用」还是「直接报错」
     */
    private static function createV14(): void
    {
        Db::pdo()->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS model_health (
                model              VARCHAR(191) NOT NULL,
                consecutive_errors INTEGER      NOT NULL DEFAULT 0,
                total_calls        INTEGER      NOT NULL DEFAULT 0,
                total_errors       INTEGER      NOT NULL DEFAULT 0,
                last_ok_at         INTEGER      NOT NULL DEFAULT 0,
                last_error_at      INTEGER      NOT NULL DEFAULT 0,
                last_status        INTEGER      NOT NULL DEFAULT 0,
                last_error         VARCHAR(255) NULL,
                avg_latency_ms     INTEGER      NOT NULL DEFAULT 0,
                unavailable_until  INTEGER      NOT NULL DEFAULT 0,
                updated_at         INTEGER      NOT NULL DEFAULT 0,
                PRIMARY KEY (model)
            )
        SQL);
    }

    /**
     * v13：令牌的可回显副本（`tokens.key_enc`）
     *
     * ═══ 为什么加这一列 ═══
     *
     * 令牌原来只存 SHA-256 哈希，**设计上不可还原** —— 好处是库被拖走也拿不到明文，
     * 代价是用户掉了令牌就只能新建一把，然后去各处改配置。站长反馈这一步太折腾，
     * 需要「随时复制」。
     *
     * 于是加一列存**可逆**副本（AES-256-GCM，与渠道 API Key 同一套 Crypto）。
     * 安全边界随之变化，所以配套三件事：
     *   1. 后台开关 `security.token_reveal` 控制「要不要存明文、要不要显示」，
     *      关掉就不存、也不显示 —— 想回到「只存哈希」的口径随时可以
     *   2. **鉴权路径完全不变**：仍然用 key_hash 比对，明文只用于展示
     *   3. 老令牌没有这一列的值 → 界面上如实说「创建时未保存副本，需要请新建」，
     *      而不是给一个点了没反应的按钮
     */
    private static function createV13(): void
    {
        self::addColumnIfMissing('tokens', 'key_enc', 'VARCHAR(512) NULL');
    }

    /**
     * v12：邮箱验证码。
     *
     * 为什么需要一张表，而不是把验证码放在会话里：
     *   会话只活在「发码的那个浏览器」上 —— 用户在手机上申请、在电脑上填写就废了；
     *   而注册这一步本来就常常发生在换设备/换网络的情况下。
     *   放数据库还能顺便做限流（同一个邮箱/IP 每小时能发几次），
     *   这是防「拿别人邮箱刷验证码」的关键。
     *
     * 只存**验证码的哈希**，不存明文：即使数据库被读走，
     * 也无法直接拿去通过别人的注册验证（与用户密码同一原则）。
     */
    private static function createV12(): void
    {
        $pdo = Db::pdo();
        $autoId = self::autoId();

        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS email_codes (
                id         {$autoId},
                email      VARCHAR(191) NOT NULL,
                purpose    VARCHAR(32)  NOT NULL,
                code_hash  VARCHAR(64)  NOT NULL,
                ip         VARCHAR(64)  NOT NULL,
                attempts   INTEGER      NOT NULL DEFAULT 0,
                expires_at INTEGER      NOT NULL,
                used_at    INTEGER      NULL,
                created_at INTEGER      NOT NULL
            )
        SQL);

        self::createIndexIfMissing('email_codes', 'idx_email_codes_lookup', ['email', 'purpose']);
    }

    /**
     * v10：渠道模型探测任务与逐项结果。
     */
    private static function createV10(): void
    {
        $pdo = Db::pdo();
        $autoId = self::autoId();

        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS channel_model_probe_tasks (
                id                 {$autoId},
                channel_id         INTEGER      NOT NULL,
                status             VARCHAR(16)  NOT NULL,
                total_count        INTEGER      NOT NULL DEFAULT 0,
                completed_count    INTEGER      NOT NULL DEFAULT 0,
                current_model      VARCHAR(191) NULL,
                ok_count           INTEGER      NOT NULL DEFAULT 0,
                no_access_count    INTEGER      NOT NULL DEFAULT 0,
                unroutable_count   INTEGER      NOT NULL DEFAULT 0,
                inconclusive_count INTEGER      NOT NULL DEFAULT 0,
                error_message      VARCHAR(500) NULL,
                created_at         INTEGER      NOT NULL,
                started_at         INTEGER      NULL,
                heartbeat_at       INTEGER      NULL,
                finished_at        INTEGER      NULL,
                applied_at         INTEGER      NULL,
                applied_by         VARCHAR(64)  NULL,
                removed_count      INTEGER      NOT NULL DEFAULT 0,
                backup_path        VARCHAR(500) NULL
            )
        SQL);

        $pdo->exec(<<<SQL
            CREATE TABLE IF NOT EXISTS channel_model_probe_results (
                id             {$autoId},
                task_id        INTEGER      NOT NULL,
                model          VARCHAR(191) NOT NULL,
                upstream_model VARCHAR(191) NOT NULL,
                classification VARCHAR(16)  NOT NULL,
                http_status    INTEGER      NOT NULL DEFAULT 0,
                response_summary VARCHAR(500) NULL,
                completed_at   INTEGER      NOT NULL,
                CONSTRAINT uq_probe_task_model UNIQUE (task_id, model)
            )
        SQL);

        self::createIndexIfMissing('channel_model_probe_tasks', 'idx_probe_tasks_channel', ['channel_id']);
        self::createIndexIfMissing('channel_model_probe_tasks', 'idx_probe_tasks_status', ['status']);
        self::createIndexIfMissing('channel_model_probe_results', 'idx_probe_results_task', ['task_id']);
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
            if (self::indexExists($table, $index)) {
                return;
            }
        } catch (Throwable) {
            return;
        }

        try {
            $pdo->exec(sprintf(
                'CREATE INDEX %s ON %s (%s)',
                $index,
                $table,
                implode(', ', $columns)
            ));
        } catch (Throwable $e) {
            // 与 addColumnIfMissing 同一个理由：多个工作进程同时启动时会抢着建同一个索引，
            // 抢输的那个收到「Duplicate key name」。复查一次再决定是抢输了还是 SQL 写错了
            if (!self::indexExists($table, $index)) {
                throw $e;
            }
        }
    }

    /** 这个索引在不在 */
    private static function indexExists(string $table, string $index): bool
    {
        $pdo = Db::pdo();

        if (Db::isSqlite()) {
            foreach ($pdo->query("PRAGMA index_list({$table})") as $row) {
                if (($row['name'] ?? '') === $index) {
                    return true;
                }
            }

            return false;
        }

        $row = Db::selectOne(
            'SELECT COUNT(*) AS c FROM information_schema.statistics
             WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?',
            [$table, $index]
        );

        return ((int) ($row['c'] ?? 0)) > 0;
    }

    /** 这一列在不在 */
    private static function columnExists(string $table, string $column): bool
    {
        $pdo = Db::pdo();

        if (Db::isSqlite()) {
            foreach ($pdo->query("PRAGMA table_info({$table})") as $row) {
                if (($row['name'] ?? '') === $column) {
                    return true;
                }
            }

            return false;
        }

        $row = Db::selectOne(
            'SELECT COUNT(*) AS c FROM information_schema.columns
             WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?',
            [$table, $column]
        );

        return ((int) ($row['c'] ?? 0)) > 0;
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
            if (self::columnExists($table, $column)) {
                return;
            }
        } catch (Throwable) {
            // 表不存在（全新库）—— 建表语句里已含该列，无需补
            return;
        }

        try {
            $pdo->exec("ALTER TABLE {$table} ADD COLUMN {$column} {$definition}");
        } catch (Throwable $e) {
            // 多个工作进程（生产是 8 个）同时启动时，会有进程「慢半拍」走到这里，
            // 而列已经被别的进程加好了 —— 数据库回的是 Duplicate column。
            // 这种情况**必须当成功**：否则这个进程会中断整段迁移，
            // 版本号也不会写（生产上真的出现过，日志里一片 Duplicate column name）。
            //
            // 但也不能一律吞掉 —— 万一是我自己把类型名写错了（那也是这个异常），
            // 复查一次就能分辨：列已存在 = 抢输了；列仍不存在 = 真错了，照抛
            if (!self::columnExists($table, $column)) {
                throw $e;
            }
        }
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
