/**
 * 统一 API 客户端：所有 HTTP 请求的唯一出口。
 *
 * 意图（Why）：
 *   1) 会话令牌只在登录后写入一处存储，其余代码无需关心如何附加 Authorization；
 *   2) 后端错误格式是统一的（{ error: { message, type, code } }），
 *      在客户端统一翻译成人话（ApiError），视图层只需 catch 后展示 message；
 *   3) 收到 401 说明会话已失效，集中处理「清登录态 + 跳登录页」，
 *      避免每个页面各写一遍。
 *
 * 流转（Flow）：
 *   .vue → src/api/{site,auth,portal,admin}.ts → api.get/post/... → axios
 *        → 请求拦截器附加 Bearer 会话令牌 → 后端 /api/*
 *        → 响应拦截器：成功返回 data；失败抛 ApiError；401 触发失效回调
 *
 * 扩展（Extend）：
 *   新增接口：在 api 模块里用 api.get<T>() 等调用，路径统一写 "/xxx"（不含 /api 前缀，
 *   前缀由 baseURL 统一拼接）。若要新增全局请求头（如 X-Request-Id），在下方 request 拦截器加。
 *   注意：令牌存储键名变更需与 stores/auth.ts 保持一致（两边共用本文件的读写函数）。
 */
import axios, { AxiosError, type AxiosRequestConfig } from 'axios'

import type { ApiErrorBody } from './types'

/** API 前缀：契约约定管理/门户接口前缀为 /api */
const API_PREFIX = import.meta.env.VITE_API_BASE || '/api'

/** 会话令牌与用户快照在 localStorage 中的键名（刷新页面后仍保持登录态） */
const TOKEN_KEY = 'aqua.session_token'
const USER_KEY = 'aqua.session_user'

/**
 * 需要豁免 401 自动跳转的路径：登录/注册本身就是「凭据可能无效」的场景，
 * 若也触发跳转，用户在登录页输错密码会被强制刷新，体验很差。
 */
const UNAUTHORIZED_EXEMPT = ['/auth/login', '/auth/register']

/** 各类 HTTP 状态码的兜底提示（后端未返回 message 时使用） */
const FALLBACK_MESSAGE: Record<number, string> = {
  400: '请求参数有误',
  401: '登录已失效，请重新登录',
  403: '没有权限执行该操作',
  404: '请求的资源不存在',
  409: '操作冲突，该记录可能已存在',
  429: '请求过于频繁或额度已耗尽',
  500: '服务端出错了，请稍后重试',
  503: '暂时没有可用的上游渠道',
}

/**
 * 业务错误：把后端错误体与网络异常统一成一种类型。
 *
 * 为什么要自定义而不是直接用 AxiosError：
 *   视图层只关心「给人看的一句话 + HTTP 状态码」，不需要理解 axios 的错误结构。
 */
export class ApiError extends Error {
  /** HTTP 状态码；0 表示请求未到达服务端（网络错误/超时） */
  readonly status: number
  /** 后端错误码，如 invalid_api_key */
  readonly code: string
  /** 后端错误类型，如 invalid_request_error */
  readonly type: string

  constructor(message: string, status = 0, code = '', type = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.type = type
  }

  /** 是否为「未登录/会话失效」类错误 */
  get isUnauthorized(): boolean {
    return this.status === 401
  }
}

/** 会话令牌读写：统一走本文件，避免多处硬编码 localStorage 键名 */
export function getSessionToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    // 隐私模式等场景 localStorage 不可用，此时退化为「仅内存登录」（刷新即失效）
    return ''
  }
}

export function setSessionToken(token: string): void {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* 忽略：不可写时不影响当前会话内使用 */
  }
}

/** 读取登录用户快照（用于刷新后立即还原角色判断，随后由 /auth/me 校正） */
export function getCachedUser<T>(): T | null {
  try {
    const raw = localStorage.getItem(USER_KEY)
    return raw ? (JSON.parse(raw) as T) : null
  } catch {
    return null
  }
}

export function setCachedUser(user: unknown | null): void {
  try {
    if (user) localStorage.setItem(USER_KEY, JSON.stringify(user))
    else localStorage.removeItem(USER_KEY)
  } catch {
    /* 忽略 */
  }
}

/** 清空本地登录态（令牌 + 用户快照） */
export function clearSession(): void {
  setSessionToken('')
  setCachedUser(null)
}

/** 会话失效回调：由 main.ts 注入（清 store 状态 + 跳转登录页） */
type UnauthorizedHandler = () => void
let unauthorizedHandler: UnauthorizedHandler | null = null

export function setUnauthorizedHandler(handler: UnauthorizedHandler): void {
  unauthorizedHandler = handler
}

/** 去掉查询参数中的 undefined / null / 空字符串，避免后端收到 `?model=` 这类无意义条件 */
function cleanParams(params?: Record<string, unknown>): Record<string, unknown> | undefined {
  if (!params) return undefined
  const out: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    out[key] = value
  }
  return out
}

/** 把 axios 抛出的任意异常统一转换为 ApiError */
function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) return error

  const axiosError = error as AxiosError<ApiErrorBody>
  if (!axiosError.response) {
    if (axiosError.code === 'ECONNABORTED' || axiosError.code === 'ETIMEDOUT') {
      return new ApiError('请求超时，请检查网络后重试', 0, 'timeout')
    }
    return new ApiError('无法连接服务器，请确认后端服务是否已启动', 0, 'network_error')
  }

  const { status, data } = axiosError.response
  const message = data?.error?.message || FALLBACK_MESSAGE[status] || `请求失败（HTTP ${status}）`
  return new ApiError(message, status, data?.error?.code ?? '', data?.error?.type ?? '')
}

/** 内部请求实现：附加令牌 → 统一错误转换 → 401 集中处理 */
async function request<T>(config: AxiosRequestConfig): Promise<T> {
  const token = getSessionToken()
  const headers: Record<string, string> = { ...(config.headers as Record<string, string>) }
  if (token) headers.Authorization = `Bearer ${token}`

  try {
    const response = await axios.request<T>({
      ...config,
      baseURL: API_PREFIX,
      headers,
      // 大文件上传/长任务不在本阶段范围，30s 足够覆盖测活等慢接口
      timeout: 30000,
    })
    return response.data
  } catch (error) {
    const apiError = toApiError(error)
    const url = config.url ?? ''
    if (apiError.status === 401 && !UNAUTHORIZED_EXEMPT.some((path) => url.includes(path))) {
      clearSession()
      unauthorizedHandler?.()
    }
    throw apiError
  }
}

/** 各视图与 api 模块统一使用的请求方法集合（路径不含 /api 前缀） */
export const api = {
  get: <T>(url: string, params?: Record<string, unknown>): Promise<T> =>
    request<T>({ url, method: 'GET', params: cleanParams(params) }),

  post: <T>(url: string, data?: unknown): Promise<T> => request<T>({ url, method: 'POST', data }),

  put: <T>(url: string, data?: unknown): Promise<T> => request<T>({ url, method: 'PUT', data }),

  patch: <T>(url: string, data?: unknown): Promise<T> => request<T>({ url, method: 'PATCH', data }),

  delete: <T>(url: string): Promise<T> => request<T>({ url, method: 'DELETE' }),
}
