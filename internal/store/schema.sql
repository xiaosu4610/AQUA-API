-- AQUA-API 数据库结构定义（迁移脚本 v1）
--
-- 使用说明：
--   本文件通过 Go 的 //go:embed 嵌入二进制，由 internal/store 的迁移器按版本顺序执行。
--   修改规则：
--     1) 已发布的迁移脚本【禁止修改】（历史环境已执行过，改了会导致新旧库结构不一致）；
--     2) 需要变更结构时，请在下方追加新的一段并在 store.go 的 migrations 列表中登记新版本；
--     3) 所有时间字段统一使用 Unix 秒（INTEGER），避免时区与字符串比较问题。
--
-- 命名约定：
--   - 布尔语义用 INTEGER（0/1），SQLite 无原生布尔类型；
--   - group 是 SQL 保留字，故分组字段命名为 group_name；
--   - 所有密钥类字段一律以 _enc 结尾，表示存储的是密文，禁止写入明文。

-- ---------------------------------------------------------------------------
-- 迁移版本记录表：由迁移器维护，记录每个已执行的版本，保证幂等
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY, -- 迁移版本号（与 store.go 中登记的一致）
    name       TEXT    NOT NULL,    -- 迁移名称，便于人工排查
    applied_at INTEGER NOT NULL     -- 执行时间（Unix 秒）
);

-- ---------------------------------------------------------------------------
-- 渠道表：一条记录代表一个可用的上游渠道（一个供应商配置）
--
-- 设计说明：
--   - 一个渠道可携带多个密钥（M1 先单密钥，多密钥池化在 M3 通过独立表实现）；
--   - models 用逗号分隔的字符串而非关联表：M1 追求简单，后续若需要按模型检索
--     路由索引，会新增独立的 ability 索引表（见后续里程碑），届时本字段仅作展示。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS channels (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,             -- 渠道显示名，如 "OpenAI 官方"
    type        INTEGER NOT NULL,             -- 渠道类型编号（M2 起定义枚举，M1 统一按 OpenAI 兼容处理）
    base_url    TEXT    NOT NULL DEFAULT '',  -- 上游基础地址，如 https://api.openai.com
    api_key_enc TEXT    NOT NULL DEFAULT '',  -- 上游密钥【密文】，切勿存明文
    models      TEXT    NOT NULL DEFAULT '',  -- 逗号分隔的可用模型列表，如 "gpt-4o,gpt-4o-mini"
    group_name  TEXT    NOT NULL DEFAULT 'default', -- 所属分组（用于按分组路由与计费倍率）
    priority    INTEGER NOT NULL DEFAULT 0,   -- 优先级，数值越大越优先被选中
    weight      INTEGER NOT NULL DEFAULT 1,   -- 同优先级内的随机权重
    status      INTEGER NOT NULL DEFAULT 1,   -- 1=启用 2=手动禁用 3=自动熔断禁用
    created_at  INTEGER NOT NULL,             -- 创建时间（Unix 秒）
    updated_at  INTEGER NOT NULL              -- 更新时间（Unix 秒）
);

-- 路由查询索引：按「分组 + 状态」筛选候选渠道是最热路径
CREATE INDEX IF NOT EXISTS idx_channels_group_status
    ON channels (group_name, status);

-- 按类型检索（运维排查"某类渠道有哪些"时使用）
CREATE INDEX IF NOT EXISTS idx_channels_type
    ON channels (type);
