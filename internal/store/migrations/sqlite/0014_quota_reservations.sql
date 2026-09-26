-- 迁移 0014：额度预扣（在途预留）
--
-- 背景（为什么必须有它）：
--   升级前额度【只在鉴权时判一次】，扣费却发生在响应之后。于是并发 N 个请求会
--   全部通过检查、再全部后扣费；而 users.used_quota 是纯自增、不封顶，
--   最终会出现「已用 > 总额度」的倒欠。本迁移新增一张「预留台账」，
--   把额度从「事后扣」改成「事前预扣 + 事后结算/退还」：
--     · 鉴权时先按估算量预扣（写一条在途记录 + 原子扣减额度）；
--     · 响应后用实际用量结算（多退少补）；
--     · 请求失败则全额退还。
--   这样并发的多个请求会相互挤压同一份额度，堵住超支漏洞。
--
-- 设计说明：
--   1) request_id 是一次调用的幂等键：同一 request_id 的重复预留/结算/释放
--      都不再重复加减额度（并发重复提交或客户端重试都不会造成重复扣费）；
--   2) reserved 是预扣额度；settled 是结算后的实际额度，未结算时为 0；
--   3) status 用整数枚举：1 在途 / 2 已结算 / 3 已释放，与全项目 status 口径一致；
--   4) expires_at 是在途超时兜底时间（Unix 秒）：进程崩溃会留下"在途"残留，
--      必须有超时回收，否则这部分额度永远退不回来；
--   5) created_at / expires_at 统一用 Unix 秒，取值 0 表示未设置，与既有表一致。
--
-- 兼容性：
--   本表不引入外键约束，与既有表保持一致（用户/令牌被删除时预留记录仍保留，
--   仅作为历史痕迹，不影响主流程）。

CREATE TABLE IF NOT EXISTS quota_reservations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT    NOT NULL,             -- 幂等键：一次调用的唯一标识
    user_id    INTEGER NOT NULL DEFAULT 0,   -- 归属用户（0 表示无用户，如系统令牌）
    token_id   INTEGER NOT NULL DEFAULT 0,   -- 使用的访问令牌（0 表示未使用令牌）
    reserved   INTEGER NOT NULL DEFAULT 0,   -- 预扣额度
    settled    INTEGER NOT NULL DEFAULT 0,   -- 结算后实际额度（未结算为 0）
    status     INTEGER NOT NULL DEFAULT 1,   -- 1 在途 / 2 已结算 / 3 已释放
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0    -- 在途超时兜底（Unix 秒，0 = 不回收）
);

-- 幂等键唯一：这是"同一 request_id 不重复扣减"的第一道保证。
-- 注意：并发下的防重复不能只依赖唯一索引，必须由仓储层在事务内以
-- "插入冲突即返回已有记录 + 条件更新 + 受影响行数"判定（见 store/quota_repo.go）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_reservations_request ON quota_reservations (request_id);

-- 计算某用户的"在途预留合计"（可用额度 = 总额度 − 已用 − 在途预留）
CREATE INDEX IF NOT EXISTS idx_quota_reservations_user_status ON quota_reservations (user_id, status);

-- 在途超时回收：后台/启动时扫描"在途且已过期"的陈旧预留
CREATE INDEX IF NOT EXISTS idx_quota_reservations_status_expires ON quota_reservations (status, expires_at);
