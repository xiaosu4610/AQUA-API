-- 迁移 0020：站点公告
--
-- 背景（为什么必须有它）：
--   站点需要一个"管理员发布、全体用户可见"的通知渠道：维护窗口、活动上线、
--   计费规则调整、临时故障说明……此前这些只能靠外部群聊口头传达，
--   用户进入站点时看不到任何官方说明，容易把已知问题当成故障去投诉。
--   本迁移新增公告表，把这份能力沉淀为可维护的持久化数据。
--
-- 设计说明：
--   1) level 用字符串（info/success/warning/danger）而非整数枚举：
--      它是"展示语义"而非业务状态，字符串自解释、便于排查，
--      新增一种语气也不需要改动数据库取值映射；合法取值由 model 层白名单校验；
--   2) pinned / enabled 用整数 0/1（SQLite 无原生布尔类型）：
--      pinned 控制前台是否置顶，enabled 控制是否发布（停用的草稿对前台不可见）；
--   3) publish_at 与 expire_at 用 Unix 秒，取值 0 表示【不限制】——
--      与全项目"0 = 未设置/永不"的约定一致（见 server.unixOrZero 的说明）：
--        · publish_at = 0 表示"立即发布"；
--        · expire_at  = 0 表示"永不过期"。
--      用"两列 + 0 哨兵"而非单独的 status 字段，是因为定时发布/自动下线
--      本质是时间窗口问题，由前台查询按 now 过滤即可，无需后台任务改状态；
--   4) content 不做数据库层长度约束：正文上限（20000 字符）是应用层策略，
--      改策略时不应需要动数据库结构（同 audit_logs.detail 的处理）；
--   5) 不引入外键（与既有迁移一致）：公告不归属任何实体，且越简单越可靠。
--
-- 索引设计：
--   idx_announcements_enabled_publish：公开端查询固定按「启用 + 时间窗口」过滤，
--   该复合索引让 ListActive 稳定走索引；enabled 选择性低（多数行可能都启用），
--   因此把 publish_at 放在其后，用于同 enabled 下的范围扫描与排序。
--
-- 兼容性：
--   全新表，对已有数据无影响；脚本使用 IF NOT EXISTS，可安全重复执行。

CREATE TABLE IF NOT EXISTS announcements (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    title      TEXT    NOT NULL,             -- 标题（应用层限制 ≤200 字符）
    content    TEXT    NOT NULL DEFAULT '',  -- 正文（纯文本，应用层限制 ≤20000 字符）
    level      TEXT    NOT NULL DEFAULT 'info', -- 展示语气：info/success/warning/danger
    pinned     INTEGER NOT NULL DEFAULT 0,   -- 是否置顶（1 置顶，0 普通）
    enabled    INTEGER NOT NULL DEFAULT 1,   -- 是否发布（1 发布，0 停用/草稿）
    publish_at INTEGER NOT NULL DEFAULT 0,   -- 开始展示时间（Unix 秒，0 = 立即发布）
    expire_at  INTEGER NOT NULL DEFAULT 0,   -- 停止展示时间（Unix 秒，0 = 永不过期）
    created_at INTEGER NOT NULL,             -- 创建时间（Unix 秒）
    updated_at INTEGER NOT NULL              -- 最近更新时间（Unix 秒）
);

-- 公开端 ListActive 的核心过滤条件（enabled + 时间窗口），建复合索引以稳定命中
CREATE INDEX IF NOT EXISTS idx_announcements_enabled_publish ON announcements (enabled, publish_at);
