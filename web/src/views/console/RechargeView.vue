<script setup lang="ts">
/**
 * 用户门户 · 账户充值：下单、支付与充值记录。
 *
 * 意图（Why）：
 *   充值链路的体验关键在于"钱付出去了，额度到了没有"。因此本页做三件事：
 *     1) 下单前把口径讲清楚：1 元 = 多少额度（由后端配置决定），避免到账后才发现不符预期；
 *     2) 支付后回到本页自动轮询订单状态，直到入账或关闭；
 *     3) 保留完整充值记录（含"已入账"标记），让用户能自查。
 *
 * 为什么额度由服务端算：前端只提交"金额"，额度一律由后端按当前汇率计算，
 *   否则用户改一个数字就能给自己加天文数字的额度（这是最直接的资损漏洞）。
 *
 * 流转（Flow）：
 *   onMounted → fetchPaymentInfo() + listMyOrders()
 *   下单 → createOrder() → 若有 pay_url 则新窗口打开收银台 → 轮询 getMyOrder()
 *   从收银台返回（?trade_no=xxx）→ 直接轮询该订单
 *
 * 扩展（Extend）：
 *   新增支付方式时无需改动本页（通道列表由后端返回）；
 *   需要"自定义金额输入键盘"等交互优化时，改 amount 相关一段即可。
 */
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import { createOrder, getMyOrder, listMyOrders } from '@/api/portal'
import { fetchPaymentInfo } from '@/api/site'
import type { PaymentOrder, PublicPaymentInfo } from '@/api/types'
import { ORDER_STATUS_PAID, ORDER_STATUS_PENDING } from '@/api/types'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatDateTime, formatNumber } from '@/utils/format'

const route = useRoute()

const info = ref<PublicPaymentInfo | null>(null)
const infoLoading = ref(true)
const infoError = ref('')

/** 表单：金额用"元"字符串承载，提交前换算成"分" */
const amountYuan = ref('10')
const method = ref('')
const subMethod = ref('')
const submitting = ref(false)
const formError = ref('')

/** 当前等待支付的订单（用于展示"等待支付"引导与轮询） */
const pendingOrder = ref<PaymentOrder | null>(null)

