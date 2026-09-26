-- 迁移 0019：管理后台操作审计日志
--
-- 背景（为什么必须有它）：
--   在此之前，后台的每一次写操作（建渠道、改设置、删用户、退款……）都不留痕：
--   出了误操作或越权只能靠猜，既无法定责，也无法复盘"这行配置是谁改的"。
--   本迁移新增审计表，把后台写操作沉淀为可查询的记录。
--
-- 设计说明：
--   1) 只记"写操作"（POST/PUT/PATCH/DELETE）。GET 是只读且量极大，
--      记录它既无追责价值，也会把表迅速撑爆（由应用层过滤，见 middleware.AdminAudit）；
--   2) admin_username 冗余存一份，而不只存 admin_id：审计的价值在于"多年后仍能看懂"，
--      而用户可能被改名或删除。冗余用户名让历史记录不随用户表变化而失真；
--   3) action 与 target 拆开：
--        · action 是方法 + 路由模式推导出的可读动作（如"更新渠道"），
--          便于人快速扫读，不依赖前端解析路径；
--        · target 是路径参数的实际取值（如 id=12、tradeNo=2025...），
--          回答"改的是哪一条"；
--   4) detail 是请求体摘要：入库前已由应用层脱敏（password/secret/key/token/credential
--      等字段值替换为 ***）并截断到 1000 字符以内。这里不再做长度约束，
--      因为截断规则属于应用层策略，改策略时不应需要动数据库；
--   5) method / path 用字符串而非枚举数字：新增接口不该需要改数据库；
--   6) 不引入外键（与既有迁移一致）：审计记录必须比被审计对象活得更久，
--      用外键会在删除用户/渠道时被级联清理或阻塞，正好违背审计的初衷。
--
-- 索引设计：
--   idx_audit_logs_created_at：按时间倒序分页（列表默认排序）
--   idx_audit_logs_admin_id  ：按管理员筛选（"某人做过哪些操作"）
--
-- 兼容性：
--   全新表，对已有数据无影响；脚本使用 IF NOT EXISTS，可安全重复执行。

CREATE TABLE IF NOT EXISTS audit_logs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    admin_id       INTEGER NOT NULL DEFAULT 0,   -- 操作管理员 ID；0 表示未能识别（理论上不应出现）
    admin_username TEXT    NOT NULL DEFAULT '',  -- 冗余用户名，保证历史可读
    method         TEXT    NOT NULL,             -- HTTP 方法（POST/PUT/PATCH/DELETE）
    path           TEXT    NOT NULL,             -- 实际请求路径（含具体 ID）
    action         TEXT    NOT NULL DEFAULT '',  -- 可读动作描述（如"更新渠道"）
    target         TEXT    NOT NULL DEFAULT '',  -- 路径参数取值（如 "id=12"）
    detail         TEXT    NOT NULL DEFAULT '',  -- 请求体摘要（已脱敏、已截断）
    status_code    INTEGER NOT NULL DEFAULT 0,   -- 回写给客户端的状态码
    latency_ms     INTEGER NOT NULL DEFAULT 0,   -- 处理耗时（毫秒）
    client_ip      TEXT    NOT NULL DEFAULT '',  -- 客户端 IP
    user_agent     TEXT    NOT NULL DEFAULT '',  -- User-Agent（已截断）
    created_at     INTEGER NOT NULL              -- 记录时间（Unix 秒）
);

-- 列表默认按时间倒序分页：created_at 建索引让分页扫描稳定走索引而非全表排序
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs (created_at DESC);

-- 按管理员筛选（"某管理员做过哪些操作"）
CREATE INDEX IF NOT EXISTS idx_audit_logs_admin_id ON audit_logs (admin_id);
