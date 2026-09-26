-- 迁移 0011：模型分组与计费倍率
--
-- 背景：
--   渠道（channels.group）与计价规则（model_prices.group_name）都已经有"分组"
--   这个概念，但分组本身没有任何实体——它只是一个散落的字符串。
--   后果有三个：
--     1) 管理员无法知道"系统里到底有哪几个分组"，只能靠翻渠道列表去猜；
--     2) 无法为分组设置"计费倍率"（如"金币分组按 1.5 倍计价"），
--        而按分组差异化定价是网关运营的常见诉求；
--     3) 分组名写错时没有任何提示，渠道会静默地从所有请求中消失。
--   本迁移把分组提升为一等实体。
--
-- 设计说明：
--   1) name 是分组标识（同时也被 channels.group / model_prices.group_name 引用），
--      强制小写以避免 "VIP" 与 "vip" 被当成两个分组；
--   2) ratio 是计费倍率，用【百分比整数】表示：100 = 1.0 倍、150 = 1.5 倍。
--      不用浮点是全项目的统一口径（见 model.ModelPrice 的说明），
--      额度扣减每天都在发生，浮点误差会累积成对不上的账；
--   3) 默认分组（default）在本迁移中直接插入：
--      它是历史数据的隐含分组，若不存在，倍率查询会退回默认 100，
--      但管理员在界面上将看不到它，容易产生"为什么改不了默认倍率"的困惑。
--
-- 兼容性：
--   本表不引入外键约束。理由是历史数据里可能存在不在本表中的分组名
--   （渠道先于分组被创建），若加外键会让已有部署启动失败。
--   运行期遇到未知分组时按默认倍率（100）处理，见 relay.Billing.groupRatio。

CREATE TABLE IF NOT EXISTS model_groups (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,              -- 分组标识（小写）
    display_name TEXT    NOT NULL DEFAULT '',   -- 展示名（留空时界面回退 name）
    ratio        INTEGER NOT NULL DEFAULT 100,  -- 计费倍率（百分比，100 = 1.0 倍）
    description  TEXT    NOT NULL DEFAULT '',   -- 说明（便于说明该分组面向谁）
    enabled      INTEGER NOT NULL DEFAULT 1,    -- 是否启用（停用后不参与新请求）
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- 分组名唯一：它是被其他表引用的标识，重复会让倍率取值变得不确定
CREATE UNIQUE INDEX IF NOT EXISTS idx_model_groups_name ON model_groups (name);

-- 初始化默认分组。
--
-- INSERT OR IGNORE 保证幂等：本迁移在已存在的部署上重复执行也不会报错，
-- 且不会覆盖管理员已经改过的倍率。
INSERT OR IGNORE INTO model_groups (name, display_name, ratio, description, enabled, created_at, updated_at)
VALUES ('default', '默认分组', 100, '未指定分组时使用的分组', 1, strftime('%s', 'now'), strftime('%s', 'now'));
