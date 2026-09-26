<script setup lang="ts">
/**
 * 管理后台布局：在共用外壳上装配管理导航。
 *
 * 意图（Why）：
 *   与管理接口一样，管理导航也按「运营视角」分组，便于快速定位；
 *   布局行为复用 AppShell，保证与门户的操作习惯一致（降低切换成本）。
 *
 * 流转（Flow）：
 *   router → /admin/*（守卫已校验 role=10）→ 本布局 → AppShell + RouterView
 *
 * 扩展（Extend）：
 *   新增管理页面时：在 router/index.ts 注册子路由，并在此追加导航项。
 */
import AppShell from '@/components/AppShell.vue'
import type { NavGroup } from '@/components/nav'

const groups: NavGroup[] = [
  {
    title: '总览',
    items: [{ label: '仪表盘', to: '/admin', icon: 'chart' }],
  },
  {
    title: '资源',
    items: [
      { label: '渠道管理', to: '/admin/channels', icon: 'server' },
      { label: '令牌管理', to: '/admin/tokens', icon: 'key' },
      { label: '用户管理', to: '/admin/users', icon: 'users' },
    ],
  },
  {
    title: '运维',
    items: [{ label: '调用日志', to: '/admin/logs', icon: 'list' }],
  },
]
</script>

<template>
  <AppShell variant-label="管理后台" :groups="groups">
    <RouterView />
  </AppShell>
</template>
