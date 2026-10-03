-- 迁移 0050：告警通知通道
--
-- 意图（Why）：
--   此前渠道熔断、成功率自动停用、登录锁定这些关键事件只打到 stdout。
--   那意味着"站点出事了"这件事只有站长主动翻日志才会知道——
--   而凌晨三点渠道全挂、早上才发现，是中转站最常见也最贵的故障形态。
--   本迁移提供"事件往哪儿发"的配置：邮件 / Webhook / 钉钉 / 企业微信。
--
-- 取值语义：
--   kind     通道类型：email / webhook / dingtalk / wecom
--   target   投递目标。email 为邮箱地址；其余三种为完整 URL
--            （钉钉/企微的机器人地址本身就带 access_token，故视为密文）
--   events   订阅的事件键，英文逗号分隔；空串 = 订阅全部事件
--   enabled  0 = 临时停用（配置留着，只是不发）
--
-- 为什么 target 视为密文：
--   钉钉与企业微信的机器人 URL 里直接含 access_token，谁拿到这条记录
--   谁就能往那个群发消息。因此接口层一律脱敏返回（见 handler_alert_channel.go），
--   后台列表只能看到"scheme://host/…"，改目标必须整条重填。
--
-- 为什么不复用 settings 的键值表：
--   告警通道是"一组结构相同的记录"（要列表、要启停、要按事件订阅），
--   塞进单个设置项就得自己维护序列化与并发覆盖，收益不抵成本。
--
-- 本迁移【不预置任何行数据】：不得内置任何收件地址或 Webhook URL，
-- 否则全新部署的站点会把站内事件发到第三方地址，属严重的数据外泄。
--
-- 流转（Flow）：
--   业务触发点（巡检转坏 / 自动停用 / 登录锁定）
--     → notify.Dispatcher.Alert(Alert)
--     → 总开关（alert_enabled）判定 → 去重节流 → 按 events 过滤 → 投递
--
-- 扩展（Extend）：
--   新增通道类型（如飞书、Telegram）：model.NormalizeAlertChannelKind 认新的取值
--   + notify 里加一个 sender；表结构不用动。
CREATE TABLE IF NOT EXISTS alert_channels (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL DEFAULT '',
    target      TEXT    NOT NULL DEFAULT '',
    events      TEXT    NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

-- 发送时按"启用"过滤，全表扫的行数等于站长配置的通道数（通常 <10），
-- 不建索引：为一个必然很小的表建索引，收益低于索引本身带来的写放大。
CREATE INDEX IF NOT EXISTS idx_alert_channels_enabled
    ON alert_channels (enabled);
