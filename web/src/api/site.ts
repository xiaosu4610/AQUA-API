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
import type { ModelPlaza, PublicPaymentInfo, SiteStatus } from './types'

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
    // 这两个开关决定注册页的字段是否必填。字段缺失时按「不需要验证码」处理，
    // 与后端默认值（需要验证码）相反——但后端一旦运行就必然返回该字段，
    // 缺失只可能出现在极早期的旧版本，此时按宽松处理可避免注册页被锁死。
    email_code_required: Boolean(status?.email_code_required),
    email_service_ready: status?.email_service_ready !== false,
  }
  cachedStatus = normalized
  cachedAt = Date.now()
  return normalized
}

/** 供页脚等处读取「已缓存的」站点名，未加载时返回默认值（不触发请求） */
export function peekSiteName(): string {
  return cachedStatus?.name || 'AQUA-API'
}

/* ── 模型广场与充值参数（均无需登录）─────────────────────── */

/**
 * GET /api/models：模型广场数据（模型卡片 + 分组视图）。
 *
 * 为什么会带查询参数：模型广场通常有几十到上百个模型，
 * 让后端做过滤可以避免把整份清单都传到浏览器再筛。
 * 参数：
 *   - group：只看该分组下可用的模型
 *   - keyword：按模型名模糊匹配
 */
export function fetchModelPlaza(params: { group?: string; keyword?: string } = {}): Promise<ModelPlaza> {
  return api.get<ModelPlaza>('/models', { ...params })
}

/**
 * GET /api/payment/public：充值参数（是否开放、有哪些通道、汇率与限额）。
 *
 * 登录页/落地页需要在未登录时就知道"本站是否支持充值"，
 * 因此该接口不要求鉴权，且只返回非敏感参数。
 */
export function fetchPaymentInfo(): Promise<PublicPaymentInfo> {
  return api.get<PublicPaymentInfo>('/payment/public')
}
