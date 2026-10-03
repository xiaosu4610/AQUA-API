/** 管理后台布局（/admin/*，route group (panel)）：鉴权守卫（管理员）+ 后台导航。
 *
 * 意图（Why）：
 *   后台全部页面需要「登录 + 管理员」双重权限。守卫在布局层统一完成，
 *   页面组件不必各自判断。导航按功能域分组（资源 / 合规 / 运维）。
 *
 * 流转（Flow）：
 *   /admin/login 放在 (panel) 之外独立渲染，不经过本守卫外壳，
 *   避免「要登录后台才能看到登录页」死循环；其余 /admin/* 全部套本布局。
 *   导航文案一律走 t('admin.nav.*')（词条见 locales/<lang>/admin.ts）。
 */
'use client'

import { usePathname, useRouter } from 'next/navigation'
import { useEffect } from 'react'

import { AppShell, type ShellNavGroup } from '@/components/AppShell'
import type { IconName } from '@/components/AppIcon'
import { useI18n } from '@/i18n'
import { useAuth } from '@/lib/auth/auth-context'
import { useToast } from '@/lib/toast/toast-context'

/** 导航分组：labelKey / titleKey 为词条键，渲染时经 t() 解析（见组件） */
const GROUPS: {
  titleKey: string
  items: { labelKey: string; href: string; icon: IconName; exact?: boolean }[]
}[] = [
  {
    titleKey: 'admin.nav.overview',
    items: [
      { labelKey: 'admin.nav.dashboard', href: '/admin', icon: 'home', exact: true },
      { labelKey: 'admin.nav.models', href: '/admin/models', icon: 'grid' },
    ],
  },
  {
    titleKey: 'admin.nav.resources',
    items: [
      { labelKey: 'admin.nav.channels', href: '/admin/channels', icon: 'server' },
      { labelKey: 'admin.nav.speedtest', href: '/admin/speedtest', icon: 'bolt' },
      { labelKey: 'admin.nav.modelMappings', href: '/admin/model-mappings', icon: 'layers' },
      { labelKey: 'admin.nav.groups', href: '/admin/groups', icon: 'tag' },
      { labelKey: 'admin.nav.prices', href: '/admin/prices', icon: 'quota' },
      { labelKey: 'admin.nav.tasks', href: '/admin/tasks', icon: 'image' },
    ],
  },
  {
    titleKey: 'admin.nav.business',
    items: [
      { labelKey: 'admin.nav.orders', href: '/admin/orders', icon: 'cart' },
      { labelKey: 'admin.nav.finance', href: '/admin/finance', icon: 'wallet' },
      { labelKey: 'admin.nav.oauth', href: '/admin/oauth', icon: 'globe' },
      { labelKey: 'admin.nav.tokens', href: '/admin/tokens', icon: 'key' },
      { labelKey: 'admin.nav.users', href: '/admin/users', icon: 'users' },
      { labelKey: 'admin.nav.redeemCodes', href: '/admin/redeem-codes', icon: 'tag' },
      { labelKey: 'admin.nav.broadcast', href: '/admin/broadcast', icon: 'info' },
    ],
  },
  {
    titleKey: 'admin.nav.compliance',
    items: [
      { labelKey: 'admin.nav.logs', href: '/admin/logs', icon: 'list' },
      { labelKey: 'admin.nav.audit', href: '/admin/audit-logs', icon: 'shield' },
      { labelKey: 'admin.nav.announcements', href: '/admin/announcements', icon: 'info' },
      { labelKey: 'admin.nav.sensitiveWords', href: '/admin/sensitive-words', icon: 'filter' },
      { labelKey: 'admin.nav.corpus', href: '/admin/corpus', icon: 'layers' },
      { labelKey: 'admin.nav.maintenance', href: '/admin/maintenance', icon: 'trend' },
      { labelKey: 'admin.nav.settings', href: '/admin/settings', icon: 'sliders' },
      { labelKey: 'admin.nav.limits', href: '/admin/limits', icon: 'shield' },
      { labelKey: 'admin.nav.alertChannels', href: '/admin/alert-channels', icon: 'alert' },
    ],
  },
]

export default function AdminLayout({ children }: { children: React.ReactNode }) {
  const { ready, isLoggedIn, isAdmin } = useAuth()
  const pathname = usePathname()
  const router = useRouter()
  const { toastError } = useToast()
  const { t } = useI18n()

  useEffect(() => {
    if (!ready) return
    // /admin/login 在 (panel) 之外独立渲染，不受本守卫约束（结构上已隔离）
    if (!isLoggedIn) {
      router.replace(`/admin/login?redirect=${encodeURIComponent(pathname)}`)
    } else if (!isAdmin) {
      toastError(t('admin.nav.noPermission'))
      router.replace('/console')
    }
  }, [ready, isLoggedIn, isAdmin, pathname, router, toastError, t])

  if (!ready || !isLoggedIn || !isAdmin) {
    return <div className="flex min-h-screen items-center justify-center text-[13px] text-ink-3">{t('admin.nav.entering')}</div>
  }

  // 把词条键解析为 AppShell 需要的展示文本（导航随语言切换实时更新）
  const groups: ShellNavGroup[] = GROUPS.map((group) => ({
    title: t(group.titleKey),
    items: group.items.map((item) => ({
      label: t(item.labelKey),
      href: item.href,
      icon: item.icon,
      exact: item.exact,
    })),
  }))

  return (
    <AppShell groups={groups} brand={t('admin.nav.brand')}>
      {children}
    </AppShell>
  )
}
