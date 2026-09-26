<script setup lang="ts">
/**
 * 安装向导页（/install）。
 *
 * 意图（Why）：
 *   把"部署完第一件事"从命令行搬到浏览器：新实例没有管理员时，
 *   访问本页即可设置管理员账号与站点名称，不必登录服务器执行命令。
 *   安装完成后本页不再可用（后端接口会返回 409），因此它天然是一次性页面。
 *
 * 流转（Flow）：
 *   进入 → fetchInstallStatus()
 *     ├─ installed=true  → 展示"已完成安装"，引导去 /admin/login
 *     └─ installed=false → 展示表单 → submitInstall() → 跳 /admin/login
 *
 * 扩展（Extend）：
 *   增加安装步骤（如校验数据库连通性）时，在此按 status 的字段分流展示；
 *   密码强度规则的权威在后端（crypto.ValidatePasswordStrength），
 *   前端只做"非空 / 两次一致"这类即时反馈，避免两边规则漂移。
 */
import { computed, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import { ApiError } from '@/api/client'
import { fetchInstallStatus, submitInstall, type InstallStatus } from '@/api/install'
import { toastSuccess } from '@/composables/useToast'

const router = useRouter()

const status = ref<InstallStatus | null>(null)
const loadingStatus = ref(true)
const loadError = ref('')

const username = ref('admin')
const password = ref('')
const confirmPassword = ref('')
const siteName = ref('')
const submitting = ref(false)
const submitError = ref('')

const installed = computed(() => status.value?.installed === true)

/** 页面加载时读取安装状态：决定展示安装表单还是引导去登录 */
async function loadStatus(): Promise<void> {
  loadingStatus.value = true
  loadError.value = ''
  try {
    const data = await fetchInstallStatus()
    status.value = data
    // 站点名留空时用后端值作占位，减少站长需要填的项
    siteName.value = data.site_name
  } catch (error) {
    loadError.value = error instanceof ApiError ? error.message : '无法连接服务端'
  } finally {
    loadingStatus.value = false
  }
}

async function handleSubmit(): Promise<void> {
  submitError.value = ''

  if (!password.value) {
    submitError.value = '请设置管理员密码'
    return
  }
  if (confirmPassword.value && confirmPassword.value !== password.value) {
    submitError.value = '两次输入的密码不一致'
    return
  }

  submitting.value = true
  try {
    const result = await submitInstall({
      username: username.value.trim(),
      password: password.value,
      confirm_password: confirmPassword.value,
      site_name: siteName.value.trim(),
    })
    toastSuccess(`安装完成，管理员账号：${result.admin_username}`)
    await router.replace(result.admin_login_path || '/admin/login')
  } catch (error) {
    submitError.value = error instanceof ApiError ? error.message : '安装失败，请稍后重试'
  } finally {
    submitting.value = false
  }
}

onMounted(loadStatus)
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
        <span class="text-sm font-semibold text-ink-50">AQUA-API 安装向导</span>
      </RouterLink>
      <RouterLink to="/" class="btn btn-ghost btn-sm">
        <AppIcon name="chevron-left" :size="14" />
        返回首页
      </RouterLink>
    </header>

    <main class="relative flex flex-1 items-center justify-center px-5 pb-16">
      <div class="w-full max-w-lg">
        <!-- 状态读取中 -->
        <div v-if="loadingStatus" class="card card-pad flex items-center gap-3 shadow-pop">
          <span class="h-4 w-4 animate-spin rounded-full border-2 border-brand-500/40 border-t-brand-500" />
          <span class="text-sm text-ink-300">正在读取安装状态…</span>
        </div>

        <!-- 状态读取失败：给出重试入口而不是空白页 -->
        <div v-else-if="loadError" class="card card-pad shadow-pop">
          <div class="flex items-start gap-2 rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs text-red-800">
            <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
            <span>{{ loadError }}</span>
          </div>
          <button type="button" class="btn btn-secondary mt-4 w-full" @click="loadStatus">重试</button>
        </div>

        <!-- 已安装：本页只做引导，不再提供任何写入入口 -->
        <div v-else-if="installed" class="card card-pad shadow-pop">
          <div class="flex items-center gap-2 text-emerald-700">
            <AppIcon name="check" :size="18" />
            <h1 class="text-lg font-semibold text-ink-50">系统已完成安装</h1>
          </div>
          <p class="mt-2 text-sm leading-relaxed text-ink-400">
            出于安全考虑，安装入口在首次安装完成后即永久关闭，避免他人重新初始化并接管站点。
            如需重置管理员密码，请在服务器上执行
            <code class="rounded bg-ink-850 px-1.5 py-0.5 text-xs text-ink-200">aqua -reset-password &lt;用户名&gt;</code>。
          </p>
          <div class="mt-5 flex flex-col gap-2 sm:flex-row">
            <RouterLink to="/admin/login" class="btn btn-primary flex-1 justify-center">
              <AppIcon name="shield" :size="16" />
              前往后台登录
            </RouterLink>
            <RouterLink to="/login" class="btn btn-secondary flex-1 justify-center">普通用户登录</RouterLink>
          </div>
        </div>

        <!-- 未安装：安装表单 -->
        <div v-else class="card card-pad shadow-pop">
          <div>
            <h1 class="text-xl font-semibold tracking-tight text-ink-50">初始化你的站点</h1>
            <p class="mt-1.5 text-sm text-ink-400">
              只需设置管理员账号，即可开始使用。安装完成后本页入口会自动关闭。
            </p>
          </div>

          <!-- 当前运行环境（只读）：让站长确认"数据存在哪里" -->
          <dl v-if="status" class="mt-5 space-y-1.5 rounded-xl bg-ink-850/60 px-3.5 py-3 text-xs">
            <div class="flex items-center justify-between gap-3">
              <dt class="text-ink-400">数据库</dt>
              <dd class="font-medium text-ink-200">
                {{ status.database_driver || '未知' }}
                <span v-if="status.sqlite_zero_config" class="ml-1 text-ink-500">（免配置，文件随服务自动创建）</span>
              </dd>
            </div>
            <div class="flex items-center justify-between gap-3">
              <dt class="text-ink-400">服务版本</dt>
              <dd class="font-medium text-ink-200">{{ status.version || '未知' }}</dd>
            </div>
          </dl>

          <form class="mt-6 space-y-4" @submit.prevent="handleSubmit">
            <div>
              <label class="label" for="install-username">管理员用户名</label>
              <input
                id="install-username"
                v-model="username"
                class="input"
                type="text"
                autocomplete="username"
                placeholder="默认 admin"
                :disabled="submitting"
              />
              <p class="mt-1.5 text-[11px] text-ink-500">后台入口只要求输入密码，用户名仅用于区分多个管理员。</p>
            </div>

            <div>
              <label class="label" for="install-password">管理员密码</label>
              <input
                id="install-password"
                v-model="password"
                class="input"
                type="password"
                autocomplete="new-password"
                placeholder="请设置一个足够强的密码"
                :disabled="submitting"
              />
            </div>

            <div>
              <label class="label" for="install-password-confirm">确认密码</label>
              <input
                id="install-password-confirm"
                v-model="confirmPassword"
                class="input"
                type="password"
                autocomplete="new-password"
                placeholder="再次输入相同密码"
                :disabled="submitting"
              />
            </div>

            <div>
              <label class="label" for="install-site-name">站点名称（可选）</label>
              <input
                id="install-site-name"
                v-model="siteName"
                class="input"
                type="text"
                placeholder="显示在页面标题与导航处"
                :disabled="submitting"
              />
            </div>

            <p
              v-if="submitError"
              class="flex items-start gap-2 rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs leading-relaxed text-red-800"
            >
              <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
              {{ submitError }}
            </p>

            <button type="submit" class="btn btn-primary w-full" :disabled="submitting">
              <span
                v-if="submitting"
                class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
                aria-hidden="true"
              />
              <AppIcon v-else name="shield" :size="16" />
              {{ submitting ? '正在安装…' : '完成安装' }}
            </button>
          </form>

          <p class="mt-4 border-t border-ink-800 pt-4 text-[11px] leading-relaxed text-ink-500">
            安装完成后请立即用该账号登录后台，并在「系统设置」中确认站点域名与收录参数。
          </p>
        </div>
      </div>
    </main>
  </div>
</template>
