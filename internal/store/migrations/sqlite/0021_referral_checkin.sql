-- 迁移 0021：邀请返利与每日签到
--
-- 背景（为什么必须有它）：
--   本站是"账号聚合 + 订阅制"融合站点。参考行业通行做法，增长与留存各需要一张底牌：
--     · 邀请返利 —— 老用户带新用户注册/充值后获得额度奖励（拉新）；
--     · 每日签到 —— 用户每天来站点领一点额度，维持日活（留存）。
--   此前这两项完全没有承载结构，本迁移一次性补齐所需的四样东西：
--     1) users.invite_code —— 每个用户唯一的邀请码（对外分享凭证）；
--     2) users.inviter_id  —— 邀请关系（记录"我是被谁邀请来的"，0 表示无邀请人）；
--     3) referral_rewards  —— 邀请奖励台账（一次注册奖 / 一次充值返利各一行）；
--     4) checkin_records   —— 每日签到记录（每人每天至多一行）。
--
-- 设计说明（逐条都是踩过坑才会写下来的约束）：
--   1) invite_code 唯一索引【必须是部分索引】(WHERE invite_code <> '')：
--      历史用户在本迁移落地时 invite_code 全为空串。若建普通唯一索引，
--      这些空串会互相冲突，导致迁移直接失败。部分索引只约束"非空邀请码"，
--      从而允许"尚未生成邀请码"的用户大量共存——老用户的邀请码由服务端
--      首次查询邀请信息时懒生成（见 store.referralRepository.EnsureInviteCode）。
--   2) referral_rewards 对 (kind, invitee_id, order_trade_no) 建唯一索引，
--      这是【充值返利幂等的关键】：同一笔订单重复回调时，第二次插入会撞唯一约束，
--      服务端据此直接跳过、不再给邀请人加额度，从数据库层面杜绝重复发奖。
--   3) checkin_records 对 (user_id, checkin_date) 建唯一索引：
--      把"每人每天只能签一次"的判定交给数据库，避免并发下"先查后写"的竞态。
--      checkin_date 存北京时间日期字符串（YYYY-MM-DD），见 server 的 beijingDate。
--   4) 不引入外键约束（与既有迁移一致）：历史数据/补偿脚本可能落到不一致的 id，
--      外键会让写入或迁移失败；关系完整性由服务端的业务校验兜底。
--
-- 兼容性：
--   两列均 NOT NULL + 常量默认值，SQLite 允许对已有表直接 ADD COLUMN；
--   已有用户行会一次性获得空串邀请码与 0 邀请人，行为与迁移前完全一致
--   （返利与签到默认关闭，见设置项 referral_enabled / checkin_enabled 默认 false）。

ALTER TABLE users ADD COLUMN invite_code TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN inviter_id INTEGER NOT NULL DEFAULT 0;

-- 邀请码唯一索引（部分索引：只约束非空邀请码，否则历史空串会互相冲突使迁移失败）
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_invite_code ON users (invite_code) WHERE invite_code <> '';

-- 邀请奖励台账：kind 区分 'register'（邀请注册奖）/ 'recharge'（充值返利）。
-- (kind, invitee_id, order_trade_no) 唯一索引 = 幂等闸门（见上方设计说明第 2 条）。
CREATE TABLE IF NOT EXISTS referral_rewards (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    inviter_id     INTEGER NOT NULL,                    -- 获得奖励的邀请人 user id
    invitee_id     INTEGER NOT NULL,                    -- 触发奖励的被邀请人 user id
    kind           TEXT    NOT NULL,                    -- 'register' | 'recharge'
    quota          INTEGER NOT NULL,                    -- 本次奖励额度（正整数）
    order_trade_no TEXT    NOT NULL DEFAULT '',         -- 充值返利对应订单号；注册奖为空串
    created_at     INTEGER NOT NULL                     -- Unix 秒
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_referral_rewards_idem
    ON referral_rewards (kind, invitee_id, order_trade_no);

-- 邀请人维度查询（"我累计返利多少 / 邀请了多少人"）用：按邀请人聚合。
CREATE INDEX IF NOT EXISTS idx_referral_rewards_inviter ON referral_rewards (inviter_id);

-- 每日签到记录：每人每天至多一行（唯一约束保证并发下只成功一次）。
CREATE TABLE IF NOT EXISTS checkin_records (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,                      -- 签到用户 user id
    checkin_date TEXT    NOT NULL,                      -- 北京时间日期（YYYY-MM-DD）
    quota        INTEGER NOT NULL,                      -- 本次签到发放额度
    created_at   INTEGER NOT NULL                       -- Unix 秒
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_checkin_records_user_date
    ON checkin_records (user_id, checkin_date);
