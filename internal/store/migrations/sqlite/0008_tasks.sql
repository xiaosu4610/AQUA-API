-- 迁移 0008：异步任务（图像 / 视频 / 音乐等生成类能力）
--
-- 背景：
--   对话类是"一问一答"的同步请求；而绘图、视频生成这类上游是**异步**的：
--   提交后返回一个任务号，需要轮询直到完成。若沿用同步转发模型，
--   客户端会长时间挂起（视频可能几分钟），既容易超时也无法查看进度。
--   因此需要独立的任务表来承载"提交 → 轮询 → 取结果"的生命周期。
--
-- 设计说明：
--   1) task_ref 是对外任务号（客户端用它查询），与自增主键分开：
--      连续自增的 ID 会泄露业务量，也便于将来迁移到分布式 ID；
--   2) kind 表示任务类别（image/video/music/...），provider 表示由哪个上游实现，
--      两者分开是因为"同一类别可由不同上游提供"（如绘图既可用 A 平台也可用 B 平台）；
--   3) params 保存原始请求参数的 JSON：任务失败重试或人工排查时，
--      必须能还原"当时到底提交了什么"，否则无法复现；
--   4) result_data 存上游返回的完整结果（可能是多个图片 URL 的数组），
--      result_url 冗余保存首个可预览地址，方便列表页直接展示缩略图；
--   5) quota 记录该任务实际扣减的额度（异步任务通常按次计费，
--      与按 token 计费的对话任务是两套口径，但都记在各自表里，报表可合并）。
--
-- 状态机（status）：
--   1 排队中 → 2 进行中 → 3 已完成
--              ↘ 4 已失败
--              ↘ 5 已取消
--   终态（3/4/5）之后不再变化；轮询器只更新非终态任务。

CREATE TABLE IF NOT EXISTS tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_ref    TEXT    NOT NULL,               -- 对外任务号
    user_id     INTEGER NOT NULL,               -- 归属用户
    token_id    INTEGER NOT NULL DEFAULT 0,     -- 使用的访问令牌（用于计费与审计）
    channel_id  INTEGER NOT NULL DEFAULT 0,     -- 实际执行的渠道
    kind        TEXT    NOT NULL,               -- image / video / music ...
    provider    TEXT    NOT NULL,               -- 上游适配器名（midjourney 等）
    model       TEXT    NOT NULL DEFAULT '',    -- 模型或动作名
    prompt      TEXT    NOT NULL DEFAULT '',    -- 提示词（便于列表展示与检索）
    params      TEXT    NOT NULL DEFAULT '{}',  -- 原始请求参数（JSON）
    status      INTEGER NOT NULL DEFAULT 1,     -- 见上方状态机
    progress    INTEGER NOT NULL DEFAULT 0,     -- 进度百分比 0-100
    upstream_id TEXT    NOT NULL DEFAULT '',    -- 上游任务号（轮询时使用）
    result_url  TEXT    NOT NULL DEFAULT '',    -- 首个结果地址（预览用）
    result_data TEXT    NOT NULL DEFAULT '',    -- 完整结果（JSON）
    error       TEXT    NOT NULL DEFAULT '',    -- 失败原因（已脱敏）
    quota       INTEGER NOT NULL DEFAULT 0,     -- 实际扣减额度
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0      -- 进入终态的时间；0 表示未结束
);

-- 对外任务号唯一：客户端凭它查询，重复即意味着生成逻辑有缺陷
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_ref ON tasks (task_ref);

-- 列表查询：用户看自己的任务，管理员看全部（按创建时间倒序分页）
CREATE INDEX IF NOT EXISTS idx_tasks_user_created ON tasks (user_id, created_at DESC);

-- 轮询器扫描：只查未结束的任务
CREATE INDEX IF NOT EXISTS idx_tasks_status_created ON tasks (status, created_at);
