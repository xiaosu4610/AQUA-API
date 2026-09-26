<script setup lang="ts">
/**
 * 分页控件。
 *
 * 意图（Why）：
 *   契约统一了 ?page=&size= 分页约定（size 上限 100），因此全站只需一个分页控件；
 *   集中实现可保证「每页条数」选项与总数展示口径一致。
 *
 * 流转（Flow）：
 *   页面持有 page/size/total → 本组件 emits update:page / update:size → 页面重新请求
 *
 * 扩展（Extend）：
 *   需要跳页输入框时在此追加；页码超范围的边界处理已在 computed 中完成。
 */
import { computed } from 'vue'

import AppIcon from './AppIcon.vue'

const props = withDefaults(
  defineProps<{
    page: number
    size: number
    total: number
    /** 每页条数可选项（契约上限 100） */
    sizeOptions?: number[]
    /** 是否禁用（请求进行中） */
    disabled?: boolean
  }>(),
  { sizeOptions: () => [10, 20, 50, 100], disabled: false },
)

const emit = defineEmits<{
  (e: 'update:page', value: number): void
  (e: 'update:size', value: number): void
}>()

/** 总页数：至少 1 页，避免空数据时出现「第 1 / 0 页」 */
const totalPages = computed(() => Math.max(1, Math.ceil(props.total / Math.max(1, props.size))))

/** 当前区间：如 21-40 / 共 128 条 */
const rangeText = computed(() => {
  if (props.total === 0) return `共 0 条`
  const start = (props.page - 1) * props.size + 1
  const end = Math.min(props.total, props.page * props.size)
  return `${start}-${end} / 共 ${props.total} 条`
})

const canPrev = computed(() => props.page > 1 && !props.disabled)
const canNext = computed(() => props.page < totalPages.value && !props.disabled)

function goPrev(): void {
  if (canPrev.value) emit('update:page', props.page - 1)
}

function goNext(): void {
  if (canNext.value) emit('update:page', props.page + 1)
}

function changeSize(event: Event): void {
  const value = Number((event.target as HTMLSelectElement).value)
  emit('update:size', value)
}

/** 页码文本（当前页 / 总页数） */
const pageText = computed(() => `第 ${Math.min(props.page, totalPages.value)} / ${totalPages.value} 页`)
</script>

<template>
  <div class="flex flex-wrap items-center justify-between gap-3 border-t border-ink-800 px-4 py-3">
    <p class="text-xs tabular-nums text-ink-400">{{ rangeText }}</p>

    <div class="flex items-center gap-3">
      <label class="flex items-center gap-1.5 text-xs text-ink-400">
        每页
        <select
          class="rounded-md border border-ink-700 bg-ink-900 px-2 py-1 text-xs text-ink-200 focus:border-brand-500/60"
          :value="size"
          :disabled="disabled"
          @change="changeSize"
        >
          <option v-for="option in sizeOptions" :key="option" :value="option">{{ option }}</option>
        </select>
        条
      </label>

      <div class="flex items-center gap-1">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="!canPrev" @click="goPrev">
          <AppIcon name="chevron-left" :size="14" />
          上一页
        </button>
        <span class="px-1 text-xs tabular-nums text-ink-400">{{ pageText }}</span>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="!canNext" @click="goNext">
          下一页
          <AppIcon name="chevron-right" :size="14" />
        </button>
      </div>
    </div>
  </div>
</template>
