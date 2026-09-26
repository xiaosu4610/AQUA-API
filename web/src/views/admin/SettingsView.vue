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

/* 充值 / 支付表单字段。
   金额与汇率一律用"分"或整数承载：金额用分、汇率用"1 元可兑换的额度"，
   避免浮点在各处来回换算（这是支付类系统最经典的资损来源）。 */
const paymentEnabled = ref(false)
const paymentMethods = ref<string[]>([])
const paymentExchangeRate = ref(100)
const paymentCurrency = ref('CNY')
const paymentMinYuan = ref(1)
const paymentMaxYuan = ref(0)
const paymentOrderTTL = ref(30)
const paymentNotifyBase = ref('')
const paymentEPayGateway = ref('')
const paymentEPayPID = ref('')
const paymentEPayTypes = ref('')
const paymentStripeNote = ref('')

/** 支持的支付通道（与后端 model.PaymentMethod* 一一对应） */
const PAYMENT_METHOD_OPTIONS = [
  { name: 'epay', label: '在线支付（易支付协议）', desc: '跳转第三方收银台，回调自动入账' },
  { name: 'stripe', label: 'Stripe', desc: '海外部署常用，需配置 Webhook 密钥' },
  { name: 'manual', label: '人工确认', desc: '不依赖第三方，由管理员在订单页确认入账' },
]

/** 邮件通道是否就绪（只读，由服务端环境变量决定） */
const emailReady = computed(() => settings.value?.email_service_ready === true)
const emailFrom = computed(() => settings.value?.email_from || '')

/** 各通道密钥是否就绪（只读） */
const secretStatus = computed(() => settings.value?.payment_secrets ?? {})

/** 兑换比例是否有效：启用充值但比例为 0 会让用户"付钱却不到账" */
const paymentRateInvalid = computed(() => paymentEnabled.value && Number(paymentExchangeRate.value) <= 0)

/** 启用易支付但没填网关/商户号 */
const epayIncomplete = computed(
  () =>
    paymentEnabled.value &&
    paymentMethods.value.includes('epay') &&
    (!paymentEPayGateway.value.trim() || !paymentEPayPID.value.trim()),
)

/** 充值配置存在硬错误时禁用保存（避免保存出"能下单却付不了款"的站点） */
const paymentInvalid = computed(() => paymentRateInvalid.value || epayIncomplete.value)

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

  // 支付参数：分 → 元的换算只在这一处发生（反向换算在 handleSave）
  const payment = data.payment
  paymentEnabled.value = payment?.enabled === true
  paymentMethods.value = Array.isArray(payment?.methods) ? [...payment.methods] : []
  paymentExchangeRate.value = payment?.exchange_rate ?? 100
  paymentCurrency.value = payment?.currency || 'CNY'
  paymentMinYuan.value = (payment?.min_cents ?? 0) / 100
  paymentMaxYuan.value = (payment?.max_cents ?? 0) / 100
  paymentOrderTTL.value = payment?.order_ttl_minutes ?? 30
  paymentNotifyBase.value = payment?.notify_base || ''
  paymentEPayGateway.value = payment?.epay_gateway || ''
  paymentEPayPID.value = payment?.epay_pid || ''
  paymentEPayTypes.value = Array.isArray(payment?.epay_types) ? payment.epay_types.join(',') : ''
  paymentStripeNote.value = payment?.stripe_note || ''
}

/** 元 → 分：用四舍五入到整数，避免 0.1+0.2 类浮点误差 */
function yuanToCents(yuan: number): number {
  if (!Number.isFinite(yuan) || yuan < 0) return 0
  return Math.round(yuan * 100)
}

