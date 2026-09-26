<script setup lang="ts">
/**
 * 管理后台 · 充值订单：人工确认入账、关单与退款。
 *
 * 意图（Why）：
 *   这是"钱与额度"的对账页面，三类操作的语义必须让管理员一眼看懂：
 *     - 确认入账：人工通道的核心动作，也用于"用户已付款但回调丢失"的补救。
 *       后端是幂等的，重复点击不会重复加额度。
 *     - 关闭订单：仅对「待支付」有效，用于用户下错单但收银台未过期的情况。
 *     - 退款：仅对「已支付」有效，会把已入账额度扣回（订单置为已退款）。
 *
 * 为什么把"是否已入账"单独显示：支付成功与额度到账是两件事。
 *   正常情况下两者同时完成；若出现"已支付但未入账"，说明需要人工补入账
 *   （后端启动时会自动补偿，此列用于确认补偿结果）。
 *
 * 流转（Flow）：
 *   进入页面 → listAllOrders({page,size,status}) → 表格
 *   操作 → markOrderPaid / closeOrder / refundOrder → 重新加载
 *
 * 扩展（Extend）：
 *   新增订单状态时：后端 model.PaymentStatus 加常量 → 本页筛选下拉与徽标各补一处。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import { closeOrder, listAllOrders, markOrderPaid, refundOrder } from '@/api/admin'
import type { PaymentOrder } from '@/api/types'
import {
  ORDER_STATUS_CLOSED,
  ORDER_STATUS_PAID,
  ORDER_STATUS_PENDING,
  ORDER_STATUS_REFUNDED,
} from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatDateTime, formatNumber } from '@/utils/format'

const orders = ref<PaymentOrder[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')
const acting = ref('')

const statusFilter = ref<'' | number>('')
const methodFilter = ref('')

const hasFilter = computed(() => statusFilter.value !== '' || methodFilter.value !== '')

/** 待处理订单数：管理员最需要立刻看到的信息 */
const pendingCount = computed(() => orders.value.filter((order) => order.status === ORDER_STATUS_PENDING).length)

/** 需要人工补入账的订单（已支付但未入账） */
const uncreditedCount = computed(
  () => orders.value.filter((order) => order.status === ORDER_STATUS_PAID && !order.credited).length,
)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listAllOrders({
      page: page.value,
      size: size.value,
      status: statusFilter.value === '' ? undefined : Number(statusFilter.value),
      method: methodFilter.value || undefined,
    })
    orders.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    orders.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '订单加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(load)

function applyFilter(): void {
  page.value = 1
  void load()
}

function resetFilter(): void {
  statusFilter.value = ''
  methodFilter.value = ''
  applyFilter()
}

function changePage(next: number): void {
  page.value = next
  void load()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void load()
}

async function run(tradeNo: string, action: () => Promise<unknown>, successText: string): Promise<void> {
  acting.value = tradeNo
  try {
    await action()
    toastSuccess(successText)
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    acting.value = ''
  }
}

async function handleMarkPaid(order: PaymentOrder): Promise<void> {
  const alreadyPaid = order.status === ORDER_STATUS_PAID
  const ok = await confirmDialog({
    title: alreadyPaid ? '补入账' : '确认入账',
    message: alreadyPaid
      ? `订单 ${order.trade_no} 已标记为已支付，但额度尚未入账。将为其归属用户补上 ${formatNumber(order.quota)} 额度（幂等，重复执行不会重复加）。`
      : `确认已收到用户付款？将为该用户增加 ${formatNumber(order.quota)} 额度。该操作会写入订单记录，重复点击不会重复入账。`,
    confirmText: '确认入账',
  })
  if (!ok) return
  await run(order.trade_no, () => markOrderPaid(order.trade_no), '已入账')
}

