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
 *   新增通用设置项：在 types.ts 的 SiteSettings / UpdateSiteSettingsPayload 加字段，
 *   本页加表单项，后端 LoadSiteSettings / ToMap 同步补映射（三处必须同步）。
 *   支付通道不在此硬编码：通道与字段由后端 payment_channels 下发，
 *   本页只按 field.kind 触发式渲染（勾选哪个通道才展开它的字段），
 *   因此后端新增支付通道时本页无需改动。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import CopyButton from '@/components/CopyButton.vue'
import DataState from '@/components/DataState.vue'
import { fetchSettings, updateSettings } from '@/api/admin'
import { ApiError } from '@/api/client'
import type { PaymentChannel, SiteSettings, UpdateSiteSettingsPayload } from '@/api/types'
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

/* 充值 / 支付的「通用项」。
   金额与汇率一律用"分"或整数承载：金额用分、汇率用"1 元可兑换的额度"，
   避免浮点在各处来回换算（这是支付类系统最经典的资损来源）。
   通道各自的参数不在这里硬编码 —— 见下方 paymentChannels。 */
const paymentEnabled = ref(false)
const paymentExchangeRate = ref(100)
const paymentCurrency = ref('CNY')
const paymentMinYuan = ref(1)
const paymentMaxYuan = ref(0)
const paymentOrderTTL = ref(30)
const paymentNotifyBase = ref('')

/**
 * 支付通道清单：完全由后端下发（字段描述 + 当前值 + 密钥就绪状态）。
 *
 * 为什么不在前端硬编码：通道与字段是"该支付方式需要什么"的知识，只有后端知道；
 * 放前端会出现"后端加了字段、前端忘了加输入框"的静默漏配。
 * 前端只负责按 kind 触发式渲染（勾选哪个通道才展开它的字段）。
 */
const paymentChannels = ref<PaymentChannel[]>([])
/** 通道勾选态：channel.key -> 是否启用 */
const checkedChannels = ref<Record<string, boolean>>({})
/**
 * setting 字段的本地值：setting_key -> value。
 *
 * 键形如 "epay.gateway"，本身已含通道前缀，因此可全局唯一，无需按通道嵌套；
 * 提交时原样组装成 payment.params 交给后端。
 */
const paymentParams = ref<Record<string, string>>({})

/** 邮件通道是否就绪（只读，由服务端环境变量决定） */
const emailReady = computed(() => settings.value?.email_service_ready === true)
const emailFrom = computed(() => settings.value?.email_from || '')

/** 兑换比例是否有效：启用充值但比例为 0 会让用户"付钱却不到账" */
const paymentRateInvalid = computed(() => paymentEnabled.value && Number(paymentExchangeRate.value) <= 0)

/** 站点对外基址：优先用管理员填写的回调基址，否则退回浏览器访问地址 */
const siteOrigin = computed(() => {
  const base = paymentNotifyBase.value.trim().replace(/\/+$/, '')
  return base || window.location.origin
})

/** 通道状态文案：即将支持 / 密钥未注入 / 可用 */
function channelStatusText(channel: PaymentChannel): string {
  if (!channel.available) return '即将支持'
  if (channel.missing_env.length > 0) return '密钥未注入'
  return '可用'
}

/** 通道状态配色：可用绿、未就绪黄、未实现灰 */
function channelStatusClass(channel: PaymentChannel): string {
  if (!channel.available) return 'badge badge-off'
  if (channel.missing_env.length > 0) return 'badge badge-warn'
  return 'badge badge-ok'
}

/** 拼出该通道的完整回调地址（配错它是"付了钱不到账"的最常见原因） */
function notifyURL(channel: PaymentChannel): string {
  return channel.notify_path ? `${siteOrigin.value}${channel.notify_path}` : ''
}

/** 通道是否被勾选 */
function isChannelChecked(channel: PaymentChannel): boolean {
  return checkedChannels.value[channel.key] === true
}

/** 勾选 / 取消勾选通道（未实现的通道不允许勾选） */
function toggleChannel(channel: PaymentChannel, checked: boolean): void {
  if (!channel.available) return
  checkedChannels.value[channel.key] = checked
}

/** switch 字段的当前状态（以字符串 "true"/"false" 存，与后端 params 的字符串值一致） */
function isSwitchOn(settingKey: string): boolean {
  return paymentParams.value[settingKey] === 'true'
}

/** 切换 switch 字段 */
function setSwitch(settingKey: string, on: boolean): void {
  paymentParams.value[settingKey] = on ? 'true' : 'false'
}

