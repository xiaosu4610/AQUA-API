-- 迁移 0010：充值订单（M5 计费与支付）
--
-- 背景：
--   额度体系需要一个"钱从哪来"的入口。没有订单表时，额度只能由管理员
--   手工调整，既无法对外运营，也无法对账（"这笔额度是谁给的、给了多少"无据可查）。
--
-- 设计说明：
--   1) amount 单位为【分】（整数）。用浮点存金额必然出现 0.1+0.2 != 0.3 这类
--      无法解释的账目差异，而支付系统一旦对不上账就失去信任；
--   2) quota 在【下单时】就固化，而不是支付成功后再按当时的兑换比例计算。
--      理由：管理员随时可能调整"1 元 = 多少额度"，若按支付时刻计算，
--      用户会看到"我下单时是 100 额度，付款后到账 80"，属于最糟糕的体验；
--   3) status 状态机：
--        1 待支付 → 2 已支付
--                 ↘ 3 已关闭（超时/管理员关闭）
--        2 已支付 ↘ 4 已退款
--      终态（2/3/4）之后不再变化；"入账"只允许发生在 1 → 2 的跃迁上，
--      这是回调重复到达时的幂等依据；
--   4) provider_trade_no 保存第三方订单号，用于与第三方平台对账；
--      notify_payload 保存回调原文——出现"用户说付了但没到账"时，
--      必须能还原"到底收到了什么"，否则排查只能靠猜；
--   5) method / sub_method 用字符串而非数字：新增支付通道不该需要改数据库；
--   6) credited 是"额度是否已加给用户"的标记。为什么要与 status 分开：
--      回调重试、进程崩溃都可能让"订单已标记支付"与"额度已到账"之间出现缺口。
--      有了这两个字段，就能做到"标记支付"与"加额度"在同一事务内完成，
--      并且事后能查出"已支付但未入账"的订单进行补偿（见 CreditOrder）。
--
-- 索引设计：
--   idx_orders_trade_no  ：按订单号查询（回调、查单），唯一
--   idx_orders_user_created：用户查看自己的充值记录
--   idx_orders_status_expires：定时关闭超时订单（只扫待支付）

CREATE TABLE IF NOT EXISTS payment_orders (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    trade_no          TEXT    NOT NULL,              -- 对外订单号
    user_id           INTEGER NOT NULL,              -- 归属用户
    amount            INTEGER NOT NULL,              -- 实付金额（分）
    currency          TEXT    NOT NULL DEFAULT 'CNY',
    quota             INTEGER NOT NULL DEFAULT 0,    -- 下单时固化的入账额度
    method            TEXT    NOT NULL,              -- epay / stripe / manual
    sub_method        TEXT    NOT NULL DEFAULT '',   -- alipay / wxpay / card ...
    status            INTEGER NOT NULL DEFAULT 1,    -- 见上方状态机
    credited          INTEGER NOT NULL DEFAULT 0,    -- 额度是否已加到用户账户
    pay_url           TEXT    NOT NULL DEFAULT '',   -- 第三方收银台地址
    provider_trade_no TEXT    NOT NULL DEFAULT '',   -- 第三方订单号（对账用）
    notify_payload    TEXT    NOT NULL DEFAULT '',   -- 回调原文（排查用）
    remark            TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    paid_at           INTEGER NOT NULL DEFAULT 0,    -- 支付完成时间；0 表示未支付
    credited_at       INTEGER NOT NULL DEFAULT 0,    -- 入账时间；0 表示未入账
    expires_at        INTEGER NOT NULL DEFAULT 0     -- 支付截止时间；0 表示不过期
);

-- 订单号唯一：它是回调与查单的唯一凭据，重复意味着生成逻辑有缺陷
CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_trade_no ON payment_orders (trade_no);

-- 用户充值记录列表（按创建时间倒序分页）
CREATE INDEX IF NOT EXISTS idx_orders_user_created ON payment_orders (user_id, created_at DESC);

-- 超时关单扫描：只查待支付且已到期的订单
CREATE INDEX IF NOT EXISTS idx_orders_status_expires ON payment_orders (status, expires_at);
