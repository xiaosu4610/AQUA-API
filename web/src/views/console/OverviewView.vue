<script setup lang="ts">
/**
 * 用户门户 · 概览：额度、用量趋势、模型分布、最近调用。
 *
 * 意图（Why）：
 *   用户进入控制台最先关心两件事——「我还有多少额度」和「我的调用是否正常」；
 *   因此页面顺序固定为：额度概览卡 → 用量趋势 → 模型分布 → 最近调用。
 *
 * 流转（Flow）：
 *   进入页面 → auth.refreshUser()（校正额度）+ fetchMyUsage(days) + listMyLogs(最近 5 条)
 *   → 渲染统计卡与图表；任一请求失败只影响对应区块（分区错误态，不整页白屏）
 *
 * 扩展（Extend）：
 *   新增指标卡：在 <section> 的网格里追加 StatCard（值需先用 utils/format 格式化）；
 *   新增图表：复用 EChart.vue + utils/chart.ts 的配色常量。
 */
import type { EChartsOption } from 'echarts'
import { computed, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import EChart from '@/components/EChart.vue'
import LogTable from '@/components/LogTable.vue'
import StatCard from '@/components/StatCard.vue'
import { ApiError } from '@/api/client'
import { fetchMyUsage, listMyLogs } from '@/api/portal'
import type { UsageLog, UsageStats } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import {
  AXIS_LABEL_STYLE,
  AXIS_LINE_STYLE,
  CHART_PALETTE,
  SPLIT_LINE_STYLE,
  TOOLTIP_STYLE,
  areaGradient,
} from '@/utils/chart'
import { formatCompact, formatNumber } from '@/utils/format'

const auth = useAuthStore()

/** 可选统计区间（契约通过 ?days= 控制，缺省 7） */
const DAY_OPTIONS = [7, 14, 30]
const days = ref(7)

const usage = ref<UsageStats | null>(null)
const usageLoading = ref(true)
const usageError = ref('')

const logs = ref<UsageLog[]>([])
const logsLoading = ref(true)
const logsError = ref('')

/** 拉取用量统计（区间切换时复用） */
async function loadUsage(): Promise<void> {
  usageLoading.value = true
  usageError.value = ''
  try {
    usage.value = await fetchMyUsage(days.value)
  } catch (error) {
    usage.value = null
    usageError.value = error instanceof ApiError ? error.message : '用量数据加载失败'
  } finally {
    usageLoading.value = false
  }
}

/** 拉取最近调用（只取前 5 条，完整记录在「调用日志」页） */
async function loadRecentLogs(): Promise<void> {
  logsLoading.value = true
  logsError.value = ''
  try {
    const page = await listMyLogs({ page: 1, size: 5 })
    logs.value = page.items ?? []
  } catch (error) {
    logs.value = []
    logsError.value = error instanceof ApiError ? error.message : '调用记录加载失败'
  } finally {
    logsLoading.value = false
  }
}

onMounted(async () => {
  // 并行发起：三个区块互不依赖，减少首屏等待
  await Promise.all([auth.refreshUser(), loadUsage(), loadRecentLogs()])
})

/** 区间切换：只重取用量统计（日志不受区间影响） */
function selectDays(value: number): void {
  if (days.value === value) return
  days.value = value
  void loadUsage()
}

/* ── 统计卡数值 ───────────────────────────────────────── */

/** 剩余额度：契约未定义 quota 的单位与语义，这里按「账户额度」如实展示数值 */
const quotaText = computed(() => formatNumber(auth.user?.quota ?? 0))
const usedQuotaText = computed(() => formatNumber(auth.user?.used_quota ?? 0))
const requestsText = computed(() => formatNumber(usage.value?.total_requests ?? 0))
const tokensText = computed(() => formatCompact(usage.value?.total_tokens ?? 0))

/* ── 图表配置 ───────────────────────────────────────── */

/** 日期轴标签：'2026-09-20' → '09-20'，缩短宽度避免拥挤 */
function shortDate(date: string): string {
  return date ? date.slice(5) : ''
}

const trendOption = computed<EChartsOption>(() => {
  const points = usage.value?.series ?? []
  return {
    color: CHART_PALETTE,
    tooltip: { trigger: 'axis', ...TOOLTIP_STYLE },
    legend: {
      data: ['请求数', 'Token 数'],
      right: 0,
      top: 0,
      icon: 'roundRect',
      itemWidth: 8,
      itemHeight: 8,
      textStyle: { color: '#9aa8bc', fontSize: 11 },
    },
    grid: { left: 4, right: 8, top: 36, bottom: 0, containLabel: true },
    xAxis: {
      type: 'category',
      data: points.map((point) => shortDate(point.date)),
      axisLabel: AXIS_LABEL_STYLE,
      axisLine: AXIS_LINE_STYLE,
      axisTick: { show: false },
    },
    yAxis: [
      {
        type: 'value',
        name: '请求',
        nameTextStyle: { color: '#6f7f96', fontSize: 11 },
        axisLabel: AXIS_LABEL_STYLE,
        splitLine: SPLIT_LINE_STYLE,
      },
      {
        type: 'value',
        name: 'Token',
        nameTextStyle: { color: '#6f7f96', fontSize: 11 },
        axisLabel: AXIS_LABEL_STYLE,
        splitLine: { show: false },
      },
    ],
    series: [
      {
        name: '请求数',
        type: 'bar',
        barMaxWidth: 16,
        itemStyle: { borderRadius: [4, 4, 0, 0], color: '#22d3ee' },
        data: points.map((point) => point.requests),
      },
      {
        name: 'Token 数',
        type: 'line',
        yAxisIndex: 1,
        smooth: true,
        symbolSize: 5,
        lineStyle: { width: 2 },
        areaStyle: { color: areaGradient('#818cf8') },
        data: points.map((point) => point.tokens),
      },
    ],
  }
})

/** 模型分布：横向条形图（模型名较长，横向排版更易读）——取前 8 个 */
const modelOption = computed<EChartsOption>(() => {
  const list = [...(usage.value?.by_model ?? [])]
    .sort((a, b) => b.requests - a.requests)
    .slice(0, 8)
    // 横向条形图 y 轴自下而上绘制，反转后最大的模型显示在顶部
    .reverse()
  return {
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' }, ...TOOLTIP_STYLE },
    grid: { left: 4, right: 24, top: 8, bottom: 0, containLabel: true },
    xAxis: { type: 'value', axisLabel: AXIS_LABEL_STYLE, splitLine: SPLIT_LINE_STYLE },
    yAxis: {
      type: 'category',
      data: list.map((item) => item.model),
      axisLabel: { color: '#9aa8bc', fontSize: 11 },
      axisLine: AXIS_LINE_STYLE,
      axisTick: { show: false },
    },
    series: [
      {
        name: '请求数',
        type: 'bar',
        barMaxWidth: 14,
        itemStyle: { borderRadius: [0, 4, 4, 0], color: '#22d3ee' },
        data: list.map((item) => item.requests),
      },
    ],
  }
})

const hasTrendData = computed(() => (usage.value?.series ?? []).some((point) => point.requests > 0 || point.tokens > 0))
const hasModelData = computed(() => (usage.value?.by_model ?? []).length > 0)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">概览</h2>
        <p class="page-desc">这里是你的账户额度与调用概况。</p>
      </div>
      <RouterLink to="/console/tokens" class="btn btn-primary btn-sm">
        <AppIcon name="key" :size="15" />
        管理访问令牌
      </RouterLink>
    </div>

    <!-- 额度与用量汇总 -->
    <section class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <StatCard label="账户额度" :value="quotaText" icon="quota" tone="brand">
        <p class="text-xs text-ink-400">额度单位由后端定义，用于计量模型调用消耗。</p>
      </StatCard>
      <StatCard label="已用额度" :value="usedQuotaText" icon="trend" tone="warn">
        <p class="text-xs text-ink-400">累计消耗，随调用实时增长。</p>
      </StatCard>
      <StatCard
        label="请求数"
        :value="requestsText"
        :hint="`最近 ${usage?.range_days ?? days} 天`"
        icon="bolt"
        tone="ok"
      />
      <StatCard
        label="Token 消耗"
        :value="tokensText"
        :hint="`最近 ${usage?.range_days ?? days} 天`"
        icon="chart"
        tone="mute"
      />
    </section>

    <!-- 用量趋势 -->
    <section class="mt-6 card">
      <div class="card-head">
        <div>
          <h3 class="section-title">用量趋势</h3>
          <p class="mt-1 text-xs text-ink-400">按天统计的请求数与 Token 消耗。</p>
        </div>
        <div class="flex items-center gap-1 rounded-lg border border-ink-700 bg-ink-900 p-1">
          <button
            v-for="option in DAY_OPTIONS"
            :key="option"
            type="button"
            class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors"
            :class="days === option ? 'bg-brand-500/15 text-brand-200' : 'text-ink-400 hover:text-ink-200'"
            @click="selectDays(option)"
          >
            {{ option }} 天
          </button>
        </div>
      </div>

      <div class="card-pad">
        <div v-if="usageError" class="flex flex-wrap items-center gap-3 rounded-xl border border-red-500/25 bg-red-500/10 px-4 py-3">
          <AppIcon name="alert" :size="16" class="text-red-300" />
          <p class="flex-1 text-sm text-red-200">{{ usageError }}</p>
          <button type="button" class="btn btn-secondary btn-sm" @click="loadUsage">
            <AppIcon name="refresh" :size="14" />
            重试
          </button>
        </div>
        <div v-else-if="usageLoading" class="flex h-[300px] items-center justify-center gap-2 text-sm text-ink-400">
          <span class="h-5 w-5 animate-spin rounded-full border-2 border-ink-600 border-t-brand-400" />
          正在加载用量数据…
        </div>
        <EChart v-else :option="trendOption" :has-data="hasTrendData" height="300px" empty-text="该区间内暂无调用记录" />
      </div>
    </section>

    <div class="mt-6 grid gap-6 xl:grid-cols-2">
      <!-- 模型分布 -->
      <section class="card">
        <div class="card-head">
          <div>
            <h3 class="section-title">模型分布</h3>
            <p class="mt-1 text-xs text-ink-400">按请求数排序的前 8 个模型。</p>
          </div>
        </div>
        <div class="card-pad">
          <div v-if="usageError" class="text-sm text-red-300">{{ usageError }}</div>
          <div v-else-if="usageLoading" class="flex h-[280px] items-center justify-center text-sm text-ink-400">
            正在加载…
          </div>
          <EChart v-else :option="modelOption" :has-data="hasModelData" height="280px" empty-text="该区间内暂无可统计的模型调用" />
        </div>
      </section>

      <!-- 最近调用 -->
      <section class="card">
        <div class="card-head">
          <div>
            <h3 class="section-title">最近调用</h3>
            <p class="mt-1 text-xs text-ink-400">最新 5 条调用记录。</p>
          </div>
          <RouterLink to="/console/logs" class="btn btn-ghost btn-sm">
            查看全部
            <AppIcon name="chevron-right" :size="14" />
          </RouterLink>
        </div>
        <div class="p-0">
          <LogTable
            :logs="logs"
            :loading="logsLoading"
            :error="logsError"
            empty-text="还没有调用记录"
            empty-hint="创建访问令牌并发起一次模型请求后，这里会显示最近的调用。"
            @retry="loadRecentLogs"
          />
        </div>
      </section>
    </div>
  </div>
</template>
