<script setup lang="ts">
/**
 * 404 页面。
 *
 * 意图（Why）：
 *   用户可能通过旧书签或手输地址进入不存在的路径；
 *   给出明确的「不存在」结论 + 返回入口，避免停留在空白页。
 *
 * 流转（Flow）：
 *   路由未匹配 → 本组件 → 提供回首页/登录/门户的出口
 *
 * 扩展（Extend）：
 *   若需要区分 403（无权限），可传 props 决定文案；当前守卫已处理权限跳转，故不实现。
 */
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
</script>

<template>
  <div class="relative flex min-h-screen flex-col items-center justify-center bg-ink-950 px-5">
    <div class="pointer-events-none absolute inset-0 bg-grid opacity-40" aria-hidden="true" />

    <div class="relative w-full max-w-md text-center">
      <span class="mx-auto flex h-12 w-12 items-center justify-center rounded-2xl bg-ink-900 text-ink-300 ring-1 ring-inset ring-ink-700">
        <AppIcon name="search" :size="22" />
      </span>
      <p class="mt-6 font-mono text-5xl font-semibold tracking-tight text-ink-700">404</p>
      <h1 class="mt-3 text-lg font-semibold text-ink-50">页面不存在</h1>
      <p class="mt-2 text-sm leading-relaxed text-ink-400">
        你访问的地址可能已被移除或输入有误。可以从下面的入口继续。
      </p>

      <div class="mt-7 flex flex-wrap justify-center gap-3">
        <RouterLink to="/" class="btn btn-primary">
          <AppIcon name="home" :size="16" />
          回到首页
        </RouterLink>
        <RouterLink v-if="auth.isLoggedIn" :to="auth.isAdmin ? '/admin' : '/console'" class="btn btn-secondary">
          <AppIcon :name="auth.isAdmin ? 'shield' : 'chart'" :size="16" />
          进入控制台
        </RouterLink>
        <RouterLink v-else to="/login" class="btn btn-secondary">
          <AppIcon name="lock" :size="16" />
          去登录
        </RouterLink>
      </div>
    </div>
  </div>
</template>
