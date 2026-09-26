<script setup lang="ts">
/**
 * 管理后台 · 系统设置。
 *
 * 意图（Why）：
 *   站点名称、注册开关、注册是否必须邮箱验证码等运营参数必须由管理员在界面上
 *   调整，而不是改配置文件或重启服务。
 *
 *   本页最关键的设计是「注册开关」与「邮箱验证码校验」的联动提示：
 *   若邮件通道未就绪（SMTP 未配置），开启验证码校验会导致所有用户注册失败，
 *   因此这里会把状态直接展示出来，并在保存时给出明确阻拦。
 *
 * 流转（Flow）：
 *   进入页面 → fetchSettings() 读取 → 表单双向绑定
 *   → 「保存」→ updateSettings(仅变更字段) → toast 反馈 → 重新拉取（校正只读字段）
 *
 * 扩展（Extend）：
 *   新增设置项：在 types.ts 的 SiteSettings / UpdateSiteSettingsPayload 加字段，
 *   本页加表单项，后端 LoadSiteSettings / ToMap 同步补映射（三处必须同步）。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import { fetchSettings, updateSettings } from '@/api/admin'
import { ApiError } from '@/api/client'
import type { SiteSettings, UpdateSiteSettingsPayload } from '@/api/types'
import { toastError, toastSuccess } from '@/composables/useToast'
import { useSiteStore } from '@/stores/site'

const site = useSiteStore()

const loading = ref(false)
const loadError = ref('')
const saving = ref(false)
const settings = ref<SiteSettings | null>(null)

/* 表单字段（与 settings 分离：便于「取消修改」时一键还原） */
const siteName = ref('')
const siteDescription = ref('')
const registrationEnabled = ref(false)
const requireEmailCode = ref(false)
const defaultUserQuota = ref(0)
const defaultGroup = ref('')

/** 邮件通道是否就绪（只读，由服务端环境变量决定） */
const emailReady = computed(() => settings.value?.email_service_ready === true)
const emailFrom = computed(() => settings.value?.email_from || '')

/**
 * 危险组合：开启了邮箱验证码校验，但邮件通道未配置。
 * 此时任何保存动作都应被阻止，否则会立刻造成"全站无法注册"。
 */
const emailCodeUnavailable = computed(() => requireEmailCode.value && !emailReady.value)

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    const data = await fetchSettings()
    applyToForm(data)
  } catch (error) {
    loadError.value = error instanceof ApiError ? error.message : '加载系统设置失败'
  } finally {
    loading.value = false
  }
}

/** 把服务端数据填充到表单 */
function applyToForm(data: SiteSettings): void {
  settings.value = data
  siteName.value = data.site_name
  siteDescription.value = data.site_description
  registrationEnabled.value = data.registration_enabled
  requireEmailCode.value = data.registration_require_email_code
  defaultUserQuota.value = data.default_user_quota
  defaultGroup.value = data.default_group
}

