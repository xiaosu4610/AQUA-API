-- 迁移 0007：订阅账号池（OAuth 凭据）与 OAuth 提供方配置
--
-- 背景：
--   渠道密钥池（0005）解决的是"多把 API Key 轮询"；但另有一类上游凭据是
--   **订阅账号的 OAuth 令牌**：access_token 会过期，需要拿 refresh_token 定期刷新。
--   若沿用"只存一把静态密钥"的模型，令牌一过期整条渠道就不可用。
--   本迁移补齐这部分能力。
--
-- 设计取舍（重要）：扩展 channel_keys 而不是新建一张"账号表"。
--   两类凭据在调度语义上完全一致（都是池内轮询、都会失败摘除、都要记最近使用），
--   差别只在"取出来之后要不要先刷新"。若新建一张表，选池、摘除、统计三套逻辑
--   都要写两份，且将来加一种新凭据类型又要复制一次。
--   用 kind 字段区分类型，把差异收敛在"取用"这一处。
--
-- 安全说明：
--   - 所有令牌字段一律 _enc 结尾，存 AES-GCM 密文，禁止明文；
--   - oauth_providers.client_secret 同样加密存储：它属于密钥类配置，
--     泄露即可被用于伪造令牌刷新请求。

-- ---------------------------------------------------------------------------
-- 一、channel_keys 扩展 OAuth 支持
-- ---------------------------------------------------------------------------

-- 凭据类型：api_key（默认，向后兼容既有数据）/ oauth
ALTER TABLE channel_keys ADD COLUMN kind TEXT NOT NULL DEFAULT 'api_key';

-- OAuth 刷新令牌（密文）。有效期很长（数月），是长期凭据
ALTER TABLE channel_keys ADD COLUMN refresh_token_enc TEXT NOT NULL DEFAULT '';

-- OAuth 访问令牌（密文）。短期有效（通常 1 小时）
ALTER TABLE channel_keys ADD COLUMN access_token_enc TEXT NOT NULL DEFAULT '';

-- 访问令牌过期时间（Unix 秒）；0 表示无过期概念（api_key 类型）
ALTER TABLE channel_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0;

-- 账号标识（如邮箱或用户名，脱敏展示）：便于管理员在池里辨认"这是谁的账号"
ALTER TABLE channel_keys ADD COLUMN account_hint TEXT NOT NULL DEFAULT '';

-- 关联的 OAuth 提供方（kind=oauth 时使用）
ALTER TABLE channel_keys ADD COLUMN provider TEXT NOT NULL DEFAULT '';

-- ---------------------------------------------------------------------------
-- 二、OAuth 提供方配置
--
-- 为什么做成配置表而不是写死在代码里：
--   各家平台的 token 端点与 client_id 都可能变化，而且开源使用者可能接入
--   自建或小众平台。写死意味着每次变更都要改代码重新发版。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS oauth_providers (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT    NOT NULL,               -- 唯一标识，如 claude / gemini
    token_url          TEXT    NOT NULL,               -- 刷新令牌的端点
    client_id          TEXT    NOT NULL DEFAULT '',
    client_secret_enc  TEXT    NOT NULL DEFAULT '',    -- 密文
    scope              TEXT    NOT NULL DEFAULT '',    -- 部分平台刷新时需要回传 scope
    remark             TEXT    NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL DEFAULT 1,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_providers_name ON oauth_providers (name);

-- 池内查询：按渠道 + 启用的凭据类型筛选
CREATE INDEX IF NOT EXISTS idx_channel_keys_kind
    ON channel_keys (channel_id, kind, status);
