<script setup lang="ts">
/**
 * 后台外壳（侧边栏 + 顶栏 + 内容区），门户与管理端共用。
 *
 * 意图（Why）：
 *   1) 门户与管理端只差「导航项与文案」，共用一套外壳可保证两侧交互一致，
 *      也避免复制出两份会逐渐分叉的布局代码；
 *   2) 桌面端固定侧边栏（信息密度优先），窄屏同时提供「抽屉」与「底部导航」：
 *      抽屉承载全部导航项，底部导航把最高频的几项常驻在拇指区（可用性优先）；
 *   3) 顶栏内置语言切换器，六种语言随时可切（RTL 下侧边栏自动改从右侧滑出）。
 *
 * 流转（Flow）：
 *   layouts/ConsoleLayout.vue、AdminLayout.vue → 传 groups（导航分组，已按当前语言翻译）
 *   → 本组件渲染 RouterLink 导航 + 当前用户信息 + 退出登录 → 默认插槽放页面内容；
 *     底部导航项由 groups 扁平化后取前若干项自动推导（见 bottomNavItems）。
 *
 * 扩展（Extend）：
 *   新增导航项：在对应 layout 的 groups 里加一项即可（图标名须在 AppIcon 中已登记）；
 *   注意区块根路径（/console、/admin）走精确匹配高亮（见 isSectionRoot），
 *   否则子页会把根项一起点亮，侧边栏出现两个选中态。
 *   新增顶栏操作（如全局搜索）：在 header 右侧插槽区追加。
 *   调整底部导航项数：改 BOTTOM_NAV_LIMIT（当前 4 项 + 「更多」，共 5 格）。
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'

import AppIcon from './AppIcon.vue'
import AnnouncementBanner from './AnnouncementBanner.vue'
import LocaleSwitcher from './LocaleSwitcher.vue'
import type { NavGroup } from './nav'
import { confirmDialog } from '@/composables/useConfirm'
import { toastInfo } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const props = defineProps<{
  /** 外壳标识文案，如「用户门户」「管理后台」（由 layout 传已翻译文案） */
  variantLabel: string
  /** 导航分组 */
  groups: NavGroup[]
  /** 是否需要「去管理后台」的快捷入口（管理员在门户页使用） */
  showAdminLink?: boolean
}>()

