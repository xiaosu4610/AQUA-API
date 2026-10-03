/**
 * 告警外发（通道 + 总开关）接口。
 *
 * 意图（Why）：
 *   渠道熔断、成功率自动停用、账号锁定这类事件此前只落服务端日志，
 *   站点出事了要靠第二天翻日志才发现。本文件对应的后台页把「外发通道」显式管起来，
 *   让故障在几秒内出现在站长的手机上。
 *
 *   页面（/admin/alert-channels）只关心本文件的函数，不关心路径与字段名；
 *   通道类型与事件目录由后端 /alert-channel-kinds 下发，前端不硬编码一份类型表，
 *   后端新增类型时这里无需改动。
 *
 * 安全约定（Why 写在最前面，因为违反它的后果是凭据外泄）：
 *   1) 后端只回传 target_masked，永不回传明文目标——钉钉/企微的 URL 自带凭据；
 *   2) 编辑时 target 传空串表示「沿用原值」（界面上目标是脱敏的，抄不回原样）；
 *   3) 「发送测试」同步返回结果，让配错地址当场暴露，而不是等真出事才发现告警没到。
 *
 * 流转（Flow）：
 *   AlertChannelsPage → listAlertChannels / listAlertChannelKinds
 *                      / createAlertChannel / updateAlertChannel
 *                      / deleteAlertChannel / testAlertChannel
 *                      / getAlertSettings / updateAlertSettings
 *                      → /api/admin/alert-channels[...] 与 /api/admin/alert-settings
 *
 * 扩展（Extend）：
 *   后端新增一种通道类型时本文件无需改动——类型与事件目录走 /alert-channel-kinds。
 *   注意：路径不含 /api 前缀，前缀由 client.ts 的 baseURL 统一拼接。
 */
import { api } from './client'

/** 一条告警通道（target 已脱敏，永不回传明文）。 */
export interface AlertChannel {
  id: number
  name: string
  /** 通道类型标识（email / webhook / dingtalk / wecom），提交时原样带回 */
  kind: string
  /** 通道类型的中文描述（由后端下发，前端不自行映射） */
  kind_text: string
  /** 脱敏后的投递目标，例如 oapi.dingtalk.com/…/send?access_token=*** */
  target_masked: string
  /** 订阅的事件键；空数组 = 订阅全部 */
  events: string[]
  enabled: boolean
  /** 创建时间（Unix 秒） */
  created_at: number
  /** 更新时间（Unix 秒） */
  updated_at: number
}

/** GET /api/admin/alert-channels 的响应 */
export interface AlertChannelListResult {
  items: AlertChannel[]
  total: number
}

/** 通道类型 / 事件目录里的一个选项（value 为提交值，text 为展示文案） */
export interface AlertChannelOption {
  value: string
  text: string
}

/** GET /api/admin/alert-channel-kinds 的响应：下拉与多选的唯一数据来源 */
export interface AlertChannelKinds {
  kinds: AlertChannelOption[]
  events: AlertChannelOption[]
}

/**
 * 新建/修改告警通道的请求体。
 *
 * events 是逗号分隔字符串（不是数组）：后端契约如此，空串 = 订阅全部。
 * 编辑时 target 传空串 = 沿用原值；新建时必填（由页面校验兜底）。
 */
export interface AlertChannelPayload {
  name: string
  kind: string
  target: string
  events: string
  enabled: boolean
}

/** GET /api/admin/alert-settings 与 POST 的响应：外发总开关状态 */
export interface AlertSettings {
  /** 默认关闭；开启后才会真正把告警投递到通道 */
  enabled: boolean
}

/** POST /api/admin/alert-channels/{id}/test 的响应 */
export interface AlertTestResult {
  ok: boolean
  /** 后端给出的人话提示（失败时由页面直接展示） */
  message: string
}

/** GET /api/admin/alert-channels：读取全部通道（目标脱敏） */
export function listAlertChannels(): Promise<AlertChannelListResult> {
  return api.get<AlertChannelListResult>('/admin/alert-channels')
}

/** GET /api/admin/alert-channel-kinds：通道类型与事件目录 */
export function listAlertChannelKinds(): Promise<AlertChannelKinds> {
  return api.get<AlertChannelKinds>('/admin/alert-channel-kinds')
}

/** POST /api/admin/alert-channels：新建通道 */
export function createAlertChannel(payload: AlertChannelPayload): Promise<AlertChannel> {
  return api.post<AlertChannel>('/admin/alert-channels', payload)
}

/** PUT /api/admin/alert-channels/{id}：修改通道（target 传空串 = 沿用原值） */
export function updateAlertChannel(id: number, payload: AlertChannelPayload): Promise<AlertChannel> {
  return api.put<AlertChannel>(`/admin/alert-channels/${id}`, payload)
}

/** DELETE /api/admin/alert-channels/{id}：删除通道 */
export function deleteAlertChannel(id: number): Promise<{ ok: boolean }> {
  return api.delete<{ ok: boolean }>(`/admin/alert-channels/${id}`)
}

/**
 * POST /api/admin/alert-channels/{id}/test：发一条测试告警。
 *
 * 同步返回成败：这里的目的就是让站长当场确认地址可用，
 * 异步发送会让「点了没反应」与「地址错了」无法区分。
 * 失败时后端返回 502，client.ts 会把 error.message 抛成 ApiError（含失败原因）。
 */
export function testAlertChannel(id: number): Promise<AlertTestResult> {
  return api.post<AlertTestResult>(`/admin/alert-channels/${id}/test`, {})
}

/** GET /api/admin/alert-settings：读取告警外发总开关 */
export function getAlertSettings(): Promise<AlertSettings> {
  return api.get<AlertSettings>('/admin/alert-settings')
}

/** POST /api/admin/alert-settings：切换告警外发总开关（后端切换后立即生效） */
export function updateAlertSettings(enabled: boolean): Promise<AlertSettings> {
  return api.post<AlertSettings>('/admin/alert-settings', { enabled })
}
