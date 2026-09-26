/**
 * 前端路由表与导航守卫。
 *
 * 意图（Why）：
 *  1) 用 history 模式（无 # 号）配合后端 SPA 回退（未命中静态资源时返回 index.html）；
 *  2) 鉴权收敛在守卫里，页面组件不必各自判断登录态；
 *  3) 路由表同时充当「页面地图」：所有页面一目了然。
 *
 * 流转（Flow）：
 *   浏览器跳转 → beforeEach（先完成会话校验 → 判断 requiresAuth/requiresAdmin/guestOnly）
 *   → 加载布局组件 → 加载子页面（懒加载，首屏只下载落地页所需代码）
 *
 * 扩展（Extend）：
 *   新增页面：在下方 routes 中登记，并设置 meta.title（会写入 document.title）。
 *   新增权限维度：扩展 RouteMeta（本文件底部的 declare module）并在守卫中处理。
 */
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

import { peekSiteName } from '@/api/site'
import { toastError } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'

/** 路由元信息：约定所有鉴权相关标记都走 meta，避免在组件里重复判断 */
declare module 'vue-router' {
  interface RouteMeta {
    /** 页面标题（用于 document.title 与侧边栏） */
    title?: string
    /** 需要登录 */
    requiresAuth?: boolean
    /** 需要管理员（role=10） */
    requiresAdmin?: boolean
    /** 仅未登录可访问（登录/注册页）；已登录访问会自动跳转到对应首页 */
    guestOnly?: boolean
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'landing',
    component: () => import('@/views/LandingView.vue'),
    meta: { title: '自托管 LLM API 网关' },
  },
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { title: '登录', guestOnly: true },
  },
  {
    path: '/register',
    name: 'register',
    component: () => import('@/views/RegisterView.vue'),
    meta: { title: '注册', guestOnly: true },
  },
  {
    // 模型广场对访客开放：不登录也能看"能用什么、什么价"。
    // 这是落地页之外最重要的公开页面（很多用户直接搜索模型名进来）。
    path: '/models',
    name: 'model-plaza',
    component: () => import('@/views/ModelPlazaView.vue'),
    meta: { title: '模型广场' },
  },

  /* ── 用户门户 ─────────────────────────────────────────── */
  {
    path: '/console',
    component: () => import('@/layouts/ConsoleLayout.vue'),
    meta: { requiresAuth: true },
    children: [
      {
        path: '',
        name: 'console-overview',
        component: () => import('@/views/console/OverviewView.vue'),
        meta: { title: '概览' },
      },
      {
        path: 'tokens',
        name: 'console-tokens',
        component: () => import('@/views/console/TokensView.vue'),
        meta: { title: '访问令牌' },
      },
      {
        // 控制台内的模型广场：与公开页 /models 是同一个展示组件，
        // 但套在控制台外壳里，避免点一下就跳出侧边栏与身份上下文。
        path: 'models',
        name: 'console-models',
        component: () => import('@/views/PlazaEmbeddedView.vue'),
        meta: { title: '模型广场' },
      },
      {
        // 接入示例改为控制台内页面：原来指向落地页锚点（/#quickstart），
        // 会把人从控制台甩到营销页，回来后还得重新找位置。
        path: 'docs',
        name: 'console-docs',
        component: () => import('@/views/console/DocsView.vue'),
        meta: { title: '接入示例' },
      },
      {
        path: 'logs',
        name: 'console-logs',
        component: () => import('@/views/console/LogsView.vue'),
        meta: { title: '调用日志' },
      },
      {
        path: 'tasks',
        name: 'console-tasks',
        component: () => import('@/views/console/TasksView.vue'),
        meta: { title: '生成任务' },
      },
      {
        path: 'recharge',
        name: 'console-recharge',
        component: () => import('@/views/console/RechargeView.vue'),
        meta: { title: '账户充值' },
      },
    ],
  },

  /* ── 管理后台 ─────────────────────────────────────────── */
  {
    path: '/admin',
    component: () => import('@/layouts/AdminLayout.vue'),
    meta: { requiresAuth: true, requiresAdmin: true },
    children: [
      {
        path: '',
        name: 'admin-dashboard',
        component: () => import('@/views/admin/DashboardView.vue'),
        meta: { title: '仪表盘' },
      },
      {
        // 管理后台同样有内嵌的模型广场：管理员看渠道覆盖情况时，
        // 不该被切到用户门户的外壳里去（导航高亮会变，上下文会断）。
        path: 'models',
        name: 'admin-models',
        component: () => import('@/views/PlazaEmbeddedView.vue'),
        meta: { title: '模型广场' },
      },
      {
        path: 'channels',
        name: 'admin-channels',
        component: () => import('@/views/admin/ChannelsView.vue'),
        meta: { title: '渠道管理' },
      },
      {
        path: 'groups',
        name: 'admin-groups',
        component: () => import('@/views/admin/GroupsView.vue'),
        meta: { title: '模型分组' },
      },
      {
        path: 'prices',
        name: 'admin-prices',
        component: () => import('@/views/admin/PricesView.vue'),
        meta: { title: '计价规则' },
      },
      {
        path: 'tasks',
        name: 'admin-tasks',
        component: () => import('@/views/admin/TasksView.vue'),
        meta: { title: '异步任务' },
      },
      {
        path: 'orders',
        name: 'admin-orders',
        component: () => import('@/views/admin/OrdersView.vue'),
        meta: { title: '充值订单' },
      },
      {
        path: 'oauth',
        name: 'admin-oauth',
        component: () => import('@/views/admin/OAuthView.vue'),
        meta: { title: '订阅账号' },
      },
      {
        path: 'tokens',
        name: 'admin-tokens',
        component: () => import('@/views/admin/TokensView.vue'),
        meta: { title: '令牌管理' },
      },
      {
        path: 'users',
        name: 'admin-users',
        component: () => import('@/views/admin/UsersView.vue'),
        meta: { title: '用户管理' },
      },
      {
        path: 'logs',
        name: 'admin-logs',
        component: () => import('@/views/admin/LogsView.vue'),
        meta: { title: '调用日志' },
      },
      {
        path: 'settings',
        name: 'admin-settings',
        component: () => import('@/views/admin/SettingsView.vue'),
        meta: { title: '系统设置' },
      },
    ],
  },

  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { title: '页面不存在' },
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // 切换页面回到顶部；带锚点（如落地页特性区）时平滑滚动到目标
  scrollBehavior(to, _from, savedPosition) {
    if (savedPosition) return savedPosition
    if (to.hash) return { el: to.hash, behavior: 'smooth' }
    return { top: 0 }
  },
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()

  // 首次导航前完成会话校正：否则刷新 /admin 会因本地快照缺失而误判
  if (!auth.ready) await auth.bootstrap()

  const loginRedirect = { name: 'login' as const, query: { redirect: to.fullPath } }

  if ((to.meta.requiresAuth || to.meta.requiresAdmin) && !auth.isLoggedIn) {
    return loginRedirect
  }

  // 非管理员访问管理后台：明确告知原因，再送回门户概览（而不是静默跳转）
  if (to.meta.requiresAdmin && !auth.isAdmin) {
    toastError('没有权限访问管理后台')
    return { name: 'console-overview' }
  }

  // 已登录用户访问登录/注册页：直接送去各自的首页
  if (to.meta.guestOnly && auth.isLoggedIn) {
    return auth.isAdmin ? { name: 'admin-dashboard' } : { name: 'console-overview' }
  }

  return true
})

router.afterEach((to) => {
  const suffix = peekSiteName()
  document.title = to.meta.title && to.name !== 'landing' ? `${to.meta.title} · ${suffix}` : suffix
})
