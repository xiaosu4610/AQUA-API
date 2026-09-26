<script setup lang="ts">
/**
 * 用户门户布局：在共用外壳上装配门户导航。
 *
 * 意图（Why）：
 *   「导航项」是门户与管理端唯一的差异，因此这里只声明导航数据，
 *   布局行为（折叠、用户卡片、退出登录）全部复用 components/AppShell.vue。
 *
 * 流转（Flow）：
 *   router → /console/* → 本布局 → AppShell（侧边栏）+ RouterView（子页面）
 *
 * 扩展（Extend）：
 *   新增门户页面时：在 router/index.ts 注册子路由，并在此追加导航项（图标需已在 icons.ts 登记）。
 */
import AppShell from '@/components/AppShell.vue'
import type { NavGroup } from '@/components/nav'

const groups: NavGroup[] = [
  {
    items: [
      { label: '概览', to: '/console', icon: 'home' },
      { label: '访问令牌', to: '/console/tokens', icon: 'key' },
      { label: '调用日志', to: '/console/logs', icon: 'list' },
    ],
  },
  {
    title: '参考',
    items: [
      { label: '接入示例', to: '/#quickstart', icon: 'bolt' },
      { label: '返回首页', to: '/', icon: 'globe' },
    ],
  },
]
</script>

<template>
  <AppShell variant-label="用户门户" :groups="groups" :show-admin-link="true">
    <RouterView />
  </AppShell>
</template>
