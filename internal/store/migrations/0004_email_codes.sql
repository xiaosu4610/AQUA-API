-- 迁移 0004：邮箱验证码（注册前置校验）
--
-- 背景：
--   站点开放自助注册后，缺少"证明邮箱真实可用"的环节，容易被脚本批量注册；
--   本迁移引入验证码表，作为注册流程的前置校验依据。
--
-- 设计说明：
--   1) 只存验证码的 SHA-256 摘要（code_hash），不存明文——
--      即使数据库被拖走，也无法直接拿验证码去注册；
--   2) consumed_at 用 0 表示"未使用"：SQLite 无布尔类型，
--      用整数时间戳既能表达"是否用过"，又能记录"何时用的"，信息量更大；
--   3) 索引按查询模式建立：校验走 (email, purpose)，风控限流走 (request_ip, 创建时间)；
--   4) 过期记录由程序定期清理（见 main 启动逻辑），此处不设触发器。

CREATE TABLE IF NOT EXISTS email_codes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    email      TEXT    NOT NULL,               -- 收件邮箱（已规范化为小写）
    purpose    TEXT    NOT NULL DEFAULT 'register', -- 用途，便于将来复用于找回密码
    code_hash  TEXT    NOT NULL,               -- 验证码摘要；明文永不落库
    attempts   INTEGER NOT NULL DEFAULT 0,     -- 校验失败次数，达上限即作废
    expires_at INTEGER NOT NULL,               -- 过期时间（Unix 秒）
    consumed_at INTEGER NOT NULL DEFAULT 0,    -- 消费时间（Unix 秒）；0=未使用
    request_ip TEXT    NOT NULL DEFAULT '',    -- 申请来源 IP，用于风控追溯
    created_at INTEGER NOT NULL
);

-- 校验与"取最新一条"按 (email, purpose) + 时间倒序
CREATE INDEX IF NOT EXISTS idx_email_codes_email_purpose ON email_codes (email, purpose, created_at);
-- 单 IP 小时限流
CREATE INDEX IF NOT EXISTS idx_email_codes_ip_created ON email_codes (request_ip, created_at);
-- 过期清理
CREATE INDEX IF NOT EXISTS idx_email_codes_expires_at ON email_codes (expires_at);
