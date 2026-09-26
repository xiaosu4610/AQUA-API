-- 迁移 0012：兑换码
--
-- 背景：
--   运营侧最常用的"低成本发额度"手段是兑换码：管理员批量生成一批码，
--   通过活动、社群、渠道分发出去，用户在门户输入码即可领取额度。
--   它相比"直接给账号充值"有三点优势：
--     1) 不需要知道用户是谁（先发码、后绑定），分发与兑换解耦；
--     2) 天然带有效期与批次概念，适合限时活动；
--     3) 额度发放有大额和小额的区分，兑换码适合小额、批量场景。
--   本迁移新增兑换码表，作为该能力的持久化基础。
--
-- 设计说明：
--   1) code 是兑换码明文，采用大写字母数字并剔除易混字符（0/O/1/I/l），
--      避免用户手工输入时因字形相近而兑换失败；唯一索引保证一码一用；
--   2) quota 是可兑换额度，单位与全项目一致（额度整数，非浮点）；
--   3) status 用整数枚举：1 未使用 / 2 已使用 / 3 已作废。
--      用整数而非字符串是为了与 projects 现有 status 口径一致，
--      并让索引与筛选更紧凑；
--   4) expires_at 用 Unix 秒，取值 0 表示【永不过期】——
--      与全项目"0 = 未设置/永不"的约定一致（见 server.unixOrZero 的说明）；
--   5) used_by / used_at 记录领取人与领取时刻，未使用时均为 0；
--   6) batch_no 是批次号，便于按批次查询与作废（活动结束后一键清理）；
--   7) remark 供管理员标注活动来源（如"双十一活动"）。
--
-- 兼容性：
--   本表不引入外键约束，与既有表保持一致（历史数据与删除用户的场景下，
--   加外键会让已有部署启动失败）。user 被删除时其兑换记录仍保留，仅作为历史痕迹。

CREATE TABLE IF NOT EXISTS redeem_codes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code       TEXT    NOT NULL,             -- 兑换码明文（大写字母数字，剔除易混字符）
    quota      INTEGER NOT NULL,             -- 可兑换额度（正整数）
    status     INTEGER NOT NULL DEFAULT 1,   -- 1 未使用 / 2 已使用 / 3 已作废
    expires_at INTEGER NOT NULL DEFAULT 0,   -- 过期时间（Unix 秒，0 = 永不过期）
    used_by    INTEGER NOT NULL DEFAULT 0,   -- 领取用户 id（未使用为 0）
    used_at    INTEGER NOT NULL DEFAULT 0,   -- 领取时间（Unix 秒，未使用为 0）
    batch_no   TEXT    NOT NULL DEFAULT '',  -- 批次号（便于按批次查询/作废）
    remark     TEXT    NOT NULL DEFAULT '',  -- 备注（活动来源等）
    created_at INTEGER NOT NULL
);

-- 兑换码唯一：这是"一码一用"的第一道保证。
-- 注意：并发下的防重复领取不能只依赖唯一索引，必须由仓储层在事务内
-- 以"条件更新 + 受影响行数"判定（见 store/redeem_repo.go 的 Redeem）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_redeem_codes_code ON redeem_codes (code);

-- 列表筛选：后台默认按状态过滤（如"只看未使用"），并附带时间维度
CREATE INDEX IF NOT EXISTS idx_redeem_codes_status_created ON redeem_codes (status, created_at);

-- 批次操作：按批次查询与作废
CREATE INDEX IF NOT EXISTS idx_redeem_codes_batch ON redeem_codes (batch_no);
