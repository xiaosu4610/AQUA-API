-- 迁移 0002：令牌表
--
-- 背景：
--   网关需要向使用者下发访问令牌（形如 sk-xxx），并据此鉴权、限制模型范围与额度。
--   本表是"下游凭证"的存储位置，与 channels 表（"上游凭证"）方向相反。
--
-- 安全设计（与 channels 表一致，但更进一步）：
--   1) key_enc  ：AES-256-GCM 密文，用于后台展示（数据库泄露也拿不到明文）；
--   2) key_hash ：KEY 的 SHA-256 摘要，带唯一索引，用于令牌鉴权时的 O(1) 查找。
--   为什么不能只存密文：AES-GCM 每次加密结果不同（随机 nonce），无法用于等值查询，
--   若靠解密全表来匹配令牌，既慢又会在内存中暴露全部明文。因此采用"摘要查找 + 密文展示"。
--
-- 说明：
--   - expires_at = 0 表示永不过期；
--   - unlimited_quota = 1 表示不限额度（remain_quota 被忽略）；
--   - models 为逗号分隔白名单，空字符串表示不限制模型；
--   - owner_id 预留归属用户，M2 尚未引入用户体系，统一为 0（系统令牌）。

CREATE TABLE IF NOT EXISTS tokens (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id        INTEGER NOT NULL DEFAULT 0,     -- 归属用户 ID（0 = 系统令牌）
    name            TEXT    NOT NULL,               -- 令牌名称，便于使用者区分用途
    key_hash        TEXT    NOT NULL,               -- KEY 的 SHA-256 十六进制摘要
    key_enc         TEXT    NOT NULL,               -- KEY 的 AES-GCM 密文（Base64）
    status          INTEGER NOT NULL DEFAULT 1,     -- 1=启用 2=手动禁用 3=已过期 4=额度耗尽
    expires_at      INTEGER NOT NULL DEFAULT 0,     -- 过期时间（Unix 秒）；0 = 永不过期
    remain_quota    INTEGER NOT NULL DEFAULT 0,     -- 剩余额度（内部单位）
    unlimited_quota INTEGER NOT NULL DEFAULT 0,     -- 0/1，1 表示不限额度
    used_quota      INTEGER NOT NULL DEFAULT 0,     -- 已用额度（内部单位）
    models          TEXT    NOT NULL DEFAULT '',    -- 逗号分隔模型白名单，空 = 不限制
    created_at      INTEGER NOT NULL,               -- 创建时间（Unix 秒）
    updated_at      INTEGER NOT NULL                -- 更新时间（Unix 秒）
);

-- 鉴权热路径：按 KEY 摘要精确查找，必须唯一且带索引
CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_key_hash ON tokens (key_hash);

-- 后台按归属人筛选令牌列表
CREATE INDEX IF NOT EXISTS idx_tokens_owner_id ON tokens (owner_id);

-- 后台按状态筛选（如列出全部已禁用令牌）
CREATE INDEX IF NOT EXISTS idx_tokens_status ON tokens (status);
