-- 迁移 0015：渠道类型标识与类型专属扩展配置
--
-- 背景（为什么必须有它）：
--   渠道表原先只持久化一个兼容用的数字 type（恒为 1 = OpenAI 兼容），
--   没有地方记录"这条渠道到底是谁"（Azure / Anthropic / Gemini …）。
--   结果是转发层无法据此选择正确的协议适配器与鉴权方式，Azure / Anthropic
--   等渠道即使配好了也只会按 OpenAI 兼容去发请求，端到端打不通。
--   本迁移补上两个承载列：
--     · type_key      —— 渠道类型标识（与 channeltype 目录的 Key 对应，
--                        空串表示"未指定"，转发时回退为 OpenAI 兼容，保证老渠道不变）；
--     · extra_config  —— 类型专属参数（JSON 对象），承载 deployment / api_version /
--                        region 等差异项，供协议适配器组装请求时读取。
--
-- 设计说明：
--   1) type_key 默认空串而非某种具体类型：默认值一旦改变，历史渠道的转发语义
--      就会被动变化（本该原样透传的却走了专用适配器），属于静默故障；
--   2) extra_config 默认 '{}'：空 JSON 对象便于读取端统一按"无扩展配置"处理，
--      避免 NULL 与空串两种"没有值"的表示并存；
--   3) 两列一起加，避免为同一目的拆成两次迁移（迁移只允许新增、禁止修改）。
--
-- 兼容性：
--   两列均为 NOT NULL + 常量默认值，SQLite 允许对已有表直接 ADD COLUMN；
--   已有渠道行会一次性获得默认值，转发行为与迁移前完全一致。

ALTER TABLE channels ADD COLUMN type_key TEXT NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN extra_config TEXT NOT NULL DEFAULT '{}';
