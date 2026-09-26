-- 迁移 0016：模型实体与渠道级模型 ID 映射
--
-- 背景：
--   在此之前，"模型"只是渠道 channels.models 列里的一串字符串（逗号分隔清单），
--   没有实体、没有元数据，也无法把"用户请求的模型名"与"上游真正的模型名"分开。
--   后果有三个：
--     1) 管理员无法维护统一的模型清单（展示名、厂商、上下文长度、能力标签），
--        模型广场只能靠翻渠道去猜；
--     2) 上游模型名与对外模型名必须完全一致，无法做别名或版本切换；
--     3) 删除/改名模型时无从知道"有哪些令牌/分组/渠道还在引用它"。
--   本迁移把模型提升为一等实体，并新增渠道级映射，实现双向解析。
--
-- 设计说明：
--   1) models.name 是对外模型名（客户端请求时使用的名字），唯一；
--      capabilities 用 JSON 数组字符串存（如 ["chat","stream","tools"]）。
--      之所以不单独建能力表：能力是稀疏的小集合，单独建表会让"读取一个模型"
--      变成多表连接，收益不抵成本；需要按能力检索时可用 JSON 函数或应用层过滤。
--   2) channel_model_mappings 描述"对外模型名 ↔ 上游模型名"的对应关系：
--      请求方向用 public_model 匹配请求里的模型名，产出 upstream_model；
--      响应方向用 upstream_model 匹配上游回包里的模型名，回写成 public_model。
--      两侧都支持【尾部通配符 *】（前缀族，如 deepseek-*），最长前缀优先。
--   3) 唯一索引建在 (channel_id, upstream_model)：同一渠道下同一上游模型名
--      只能有一条映射，避免"同一个上游名被映射到两个对外名"导致的歧义。
--
-- 兼容性（重要）：
--   本迁移【不修改】channels.models 列。它继续作为"渠道声明支持哪些模型"的
--   快速清单被路由与模型广场使用；模型实体与映射是叠加能力，不是替代关系。
--   因此对已有部署完全向后兼容：不配置映射时行为与迁移前一致（模型名原样透传）。
--
-- 不引入外键约束：与 0011 保持一致。历史数据里可能存在指向已删除渠道的映射，
-- 加外键会让已有部署启动失败；运行期按"渠道不存在则忽略其映射"处理。

CREATE TABLE IF NOT EXISTS models (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT    NOT NULL,              -- 对外模型名（唯一，客户端请求使用）
    display_name   TEXT    NOT NULL DEFAULT '',   -- 展示名（留空时界面回退 name）
    vendor         TEXT    NOT NULL DEFAULT '',   -- 厂商（如 deepseek / openai）
    description    TEXT    NOT NULL DEFAULT '',   -- 说明
    context_length INTEGER NOT NULL DEFAULT 0,    -- 上下文长度（token），0 表示未登记
    enabled        INTEGER NOT NULL DEFAULT 1,    -- 是否启用（停用后不出现在清单）
    capabilities   TEXT    NOT NULL DEFAULT '[]', -- 能力标签 JSON 数组，如 ["chat","stream"]
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

-- 对外模型名唯一：它是被令牌白名单、渠道清单与映射引用的标识
CREATE UNIQUE INDEX IF NOT EXISTS idx_models_name ON models (name);

-- 厂商筛选（后台按厂商过滤）与启用状态筛选是最常用的两个查询维度
CREATE INDEX IF NOT EXISTS idx_models_vendor ON models (vendor);
CREATE INDEX IF NOT EXISTS idx_models_enabled ON models (enabled);

CREATE TABLE IF NOT EXISTS channel_model_mappings (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id     INTEGER NOT NULL,              -- 所属渠道
    upstream_model TEXT    NOT NULL,              -- 上游模型名（支持尾部通配符 *）
    public_model   TEXT    NOT NULL,              -- 对外模型名（支持尾部通配符 *）
    priority       INTEGER NOT NULL DEFAULT 0,    -- 优先级，数值越大越优先
    enabled        INTEGER NOT NULL DEFAULT 1,    -- 是否启用
    remark         TEXT    NOT NULL DEFAULT '',   -- 备注（说明该映射用途）
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

-- 同一渠道下同一上游模型名唯一：同上游名映射到多个对外名会产生歧义，
-- 使"上游回包该回写成哪个对外名"变得不确定，直接在数据层拦住。
CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_model_mappings_unique
    ON channel_model_mappings (channel_id, upstream_model);

-- 转发热路径按 (渠道, 启用状态) 取映射，故建此组合索引
CREATE INDEX IF NOT EXISTS idx_channel_model_mappings_channel_enabled
    ON channel_model_mappings (channel_id, enabled);
