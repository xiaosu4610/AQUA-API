<script setup lang="ts">
/**
 * 超管入口登录页（/admin/login）。
 *
 * 意图（Why）：
 *   后台是站长的私人入口，与面向用户的 /login 分开有两个好处：
 *     1) 站长只需记住并输入密码，不必回忆用户名（自托管站点通常只有一个管理员）；
 *     2) 普通用户看不到后台入口，减少"后台在哪、我能不能进"的噪音。
 *
 * 流转（Flow）：
 *   提交密码 → stores/auth.signInAsAdmin() → POST /api/auth/admin-login
 *   → 保存会话 → 跳转：优先 query.redirect（站内相对路径），否则 /admin
 *
 * 扩展（Extend）：
 *   将来支持多管理员时，后端会在管理员数量过多时返回明确错误并提示改用
 *   "用户名 + 密码"（见 /login），本页无需改动即可把该提示展示出来。
 */
import { ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import { ApiError } from '@/api/client'
import { toastSuccess } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const auth = useAuthStore()
const site = useSiteStore()
const router = useRouter()
const route = useRoute()

const password = ref('')
const showPassword = ref(false)
const submitting = ref(false)
const errorMessage = ref('')

/**
 * 只接受站内相对路径作为回跳目标，避免开放重定向。
 * 与 /login 的实现保持一致（避免两处规则漂移）。
 */
function resolveRedirect(): string {
  const raw = route.query.redirect
  const target = Array.isArray(raw) ? raw[0] : raw
  if (typeof target === 'string' && target.startsWith('/') && !target.startsWith('//')) return target
  return '/admin'
}

async function handleSubmit(): Promise<void> {
  errorMessage.value = ''
  if (!password.value) {
    errorMessage.value = '请输入管理员密码'
    return
  }

  submitting.value = true
  try {
    const user = await auth.signInAsAdmin(password.value)
    toastSuccess(`欢迎回来，${user.username}`)
    await router.replace(resolveRedirect())
  } catch (error) {
    errorMessage.value = error instanceof ApiError ? error.message : '登录失败，请稍后重试'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="relative flex min-h-screen flex-col bg-ink-950">
    <div class="pointer-events-none absolute inset-0 bg-grid opacity-40" aria-hidden="true" />
    <div
      class="pointer-events-none absolute left-1/2 top-0 h-72 w-72 -translate-x-1/2 rounded-full bg-brand-500/15 blur-[100px]"
      aria-hidden="true"
    />

    <header class="relative mx-auto flex w-full max-w-6xl items-center justify-between px-5 py-5 lg:px-8">
      <RouterLink to="/" class="flex items-center gap-2.5">
        <img src="/favicon.ico" alt="" class="h-7 w-7 rounded-lg" />
        <span class="text-sm font-semibold text-ink-50">{{ site.siteName }}</span>
      </RouterLink>
      <RouterLink to="/login" class="btn btn-ghost btn-sm">
        <AppIcon name="chevron-left" :size="14" />
        普通用户登录
      </RouterLink>
    </header>

    <main class="relative flex flex-1 items-center justify-center px-5 pb-16">
      <div class="w-full max-w-md">
        <div class="card card-pad shadow-pop">
          <div class="flex items-center gap-2">
            <span class="flex h-9 w-9 items-center justify-center rounded-lg bg-brand-500/15 text-brand-700">
              <AppIcon name="shield" :size="18" />
            </span>
            <div>
              <h1 class="text-lg font-semibold tracking-tight text-ink-50">管理后台</h1>
              <p class="text-xs text-ink-400">请输入管理员密码</p>
            </div>
          </div>

          <form class="mt-6 space-y-4" @submit.prevent="handleSubmit">
            <div>
              <label class="label" for="admin-password">密码</label>
              <div class="relative">
                <input
                  id="admin-password"
                  v-model="password"
                  class="input pr-10"
                  :type="showPassword ? 'text' : 'password'"
                  autocomplete="current-password"
                  placeholder="管理员密码"
                  :disabled="submitting"
                  autofocus
                />
                <button
                  type="button"
                  class="absolute right-2 top-1/2 -translate-y-1/2 rounded-md p-1 text-ink-400 transition-colors hover:text-ink-200"
                  :aria-label="showPassword ? '隐藏密码' : '显示密码'"
                  @click="showPassword = !showPassword"
                >
                  <AppIcon :name="showPassword ? 'eye-off' : 'eye'" :size="16" />
                </button>
              </div>
            </div>

            <p
              v-if="errorMessage"
              class="flex items-start gap-2 rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs leading-relaxed text-red-800"
            >
              <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
              {{ errorMessage }}
            </p>

            <button type="submit" class="btn btn-primary w-full" :disabled="submitting">
              <span
                v-if="submitting"
                class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
                aria-hidden="true"
              />
              <AppIcon v-else name="lock" :size="16" />
              {{ submitting ? '登录中…' : '进入后台' }}
            </button>
          </form>

          <p class="mt-5 border-t border-ink-800 pt-4 text-[11px] leading-relaxed text-ink-500">
            首次部署请先访问 <RouterLink to="/install" class="text-brand-700">/install</RouterLink> 完成安装。
            忘记密码可在服务器上执行
            <code class="rounded bg-ink-850 px-1.5 py-0.5 text-ink-200">aqua -reset-password &lt;用户名&gt;</code>。
          </p>
        </div>
      </div>
    </main>
  </div>
</template>
