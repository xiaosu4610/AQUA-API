<script setup lang="ts">
/**
 * 统计卡片（仪表盘 / 概览页的汇总数字）。
 *
 * 意图（Why）：
 *   汇总卡片在门户与管理端共 8 个实例，尺寸、字号、间距必须一致；
 *   抽成组件后，只需改一处即可统一调整全部卡片的密度。
 *
 * 流转（Flow）：
 *   页面传入 label/value/icon/tone → 渲染；差异化的明细行用默认插槽补充
 *
 * 扩展（Extend）：
 *   需要迷你趋势线（sparkline）时，在插槽里放 <EChart height="40px" />。
 */
import AppIcon from './AppIcon.vue'
import { type IconName } from './icons'

withDefaults(
  defineProps<{
    /** 指标名称 */
    label: string
    /** 指标值（已格式化好的字符串，卡片不负责数字格式） */
    value: string
    /** 辅助说明（如「已启用 7 / 共 8」） */
    hint?: string
    /** 左上角图标 */
    icon: IconName
    /** 语义色调：决定图标底色与文字色 */
    tone?: 'brand' | 'ok' | 'warn' | 'err' | 'mute'
  }>(),
  { tone: 'brand', hint: '' },
)

/** 色调 → 样式（图标底色 + 图标色） */
const TONE_CLASS: Record<string, string> = {
  brand: 'bg-brand-500/10 text-brand-700 ring-brand-500/20',
  ok: 'bg-emerald-500/10 text-emerald-700 ring-emerald-500/20',
  warn: 'bg-amber-500/10 text-amber-700 ring-amber-500/20',
  err: 'bg-red-500/10 text-red-700 ring-red-500/20',
  mute: 'bg-ink-800 text-ink-300 ring-ink-700',
}
</script>

<template>
  <div class="card card-pad">
    <div class="flex items-start justify-between gap-3">
      <div class="min-w-0">
        <p class="text-xs font-medium text-ink-400">{{ label }}</p>
        <p class="mt-2 truncate text-2xl font-semibold tracking-tight text-ink-50 tabular-nums" :title="value">
          {{ value }}
        </p>
      </div>
      <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl ring-1 ring-inset" :class="TONE_CLASS[tone]">
        <AppIcon :name="icon" :size="18" />
      </span>
    </div>

    <p v-if="hint" class="mt-3 text-xs text-ink-400">{{ hint }}</p>
    <div v-if="$slots.default" class="mt-3">
      <slot />
    </div>
  </div>
</template>
