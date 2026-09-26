/**
 * 管理后台接口（需登录且 role = 10）。
 *
 * 意图（Why）：
 *   仪表盘、渠道、令牌、用户、日志五个管理页面共用这组接口；
 *   与契约「四、管理后台接口」表格一一对应，便于审计「哪些接口已被前端使用」。
 *
 * 流转（Flow）：
 *   views/admin/* → 本文件 → /api/admin/*
 *
 * 扩展（Extend）：
 *   新增管理接口：在此加函数 + 更新 types.ts。
 *   注意：渠道的 update 用 PUT（全量字段）；令牌/用户用 PUT 提交需变更字段（后端应支持部分更新）。
 */
import { api } from './client'
import type {
  AccessToken,
  AdminUpdateTokenPayload,
  AdminUser,
  Channel,
  ChannelPayload,
  ChannelTestResult,
  CreateTokenPayload,
  CreateTokenResult,
  CreateUserPayload,
  DashboardStats,
  LogQuery,
  Paged,
  UpdateUserPayload,
  UsageLog,
} from './types'

/* ── 仪表盘 ─────────────────────────────────────────────── */

/** GET /api/admin/dashboard：汇总卡片 + 趋势 + Top 模型 */
export function fetchDashboard(): Promise<DashboardStats> {
  return api.get<DashboardStats>('/admin/dashboard')
}

/* ── 渠道 ───────────────────────────────────────────────── */

/** GET /api/admin/channels：渠道列表（响应只有 masked_key） */
export function listChannels(paged: { page?: number; size?: number } = {}): Promise<Paged<Channel>> {
  return api.get<Paged<Channel>>('/admin/channels', paged)
}

/** GET /api/admin/channels/{id}：渠道详情 */
export function getChannel(id: number): Promise<Channel> {
  return api.get<Channel>(`/admin/channels/${id}`)
}

/** POST /api/admin/channels：新建渠道（api_key 为明文，仅存在于请求体） */
export function createChannel(payload: ChannelPayload): Promise<Channel> {
  return api.post<Channel>('/admin/channels', payload)
}

/** PUT /api/admin/channels/{id}：更新渠道（api_key 留空表示不修改） */
export function updateChannel(id: number, payload: Partial<ChannelPayload>): Promise<Channel> {
  return api.put<Channel>(`/admin/channels/${id}`, payload)
}

/** DELETE /api/admin/channels/{id}：删除渠道 */
export function deleteChannel(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/channels/${id}`)
}

/** POST /api/admin/channels/{id}/test：测活（后端会真实发起一次请求，可能较慢） */
export function testChannel(id: number): Promise<ChannelTestResult> {
  return api.post<ChannelTestResult>(`/admin/channels/${id}/test`)
}

/* ── 令牌 ───────────────────────────────────────────────── */

/** GET /api/admin/tokens：全部令牌 */
export function listAllTokens(paged: { page?: number; size?: number } = {}): Promise<Paged<AccessToken>> {
  return api.get<Paged<AccessToken>>('/admin/tokens', paged)
}

/** POST /api/admin/tokens：为指定用户创建令牌（响应含一次性明文 key） */
export function createTokenForUser(payload: CreateTokenPayload): Promise<CreateTokenResult> {
  return api.post<CreateTokenResult>('/admin/tokens', payload)
}

/** PUT /api/admin/tokens/{id}：更新令牌（改名 / 启停 / 额度 / 到期） */
export function updateToken(id: number, payload: AdminUpdateTokenPayload): Promise<AccessToken> {
  return api.put<AccessToken>(`/admin/tokens/${id}`, payload)
}

/** DELETE /api/admin/tokens/{id}：删除令牌 */
export function deleteToken(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/tokens/${id}`)
}

/* ── 用户 ───────────────────────────────────────────────── */

/** GET /api/admin/users：用户列表 */
export function listUsers(paged: { page?: number; size?: number } = {}): Promise<Paged<AdminUser>> {
  return api.get<Paged<AdminUser>>('/admin/users', paged)
}

/** POST /api/admin/users：新建用户 */
export function createUser(payload: CreateUserPayload): Promise<AdminUser> {
  return api.post<AdminUser>('/admin/users', payload)
}

/** PUT /api/admin/users/{id}：更新用户（含调整额度） */
export function updateUser(id: number, payload: UpdateUserPayload): Promise<AdminUser> {
  return api.put<AdminUser>(`/admin/users/${id}`, payload)
}

/** DELETE /api/admin/users/{id}：删除用户 */
export function deleteUser(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/users/${id}`)
}

/* ── 调用日志 ───────────────────────────────────────────── */

/** GET /api/admin/logs：全站调用日志（分页 + 多条件筛选） */
export function listAllLogs(query: LogQuery): Promise<Paged<UsageLog>> {
  return api.get<Paged<UsageLog>>('/admin/logs', { ...query })
}