async function handleClose(order: PaymentOrder): Promise<void> {
  const ok = await confirmDialog({
    title: '关闭订单',
    message: `订单 ${order.trade_no} 将被关闭，用户无法再完成支付。若用户仍在等待支付，请勿关闭。`,
    confirmText: '关闭订单',
    danger: true,
  })
  if (!ok) return
  await run(order.trade_no, () => closeOrder(order.trade_no), '订单已关闭')
}

async function handleRefund(order: PaymentOrder): Promise<void> {
  const ok = await confirmDialog({
    title: '退款并扣回额度',
    message: `将扣回该订单已入账的 ${formatNumber(order.quota)} 额度，并把订单标记为已退款。请先确认已完成实际退款（本操作不涉及第三方支付平台的退款流程）。`,
    confirmText: '确认退款',
    danger: true,
  })
  if (!ok) return
  await run(order.trade_no, () => refundOrder(order.trade_no), '已退款并扣回额度')
}

function orderBadgeClass(order: PaymentOrder): string {
  switch (order.status) {
    case ORDER_STATUS_PAID:
      return 'badge badge-ok'
    case ORDER_STATUS_PENDING:
      return 'badge badge-warn'
    case ORDER_STATUS_REFUNDED:
      return 'badge badge-info'
    default:
      return 'badge badge-off'
  }
}

