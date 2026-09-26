/**
 * 用户门户接口（需登录）。
 *
 * 意图（Why）：
 *   门户三个页面（概览 / 令牌 / 日志）共用这组接口；
 *   集中在此便于与契约的「三、用户门户接口」表格逐行对照。
 *
 * 流转（Flow）：
 *   views/console/* → 本文件 → /api/user/*
 *
 * 扩展（Extend）：
 *   新增门户接口：在此加函数 + 更新 types.ts。
 *   注意：创建令牌会返回一次性明文 key，函数返回类型必须是 CreateTokenResult。
 */
import { api } from './client'
import type {
  AccessToken,
  CreateTokenPayload,
  CreateTokenResult,
  LogQuery,
  Paged,
  UpdateTokenPayload,
  UsageLog,
  UsageStats,
} from './types'

/** GET /api/user/tokens：我的访问令牌列表 */
export function listMyTokens(paged: { page?: number; size?: number } = {}): Promise<Paged<AccessToken>> {
  return api.get<Paged<AccessToken>>('/user/tokens', paged)
}

/** POST /api/user/tokens：创建访问令牌（响应含一次性明文 key） */
export function createMyToken(payload: CreateTokenPayload): Promise<CreateTokenResult> {
  return api.post<CreateTokenResult>('/user/tokens', payload)
}

/** PATCH /api/user/tokens/{id}：改名 / 启停 */
export function updateMyToken(id: number, payload: UpdateTokenPayload): Promise<AccessToken> {
  return api.patch<AccessToken>(`/user/tokens/${id}`, payload)
}

/** DELETE /api/user/tokens/{id}：删除令牌 */
export function deleteMyToken(id: number): Promise<unknown> {
  return api.delete<unknown>(`/user/tokens/${id}`)
}

/** GET /api/user/usage?days=7：我的用量统计（days 由页面控件决定） */
export function fetchMyUsage(days: number): Promise<UsageStats> {
  return api.get<UsageStats>('/user/usage', { days })
}

/** GET /api/user/logs：我的调用日志（分页 + 筛选） */
export function listMyLogs(query: LogQuery): Promise<Paged<UsageLog>> {
  return api.get<Paged<UsageLog>>('/user/logs', { ...query })
}
