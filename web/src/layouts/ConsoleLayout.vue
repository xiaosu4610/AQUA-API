<script setup lang="ts">
/**
 * 用户门户布局：在共用外壳上装配门户导航。
 *
 * 意图（Why）：
 *   「导航项」是门户与管理端唯一的差异，因此这里只声明导航数据，
 *   布局行为（折叠、用户卡片、退出登录）全部复用 components/AppShell.vue。
 *
 *   导航按「用户的使用顺序」分组（先看有什么 → 再拿凭据 → 最后查账），
 *   而不是把所有入口平铺成一长列：平铺会让新用户在侧边栏里迷路。
 *   组名也刻意用用户视角的词（控制台 / 资源 / 账务），而不是技术名词。
 *   导航文案走词条（components.nav.console.*），语言切换后自动重算。
 *
 * 流转（Flow）：
 *   router → /console/* → 本布局 → AppShell（侧边栏）+ RouterView（子页面）
 *
 * 扩展（Extend）：
 *   新增门户页面时：在 router/index.ts 注册子路由，并在此追加导航项（图标需已在 icons.ts 登记）。
 *   注意：所有 to 都必须落在 /console 下 —— 指向站外或公开页会让用户丢掉控制台外壳。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import AppShell from '@/components/AppShell.vue'
import type { NavGroup } from '@/components/nav'

const { t } = useI18n()

/** 导航分组（computed：语言切换后自动重算文案） */
const groups = computed<NavGroup[]>(() => [
  {
    title: t('components.nav.console.console'),
    items: [
      { label: t('components.nav.console.overview'), to: '/console', icon: 'home' },
      { label: t('components.nav.console.models'), to: '/console/models', icon: 'grid' },
      { label: t('components.nav.console.docs'), to: '/console/docs', icon: 'book' },
      { label: t('components.nav.console.playground'), to: '/console/playground', icon: 'send' },
    ],
  },
  {
    title: t('components.nav.console.resources'),
    items: [
      { label: t('components.nav.console.tokens'), to: '/console/tokens', icon: 'key' },
      { label: t('components.nav.console.tasks'), to: '/console/tasks', icon: 'image' },
    ],
  },
  {
    title: t('components.nav.console.billing'),
    items: [
      { label: t('components.nav.console.recharge'), to: '/console/recharge', icon: 'wallet' },
      { label: t('components.nav.console.logs'), to: '/console/logs', icon: 'list' },
    ],
  },
])
</script>

<template>
  <AppShell :variant-label="t('components.shell.portal')" :groups="groups" :show-admin-link="true">
    <RouterView />
  </AppShell>
</template>
