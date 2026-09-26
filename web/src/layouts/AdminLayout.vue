<script setup lang="ts">
/**
 * 管理后台布局：在共用外壳上装配管理导航。
 *
 * 意图（Why）：
 *   与管理接口一样，管理导航也按「运营视角」分组，便于快速定位；
 *   布局行为复用 AppShell，保证与门户的操作习惯一致（降低切换成本）。
 *   导航文案走词条（components.nav.admin.*），语言切换后随 i18n 自动重算。
 *
 * 流转（Flow）：
 *   router → /admin/*（守卫已校验 role=10）→ 本布局 → AppShell + RouterView
 *
 * 扩展（Extend）：
 *   新增管理页面时：在 router/index.ts 注册子路由，并在此追加导航项（含词条键）。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import AppShell from '@/components/AppShell.vue'
import type { NavGroup } from '@/components/nav'

const { t } = useI18n()

/** 导航分组（computed：语言切换后自动重算文案） */
const groups = computed<NavGroup[]>(() => [
  {
    title: t('components.nav.admin.overview'),
    items: [
      { label: t('components.nav.admin.dashboard'), to: '/admin', icon: 'chart' },
      { label: t('components.nav.admin.models'), to: '/admin/models', icon: 'grid' },
    ],
  },
  {
    title: t('components.nav.admin.resources'),
    items: [
      { label: t('components.nav.admin.channels'), to: '/admin/channels', icon: 'server' },
      { label: t('components.nav.admin.groups'), to: '/admin/groups', icon: 'tag' },
      { label: t('components.nav.admin.prices'), to: '/admin/prices', icon: 'quota' },
      { label: t('components.nav.admin.tokens'), to: '/admin/tokens', icon: 'key' },
      { label: t('components.nav.admin.users'), to: '/admin/users', icon: 'users' },
      { label: t('components.nav.admin.redeemCodes'), to: '/admin/redeem-codes', icon: 'cart' },
    ],
  },
  {
    title: t('components.nav.admin.operations'),
    items: [
      { label: t('components.nav.admin.tasks'), to: '/admin/tasks', icon: 'image' },
      { label: t('components.nav.admin.orders'), to: '/admin/orders', icon: 'cart' },
      { label: t('components.nav.admin.oauth'), to: '/admin/oauth', icon: 'shield' },
    ],
  },
  {
    title: t('components.nav.admin.maintenance'),
    items: [
      { label: t('components.nav.admin.logs'), to: '/admin/logs', icon: 'list' },
      { label: t('components.nav.admin.audit'), to: '/admin/audit-logs', icon: 'shield' },
      { label: t('components.nav.admin.announcements'), to: '/admin/announcements', icon: 'book' },
      { label: t('components.nav.admin.maintenanceMonitor'), to: '/admin/maintenance', icon: 'server' },
      { label: t('components.nav.admin.settings'), to: '/admin/settings', icon: 'sliders' },
    ],
  },
])
</script>

<template>
  <AppShell :variant-label="t('components.shell.admin')" :groups="groups">
    <RouterView />
  </AppShell>
</template>
