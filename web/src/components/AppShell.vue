<script setup lang="ts">
/**
 * 后台外壳（侧边栏 + 顶栏 + 内容区），门户与管理端共用。
 *
 * 意图（Why）：
 *   1) 门户与管理端只差「导航项与文案」，共用一套外壳可保证两侧交互一致，
 *      也避免复制出两份会逐渐分叉的布局代码；
 *   2) 桌面端固定侧边栏（信息密度优先），窄屏折叠为抽屉（可用性优先）。
 *
 * 流转（Flow）：
 *   layouts/ConsoleLayout.vue、AdminLayout.vue → 传 groups（导航分组）
 *   → 本组件渲染 RouterLink 导航 + 当前用户信息 + 退出登录 → 默认插槽放页面内容
 *
 * 扩展（Extend）：
 *   新增导航项：在对应 layout 的 groups 里加一项即可（图标名须在 AppIcon 中已登记）；
 *   新增顶栏操作（如全局搜索）：在 header 右侧插槽区追加。
 */
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import AppIcon from './AppIcon.vue'
import type { NavGroup } from './nav'
import { confirmDialog } from '@/composables/useConfirm'
import { toastInfo } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'
import { roleLabel } from '@/utils/display'

const props = defineProps<{
  /** 外壳标识文案，如「用户门户」「管理后台」 */
  variantLabel: string
  /** 导航分组 */
  groups: NavGroup[]
  /** 是否需要「去管理后台」的快捷入口（管理员在门户页使用） */
  showAdminLink?: boolean
}>()

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const site = useSiteStore()

/** 窄屏下的抽屉式侧边栏开关 */
const sidebarOpen = ref(false)

/** 路由变化后自动收起：移动端点击导航项后应立即看到内容 */
watch(
  () => route.fullPath,
  () => (sidebarOpen.value = false),
)

/** 顶栏标题取当前路由 meta.title */
const currentTitle = computed(() => (route.meta.title as string | undefined) || props.variantLabel)

/** 用户名首字母作为头像占位（不引入图片资源） */
const avatarText = computed(() => (auth.user?.username || '?').slice(0, 1).toUpperCase())

const adminEntry = computed(() => props.showAdminLink && auth.isAdmin)

async function handleSignOut(): Promise<void> {
  const ok = await confirmDialog({
    title: '退出登录',
    message: '退出后当前会话令牌将立即失效，需要重新登录才能访问门户。',
    confirmText: '退出登录',
    danger: true,
  })
  if (!ok) return
  await auth.signOut()
  toastInfo('已退出登录')
  await router.replace({ name: 'login' })
}
</script>

<template>
  <div class="min-h-screen bg-ink-950">
    <!-- 窄屏遮罩 -->
    <div
      v-if="sidebarOpen"
      class="fixed inset-0 z-30 bg-ink-50/30 backdrop-blur-[2px] lg:hidden"
      @click="sidebarOpen = false"
    />

    <!-- 侧边栏 -->
    <aside
      class="fixed inset-y-0 left-0 z-40 flex w-60 flex-col border-r border-ink-800 bg-ink-900/80 backdrop-blur
        transition-transform duration-200 lg:translate-x-0"
      :class="sidebarOpen ? 'translate-x-0' : '-translate-x-full'"
    >
      <!-- 品牌区 -->
      <RouterLink to="/" class="flex items-center gap-2.5 border-b border-ink-800 px-4 py-4">
        <img src="/favicon.ico" alt="" class="h-8 w-8 rounded-lg" />
        <span class="min-w-0">
          <span class="block truncate text-sm font-semibold text-ink-50">{{ site.siteName }}</span>
          <span class="block text-[11px] text-ink-400">{{ variantLabel }}</span>
        </span>
      </RouterLink>

      <!-- 导航 -->
      <nav class="flex-1 space-y-5 overflow-y-auto px-3 py-4">
        <div v-for="(group, index) in groups" :key="index" class="space-y-1">
          <p v-if="group.title" class="px-3 pb-1 text-[11px] font-medium uppercase tracking-wider text-ink-500">
            {{ group.title }}
          </p>
          <RouterLink
            v-for="item in group.items"
            :key="item.to"
            :to="item.to"
            class="nav-item"
            active-class="nav-item-active"
          >
            <AppIcon :name="item.icon" :size="17" />
            {{ item.label }}
          </RouterLink>
        </div>

        <div v-if="adminEntry" class="space-y-1 border-t border-ink-800 pt-4">
          <RouterLink to="/admin" class="nav-item">
            <AppIcon name="shield" :size="17" />
            管理后台
          </RouterLink>
        </div>
      </nav>

      <!-- 用户区 -->
      <div class="border-t border-ink-800 p-3">
        <div class="flex items-center gap-2.5 rounded-lg bg-ink-850/70 px-3 py-2.5">
          <span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-brand-500/15 text-sm font-semibold text-brand-700">
            {{ avatarText }}
          </span>
          <span class="min-w-0 flex-1">
            <span class="block truncate text-sm font-medium text-ink-100">{{ auth.displayName }}</span>
            <span class="block text-[11px] text-ink-400">{{ roleLabel(auth.user?.role) }}</span>
          </span>
          <button type="button" class="btn btn-ghost btn-icon" title="退出登录" @click="handleSignOut">
            <AppIcon name="logout" :size="16" />
          </button>
        </div>
        <p v-if="site.version" class="mt-2 px-1 text-[11px] text-ink-500">版本 {{ site.version }}</p>
      </div>
    </aside>

    <!-- 主内容区 -->
    <div class="lg:pl-60">
      <header
        class="sticky top-0 z-20 flex items-center gap-3 border-b border-ink-800 bg-white/70 px-4 py-3 backdrop-blur lg:px-8"
      >
        <button
          type="button"
          class="btn btn-secondary btn-icon lg:hidden"
          aria-label="展开导航"
          @click="sidebarOpen = true"
        >
          <AppIcon name="menu" :size="18" />
        </button>

        <h1 class="min-w-0 flex-1 truncate text-sm font-semibold text-ink-100">{{ currentTitle }}</h1>

        <RouterLink v-if="!showAdminLink && auth.isAdmin" to="/console" class="btn btn-ghost btn-sm hidden sm:inline-flex">
          <AppIcon name="home" :size="15" />
          用户门户
        </RouterLink>
        <RouterLink v-else-if="adminEntry" to="/admin" class="btn btn-ghost btn-sm hidden sm:inline-flex">
          <AppIcon name="shield" :size="15" />
          管理后台
        </RouterLink>
      </header>

      <main class="pb-16">
        <slot />
      </main>
    </div>
  </div>
</template>
