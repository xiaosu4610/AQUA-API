-- 迁移 0006：模型计费价格表
--
-- 背景：
--   此前 usage_logs.quota 恒为 0，令牌与用户的 used_quota 从不增长——
--   也就是说"额度"只是数据库里的一个摆设字段，永远不会耗尽。
--   本迁移引入价格表，让"按用量计费"真正落地。
--
-- 计费口径（务必与 model/model_price.go 的实现保持一致）：
--   quota = (prompt_tokens × prompt_price + completion_tokens × completion_price) / 1_000_000
--   即价格字段表示「每 100 万 token 消耗的站点额度单位」。
--   为什么用"每百万"而不是"每千"：
--     主流上游都按百万 token 报价（如 $3 / 1M tokens），
--     直接用同一口径可以照抄官方价格，减少一次换算就少一处出错机会。
--
-- 设计说明：
--   1) 额度单位是站点自定义的整数单位（不是货币），由站长决定 1 单位值多少钱；
--      这样变更币种或调价倍率时不需要改动表结构；
--   2) model 支持前缀通配（如 "gpt-4*"），匹配优先级为
--      「精确 → 最长前缀通配 → 全局通配 *」，用于给一整类模型定价；
--   3) 按分组（group_name）分别定价：同一模型在不同业务线下可以有不同倍率；
--   4) 全部使用 INTEGER 存储，避免浮点误差累积（金额类数据一律不用 REAL）。

CREATE TABLE IF NOT EXISTS model_prices (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    model            TEXT    NOT NULL,               -- 模型名，支持 "gpt-4*" 与 "*" 通配
    prompt_price     INTEGER NOT NULL DEFAULT 0,     -- 每 1M 输入 token 的额度
    completion_price INTEGER NOT NULL DEFAULT 0,     -- 每 1M 输出 token 的额度
    group_name       TEXT    NOT NULL DEFAULT 'default',
    enabled          INTEGER NOT NULL DEFAULT 1,     -- 1=启用 0=停用（停用视为未定价）
    remark           TEXT    NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- 唯一约束：同一分组下同一模型规则只能有一条，避免"两条价格谁生效"的歧义
CREATE UNIQUE INDEX IF NOT EXISTS idx_model_prices_model_group
    ON model_prices (model, group_name);

-- 计费热路径：按分组取全部启用规则（规则数量通常在几十条内，可整表缓存）
CREATE INDEX IF NOT EXISTS idx_model_prices_group_enabled
    ON model_prices (group_name, enabled);
