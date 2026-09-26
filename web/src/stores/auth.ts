/**
 * 登录态状态（Pinia）——全站唯一的会话真相来源。
 *
 * 意图（Why）：
 *   1) 路由守卫需要在「跳转前」同步判断是否登录、是否管理员，因此用户信息必须
 *      在应用启动时一次性校正（bootstrap），否则刷新 /admin 页面会误判无权限；
 *   2) 会话令牌由 api/client.ts 负责落盘并在请求头附带，store 只做「登录/登出/校正」语义，
 *      避免两处都写 localStorage 造成不一致。
 *
 * 流转（Flow）：
 *   启动：main.ts → bootstrap() → GET /api/auth/me → 校正 user（角色/额度可能已变）
 *   登录：LoginView → signIn() → POST /api/auth/login → 保存 token + user
 *   失效：任意请求 401 → client 清本地 + 回调（main.ts 注入）→ router 跳登录页
 *   登出：AppShell 的退出按钮 → signOut() → POST /api/auth/logout（失败也本地清除）
 *
 * 扩展（Extend）：
 *   新增鉴权维度（如「受限用户」）时：在 types.ts 补角色常量，
 *   在此加对应 getter（如 isRestricted），并在 router 守卫里使用。
 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { adminLogin, fetchMe, login, logout, register } from '@/api/auth'
import {
  ApiError,
  clearSession,
  getCachedUser,
  getSessionToken,
  setCachedUser,
  setSessionToken,
} from '@/api/client'
import { ROLE_ADMIN, type AuthResult, type AuthUser, type LoginPayload, type RegisterPayload } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  // 启动时先用本地快照渲染（避免刷新瞬间跳登录页），随后由 bootstrap 校正
  const user = ref<AuthUser | null>(getCachedUser<AuthUser>())
  const token = ref<string>(getSessionToken())
  /** 是否已完成启动时的会话校验；路由守卫依赖它，防止用旧快照做错误判断 */
  const ready = ref(false)

  const isLoggedIn = computed(() => Boolean(token.value))
  const isAdmin = computed(() => user.value?.role === ROLE_ADMIN)
  const displayName = computed(() => user.value?.username || '未登录')

  /** 会话数据落盘（登录/注册成功后调用） */
  function applySession(result: AuthResult): void {
    token.value = result.session_token
    setSessionToken(result.session_token)
    // 后端可能不回传完整用户（如仅 id/username/role），此处以返回值覆盖快照
    user.value = result.user
    setCachedUser(result.user)
  }

  /** 仅清除本地登录态（不调用后端），供 401 回调与登出共用 */
  function clearLocal(): void {
    token.value = ''
    user.value = null
    clearSession()
  }

  /** 登录：成功后返回用户信息，由调用方决定跳转目标 */
  async function signIn(payload: LoginPayload): Promise<AuthUser> {
    const result = await login(payload)
    applySession(result)
    return result.user
  }

  /**
   * 超管入口登录：只需密码。
   *
   * 与 signIn 的区别仅在于凭据形态（后端按"仅密码"匹配管理员），
   * 会话落盘、角色判断等后续流程完全一致，因此共用 applySession。
   */
  async function signInAsAdmin(password: string): Promise<AuthUser> {
    const result = await adminLogin(password)
    applySession(result)
    return result.user
  }

  /** 注册：契约中注册成功即返回会话令牌，因此直接进入登录态 */
  async function signUp(payload: RegisterPayload): Promise<AuthUser> {
    const result = await register(payload)
    applySession(result)
    return result.user
  }

  /**
   * 启动时校正会话：
   *   有令牌 → 调 /auth/me 校验；失败（401/网络）则清空本地，避免「假登录」状态。
   *   无令牌 → 直接标记 ready。
   */
  async function bootstrap(): Promise<void> {
    if (ready.value) return
    if (!token.value) {
      ready.value = true
      return
    }
    try {
      const me = await fetchMe()
      user.value = me
      setCachedUser(me)
    } catch (err) {
      // 401 已由 client 清过一遍；这里兜底处理网络错误等场景
      if (err instanceof ApiError && err.status === 401) clearLocal()
    } finally {
      ready.value = true
    }
  }

  /**
   * 重新拉取当前用户信息（额度可能已变化）。
   * 与 bootstrap 的区别：bootstrap 只在启动时执行一次并标记 ready，
   * 而门户概览页需要在每次进入时刷新额度，故单独暴露本方法。
   */
  async function refreshUser(): Promise<void> {
    try {
      const me = await fetchMe()
      user.value = me
      setCachedUser(me)
    } catch {
      // 静默失败：页面会展示上一次的快照，401 由 client 统一处理
    }
  }

  /** 退出登录：后端吊销失败也要本地登出（否则用户会觉得「退不出去」） */
  async function signOut(): Promise<void> {
    try {
      await logout()
    } catch {
      /* 忽略：会话令牌已失效时后端可能返回 401 */
    } finally {
      clearLocal()
    }
  }

  return {
    user,
    token,
    ready,
    isLoggedIn,
    isAdmin,
    displayName,
    signIn,
    signInAsAdmin,
    signUp,
    signOut,
    clearLocal,
    bootstrap,
    refreshUser,
  }
})
