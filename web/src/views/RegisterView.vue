<script setup lang="ts">
/**
 * 注册页（受站点 registration_enabled 开关控制）。
 *
 * 意图（Why）：
 *   注册开关是站点级配置，若未开放必须「明确告知原因」而不是给一个点了报错的表单；
 *   因此页面在加载站点信息时会区分三种状态：加载中 / 未开放 / 可注册。
 *
 * 流转（Flow）：
 *   stores/site 提供 registration_enabled → 本页决定渲染表单或提示
 *   → stores/auth.signUp() → POST /api/auth/register → 直接进入登录态 → 跳 /console
 *
 * 扩展（Extend）：
 *   若后端将来要求邮箱验证，在提交成功分支改为「提示查收邮件」而不是直接跳转。
 */
import { computed, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import { ApiError } from '@/api/client'
import { toastSuccess } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const auth = useAuthStore()
const site = useSiteStore()
const router = useRouter()

const username = ref('')
const password = ref('')
const confirmPassword = ref('')
const email = ref('')
const showPassword = ref(false)
const submitting = ref(false)
const errorMessage = ref('')

/** 契约要求密码至少 8 位；这里同时校验两次输入一致，减少无谓的请求 */
const passwordRule = computed(() => password.value.length >= 8)
const passwordMismatch = computed(() => Boolean(confirmPassword.value) && password.value !== confirmPassword.value)

async function handleSubmit(): Promise<void> {
  errorMessage.value = ''

  if (!username.value.trim()) {
    errorMessage.value = '请输入用户名'
    return
  }
  if (username.value.trim().length < 3) {
    errorMessage.value = '用户名至少 3 个字符'
    return
  }
  if (!passwordRule.value) {
    errorMessage.value = '密码至少 8 位'
    return
  }
  if (password.value !== confirmPassword.value) {
    errorMessage.value = '两次输入的密码不一致'
    return
  }

  submitting.value = true
  try {
    const user = await auth.signUp({
      username: username.value.trim(),
      password: password.value,
      email: email.value.trim() || undefined,
    })
    toastSuccess(`注册成功，欢迎 ${user.username}`)
    await router.replace('/console')
  } catch (error) {
    errorMessage.value = error instanceof ApiError ? error.message : '注册失败，请稍后重试'
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
        <span class="text-sm font-semibold text-white">{{ site.siteName }}</span>
      </RouterLink>
      <RouterLink to="/login" class="btn btn-ghost btn-sm">
        <AppIcon name="chevron-left" :size="14" />
        返回登录
      </RouterLink>
    </header>

    <main class="relative flex flex-1 items-center justify-center px-5 pb-16">
      <div class="w-full max-w-md">
        <!-- 站点信息加载中：先给骨架，避免「未开放注册」的误读 -->
        <div v-if="site.loading && !site.status" class="card card-pad">
          <div class="h-6 w-40 skeleton" />
          <div class="mt-3 h-4 w-56 skeleton" />
          <div class="mt-6 space-y-3">
            <div class="h-10 w-full skeleton" />
            <div class="h-10 w-full skeleton" />
            <div class="h-10 w-full skeleton" />
          </div>
        </div>

        <!-- 站点信息加载失败且未开放：给出重试 -->
        <div v-else-if="site.error && !site.status" class="card card-pad text-center">
          <span class="mx-auto flex h-11 w-11 items-center justify-center rounded-xl bg-ink-850 text-amber-300 ring-1 ring-inset ring-amber-500/25">
            <AppIcon name="alert" :size="20" />
          </span>
          <h1 class="mt-4 text-base font-semibold text-white">无法获取站点信息</h1>
          <p class="mt-2 text-sm leading-relaxed text-ink-400">{{ site.error }}</p>
          <div class="mt-5 flex justify-center gap-2">
            <button type="button" class="btn btn-secondary" @click="site.load(true)">
              <AppIcon name="refresh" :size="15" />
              重试
            </button>
            <RouterLink to="/login" class="btn btn-ghost">直接登录</RouterLink>
          </div>
        </div>

        <!-- 未开放注册：明确告知 + 给出替代路径 -->
        <div v-else-if="!site.registrationEnabled" class="card card-pad text-center">
          <span class="mx-auto flex h-11 w-11 items-center justify-center rounded-xl bg-ink-850 text-ink-300 ring-1 ring-inset ring-ink-700">
            <AppIcon name="lock" :size="20" />
          </span>
          <h1 class="mt-4 text-base font-semibold text-white">当前未开放自助注册</h1>
          <p class="mt-2 text-sm leading-relaxed text-ink-400">
            站点管理员已关闭注册入口。如需要账号，请联系管理员为你在后台创建。
          </p>
          <div class="mt-5 flex justify-center gap-2">
            <RouterLink to="/login" class="btn btn-primary">返回登录</RouterLink>
            <RouterLink to="/" class="btn btn-secondary">回到首页</RouterLink>
          </div>
        </div>

        <!-- 注册表单 -->
        <div v-else class="card card-pad shadow-pop">
          <h1 class="text-xl font-semibold tracking-tight text-white">注册 {{ site.siteName }}</h1>
          <p class="mt-1.5 text-sm text-ink-400">注册后即可登录控制台，创建访问令牌并查看调用用量。</p>

          <form class="mt-6 space-y-4" @submit.prevent="handleSubmit">
            <div>
              <label class="label" for="register-username">用户名 <span class="text-red-400">*</span></label>
              <input
                id="register-username"
                v-model="username"
                class="input"
                type="text"
                autocomplete="username"
                placeholder="3 个字符以上，建议使用英文或数字"
                :disabled="submitting"
              />
            </div>

            <div>
              <label class="label" for="register-password">密码 <span class="text-red-400">*</span></label>
              <div class="relative">
                <input
                  id="register-password"
                  v-model="password"
                  class="input pr-10"
                  :type="showPassword ? 'text' : 'password'"
                  autocomplete="new-password"
                  placeholder="至少 8 位"
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
              <p class="hint" :class="password && !passwordRule ? 'text-amber-300' : ''">
                密码长度至少 8 位。
              </p>
            </div>

            <div>
              <label class="label" for="register-confirm">确认密码 <span class="text-red-400">*</span></label>
              <input
                id="register-confirm"
                v-model="confirmPassword"
                class="input"
                :class="passwordMismatch ? 'input-invalid' : ''"
                :type="showPassword ? 'text' : 'password'"
                autocomplete="new-password"
                placeholder="再次输入密码"
                :disabled="submitting"
              />
              <p v-if="passwordMismatch" class="field-error">两次输入的密码不一致</p>
            </div>

            <div>
              <label class="label" for="register-email">邮箱（可选）</label>
              <input
                id="register-email"
                v-model="email"
                class="input"
                type="email"
                autocomplete="email"
                placeholder="用于后续找回密码等场景"
                :disabled="submitting"
              />
            </div>

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
              <AppIcon v-else name="plus" :size="16" />
              {{ submitting ? '注册中…' : '创建账号' }}
            </button>
          </form>

          <p class="mt-5 border-t border-ink-800 pt-4 text-center text-xs text-ink-400">
            已有账号？
            <RouterLink to="/login" class="font-medium text-brand-300 transition-colors hover:text-brand-200">
              返回登录
            </RouterLink>
          </p>
        </div>
      </div>
    </main>
  </div>
</template>