async function handleSave(): Promise<void> {
  if (emailCodeUnavailable.value) {
    toastError('邮件服务未配置，无法开启邮箱验证码校验')
    return
  }
  if (paymentInvalid.value) {
    toastError(paymentRateInvalid.value ? '充值兑换比例必须大于 0' : '启用易支付需填写网关地址与商户号')
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
      methods: paymentMethods.value,
      exchange_rate: Math.round(Number(paymentExchangeRate.value) || 0),
      currency: paymentCurrency.value.trim() || 'CNY',
      min_cents: yuanToCents(Number(paymentMinYuan.value)),
      max_cents: yuanToCents(Number(paymentMaxYuan.value)),
      order_ttl_minutes: Math.round(Number(paymentOrderTTL.value) || 0),
      notify_base: paymentNotifyBase.value.trim(),
      epay_gateway: paymentEPayGateway.value.trim(),
      epay_pid: paymentEPayPID.value.trim(),
      epay_types: paymentEPayTypes.value
        .split(/[,，\s]+/)
        .map((item) => item.trim())
        .filter(Boolean),
      stripe_note: paymentStripeNote.value.trim(),
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

          <!-- 支付通道 -->
          <div>
            <p class="label">启用的支付通道</p>
            <div class="grid gap-2 sm:grid-cols-3">
              <label
                v-for="option in PAYMENT_METHOD_OPTIONS"
                :key="option.name"
                class="flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2.5 transition-colors"
                :class="
                  paymentMethods.includes(option.name)
                    ? 'border-brand-500/60 bg-brand-500/5'
                    : 'border-ink-800 hover:border-ink-700'
                "
              >
                <input v-model="paymentMethods" class="checkbox mt-0.5" type="checkbox" :value="option.name" />
                <span class="min-w-0">
                  <span class="flex flex-wrap items-center gap-1.5 text-sm text-ink-100">
                    {{ option.label }}
                    <span
                      class="badge"
                      :class="secretStatus[option.name] ? 'badge-ok' : 'badge-warn'"
                      :title="secretStatus[option.name] ? '密钥已通过环境变量就绪' : '密钥未配置（环境变量）'"
                    >
                      {{ secretStatus[option.name] ? '密钥就绪' : '缺密钥' }}
                    </span>
                  </span>
                  <span class="mt-0.5 block text-xs leading-relaxed text-ink-400">{{ option.desc }}</span>
                </span>
              </label>
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
              </p>
            </div>
          </div>

          <!-- 易支付参数 -->
          <div class="rounded-lg border border-ink-800 p-4">
            <p class="section-title">易支付参数</p>
            <div class="mt-3 grid gap-4 sm:grid-cols-3">
              <div>
                <label class="label" for="payment-epay-gateway">网关地址</label>
                <input
                  id="payment-epay-gateway"
                  v-model="paymentEPayGateway"
                  class="input input-mono"
                  type="url"
                  placeholder="https://pay.example.com"
                />
              </div>
              <div>
                <label class="label" for="payment-epay-pid">商户号（PID）</label>
                <input id="payment-epay-pid" v-model="paymentEPayPID" class="input input-mono" type="text" />
              </div>
              <div>
                <label class="label" for="payment-epay-types">可用支付方式</label>
                <input
                  id="payment-epay-types"
                  v-model="paymentEPayTypes"
                  class="input input-mono"
                  type="text"
                  placeholder="alipay,wxpay"
                />
                <p class="hint">逗号分隔，取值以你的支付服务商文档为准。</p>
              </div>
            </div>
            <p class="hint mt-3">
              商户密钥（Key）只能通过环境变量
              <code class="chip">AQUA_EPAY_KEY</code> 注入，不在此处填写，也不会写入数据库。
            </p>
          </div>

          <!-- Stripe 参数 -->
          <div class="rounded-lg border border-ink-800 p-4">
            <p class="section-title">Stripe 参数</p>
            <div class="mt-3">
              <label class="label" for="payment-stripe-note">结账页商品名</label>
              <input
                id="payment-stripe-note"
                v-model="paymentStripeNote"
                class="input"
                type="text"
                placeholder="账户充值"
              />
            </div>
            <p class="hint mt-3">
              需要环境变量 <code class="chip">AQUA_STRIPE_SECRET_KEY</code> 与
              <code class="chip">AQUA_STRIPE_WEBHOOK_SECRET</code>；
              并把 Webhook 地址配置为本站的
              <code class="chip">/api/payments/stripe/notify</code>。
            </p>
          </div>

          <p v-if="paymentInvalid" class="field-error">
            {{ paymentRateInvalid ? '充值兑换比例必须大于 0，否则用户付钱后不会到账。' : '启用易支付需要填写网关地址与商户号（PID）。' }}
          </p>
        </div>
      </section>
    </div>
  </div>
</template>
