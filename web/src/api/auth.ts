/**
 * 认证接口：登录、注册、当前用户、退出。
 *
 * 意图（Why）：
 *   把「会话令牌的获取与吊销」收敛到一处，供 stores/auth.ts 调用；
 *   视图层不直接触碰令牌，只操作 store 的语义化方法。
 *
 * 流转（Flow）：
 *   stores/auth.ts → login()/register() → POST /api/auth/* → 返回 session_token
 *                 → client.setSessionToken() 落盘
 *   /auth/me 在每次应用启动时调用一次，用于校正本地用户快照（额度/角色可能已变）。
 *
 * 扩展（Extend）：
 *   新增认证方式（如 OAuth）时在此加函数，并在 stores/auth.ts 暴露对应 action。
 */
import { api } from './client'
import type { AuthResult, AuthUser, LoginPayload, RegisterPayload } from './types'

/** POST /api/auth/login：用户名 + 密码登录 */
export function login(payload: LoginPayload): Promise<AuthResult> {
  return api.post<AuthResult>('/auth/login', payload)
}

/** POST /api/auth/register：受站点 registration_enabled 开关控制（由调用方先行校验） */
export function register(payload: RegisterPayload): Promise<AuthResult> {
  const body: RegisterPayload = { username: payload.username, password: payload.password }
  // email 为可选字段：留空时不发送，避免后端把它当成「显式空值」
  if (payload.email) body.email = payload.email
  return api.post<AuthResult>('/auth/register', body)
}

/**
 * GET /api/auth/me：读取当前登录用户。
 *
 * 契约只写了「当前登录用户信息」，未明确响应是 { ...user } 还是 { user: {...} }，
 * 这里做兼容解析（优先取 user 字段），待后端定稿后可精简。
 */
export async function fetchMe(): Promise<AuthUser> {
  const data = await api.get<AuthUser & { user?: AuthUser }>('/auth/me')
  return (data?.user ?? data) as AuthUser
}

/** POST /api/auth/logout：吊销当前会话 */
export function logout(): Promise<unknown> {
  return api.post<unknown>('/auth/logout')
}
