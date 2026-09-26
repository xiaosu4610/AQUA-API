/**
 * 公开接口（无需登录）。
 *
 * 意图（Why）：
 *   落地页、登录页、注册页都要读站点信息（名称/描述/是否开放注册/可用模型），
 *   /api/status 是本前端唯一可在未登录时调用的数据源。
 *
 * 流转（Flow）：
 *   LandingView / LoginView / RegisterView → fetchSiteStatus() → GET /api/status
 *
 * 扩展（Extend）：
 *   新增公开接口时在本文件追加函数，并同步 api/types.ts 的类型定义。
 */
import { api } from './client'
import type { SiteStatus } from './types'

/** 站点信息缓存：多个页面共用，避免路由切换时重复请求（60s 内复用） */
let cachedStatus: SiteStatus | null = null
let cachedAt = 0
const CACHE_TTL_MS = 60_000

/**
 * 读取站点信息与功能开关。
 *
 * @param force 忽略缓存强制刷新（落地页的「重试」按钮使用）
 */
export async function fetchSiteStatus(force = false): Promise<SiteStatus> {
  const fresh = cachedStatus && Date.now() - cachedAt < CACHE_TTL_MS
  if (!force && fresh) return cachedStatus as SiteStatus

  const status = await api.get<SiteStatus>('/status')
  // 兜底：契约保证 models 为数组，但空站点/后端早期版本可能返回 null，避免视图层崩溃
  const normalized: SiteStatus = {
    name: status?.name || 'AQUA-API',
    version: status?.version || 'dev',
    registration_enabled: Boolean(status?.registration_enabled),
    site_description: status?.site_description || '',
    models: Array.isArray(status?.models) ? status.models : [],
  }
  cachedStatus = normalized
  cachedAt = Date.now()
  return normalized
}

/** 供页脚等处读取「已缓存的」站点名，未加载时返回默认值（不触发请求） */
export function peekSiteName(): string {
  return cachedStatus?.name || 'AQUA-API'
}