const { t, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const site = useSiteStore()

/** 是否为从右向左语言：侧边栏位置与滑入方向需要镜像 */
const rtl = computed(() => locale.value === 'ar')

/** 窄屏下的抽屉式侧边栏开关 */
const sidebarOpen = ref(false)

/** 路由变化后自动收起：移动端点击导航项后应立即看到内容 */
watch(
  () => route.fullPath,
  () => (sidebarOpen.value = false),
)

/*
 * 抽屉打开时锁定 body 滚动。
 * 为什么需要：抽屉在窄屏几乎铺满整屏，若背后页面仍可滚动，手指在抽屉上
 * 上下滑动会带着页面一起动，观感上像"抽屉在漏"。打开即锁、关闭即解锁，
 * 组件卸载时兜底恢复，避免把整个页面永久锁死。
 */
watch(sidebarOpen, (open) => {
  document.body.style.overflow = open ? 'hidden' : ''
})

onBeforeUnmount(() => {
  document.body.style.overflow = ''
})

/** 底部导航最多展示的主导航项数（再加一个「更多」，共 5 格，375px 下每格约 75px 不拥挤） */
const BOTTOM_NAV_LIMIT = 4

/** 按 groups 的声明顺序把所有导航项拉平 */
const flatNavItems = computed(() => props.groups.flatMap((group) => group.items))

/**
 * 底部导航项：取声明顺序最靠前的若干项。
 *
 * 为什么按顺序推导而不是在导航数据里标一个「常用」字段：
 * 各 layout 的 groups 本就是按使用顺序排列的（控制台/总览在最前），
 * 最靠前即代表最常用，直接切片即可，无需在导航数据上新增字段，
 * 也就不存在"前端写死页面路径"的问题（换一套导航自动跟着变）。
 */
const bottomNavItems = computed(() => flatNavItems.value.slice(0, BOTTOM_NAV_LIMIT))

/** 是否需要「更多」：仅当还有未展示的导航项时才出现，避免凭空多出一个无用入口 */
const hasMoreNav = computed(() => flatNavItems.value.length > bottomNavItems.value.length)

/** 顶栏标题取当前路由 meta.title */
const currentTitle = computed(() => (route.meta.title as string | undefined) || props.variantLabel)

/** 用户名首字母作为头像占位（不引入图片资源） */
const avatarText = computed(() => (auth.user?.username || '?').slice(0, 1).toUpperCase())

/** 角色文案：管理员 / 普通用户 / 未知角色（走词条，避免在布局里硬编码中文） */
const roleText = computed(() => {
  const role = auth.user?.role
  if (role === 10) return t('components.shell.roleAdmin')
  if (role === 1) return t('components.shell.roleUser')
  return t('components.shell.roleUnknown')
})

const adminEntry = computed(() => props.showAdminLink && auth.isAdmin)

/**
 * 是否为「区块根路径」（/console、/admin 这类只有一段的路径）。
 *
 * 为什么要单独判断：RouterLink 默认按前缀匹配高亮，访问 /console/models 时
 * 「概览」（to=/console）也会被前缀命中，于是侧边栏同时亮起两项，
 * 用户反而看不清自己在哪一页。区块根路径必须改成精确匹配才算选中。
 */
function isSectionRoot(to: string): boolean {
  return to.split('/').filter(Boolean).length <= 1
}

async function handleSignOut(): Promise<void> {
  const ok = await confirmDialog({
    title: t('components.shell.signOut'),
    message: t('components.shell.signOutConfirm'),
    confirmText: t('components.shell.signOut'),
    danger: true,
  })
  if (!ok) return
  await auth.signOut()
  toastInfo(t('components.shell.signOutDone'))
  await router.replace({ name: 'login' })
}
</script>

<template>
  <div class="app-shell app-ambient bg-ink-950">
    <!-- 窄屏遮罩 -->
    <div
      v-if="sidebarOpen"
      class="fixed inset-0 z-30 bg-ink-50/30 backdrop-blur-[2px] lg:hidden"
      @click="sidebarOpen = false"
    />

    <!-- 侧边栏：start-0 跟随书写方向贴边；关闭态在 RTL 下改为向左收起到右侧 -->
    <aside
      class="sidebar-safe fixed inset-y-0 start-0 z-40 flex w-60 flex-col border-e border-ink-800 bg-ink-900/95
        transition-transform duration-200 lg:translate-x-0"
      :class="sidebarOpen ? 'translate-x-0' : rtl ? 'translate-x-full' : '-translate-x-full'"
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
            :active-class="isSectionRoot(item.to) ? undefined : 'nav-item-active'"
            exact-active-class="nav-item-active"
          >
            <AppIcon :name="item.icon" :size="17" />
            {{ item.label }}
          </RouterLink>
        </div>

        <div v-if="adminEntry" class="space-y-1 border-t border-ink-800 pt-4">
          <RouterLink to="/admin" class="nav-item">
            <AppIcon name="shield" :size="17" />
            {{ t('components.shell.admin') }}
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
            <span class="block text-[11px] text-ink-400">{{ roleText }}</span>
          </span>
          <button
            type="button"
            class="btn btn-ghost btn-icon"
            :title="t('components.shell.signOut')"
            @click="handleSignOut"
          >
            <AppIcon name="logout" :size="16" />
          </button>
        </div>
        <p v-if="site.version" class="mt-2 px-1 text-[11px] text-ink-500">
          {{ t('components.shell.version', { version: site.version }) }}
        </p>
      </div>
    </aside>

    <!-- 主内容区：ps-60 为逻辑内边距，RTL 下自动改到另一侧 -->
    <div class="lg:ps-60">
      <header
        class="app-header sticky top-0 z-20 flex items-center gap-3 border-b border-ink-800 bg-white/90 px-4 lg:px-8"
      >
        <button
          type="button"
          class="btn btn-secondary btn-icon lg:hidden"
          :aria-label="t('components.shell.expandNav')"
          @click="sidebarOpen = true"
        >
          <AppIcon name="menu" :size="18" />
        </button>

        <h1 class="min-w-0 flex-1 truncate text-sm font-semibold text-ink-100">{{ currentTitle }}</h1>

        <LocaleSwitcher />

        <RouterLink v-if="!showAdminLink && auth.isAdmin" to="/console" class="btn btn-ghost btn-sm hidden sm:inline-flex">
          <AppIcon name="home" :size="15" />
          {{ t('components.shell.portal') }}
        </RouterLink>
        <RouterLink v-else-if="adminEntry" to="/admin" class="btn btn-ghost btn-sm hidden sm:inline-flex">
          <AppIcon name="shield" :size="15" />
          {{ t('components.shell.admin') }}
        </RouterLink>
      </header>

      <main class="main-offset">
        <!-- 站点公告横幅：自包含组件（自行拉取、自行记忆"已读"），
             放在页面内容之前，让运营通知在所有后台/门户页都能被看到。 -->
        <AnnouncementBanner />
        <slot />
      </main>
    </div>

    <!-- 底部导航（仅窄屏）：把最高频页面常驻在拇指区，省去「开抽屉再选」的那一步。
         项由 groups 自动推导，新增/调整导航项时这里无需改动。 -->
    <nav class="bottom-nav" :aria-label="t('components.shell.bottomNav')">
      <RouterLink
        v-for="item in bottomNavItems"
        :key="item.to"
        :to="item.to"
        class="bottom-nav-item"
        :active-class="isSectionRoot(item.to) ? undefined : 'bottom-nav-item-active'"
        exact-active-class="bottom-nav-item-active"
      >
        <AppIcon :name="item.icon" :size="20" />
        <span class="max-w-full truncate">{{ item.label }}</span>
      </RouterLink>

      <!-- 「更多」：打开抽屉查看全部导航项；抽屉打开时同步高亮，形成位置反馈 -->
      <button
        v-if="hasMoreNav"
        type="button"
        class="bottom-nav-item"
        :class="sidebarOpen ? 'bottom-nav-item-active' : ''"
        :aria-label="t('components.shell.moreNav')"
        @click="sidebarOpen = true"
      >
        <AppIcon name="menu" :size="20" />
        <span>{{ t('common.action.more') }}</span>
      </button>
    </nav>
  </div>
</template>