const orders = ref<PaymentOrder[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(10)
const ordersLoading = ref(true)
const ordersError = ref('')

let pollTimer = 0

/** 兑换比例文案：让用户在付款前就明确"到账多少" */
const exchangeHint = computed(() => {
  const rate = info.value?.exchange_rate ?? 0
  if (rate <= 0) return '充值比例未配置，请联系管理员'
  return `1 ${info.value?.currency || 'CNY'} = ${formatNumber(rate)} 额度`
})

/** 预计到账额度：与后端口径一致（元 × 汇率，向下取整） */
const estimatedQuota = computed(() => {
  const rate = info.value?.exchange_rate ?? 0
  const cents = yuanToCents(amountYuan.value)
  if (cents <= 0 || rate <= 0) return 0
  return Math.floor((cents * rate) / 100)
})

const minYuan = computed(() => ((info.value?.min_cents ?? 0) / 100).toFixed(2))
const maxYuan = computed(() => {
  const max = info.value?.max_cents ?? 0
  return max > 0 ? (max / 100).toFixed(2) : ''
})

/** 常用金额预设（元） */
const presets = [10, 30, 50, 100, 200]

/**
 * 把"元"字符串解析为"分"。
 *
 * 手工解析而不走 parseFloat：浮点会把 10.01 解析成 10.009999…，
 * 转成分时可能出现 1000 或 1001 的不确定结果，属于资损隐患。
 */
function yuanToCents(raw: string): number {
  const text = raw.trim()
  if (!text) return 0
  const [intPart, fracPart = ''] = text.split('.')
  const whole = Number.parseInt(intPart || '0', 10)
  if (!Number.isFinite(whole) || whole < 0) return 0
  const frac = Number.parseInt((fracPart + '00').slice(0, 2), 10)
  if (!Number.isFinite(frac)) return 0
  return whole * 100 + frac
}

async function loadInfo(): Promise<void> {
  infoLoading.value = true
  infoError.value = ''
  try {
    info.value = await fetchPaymentInfo()
    if (!method.value && info.value.methods.length) {
      // 默认选中第一个"密钥已就绪"的通道，避免用户选到还没配好的通道后报错
      method.value = (info.value.methods.find((item) => item.ready) || info.value.methods[0]).name
    }
  } catch (err) {
    infoError.value = err instanceof ApiError ? err.message : '充值信息加载失败'
  } finally {
    infoLoading.value = false
  }
}

async function loadOrders(): Promise<void> {
  ordersLoading.value = true
  ordersError.value = ''
  try {
    const result = await listMyOrders({ page: page.value, size: size.value })
    orders.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    orders.value = []
    total.value = 0
    ordersError.value = err instanceof ApiError ? err.message : '充值记录加载失败'
  } finally {
    ordersLoading.value = false
  }
}

/** 轮询待支付订单：3 秒一次，最多持续 5 分钟（避免页面长期占用请求） */
function startPolling(tradeNo: string): void {
  stopPolling()
  const startedAt = Date.now()
  pollTimer = window.setInterval(async () => {
    if (Date.now() - startedAt > 5 * 60 * 1000) {
      stopPolling()
      return
    }
    try {
      const order = await getMyOrder(tradeNo)
      pendingOrder.value = order
      if (order.status !== ORDER_STATUS_PENDING) {
        stopPolling()
        if (order.status === ORDER_STATUS_PAID) {
          toastSuccess(`充值成功，已到账 ${formatNumber(order.quota)} 额度`)
        }
        await loadOrders()
      }
    } catch {
      // 单次轮询失败不终止：网络抖动很常见，下一轮会继续
    }
  }, 3000)
}

function stopPolling(): void {
  if (pollTimer) {
    window.clearInterval(pollTimer)
    pollTimer = 0
  }
}

async function submit(): Promise<void> {
  const cents = yuanToCents(amountYuan.value)
  if (cents <= 0) {
    formError.value = '请输入正确的充值金额'
    return
  }
  if (!method.value) {
    formError.value = '请选择支付方式'
    return
  }
  const minCents = info.value?.min_cents ?? 0
  if (cents < minCents) {
    formError.value = `单笔充值不能少于 ${minYuan.value} 元`
    return
  }
  const maxCents = info.value?.max_cents ?? 0
  if (maxCents > 0 && cents > maxCents) {
    formError.value = `单笔充值不能超过 ${maxYuan.value} 元`
    return
  }

  submitting.value = true
  formError.value = ''
  try {
    const order = await createOrder({
      amount_cents: cents,
      method: method.value,
      sub_method: subMethod.value || undefined,
    })
    pendingOrder.value = order
    await loadOrders()

    if (order.pay_url) {
      // 新窗口打开收银台：保留本页以便回来后继续轮询状态
      window.open(order.pay_url, '_blank', 'noopener,noreferrer')
      startPolling(order.trade_no)
    } else {
      // 人工确认通道：没有收银台，提示用户按站长公布的方式付款
      toastSuccess('充值申请已提交，请联系管理员确认收款后入账')
    }
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '下单失败'
  } finally {
    submitting.value = false
  }
}

function changePage(next: number): void {
  page.value = next
  void loadOrders()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void loadOrders()
}

/** 订单状态徽标：待支付用警示色（需要用户行动），已入账用成功色 */
function orderBadgeClass(order: PaymentOrder): string {
  switch (order.status) {
    case ORDER_STATUS_PAID:
      return 'badge badge-ok'
    case ORDER_STATUS_PENDING:
      return 'badge badge-warn'
    default:
      return 'badge badge-off'
  }
}

/** 从收银台返回时（?trade_no=），自动查询该订单并开始轮询 */
watch(
  () => route.query.trade_no,
  async (tradeNo) => {
    if (typeof tradeNo !== 'string' || !tradeNo) return
    try {
      const order = await getMyOrder(tradeNo)
      pendingOrder.value = order
      if (order.status === ORDER_STATUS_PENDING) startPolling(order.trade_no)
    } catch {
      // 订单号可能属于其它账号或已过期：静默忽略，不打扰用户
    }
  },
)

onMounted(async () => {
  await Promise.all([loadInfo(), loadOrders()])
  const tradeNo = route.query.trade_no
  if (typeof tradeNo === 'string' && tradeNo) {
    try {
      const order = await getMyOrder(tradeNo)
      pendingOrder.value = order
      if (order.status === ORDER_STATUS_PENDING) startPolling(order.trade_no)
    } catch {
      /* 忽略：见 watch 的说明 */
    }
  }
})

onBeforeUnmount(stopPolling)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">账户充值</h2>
        <p class="page-desc">
          充值后额度立即到账，可用于全部已定价模型。{{ exchangeHint }}
        </p>
      </div>
      <RouterLink to="/console" class="btn btn-secondary btn-sm">
        <AppIcon name="home" :size="14" />
        返回概览
      </RouterLink>
    </div>

    <DataState
      :loading="infoLoading"
      :error="infoError"
      :empty="!infoLoading && !infoError && !info?.enabled"
      compact
      loading-text="正在读取充值配置…"
      empty-text="本站未开放充值"
      empty-hint="请联系管理员为账号分配额度；或由管理员在后台「系统设置 → 充值」中开启在线充值。"
      @retry="loadInfo"
    />

    <div v-if="!infoLoading && !infoError && info?.enabled" class="grid gap-5 lg:grid-cols-[1.15fr_1fr]">
      <!-- ── 下单区 ───────────────────────────────────── -->
      <section class="card card-pad">
        <h3 class="section-title flex items-center gap-2">
          <AppIcon name="wallet" :size="16" class="text-brand-700" />
          选择充值金额
        </h3>

        <div class="mt-4 flex flex-wrap gap-2">
          <button
            v-for="preset in presets"
            :key="preset"
            type="button"
            class="btn btn-sm"
            :class="Number(amountYuan) === preset ? 'btn-primary' : 'btn-secondary'"
            @click="amountYuan = String(preset)"
          >
            {{ preset }} 元
          </button>
        </div>

        <div class="mt-4">
          <label class="label" for="recharge-amount">自定义金额（{{ info.currency }}）</label>
          <div class="relative">
            <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-ink-500">¥</span>
            <input
              id="recharge-amount"
              v-model="amountYuan"
              class="input pl-7 text-lg font-semibold"
              type="text"
              inputmode="decimal"
              placeholder="10.00"
            />
          </div>
          <p class="hint">
            单笔限额：{{ minYuan }} 元{{ maxYuan ? ` ~ ${maxYuan} 元` : ' 起，不限上限' }}；
            预计到账 <strong class="text-ink-100">{{ formatNumber(estimatedQuota) }}</strong> 额度。
          </p>
        </div>

        <div class="mt-4">
          <p class="label">支付方式</p>
          <div class="space-y-2">
            <label
              v-for="item in info.methods"
              :key="item.name"
              class="flex cursor-pointer items-center gap-3 rounded-lg border px-3 py-2.5 transition-colors"
              :class="method === item.name ? 'border-brand-500/60 bg-brand-500/5' : 'border-ink-800 hover:border-ink-700'"
            >
              <input v-model="method" class="checkbox" type="radio" :value="item.name" :disabled="!item.ready" />
              <span class="min-w-0 flex-1">
                <span class="block text-sm text-ink-100">{{ item.label }}</span>
                <span class="block text-[11px] text-ink-500">
                  {{ item.name === 'manual' ? '向站长付款后由管理员确认入账' : '跳转到第三方收银台完成支付' }}
                </span>
              </span>
              <span v-if="!item.ready" class="badge badge-warn">未配置密钥</span>
            </label>
          </div>
        </div>

        <p v-if="formError" class="field-error mt-3">{{ formError }}</p>

        <button type="button" class="btn btn-primary mt-4 w-full" :disabled="submitting" @click="submit">
          <AppIcon name="cart" :size="16" />
          {{ submitting ? '正在下单…' : `支付 ${amountYuan || '0'} ${info.currency}` }}
        </button>
      </section>

      <!-- ── 当前订单 / 说明 ───────────────────────────── -->
      <section class="space-y-5">
        <div v-if="pendingOrder" class="card card-pad">
          <h3 class="section-title flex items-center gap-2">
            <AppIcon name="clock" :size="16" class="text-brand-700" />
            当前订单
          </h3>
          <div class="mt-3 space-y-2 text-sm">
            <div class="flex items-center justify-between gap-3">
              <span class="text-ink-400">订单号</span>
              <code class="chip">{{ pendingOrder.trade_no }}</code>
            </div>
            <div class="flex items-center justify-between gap-3">
              <span class="text-ink-400">金额 / 额度</span>
              <span class="font-mono text-ink-100">
                ¥{{ pendingOrder.amount_text }} → {{ formatNumber(pendingOrder.quota) }}
              </span>
            </div>
            <div class="flex items-center justify-between gap-3">
              <span class="text-ink-400">状态</span>
              <span :class="orderBadgeClass(pendingOrder)">{{ pendingOrder.status_text }}</span>
            </div>
            <div v-if="pendingOrder.expires_at" class="flex items-center justify-between gap-3">
              <span class="text-ink-400">支付截止</span>
              <span class="text-ink-200">{{ formatDateTime(pendingOrder.expires_at) }}</span>
            </div>
          </div>

          <div v-if="pendingOrder.status === ORDER_STATUS_PENDING" class="mt-4 rounded-lg border border-amber-500/25 bg-amber-500/10 px-3 py-2.5">
            <p class="flex items-start gap-2 text-xs leading-relaxed text-amber-800">
              <AppIcon name="info" :size="14" class="mt-0.5" />
              <span>
                正在等待支付结果，本页会自动刷新。若收银台未自动打开，
                请点击下方按钮前往支付；支付完成后无需手动操作，额度会自动到账。
              </span>
            </p>
            <div class="mt-2 flex flex-wrap gap-2">
              <a
                v-if="pendingOrder.pay_url"
                class="btn btn-primary btn-sm"
                :href="pendingOrder.pay_url"
                target="_blank"
                rel="noopener noreferrer"
              >
                <AppIcon name="external" :size="14" />
                前往支付
              </a>
              <RouterLink to="/console/recharge" class="btn btn-secondary btn-sm">取消等待</RouterLink>
            </div>
          </div>
        </div>

        <div class="card card-pad">
          <h3 class="section-title flex items-center gap-2">
            <AppIcon name="info" :size="16" class="text-brand-700" />
            充值说明
          </h3>
          <ul class="mt-3 space-y-1.5 text-xs leading-relaxed text-ink-400">
            <li>· 额度到账后立即可用，可直接用于所有已定价的模型调用。</li>
            <li>· 支付超时的订单会被自动关闭，关闭后不再受理，重新下单即可。</li>
            <li>· 已支付订单如需退款，请联系管理员在后台处理（会扣回已入账额度）。</li>
            <li>· 本页展示的金额与额度均由服务端计算，请以到账记录为准。</li>
          </ul>
        </div>
      </section>
    </div>

    <!-- ── 充值记录 ───────────────────────────────────── -->
    <section class="mt-6">
      <h3 class="section-title mb-3">充值记录</h3>

      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>订单号</th>
              <th>金额</th>
              <th class="text-right">到账额度</th>
              <th>方式</th>
              <th>状态</th>
              <th>创建时间</th>
              <th>支付时间</th>
            </tr>
          </thead>
          <tbody>
            <DataState
              :loading="ordersLoading"
              :error="ordersError"
              :empty="!ordersLoading && !ordersError && orders.length === 0"
              :colspan="7"
              loading-text="正在读取充值记录…"
              empty-text="还没有充值记录"
              empty-hint="完成一笔充值后，这里会显示订单状态与到账情况。"
              @retry="loadOrders"
            />

            <tr v-for="order in orders" :key="order.trade_no">
              <td><code class="font-mono text-[12px] text-ink-200">{{ order.trade_no }}</code></td>
              <td class="cell-num">¥{{ order.amount_text }}</td>
              <td class="cell-num">{{ formatNumber(order.quota) }}</td>
              <td class="cell-muted">{{ order.method }}<span v-if="order.sub_method"> / {{ order.sub_method }}</span></td>
              <td>
                <span :class="orderBadgeClass(order)">{{ order.status_text }}</span>
                <span v-if="order.status === ORDER_STATUS_PAID && !order.credited" class="ml-1 badge badge-warn">
                  待入账
                </span>
              </td>
              <td class="cell-muted">{{ formatDateTime(order.created_at) }}</td>
              <td class="cell-muted">{{ order.paid_at ? formatDateTime(order.paid_at) : '—' }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="total > 0" class="mt-3 card">
        <Pagination
          :page="page"
          :size="size"
          :total="total"
          :disabled="ordersLoading"
          :size-options="[10, 20, 50]"
          @update:page="changePage"
          @update:size="changeSize"
        />
      </div>
    </section>
  </div>
</template>
