/**
 * 站点信息状态（Pinia）。
 *
 * 意图（Why）：
 *   站点名称/描述/是否开放注册/可用模型列表，被落地页、登录页、注册页、侧边栏多处使用；
 *   放在 store 里做一次性加载 + 全局共享，避免每个页面重复请求 /api/status。
 *
 * 流转（Flow）：
 *   main.ts → useSiteStore().load()（启动时预取）
 *   LandingView / RegisterView / AppShell → 读取 state
 *
 * 扩展（Extend）：
 *   后端新增站点级开关（如「是否允许注册」之外的 feature flag），
 *   在 api/types.ts 的 SiteStatus 加字段后，这里无需改动（整体透传）。
 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { fetchSiteStatus } from '@/api/site'
import { ApiError } from '@/api/client'
import type { SiteStatus } from '@/api/types'

export const useSiteStore = defineStore('site', () => {
  const status = ref<SiteStatus | null>(null)
  const loading = ref(false)
  const error = ref('')

  /** 站点显示名（未加载完成时回退为品牌名，避免标题闪空） */
  const siteName = computed(() => status.value?.name || 'AQUA-API')
  const siteDescription = computed(() => status.value?.site_description || '')
  const version = computed(() => status.value?.version || '')
  const models = computed(() => status.value?.models ?? [])
  /** 注册开关：未拿到站点信息时按「关闭」处理（保守策略，避免注册页误导用户） */
  const registrationEnabled = computed(() => status.value?.registration_enabled === true)

  /**
   * 拉取站点信息。
   * @param force 忽略缓存强制刷新（用户点击「重试」时使用）
   */
  async function load(force = false): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      status.value = await fetchSiteStatus(force)
    } catch (err) {
      // 站点信息失败不应阻断页面渲染：记录错误，由页面显示可重试的提示条
      error.value = err instanceof ApiError ? err.message : '站点信息加载失败'
    } finally {
      loading.value = false
    }
  }

  return {
    status,
    loading,
    error,
    siteName,
    siteDescription,
    version,
    models,
    registrationEnabled,
    load,
  }
})