const emptyText = computed(() => (hasFilter.value ? '没有符合筛选条件的订单' : '暂无充值订单'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '试试放宽筛选条件，或点击「重置」查看全部订单。'
    : '用户在「账户充值」页下单后，这里会出现订单。人工确认通道的订单需要你在此确认入账。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">充值订单</h2>
        <p class="page-desc">
          在线支付由第三方回调自动入账；人工确认通道的订单需要你在此
          <strong>确认入账</strong>。入账是幂等的，重复操作不会重复加额度。
        </p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
        <AppIcon name="refresh" :size="14" />
        刷新
      </button>
    </div>

    <!-- 需要立刻关注的统计：只统计当前页可见的订单，作为快速提示 -->
    <div class="mb-4 grid gap-3 sm:grid-cols-3">
      <div class="card card-pad">
        <p class="text-xs text-ink-400">当前页待支付</p>
        <p class="mt-1 text-2xl font-semibold text-ink-50">{{ pendingCount }}</p>
      </div>
      <div class="card card-pad">
        <p class="text-xs text-ink-400">已支付但未入账</p>
        <p class="mt-1 text-2xl font-semibold" :class="uncreditedCount > 0 ? 'text-amber-700' : 'text-ink-50'">
          {{ uncreditedCount }}
        </p>
        <p class="mt-1 text-[11px] text-ink-500">非 0 时请点击「确认入账」补上额度</p>
      </div>
      <div class="card card-pad">
        <p class="text-xs text-ink-400">订单总数</p>
        <p class="mt-1 text-2xl font-semibold text-ink-50">{{ total }}</p>
      </div>
    </div>

    <div class="filter-bar mb-4">
      <div>
        <label class="label" for="order-status">状态</label>
        <select id="order-status" v-model="statusFilter" class="input min-w-[9rem]">
          <option value="">全部状态</option>
          <option :value="ORDER_STATUS_PENDING">待支付</option>
          <option :value="ORDER_STATUS_PAID">已支付</option>
          <option :value="ORDER_STATUS_CLOSED">已关闭</option>
          <option :value="ORDER_STATUS_REFUNDED">已退款</option>
        </select>
      </div>

      <div>
        <label class="label" for="order-method">支付方式</label>
        <select id="order-method" v-model="methodFilter" class="input min-w-[9rem]">
          <option value="">全部方式</option>
          <option value="epay">在线支付（易支付）</option>
          <option value="stripe">Stripe</option>
          <option value="manual">人工确认</option>
        </select>
      </div>

      <div class="flex items-center gap-2">
        <button type="button" class="btn btn-primary btn-sm" :disabled="loading" @click="applyFilter">
          <AppIcon name="filter" :size="14" />
          应用筛选
        </button>
        <button v-if="hasFilter" type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="resetFilter">
          重置
        </button>
      </div>
    </div>

    <div class="table-wrap table-cards">
      <table class="data-table">
        <thead>
          <tr>
            <th>订单号</th>
            <th>用户</th>
            <th class="text-right">金额</th>
            <th class="text-right">额度</th>
            <th>方式</th>
            <th>状态</th>
            <th>创建时间</th>
            <th>支付时间</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>
        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="!loading && !error && orders.length === 0"
            :colspan="9"
            loading-text="正在读取订单…"
            :empty-text="emptyText"
            :empty-hint="emptyHint"
            @retry="load"
          />

          <tr v-for="order in orders" :key="order.trade_no">
            <td data-label="订单号"><code class="font-mono text-[12px] text-ink-200">{{ order.trade_no }}</code></td>
            <td class="cell-muted" data-label="用户">#{{ order.user_id }}</td>
            <td class="cell-num" data-label="金额">¥{{ order.amount_text }}</td>
            <td class="cell-num" data-label="额度">{{ formatNumber(order.quota) }}</td>
            <td class="cell-muted" data-label="方式">
              {{ order.method }}<span v-if="order.sub_method"> / {{ order.sub_method }}</span>
            </td>
            <td data-label="状态">
              <span :class="orderBadgeClass(order)">{{ order.status_text }}</span>
              <span
                v-if="order.status === ORDER_STATUS_PAID && !order.credited"
                class="ml-1 badge badge-warn"
                title="已支付但额度未入账，请点击「确认入账」"
              >
                待入账
              </span>
            </td>
            <td class="cell-muted" data-label="创建时间">{{ formatDateTime(order.created_at) }}</td>
            <td class="cell-muted" data-label="支付时间">{{ order.paid_at ? formatDateTime(order.paid_at) : '—' }}</td>
            <td class="cell-actions" data-label="操作">
              <div class="flex items-center justify-end gap-1">
                <button
                  v-if="order.status === ORDER_STATUS_PENDING || (order.status === ORDER_STATUS_PAID && !order.credited)"
                  type="button"
                  class="btn-row"
                  :disabled="acting === order.trade_no"
                  title="确认入账"
                  @click="handleMarkPaid(order)"
                >
                  <AppIcon name="check" :size="14" />
                </button>
                <button
                  v-if="order.status === ORDER_STATUS_PENDING"
                  type="button"
                  class="btn-row"
                  :disabled="acting === order.trade_no"
                  title="关闭订单"
                  @click="handleClose(order)"
                >
                  <AppIcon name="close" :size="14" />
                </button>
                <button
                  v-if="order.status === ORDER_STATUS_PAID"
                  type="button"
                  class="btn-row"
                  :disabled="acting === order.trade_no"
                  title="退款并扣回额度"
                  @click="handleRefund(order)"
                >
                  <AppIcon name="refresh" :size="14" />
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="total > 0" class="mt-3 card">
      <Pagination
        :page="page"
        :size="size"
        :total="total"
        :disabled="loading"
        @update:page="changePage"
        @update:size="changeSize"
      />
    </div>

    <section class="mt-5 card card-pad">
      <h3 class="section-title flex items-center gap-2">
        <AppIcon name="info" :size="16" class="text-brand-700" />
        关于退款
      </h3>
      <ul class="mt-3 space-y-1.5 text-xs leading-relaxed text-ink-400">
        <li>· 本页的「退款」只负责<strong>扣回已入账额度</strong>并把订单置为已退款，不会调用第三方支付平台的退款接口。</li>
        <li>· 请先在实际收款渠道完成退款，再回到本页操作，避免账实不符。</li>
        <li>· 若用户已把额度消耗掉，扣回后其额度可能变为负数（表现为无法继续调用），属于预期行为。</li>
      </ul>
    </section>
  </div>
</template>