/**
 * 已勾选通道的配置问题清单。
 *
 * 提前拦截三类"开了也用不了"的配置，避免站长保存后才发现充值页下不了单：
 *   1) 勾选了尚未实现的通道；
 *   2) 密钥未注入（后端也会拒绝）；
 *   3) 必填的 setting 字段为空。
 */
const paymentChannelIssues = computed<string[]>(() => {
  const issues: string[] = []
  for (const channel of paymentChannels.value) {
    if (!isChannelChecked(channel)) continue
    if (!channel.available) {
      issues.push(`「${channel.label}」尚未开放`)
      continue
    }
    if (channel.missing_env.length > 0) {
      issues.push(`「${channel.label}」缺少环境变量 ${channel.missing_env.join('、')}`)
      continue
    }
    const missing = channel.fields
      .filter(
        (field) =>
          field.source === 'setting' &&
          field.required &&
          !(paymentParams.value[field.setting_key] ?? '').trim(),
      )
      .map((field) => field.label)
    if (missing.length > 0) issues.push(`「${channel.label}」还需要填写：${missing.join('、')}`)
  }
  return issues
})

/** 充值配置存在硬错误时禁用保存（避免保存出"能下单却付不了款"的站点） */
const paymentInvalid = computed(
  () => paymentRateInvalid.value || (paymentEnabled.value && paymentChannelIssues.value.length > 0),
)

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

  // 支付通用参数：分 → 元的换算只在这一处发生（反向换算在 handleSave）
  const payment = data.payment
  paymentEnabled.value = payment?.enabled === true
  paymentExchangeRate.value = payment?.exchange_rate ?? 100
  paymentCurrency.value = payment?.currency || 'CNY'
  paymentMinYuan.value = (payment?.min_cents ?? 0) / 100
  paymentMaxYuan.value = (payment?.max_cents ?? 0) / 100
  paymentOrderTTL.value = payment?.order_ttl_minutes ?? 30
  paymentNotifyBase.value = payment?.notify_base || ''

  // 支付通道：把后端下发的字段描述与当前值复制到本地编辑态。
  // 勾选态以 methods 为准（后端用 methods 表达"启用了哪些通道"），
  // 未实现的通道强制不勾选，避免把"即将支持"的通道带进保存请求。
  const channels = data.payment_channels ?? []
  paymentChannels.value = channels
  const enabledMethods = new Set(Array.isArray(payment?.methods) ? payment.methods : [])
  const checks: Record<string, boolean> = {}
  const params: Record<string, string> = {}
  for (const channel of channels) {
    checks[channel.key] = channel.available && enabledMethods.has(channel.key)
    for (const field of channel.fields) {
      if (field.source === 'setting') params[field.setting_key] = field.value ?? ''
    }
  }
  checkedChannels.value = checks
  paymentParams.value = params
}

/** 元 → 分：用四舍五入到整数，避免 0.1+0.2 类浮点误差 */
function yuanToCents(yuan: number): number {
  if (!Number.isFinite(yuan) || yuan < 0) return 0
  return Math.round(yuan * 100)
}

/**
 * 组装提交给后端的通道级参数。
 *
 * 键沿用后端下发的 setting_key（形如 "epay.gateway"），值做 trim；
 * 未勾选的通道其参数也一并保留，避免"临时取消勾选"把已配好的值清掉。
 */
function collectPaymentParams(): Record<string, string> {
  const params: Record<string, string> = {}
  for (const [key, value] of Object.entries(paymentParams.value)) {
    params[key] = (value ?? '').trim()
  }
  return params
}

