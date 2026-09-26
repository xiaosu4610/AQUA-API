-- 迁移 0003：站点数据层（用户、会话、调用日志、系统设置）
--
-- 背景：
--   此前只有「渠道（上游凭证）」与「令牌（下游凭证）」，缺少站点运营所需的基础数据：
--   用户、登录会话、调用日志与系统设置。本迁移一次补齐，支撑前端的管理后台与用户门户。
--
-- 设计说明：
--   1) quota 用 -1 表示「不限额度」，用 0 表示「已用尽」，与令牌表语义保持一致；
--   2) 会话令牌只存 SHA-256 摘要：即使数据库泄露也无法反推出可用凭据（与令牌表同策略）；
--   3) usage_logs 为高频写入表，索引只建必要的两个（时间与用户），避免写入放大；
--   4) 对既有表采用 ALTER TABLE ADD COLUMN 追加字段——不重建表，保证已有数据不丢。

-- ---------------------------------------------------------------------------
-- 用户表
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL,               -- 登录名（唯一）
    password_hash TEXT    NOT NULL,               -- bcrypt 哈希，禁止明文
    email         TEXT    NOT NULL DEFAULT '',
    role          INTEGER NOT NULL DEFAULT 1,     -- 1=普通用户 10=管理员
    status        INTEGER NOT NULL DEFAULT 1,     -- 1=启用 2=禁用
    quota         INTEGER NOT NULL DEFAULT 0,     -- 总额度（内部单位）；-1=不限
    used_quota    INTEGER NOT NULL DEFAULT 0,     -- 已用额度
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username ON users (username);
CREATE INDEX IF NOT EXISTS idx_users_status ON users (status);

-- ---------------------------------------------------------------------------
-- 会话表（网站登录态；与「模型接口访问令牌」是两套独立凭据）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL,
    token_hash TEXT    NOT NULL,                  -- 会话令牌的 SHA-256 摘要
    expires_at INTEGER NOT NULL,                  -- 过期时间（Unix 秒）
    created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_token_hash ON sessions (token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions (user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- 调用日志表（用量统计与计费的数据基础）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS usage_logs (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id           INTEGER NOT NULL DEFAULT 0,  -- 调用者（0=系统/未认证）
    token_id          INTEGER NOT NULL DEFAULT 0,  -- 使用的访问令牌
    channel_id        INTEGER NOT NULL DEFAULT 0,  -- 命中的上游渠道
    model             TEXT    NOT NULL DEFAULT '', -- 请求的模型名
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    quota             INTEGER NOT NULL DEFAULT 0,  -- 本次消耗额度（内部单位）
    latency_ms        INTEGER NOT NULL DEFAULT 0,  -- 总耗时（毫秒）
    is_stream         INTEGER NOT NULL DEFAULT 0,  -- 是否流式
    status_code       INTEGER NOT NULL DEFAULT 0,  -- 回写给客户端的状态码
    error             TEXT    NOT NULL DEFAULT '', -- 失败原因（已脱敏）
    request_id        TEXT    NOT NULL DEFAULT '', -- 便于与客户端日志对账
    created_at        INTEGER NOT NULL
);

-- 仪表盘按时间倒序查询；用户门户按用户 + 时间过滤
CREATE INDEX IF NOT EXISTS idx_usage_logs_created_at ON usage_logs (created_at);
CREATE INDEX IF NOT EXISTS idx_usage_logs_user_created ON usage_logs (user_id, created_at);

-- ---------------------------------------------------------------------------
-- 系统设置表（KV）
--
-- 安全约束：本表【禁止】存放任何密钥类内容（如 AQUA_APP_KEY、上游密钥），
-- 密钥只允许通过环境变量注入，避免随数据库备份一同泄露。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT    NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

-- ---------------------------------------------------------------------------
-- 既有表补字段
-- ---------------------------------------------------------------------------

-- 渠道：记录最近一次测活结果，供后台列表展示
ALTER TABLE channels ADD COLUMN last_test_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN last_test_ok INTEGER NOT NULL DEFAULT 0;

-- 令牌：记录最近使用时间，供用户识别「哪些 key 还在用、哪些可清理」
ALTER TABLE tokens ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0;
