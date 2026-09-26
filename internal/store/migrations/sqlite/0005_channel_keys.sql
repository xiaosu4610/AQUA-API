-- 迁移 0005：渠道密钥池（一个渠道可携带多把上游密钥）
--
-- 背景：
--   原设计一个渠道只能填一把密钥（channels.api_key_enc）。
--   实际运营中一个上游往往有几十到几百把密钥（如批量申请的 NVIDIA 密钥），
--   需要把它们当作"轮询池"使用：请求在池内轮换，某把失效时自动摘除并换下一把。
--   若靠"每个密钥建一个渠道"来实现，渠道列表会迅速膨胀到不可维护。
--
-- 设计说明：
--   1) 独立表而非在 channels 里存 JSON 数组：
--      单密钥需要独立的失败计数、状态与最近使用时间，且要能按状态索引筛选，
--      JSON 数组无法建索引，也不便于单独更新某一把密钥的统计；
--   2) key_enc 存密文（与 channels.api_key_enc 同策略），
--      key_hash 存 SHA-256 摘要：用于去重与存在性判断，
--      这样"编辑渠道时重新粘贴同一批密钥"不会产生重复记录，
--      也不需要在内存里解密全部密钥做比对；
--   3) status: 1=启用 2=手动禁用 3=连续失败自动摘除；
--   4) fail_count 记录连续失败次数，成功一次即清零——
--      这正是"连续"失败的意义：偶发 429 不应把一把好密钥永久摘掉；
--   5) 保留 channels.api_key_enc 作为"单密钥"通路：
--      渠道若没有密钥池记录，仍按原来的单密钥方式工作，保证向后兼容。
--
-- 时间字段统一为 Unix 秒；0 表示"从未使用"。

CREATE TABLE IF NOT EXISTS channel_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id   INTEGER NOT NULL,               -- 归属渠道
    key_enc      TEXT    NOT NULL,               -- 密钥密文（AES-GCM），禁止明文
    key_hash     TEXT    NOT NULL,               -- 密钥 SHA-256 摘要，用于去重
    label        TEXT    NOT NULL DEFAULT '',    -- 备注（如"池 #001"，便于人工定位）
    status       INTEGER NOT NULL DEFAULT 1,     -- 1=启用 2=手动禁用 3=自动摘除
    fail_count   INTEGER NOT NULL DEFAULT 0,     -- 连续失败次数（成功即清零）
    last_used_at INTEGER NOT NULL DEFAULT 0,     -- 最近被选中使用的时间
    last_error   TEXT    NOT NULL DEFAULT '',    -- 最近一次失败原因（已脱敏）
    created_at   INTEGER NOT NULL
);

-- 路由热路径：按渠道筛选"可用（启用）"的密钥
CREATE INDEX IF NOT EXISTS idx_channel_keys_channel_status
    ON channel_keys (channel_id, status);

-- 同一渠道内同一把密钥只能出现一次（防止重复导入造成池内自我竞争）
CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_keys_unique
    ON channel_keys (channel_id, key_hash);
