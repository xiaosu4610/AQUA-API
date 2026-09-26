<script setup lang="ts">
/**
 * 注册页（受站点 registration_enabled 开关控制）。
 *
 * 意图（Why）：
 *   注册开关是站点级配置，若未开放必须「明确告知原因」而不是给一个点了报错的表单；
 *   因此页面在加载站点信息时会区分三种状态：加载中 / 未开放 / 可注册。
 *
 *   另一个关键点是邮箱验证码：站点可以要求"注册必须验证邮箱"，
 *   此时邮箱与验证码都是必填项。校验规则不在前端硬编码，
 *   而是读站点信息里的 email_code_required，避免前后端规则不一致。
 *
 * 流转（Flow）：
 *   stores/site 提供 registration_enabled / email_code_required → 本页决定渲染内容
 *   → 「发送验证码」→ POST /api/auth/email-code → 启动冷却倒计时
 *   → 「创建账号」→ stores/auth.signUp() → POST /api/auth/register → 进入登录态 → 跳 /console
 *
 * 扩展（Extend）：
 *   若后端新增其他校验（如手机号），沿用同样模式：站点信息暴露开关 → 本页条件渲染。
 */
import { computed, onUnmounted, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import { ApiError } from '@/api/client'
import { sendEmailCode } from '@/api/auth'
import type { RegisterPayload } from '@/api/types'
import { toastSuccess } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const auth = useAuthStore()
const site = useSiteStore()
const router = useRouter()
const route = useRoute()

const username = ref('')
const password = ref('')
const confirmPassword = ref('')
const email = ref('')
const emailCode = ref('')
const showPassword = ref(false)
const submitting = ref(false)
const errorMessage = ref('')
const codeError = ref('')
const sendingCode = ref(false)

/**
 * 邀请码：从邀请链接 ?invite=CODE 读取并随注册提交。
 *
 * 说明：邀请码是可选字段——填错或过期时后端会忽略并照常注册成功，
 * 因此这里不对其做任何格式校验，原样透传即可。
 */
const inviteCode = ref(typeof route.query.invite === 'string' ? route.query.invite.trim() : '')

/** 重发倒计时（秒）。0 表示可以发送。 */
const countdown = ref(0)
/** 用于清理计时器；组件卸载时必须清掉，否则会在页面切换后继续跑。 */
let countdownTimer: number | undefined

/** 与后端保持一致的最小邮箱校验；真正的可用性由"能否收到验证码"验证。 */
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

/**
 * 口令校验：只要求"填了且两次一致"。
 *
 * 刻意不做最小长度与字符类型限制——站点允许任意口令（1 位、纯中文、
 * 含空格或特殊符号都可以）。规则由后端统一裁决，前端只拦明显无意义的输入，
 * 避免出现"前端拒绝但后端允许"的割裂。
 */
const passwordMismatch = computed(() => Boolean(confirmPassword.value) && password.value !== confirmPassword.value)

/** 邮箱验证码是否为必填（由后台开关控制） */
const needEmailCode = computed(() => site.emailCodeRequired)
/** 开关已开启但邮件通道未就绪：提前提示，避免用户白等邮件 */
const emailUnavailable = computed(() => needEmailCode.value && !site.emailServiceReady)

function startCountdown(seconds: number): void {
  countdown.value = seconds > 0 ? seconds : 60
  if (countdownTimer !== undefined) window.clearInterval(countdownTimer)
  countdownTimer = window.setInterval(() => {
    countdown.value -= 1
    if (countdown.value <= 0) {
      window.clearInterval(countdownTimer)
      countdownTimer = undefined
    }
  }, 1000)
}

onUnmounted(() => {
  if (countdownTimer !== undefined) window.clearInterval(countdownTimer)
})

/** 申请邮箱验证码 */
async function handleSendCode(): Promise<void> {
  codeError.value = ''
  const value = email.value.trim()

  if (!value) {
    codeError.value = '请先填写邮箱'
    return
  }
  if (!EMAIL_PATTERN.test(value)) {
    codeError.value = '邮箱格式不正确'
    return
  }
  if (countdown.value > 0 || sendingCode.value) return

  sendingCode.value = true
  try {
    const result = await sendEmailCode(value)
    toastSuccess(result.message || '验证码已发送')
    // 冷却秒数由后端下发：前端硬编码会在后端调整策略后失配
    startCountdown(result.cooldown || 60)
  } catch (error) {
    codeError.value = error instanceof ApiError ? error.message : '验证码发送失败，请稍后重试'
  } finally {
    sendingCode.value = false
  }
}

async function handleSubmit(): Promise<void> {
  errorMessage.value = ''

  if (!username.value.trim()) {
    errorMessage.value = '请输入用户名'
    return
  }
  // 用户名不再要求最小长度（1 个字符即可），只拦"全空白"
  if (!password.value) {
    errorMessage.value = '请输入密码'
    return
  }
  if (password.value !== confirmPassword.value) {
    errorMessage.value = '两次输入的密码不一致'
    return
  }
  // 仅在站点要求时才强制校验邮箱与验证码（开关关闭时邮箱为可选项）
  if (needEmailCode.value) {
    if (!email.value.trim()) {
      errorMessage.value = '请填写邮箱'
      return
    }
    if (!emailCode.value.trim()) {
      errorMessage.value = '请填写邮箱验证码'
      return
    }
  }

  submitting.value = true
  try {
    // 邀请码通过扩展出一层可选字段传递：types.ts 的 RegisterPayload 由主协调者维护，
    // 这里不修改共享类型，仅在本页构造带 invite_code 的请求体。
    const payload: RegisterPayload & { invite_code?: string } = {
      username: username.value.trim(),
      password: password.value,
      email: email.value.trim() || undefined,
      code: emailCode.value.trim() || undefined,
    }
    if (inviteCode.value) payload.invite_code = inviteCode.value

    const user = await auth.signUp(payload)
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
        <span class="text-sm font-semibold text-ink-50">{{ site.siteName }}</span>
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
          <span class="mx-auto flex h-11 w-11 items-center justify-center rounded-xl bg-ink-850 text-amber-700 ring-1 ring-inset ring-amber-500/25">
            <AppIcon name="alert" :size="20" />
          </span>
          <h1 class="mt-4 text-base font-semibold text-ink-50">无法获取站点信息</h1>
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
          <h1 class="mt-4 text-base font-semibold text-ink-50">当前未开放自助注册</h1>
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
          <h1 class="text-xl font-semibold tracking-tight text-ink-50">注册 {{ site.siteName }}</h1>
          <p class="mt-1.5 text-sm text-ink-400">注册后即可登录控制台，创建访问令牌并查看调用用量。</p>

          <!-- 通过邀请链接进入时提示邀请码已带入（无需用户手动填写） -->
          <p
            v-if="inviteCode"
            class="mt-4 flex items-center gap-2 rounded-lg border border-brand-500/25 bg-brand-500/10 px-3 py-2 text-xs text-brand-700"
          >
            <AppIcon name="users" :size="14" />
            已带入邀请码 <code class="font-mono">{{ inviteCode }}</code>，注册成功后邀请人将获得奖励。
          </p>

          <form class="mt-6 space-y-4" @submit.prevent="handleSubmit">
            <div>
              <label class="label" for="register-username">用户名 <span class="text-red-600">*</span></label>
              <input
                id="register-username"
                v-model="username"
                class="input"
                type="text"
                autocomplete="username"
                placeholder="任意长度，中英文、符号均可"
                :disabled="submitting"
              />
            </div>

            <div>
              <label class="label" for="register-password">密码 <span class="text-red-600">*</span></label>
              <div class="relative">
                <input
                  id="register-password"
                  v-model="password"
                  class="input pr-10"
                  :type="showPassword ? 'text' : 'password'"
                  autocomplete="new-password"
                  placeholder="任意长度与字符，中文、符号均可"
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
              <p class="hint">
                支持任意长度与字符：中文、空格、特殊符号均可，无需符合任何复杂度规则。
              </p>
            </div>

            <div>
              <label class="label" for="register-confirm">确认密码 <span class="text-red-600">*</span></label>
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
              <label class="label" for="register-email">
                邮箱
                <span v-if="needEmailCode" class="text-red-600">*</span>
                <span v-else class="text-ink-500">（可选）</span>
              </label>
              <input
                id="register-email"
                v-model="email"
                class="input"
                type="email"
                autocomplete="email"
                :placeholder="needEmailCode ? '用于接收注册验证码' : '用于后续找回密码等场景'"
                :disabled="submitting"
              />
            </div>

            <!-- 邮箱验证码：仅在站点开启校验时出现 -->
            <div v-if="needEmailCode">
              <label class="label" for="register-code">
                邮箱验证码 <span class="text-red-600">*</span>
              </label>
              <div class="flex gap-2">
                <input
                  id="register-code"
                  v-model="emailCode"
                  class="input input-mono flex-1"
                  type="text"
                  inputmode="numeric"
                  autocomplete="one-time-code"
                  maxlength="6"
                  placeholder="6 位数字"
                  :disabled="submitting"
                />
                <button
                  type="button"
                  class="btn btn-secondary shrink-0"
                  :disabled="submitting || sendingCode || countdown > 0"
                  @click="handleSendCode"
                >
                  <span
                    v-if="sendingCode"
                    class="h-3.5 w-3.5 animate-spin rounded-full border-2 border-ink-500/40 border-t-ink-500"
                    aria-hidden="true"
                  />
                  <AppIcon v-else name="mail" :size="15" />
                  {{ countdown > 0 ? `${countdown} 秒后重发` : sendingCode ? '发送中…' : '获取验证码' }}
                </button>
              </div>
              <p v-if="codeError" class="field-error">{{ codeError }}</p>
              <p v-else-if="emailUnavailable" class="mt-1 text-xs text-amber-700">
                站点邮件服务尚未就绪，请联系管理员。
              </p>
              <p v-else class="hint">验证码 5 分钟内有效，请留意垃圾邮件目录。</p>
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
              <AppIcon v-else name="plus" :size="16" />
              {{ submitting ? '注册中…' : '创建账号' }}
            </button>
          </form>

          <p class="mt-5 border-t border-ink-800 pt-4 text-center text-xs text-ink-400">
            已有账号？
            <RouterLink to="/login" class="font-medium text-brand-700 transition-colors hover:text-brand-700">
              返回登录
            </RouterLink>
          </p>
        </div>
      </div>
    </main>
  </div>
</template>
