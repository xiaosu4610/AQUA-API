<script setup lang="ts">
/**
 * 登录页。
 *
 * 意图（Why）：
 *   登录是进入门户/后台的唯一入口，页面必须「干净聚焦」：
 *   只有必要信息（品牌、站点名、两个输入框、一个按钮），不放置营销内容分散注意力。
 *
 * 流转（Flow）：
 *   表单提交 → stores/auth.signIn() → POST /api/auth/login → 保存会话令牌
 *   → 跳转：优先 query.redirect（来源页），否则按角色 admin→/admin、普通→/console
 *
 * 扩展（Extend）：
 *   新增登录方式（如 OAuth）时，在表单下方追加区分隔线与对应按钮，
 *   并复用 auth store 的会话落盘逻辑（applySession）。
 */
import { computed, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import { ApiError } from '@/api/client'
import { toastError, toastSuccess } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const auth = useAuthStore()
const site = useSiteStore()
const router = useRouter()
const route = useRoute()

const username = ref('')
const password = ref('')
const showPassword = ref(false)
const submitting = ref(false)
const errorMessage = ref('')

/** 站点信息仍在加载时，注册入口显示为禁用态，避免误判「注册未开放」 */
const registrationReady = computed(() => !site.loading)

/**
 * 只接受站内相对路径作为回跳目标。
 * 为什么不直接用 query.redirect：避免开放重定向（?redirect=https://evil.com）被滥用。
 */
function resolveRedirect(fallback: string): string {
  const raw = route.query.redirect
  const target = Array.isArray(raw) ? raw[0] : raw
  if (typeof target === 'string' && target.startsWith('/') && !target.startsWith('//')) return target
  return fallback
}

async function handleSubmit(): Promise<void> {
  errorMessage.value = ''
  if (!username.value.trim() || !password.value) {
    errorMessage.value = '请输入用户名与密码'
    return
  }

  submitting.value = true
  try {
    const user = await auth.signIn({ username: username.value.trim(), password: password.value })
    toastSuccess(`欢迎回来，${user.username}`)
    const fallback = user.role === 10 ? '/admin' : '/console'
    await router.replace(resolveRedirect(fallback))
  } catch (error) {
    errorMessage.value = error instanceof ApiError ? error.message : '登录失败，请稍后重试'
  } finally {
    submitting.value = false
  }
}

/** 站点信息加载失败时，至少保证登录功能可用（提示条给出重试入口） */
function reloadSite(): void {
  void site.load(true)
  toastError(site.error || '重试中…')
}
</script>

<template>
  <div class="relative flex min-h-screen flex-col bg-ink-950">
    <!-- 背景：克制的一处光斑 + 网格，与落地页保持同一视觉语言 -->
    <div class="pointer-events-none absolute inset-0 bg-grid opacity-40" aria-hidden="true" />
    <div
      class="pointer-events-none absolute left-1/2 top-0 h-72 w-72 -translate-x-1/2 rounded-full bg-brand-500/15 blur-[100px]"
      aria-hidden="true"
    />

    <header class="relative mx-auto flex w-full max-w-6xl items-center justify-between px-5 py-5 lg:px-8">
      <RouterLink to="/" class="flex items-center gap-2.5">
        <img src="/favicon.ico" alt="" class="h-7 w-7 rounded-lg" />
        <span class="text-sm font-semibold text-white">{{ site.siteName }}</span>
      </RouterLink>
      <RouterLink to="/" class="btn btn-ghost btn-sm">
        <AppIcon name="chevron-left" :size="14" />
        返回首页
      </RouterLink>
    </header>

    <main class="relative flex flex-1 items-center justify-center px-5 pb-16">
      <div class="w-full max-w-md">
        <!-- 站点信息异常提示：登录本身不受影响 -->
        <div
          v-if="site.error"
          class="mb-4 flex items-center gap-2 rounded-xl border border-amber-500/25 bg-amber-500/10 px-3.5 py-2.5 text-xs text-amber-100"
        >
          <AppIcon name="alert" :size="15" class="text-amber-300" />
          <span class="flex-1">站点信息加载失败，登录仍可继续。</span>
          <button type="button" class="btn btn-ghost btn-sm text-amber-200" @click="reloadSite">重试</button>
        </div>

        <div class="card card-pad shadow-pop">
          <div>
            <h1 class="text-xl font-semibold tracking-tight text-white">登录 {{ site.siteName }}</h1>
            <p class="mt-1.5 text-sm text-ink-400">使用账号密码登录，管理你的访问令牌与调用记录。</p>
          </div>

          <form class="mt-6 space-y-4" @submit.prevent="handleSubmit">
            <div>
              <label class="label" for="login-username">用户名</label>
              <input
                id="login-username"
                v-model="username"
                class="input"
                type="text"
                autocomplete="username"
                placeholder="请输入用户名"
                :disabled="submitting"
              />
            </div>

            <div>
              <label class="label" for="login-password">密码</label>
              <div class="relative">
                <input
                  id="login-password"
                  v-model="password"
                  class="input pr-10"
                  :type="showPassword ? 'text' : 'password'"
                  autocomplete="current-password"
                  placeholder="请输入密码"
                  :disabled="submitting"
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

            <!-- 错误态：贴在按钮上方，视线自然落点 -->
            <p
              v-if="errorMessage"
              class="flex items-start gap-2 rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs leading-relaxed text-red-200"
            >
              <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
              {{ errorMessage }}
            </p>

            <button type="submit" class="btn btn-primary w-full" :disabled="submitting">
              <span
                v-if="submitting"
                class="h-4 w-4 animate-spin rounded-full border-2 border-ink-950/40 border-t-ink-950"
                aria-hidden="true"
              />
              <AppIcon v-else name="lock" :size="16" />
              {{ submitting ? '登录中…' : '登录' }}
            </button>
          </form>

          <div class="mt-5 border-t border-ink-800 pt-4 text-center text-xs text-ink-400">
            <template v-if="!registrationReady">
              <span>正在获取站点信息…</span>
            </template>
            <template v-else-if="site.registrationEnabled">
              还没有账号？
              <RouterLink to="/register" class="font-medium text-brand-300 transition-colors hover:text-brand-200">
                立即注册
              </RouterLink>
            </template>
            <template v-else>
              本站点未开放自助注册，请联系管理员开通账号。
            </template>
          </div>
        </div>

        <p class="mt-4 text-center text-xs leading-relaxed text-ink-500">
          登录凭据仅用于访问本网关控制台；调用模型需另外创建访问令牌。
        </p>
      </div>
    </main>
  </div>
</template>
