<script setup lang="ts">
/**
 * ECharts 封装组件：按需引入图表类型与组件，控制打包体积。
 *
 * 意图（Why）：
 *   1) echarts 全量引入约 1MB+，这里只注册用到的 Line/Bar/Pie 与必要组件；
 *   2) 统一处理「初始化 → 数据变化 setOption → resize → 销毁」生命周期，
 *      页面只管传 option，无需重复写 ref/onMounted 样板代码。
 *
 * 流转（Flow）：
 *   页面构造 option（用 utils/chart.ts 的配色常量）→ 本组件 setOption(notMerge)
 *   → 容器尺寸变化（含侧边栏收起）由 ResizeObserver 触发 resize
 *
 * 扩展（Extend）：
 *   需要新图表类型（如 Pie/Radar）时在下方 use([...]) 中追加对应模块；
 *   不要把 echarts 全量 import 进来（会显著增大产物）。
 */
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import {
  GridComponent,
  LegendComponent,
  TitleComponent,
  TooltipComponent,
} from 'echarts/components'
import { init, use, type ECharts } from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
// 仅类型导入（编译后会被完全擦除，不会把 echarts 全量运行时打进产物）
import type { EChartsOption } from 'echarts'
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'

use([
  CanvasRenderer,
  LineChart,
  BarChart,
  PieChart,
  GridComponent,
  TooltipComponent,
  LegendComponent,
  TitleComponent,
])

const props = withDefaults(
  defineProps<{
    /** ECharts 配置对象 */
    option: EChartsOption
    /** 容器高度（CSS 值） */
    height?: string
    /** 是否有数据（无数据时显示空态而不是空白画布） */
    hasData?: boolean
    /** 空态文案 */
    emptyText?: string
  }>(),
  { height: '300px', hasData: true, emptyText: '暂无数据' },
)

const container = ref<HTMLDivElement | null>(null)
let chart: ECharts | null = null
let observer: ResizeObserver | null = null

/** 渲染配置：数据变化时用 notMerge 覆盖，避免旧 series 残留（如从 3 条 series 变 2 条） */
function render(): void {
  if (!chart) return
  chart.setOption(props.option, { notMerge: true })
}

function resize(): void {
  chart?.resize()
}

onMounted(() => {
  if (!container.value) return
  chart = init(container.value, undefined, { renderer: 'canvas' })
  render()

  // ResizeObserver 优于 window.resize：侧边栏收起/展开不会触发 window.resize，
  // 但会导致容器宽度变化，若只听 window 事件图表会出现留白或溢出。
  if (typeof ResizeObserver !== 'undefined') {
    observer = new ResizeObserver(() => resize())
    observer.observe(container.value)
  }
})

watch(() => props.option, render, { deep: true })
watch(() => props.hasData, (value) => {
  if (value) {
    // 空态切换回有数据时容器刚出现，需要重新测量尺寸
    requestAnimationFrame(() => {
      resize()
      render()
    })
  }
})

onBeforeUnmount(() => {
  observer?.disconnect()
  observer = null
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div class="relative w-full" :style="{ height }">
    <div v-show="hasData" ref="container" class="h-full w-full" />

    <!-- 空态：直接不渲染画布，避免出现「空坐标系」造成误解 -->
    <div v-if="!hasData" class="flex h-full flex-col items-center justify-center gap-2 text-center">
      <span class="flex h-10 w-10 items-center justify-center rounded-xl bg-ink-850 text-ink-400 ring-1 ring-inset ring-ink-700">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round">
          <path d="M4 19.5h16" />
          <path d="M7 19.5V13" />
          <path d="M12 19.5V9" />
          <path d="M17 19.5v-4.5" />
        </svg>
      </span>
      <p class="text-sm text-ink-400">{{ emptyText }}</p>
    </div>
  </div>
</template>
