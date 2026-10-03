/** 超管登录页（/admin/login）：只输密码。
 *
 * 意图（Why）：
 *   刻意独立于普通登录（站长不需要回忆用户名）；与 /admin 的鉴权布局分离，
 *   避免「要登录后台才能看到登录后台的页面」死循环。
 */
'use client'

import Link from 'next/link'
import { useRouter, useSearchParams } from 'next/navigation'
import { Suspense, useState } from 'react'

import { BrandLogo } from '@/components/BrandMark'
import { Button } from '@/components/ui/Button'
import { Field, Input } from '@/components/ui/Form'
import { useI18n } from '@/i18n'
import { useAuth } from '@/lib/auth/auth-context'
import { safeRedirect } from '@/lib/auth/safe-redirect'

function AdminLoginForm() {
  const router = useRouter()
  const searchParams = useSearchParams()
  const redirect = searchParams.get('redirect') || null
  const { signInAsAdmin } = useAuth()
  const { t } = useI18n()

  const [password, setPassword] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    if (!password) {
      setError(t('admin.login.passwordRequired'))
      return
    }
    setLoading(true)
    try {
      const user = await signInAsAdmin(password)
      // 同 /login：redirect 来自查询串，必须过滤后才可用于跳转（开放重定向防护）
      router.replace(safeRedirect(redirect, user.role === 10 ? '/admin' : '/console'))
    } catch (err) {
      setError(err instanceof Error ? err.message : t('admin.login.failed'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex min-h-screen flex-col bg-surface">
      <header className="flex h-14 items-center border-b border-line bg-card px-4 sm:px-6">
        <Link href="/" className="flex items-center gap-2">
          <BrandLogo name={t('admin.login.brand')} />
        </Link>
      </header>

      <main className="flex flex-1 items-start justify-center px-4 py-16 sm:py-24">
        <div className="w-full max-w-sm">
          <h1 className="text-xl font-bold text-ink">{t('admin.login.title')}</h1>
          <p className="mt-1 text-[13px] text-ink-3">{t('admin.login.subtitle')}</p>

          <form onSubmit={handleSubmit} className="mt-6 space-y-4">
            <Field label={t('admin.login.password')}>
              <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder={t('admin.login.passwordPlaceholder')} autoComplete="current-password" />
            </Field>

            {error && <div className="rounded-md border border-err/25 bg-err/8 px-3 py-2 text-[13px] text-err">{error}</div>}

            <Button type="submit" variant="primary" size="lg" className="w-full" loading={loading}>
              {t('admin.login.submit')}
            </Button>
          </form>
        </div>
      </main>
    </div>
  )
}

export default function AdminLoginPage() {
  return (
    <Suspense fallback={null}>
      <AdminLoginForm />
    </Suspense>
  )
}