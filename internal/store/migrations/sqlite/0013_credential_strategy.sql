-- 迁移 0013：上游凭据池的调度策略与冷却
--
-- 背景：
--   0005 建了"渠道密钥池"，但池内的挑选是纯随机、失败是一次次累加、
--   达到阈值即永久摘除。实际运营中这带来两个问题：
--     1) 无法按运维意图调度（有人想严格顺序、有人想轮询、有人想按权重倾斜）；
--     2) 429（上游限流，属于临时）也被计入失败，几次抖动就把好密钥永久移出池子，
--        池容量在事故中只会单调缩小，无法自愈。
--   本迁移补齐"策略 + 冷却 + 在途/限速"这三类调度维度所需的列。
--
-- 设计说明（重要概念区分）：
--   1) key_strategy 是【渠道级】配置：同一个渠道的整池凭据共用一种挑选方式。
--      取值：sequential / round_robin / weighted_random / least_recent / least_in_flight。
--      默认 least_in_flight（最少在途）：并发下最不容易把流量压在同一把凭据上。
--   2) key_cursor 是轮询游标，只被 round_robin 策略使用；持久化在库里而非内存，
--      是为了让进程重启后轮询位置不归零（否则重启会反复压在前几把凭据上）。
--   3) cooldown_until 是【时间语义】的临时停用：到点自动恢复，无需任何人工或后台动作；
--      它与 status=3（人工/永久语义的摘除）必须分开——
--      "冷却"是"这把钥匙歇一会儿"，"摘除"是"这把钥匙不要再用了"。
--   4) in_flight 记录"当前正在使用这把凭据的请求数"，供 least_in_flight 使用。
--      进程崩溃会残留该计数，因此选择时会用 last_used_at 做陈旧兜底（见 relay 侧说明）；
--      该列是统计/调度用途，不参与正确性判定。
--   5) rpm_limit 是每分钟请求上限（0 = 不限）；window_start / window_count 构成
--      固定窗口计数，用于判断"当前窗口内是否已用满"。
--   6) weight / priority 是凭据级调度参数：weight 供 weighted_random 使用（全为 0 时
--      退化为等概率），priority 供 sequential 使用（数值越大越优先）。
--
-- 兼容性：
--   SQLite 的 ADD COLUMN 必须给出默认值；这里所有新列都有默认值，
--   保证既有数据的语义不变（默认策略即旧的"尽量分散"意图）。
--   本迁移只加列、不改动任何既有列与索引。

-- ---------------------------------------------------------------------------
-- 一、渠道级：调度策略与轮询游标
-- ---------------------------------------------------------------------------

-- 渠道级凭据调度策略；默认"最少在途"，与并发场景下"尽量分散"的目标一致
ALTER TABLE channels ADD COLUMN key_strategy TEXT NOT NULL DEFAULT 'least_in_flight';

-- 轮询游标（仅 round_robin 使用）：已分配出去的下一个序号
ALTER TABLE channels ADD COLUMN key_cursor INTEGER NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------------------
-- 二、凭据级：调度参数与运行态
-- ---------------------------------------------------------------------------

-- 权重（weighted_random 使用）：数值越大命中概率越高；全为 0 时退化为等概率
ALTER TABLE channel_keys ADD COLUMN weight INTEGER NOT NULL DEFAULT 1;

-- 优先级（sequential 使用）：数值越大越优先；相同则取 id 最小者
ALTER TABLE channel_keys ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;

-- 在途请求数（least_in_flight 使用）：Acquire +1 / Release -1
ALTER TABLE channel_keys ADD COLUMN in_flight INTEGER NOT NULL DEFAULT 0;

-- 冷却截止时间（Unix 秒）：> 当前时间时该凭据暂时不参与选择；0 表示无冷却
ALTER TABLE channel_keys ADD COLUMN cooldown_until INTEGER NOT NULL DEFAULT 0;

-- 每分钟请求上限：0 表示不限速
ALTER TABLE channel_keys ADD COLUMN rpm_limit INTEGER NOT NULL DEFAULT 0;

-- 限速窗口起点（Unix 秒）：0 表示尚未开始计数
ALTER TABLE channel_keys ADD COLUMN window_start INTEGER NOT NULL DEFAULT 0;

-- 限速窗口内已用请求数：配合 window_start 判断是否用满
ALTER TABLE channel_keys ADD COLUMN window_count INTEGER NOT NULL DEFAULT 0;