async function handleSave(): Promise<void> {
  if (emailCodeUnavailable.value) {
    toastError('邮件服务未配置，无法开启邮箱验证码校验')
    return
  }
  if (paymentInvalid.value) {
    toastError(
      paymentRateInvalid.value
        ? '充值兑换比例必须大于 0'
        : paymentChannelIssues.value[0] || '支付通道配置不完整',
    )
    return
  }

  const payload: UpdateSiteSettingsPayload = {
    site_name: siteName.value.trim(),
    site_description: siteDescription.value.trim(),
    registration_enabled: registrationEnabled.value,
    registration_require_email_code: requireEmailCode.value,
    default_user_quota: Number(defaultUserQuota.value),
    default_group: defaultGroup.value.trim(),
    payment: {
      enabled: paymentEnabled.value,
      // 启用通道集合 = 被勾选（且已实现）的通道 key
      methods: paymentChannels.value.filter((channel) => isChannelChecked(channel)).map((channel) => channel.key),
      exchange_rate: Math.round(Number(paymentExchangeRate.value) || 0),
      currency: paymentCurrency.value.trim() || 'CNY',
      min_cents: yuanToCents(Number(paymentMinYuan.value)),
      max_cents: yuanToCents(Number(paymentMaxYuan.value)),
      order_ttl_minutes: Math.round(Number(paymentOrderTTL.value) || 0),
      notify_base: paymentNotifyBase.value.trim(),
      // 通道级参数：键为 setting_key，值是各输入框的当前内容（trim 后）
      params: collectPaymentParams(),
      // 旧版专用字段不再使用，显式置空，避免与新 params 混淆
      epay_gateway: '',
      epay_pid: '',
      epay_types: [],
      stripe_note: '',
    },
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
      <button
        type="button"
        class="btn btn-primary"
        :disabled="saving || loading || emailCodeUnavailable || paymentInvalid"
        @click="handleSave"
      >
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

      <!-- 充值 / 支付 -->
      <section class="card lg:col-span-3">
        <div class="card-head">
          <div>
            <h2 class="section-title flex items-center gap-2">
              <AppIcon name="wallet" :size="16" class="text-brand-700" />
              充值 / 支付
            </h2>
            <p class="mt-0.5 text-xs text-ink-400">
              这些参数保存在数据库、修改后立即生效；<strong>密钥只允许通过环境变量注入</strong>，不会落库。
            </p>
          </div>
          <label class="flex items-center gap-2 text-sm text-ink-200">
            <input v-model="paymentEnabled" class="checkbox" type="checkbox" />
            开放充值
          </label>
        </div>

        <div class="card-pad space-y-5">
          <!-- 兑换比例与限额：决定"付多少钱得到多少额度"，必须最醒目 -->
          <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <div>
              <label class="label" for="payment-rate">兑换比例（1 元 = ? 额度）</label>
              <input id="payment-rate" v-model.number="paymentExchangeRate" class="input input-mono" type="number" min="1" />
              <p class="hint" :class="paymentRateInvalid ? 'text-red-600' : ''">
                例：填 100 表示 1 元兑换 100 额度。
              </p>
            </div>
            <div>
              <label class="label" for="payment-currency">货币代码</label>
              <input id="payment-currency" v-model="paymentCurrency" class="input input-mono" type="text" placeholder="CNY" />
            </div>
            <div>
              <label class="label" for="payment-min">单笔最小金额（元）</label>
              <input id="payment-min" v-model.number="paymentMinYuan" class="input input-mono" type="number" min="0" step="0.01" />
            </div>
            <div>
              <label class="label" for="payment-max">单笔最大金额（元）</label>
              <input id="payment-max" v-model.number="paymentMaxYuan" class="input input-mono" type="number" min="0" step="0.01" />
              <p class="hint">填 0 表示不限上限。</p>
            </div>
          </div>

          <!-- 支付通道清单：字段由后端下发，勾选后才展开该通道的配置项 -->
          <div>
            <p class="label">支付通道</p>
            <p class="hint mb-2">
              勾选某个通道后，才会展开它需要的配置项。密钥类字段只显示注入状态，密钥本身不会下发到页面。
            </p>

            <div class="space-y-2">
              <div
                v-for="channel in paymentChannels"
                :key="channel.key"
                class="rounded-lg border transition-colors"
                :class="isChannelChecked(channel) ? 'border-brand-500/60 bg-brand-500/5' : 'border-ink-800'"
              >
                <label
                  class="flex items-start gap-3 px-3 py-2.5"
                  :class="channel.available ? 'cursor-pointer' : 'cursor-not-allowed opacity-60'"
                >
                  <input
                    class="checkbox mt-0.5"
                    type="checkbox"
                    :checked="isChannelChecked(channel)"
                    :disabled="!channel.available"
                    @change="toggleChannel(channel, ($event.target as HTMLInputElement).checked)"
                  />
                  <span class="min-w-0 flex-1">
                    <span class="flex flex-wrap items-center gap-1.5 text-sm text-ink-100">
                      {{ channel.label }}
                      <span class="badge" :class="channelStatusClass(channel)">
                        {{ channelStatusText(channel) }}
                      </span>
                    </span>
                    <span class="mt-0.5 block text-xs leading-relaxed text-ink-400">
                      {{ channel.description }}
                    </span>

                    <!-- 回调地址：这一步配错是"付了钱不到账"最常见的原因，故直接给出可复制地址 -->
                    <span v-if="notifyURL(channel)" class="mt-1.5 flex flex-wrap items-center gap-1.5">
                      <span class="text-xs text-ink-400">回调地址</span>
                      <code class="chip max-w-full truncate" :title="notifyURL(channel)">{{ notifyURL(channel) }}</code>
                      <CopyButton :value="notifyURL(channel)" small outline success-text="回调地址已复制" />
                    </span>

                    <span v-if="channel.missing_env.length" class="mt-1 block text-xs text-amber-700">
                      启用前请注入环境变量：{{ channel.missing_env.join('、') }}
                    </span>
                  </span>
                </label>

                <!-- 触发式展开：仅当该通道被勾选且适配器已实现时才渲染 -->
                <div
                  v-if="isChannelChecked(channel) && channel.available"
                  class="border-t border-ink-800/60 px-3 py-3"
                >
                  <div class="grid gap-4 sm:grid-cols-2">
                    <div v-for="field in channel.fields" :key="field.setting_key || field.key">
                      <!-- 密钥字段：只读状态行，绝不渲染输入框（值不出服务端） -->
                      <template v-if="field.source === 'secret'">
                        <p class="label">
                          {{ field.label }}
                          <span v-if="field.required" class="text-red-600">*</span>
                        </p>
                        <p
                          class="flex items-center gap-1.5 rounded-lg border px-3 py-2 text-xs"
                          :class="
                            field.ready
                              ? 'border-emerald-500/25 bg-emerald-500/10 text-emerald-700'
                              : 'border-amber-500/25 bg-amber-500/10 text-amber-700'
                          "
                        >
                          <AppIcon :name="field.ready ? 'check' : 'alert'" :size="14" />
                          <span v-if="field.ready">已就绪</span>
                          <span v-else>
                            未注入，请设置环境变量 <code class="chip">{{ field.env_var }}</code>
                          </span>
                        </p>
                        <p v-if="field.help" class="hint">{{ field.help }}</p>
                      </template>

                      <!-- 可编辑字段：按 kind 渲染（text/number/list/select/switch） -->
                      <template v-else>
                        <label class="label" :for="`pf-${channel.key}-${field.key}`">
                          {{ field.label }}
                          <span v-if="field.required" class="text-red-600">*</span>
                        </label>

                        <select
                          v-if="field.kind === 'select'"
                          :id="`pf-${channel.key}-${field.key}`"
                          v-model="paymentParams[field.setting_key]"
                          class="input"
                        >
                          <option v-for="option in field.options || []" :key="option.value" :value="option.value">
                            {{ option.label }}
                          </option>
                        </select>

                        <label
                          v-else-if="field.kind === 'switch'"
                          class="flex items-center gap-2 text-sm text-ink-200"
                        >
                          <input
                            class="checkbox"
                            type="checkbox"
                            :checked="isSwitchOn(field.setting_key)"
                            @change="setSwitch(field.setting_key, ($event.target as HTMLInputElement).checked)"
                          />
                          {{ isSwitchOn(field.setting_key) ? '已开启' : '已关闭' }}
                        </label>

                        <input
                          v-else
                          :id="`pf-${channel.key}-${field.key}`"
                          v-model="paymentParams[field.setting_key]"
                          class="input input-mono"
                          :type="field.kind === 'number' ? 'number' : 'text'"
                          :placeholder="field.placeholder || undefined"
                        />
                        <p v-if="field.help" class="hint">{{ field.help }}</p>
                      </template>
                    </div>
                  </div>
                </div>
              </div>
            </div>
          </div>

          <!-- 订单与回调 -->
          <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <div>
              <label class="label" for="payment-ttl">订单有效期（分钟）</label>
              <input id="payment-ttl" v-model.number="paymentOrderTTL" class="input input-mono" type="number" min="1" />
              <p class="hint">超时未支付的订单会被自动关闭。</p>
            </div>
            <div class="sm:col-span-1 lg:col-span-3">
              <label class="label" for="payment-notify">回调基址（公网地址）</label>
              <input
                id="payment-notify"
                v-model="paymentNotifyBase"
                class="input input-mono"
                type="url"
                placeholder="https://api.example.com"
              />
              <p class="hint">
                支付平台必须能回调到本网关。留空时按浏览器访问地址推导；
                若本站经过反向代理或使用内网地址，请显式填写公网地址。
                各通道的完整回调地址见上方通道清单。
              </p>
            </div>
          </div>

          <p v-if="paymentInvalid" class="field-error">
            {{
              paymentRateInvalid
                ? '充值兑换比例必须大于 0，否则用户付钱后不会到账。'
                : paymentChannelIssues.join('；')
            }}
          </p>
        </div>
      </section>
    </div>
  </div>
</template>