async function handleSave(): Promise<void> {
  if (emailCodeUnavailable.value) {
    toastError('邮件服务未配置，无法开启邮箱验证码校验')
    return
  }

  const payload: UpdateSiteSettingsPayload = {
    site_name: siteName.value.trim(),
    site_description: siteDescription.value.trim(),
    registration_enabled: registrationEnabled.value,
    registration_require_email_code: requireEmailCode.value,
    default_user_quota: Number(defaultUserQuota.value),
    default_group: defaultGroup.value.trim(),
  }

  saving.value = true
  try {
    await updateSettings(payload)
    toastSuccess('系统设置已保存')
    // 重新拉取：邮箱通道等只读字段由服务端决定，避免前端展示与实际不一致
    await load()
    // 站点名称可能已变化，刷新全局站点信息（页脚/标题等立即生效）
    await site.load(true)
  } catch (error) {
    toastError(error instanceof ApiError ? error.message : '保存失败，请稍后重试')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1 class="page-title">系统设置</h1>
        <p class="page-desc">站点名称、注册策略与新用户默认额度。修改后立即生效，无需重启服务。</p>
      </div>
      <button type="button" class="btn btn-primary" :disabled="saving || loading || emailCodeUnavailable" @click="handleSave">
        <span
          v-if="saving"
          class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
          aria-hidden="true"
        />
        <AppIcon v-else name="check" :size="16" />
        {{ saving ? '保存中…' : '保存设置' }}
      </button>
    </div>

    <DataState
      v-if="loading || loadError"
      :loading="loading"
      :error="loadError"
      loading-text="正在读取系统设置…"
      @retry="load"
    />

    <div v-else class="grid gap-5 lg:grid-cols-3">
      <!-- 站点信息 -->
      <section class="card lg:col-span-2">
        <div class="card-head">
          <div>
            <h2 class="section-title">站点信息</h2>
            <p class="mt-0.5 text-xs text-ink-400">用于落地页、注册页与页面标题</p>
          </div>
        </div>
        <div class="card-pad space-y-4">
          <div>
            <label class="label" for="setting-site-name">站点名称</label>
            <input id="setting-site-name" v-model="siteName" class="input" type="text" placeholder="AQUA-API" />
          </div>
          <div>
            <label class="label" for="setting-site-desc">站点描述</label>
            <input
              id="setting-site-desc"
              v-model="siteDescription"
              class="input"
              type="text"
              placeholder="一句话说明本站定位"
            />
          </div>
        </div>
      </section>

      <!-- 注册策略 -->
      <section class="card">
        <div class="card-head">
          <h2 class="section-title">注册策略</h2>
        </div>
        <div class="card-pad space-y-4">
          <label class="flex cursor-pointer items-start gap-3">
            <input v-model="registrationEnabled" class="checkbox mt-0.5" type="checkbox" />
            <span>
              <span class="block text-sm text-ink-100">开放自助注册</span>
              <span class="mt-0.5 block text-xs leading-relaxed text-ink-400">
                关闭后注册页会提示"未开放"，只能由管理员在用户管理中创建账号。
              </span>
            </span>
          </label>

          <label class="flex cursor-pointer items-start gap-3 border-t border-ink-800 pt-4">
            <input v-model="requireEmailCode" class="checkbox mt-0.5" type="checkbox" :disabled="!registrationEnabled" />
            <span>
              <span class="block text-sm text-ink-100">注册必须邮箱验证码</span>
              <span class="mt-0.5 block text-xs leading-relaxed text-ink-400">
                用户需先获取邮箱验证码才能完成注册，可有效拦截脚本批量注册。
              </span>
            </span>
          </label>

          <!-- 邮件通道状态：这是"能不能开启验证码"的前置条件，必须显式展示 -->
          <div
            class="rounded-lg border px-3 py-2.5 text-xs leading-relaxed"
            :class="
              emailReady
                ? 'border-emerald-500/25 bg-emerald-500/10 text-emerald-700'
                : 'border-amber-500/25 bg-amber-500/10 text-amber-700'
            "
          >
            <p class="flex items-center gap-1.5 font-medium">
              <AppIcon :name="emailReady ? 'check' : 'alert'" :size="14" />
              {{ emailReady ? '邮件通道已就绪' : '邮件通道未配置' }}
            </p>
            <p v-if="emailReady" class="mt-1">发件地址：{{ emailFrom }}</p>
            <p v-else class="mt-1">
              需在服务端配置 SMTP 环境变量后才能开启邮箱验证码校验。
            </p>
          </div>
        </div>
      </section>

      <!-- 新用户默认值 -->
      <section class="card lg:col-span-3">
        <div class="card-head">
          <div>
            <h2 class="section-title">新用户默认值</h2>
            <p class="mt-0.5 text-xs text-ink-400">仅对"之后注册"的账号生效，已有账号不受影响</p>
          </div>
        </div>
        <div class="card-pad grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label" for="setting-quota">默认额度</label>
            <input id="setting-quota" v-model.number="defaultUserQuota" class="input input-mono" type="number" min="-1" />
            <p class="hint">-1 表示不限额度；0 表示注册后需管理员分配额度才可调用。</p>
          </div>
          <div>
            <label class="label" for="setting-group">默认分组</label>
            <input id="setting-group" v-model="defaultGroup" class="input input-mono" type="text" placeholder="default" />
            <p class="hint">用于渠道与令牌的分组匹配，通常保持默认即可。</p>
          </div>
        </div>
      </section>
    </div>
  </div>
</template>
