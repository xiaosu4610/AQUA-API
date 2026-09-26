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
import type { AuthResult, AuthUser, EmailCodeResult, LoginPayload, RegisterPayload } from './types'

/** POST /api/auth/login：用户名 + 密码登录 */
export function login(payload: LoginPayload): Promise<AuthResult> {
  return api.post<AuthResult>('/auth/login', payload)
}

/**
 * POST /api/auth/admin-login：超管入口登录（只提交密码，不提交用户名）。
 *
 * 为什么单独一个接口而不是把用户名也填成隐藏值：后端需要按"仅密码"的语义
 * 枚举管理员，并对管理员数量过多的情况给出明确拒绝理由（见后端注释）。
 */
export function adminLogin(password: string): Promise<AuthResult> {
  return api.post<AuthResult>('/auth/admin-login', { password })
}

/**
 * POST /api/auth/register：受站点 registration_enabled 开关控制（由调用方先行校验）。
 *
 * 站点开启"注册必须邮箱验证码"时，后端会强制校验 email + code；
 * 未开启时二者可选（留空则不发送，避免后端把空字符串当成显式空值）。
 *
 * invite_code 为可选邀请码（来自邀请链接 ?invite=CODE）：
 * 用交叉类型在本函数局部扩展，避免改动共享的 RegisterPayload 定义；
 * 后端对非法邀请码会忽略并照常注册，因此这里只在有值时透传。
 */
export function register(payload: RegisterPayload & { invite_code?: string }): Promise<AuthResult> {
  const body: RegisterPayload & { invite_code?: string } = { username: payload.username, password: payload.password }
  if (payload.email) body.email = payload.email
  if (payload.code) body.code = payload.code
  if (payload.invite_code) body.invite_code = payload.invite_code
  return api.post<AuthResult>('/auth/register', body)
}

/**
 * POST /api/auth/email-code：申请注册邮箱验证码。
 *
 * 后端返回 cooldown（冷却秒数）与 expires_in（有效期秒数），
 * 前端据此驱动倒计时，避免把这两个数值硬编码在页面里。
 */
export function sendEmailCode(email: string): Promise<EmailCodeResult> {
  return api.post<EmailCodeResult>('/auth/email-code', { email })
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
