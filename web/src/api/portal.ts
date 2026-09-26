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
import { fetchModelPlaza } from './site'
import type {
  AccessToken,
  CreateOrderPayload,
  CreateTokenPayload,
  CreateTokenResult,
  LogQuery,
  OrderQuery,
  Paged,
  PaymentOrder,
  PlazaGroup,
  Task,
  TaskQuery,
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

/**
 * 读取门户可用的分组清单（用于创建令牌时选择「所属分组」）。
 *
 * 为什么复用公开的模型广场接口：门户端没有后台的 GET /api/admin/groups 权限，
 * 而模型广场（GET /api/models）本就对登录用户开放，且已经带回了
 * 分组的 name / label / ratio——正好是下拉所需的全部信息。
 * 代价是只列出「有模型且已启用」的分组；这正是给用户选择时想要的口径
 * （没有模型的分组选了也调不通）。
 */
export async function listAvailableGroups(): Promise<PlazaGroup[]> {
  const plaza = await fetchModelPlaza()
  return plaza.groups ?? []
}

/** GET /api/user/usage?days=7：我的用量统计（days 由页面控件决定） */
export function fetchMyUsage(days: number): Promise<UsageStats> {
  return api.get<UsageStats>('/user/usage', { days })
}

/** GET /api/user/logs：我的调用日志（分页 + 筛选） */
export function listMyLogs(query: LogQuery): Promise<Paged<UsageLog>> {
  return api.get<Paged<UsageLog>>('/user/logs', { ...query })
}

/* ── 异步任务 ───────────────────────────────────────────── */

/** GET /api/user/tasks：我的异步任务（图像/视频等生成类能力） */
export function listMyTasks(query: TaskQuery = {}): Promise<Paged<Task>> {
  return api.get<Paged<Task>>('/user/tasks', { ...query })
}

/* ── 充值 ───────────────────────────────────────────────── */

/**
 * GET /api/user/orders：我的充值记录。
 *
 * 说明：不提供"提交异步任务"的入口——任务接口走 /v1/tasks 并需要访问令牌，
 * 门户页面只负责展示结果，避免在浏览器里暴露访问令牌。
 */
export function listMyOrders(query: OrderQuery = {}): Promise<Paged<PaymentOrder>> {
  return api.get<Paged<PaymentOrder>>('/user/orders', { ...query })
}

/** POST /api/user/orders：下单（只传金额与通道，额度由服务端计算） */
export function createOrder(payload: CreateOrderPayload): Promise<PaymentOrder> {
  return api.post<PaymentOrder>('/user/orders', payload)
}

/**
 * GET /api/user/orders/{tradeNo}：查询单笔订单。
 *
 * 用途：用户在第三方收银台支付完成后回到本站，前端轮询该接口确认到账。
 */
export function getMyOrder(tradeNo: string): Promise<PaymentOrder> {
  return api.get<PaymentOrder>(`/user/orders/${encodeURIComponent(tradeNo)}`)
}
