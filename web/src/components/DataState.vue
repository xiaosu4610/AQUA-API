<script setup lang="ts">
/**
 * 数据状态占位（加载态 / 错误态 / 空态）——全站唯一的「无内容」表达。
 *
 * 意图（Why）：
 *   若不统一处理，用户会遇到「白屏什么都不显示」的最差体验；
 *   本组件保证任何列表在任何时刻都有明确反馈：转圈、报错+重试、或空态引导。
 *
 * 流转（Flow）：
 *   页面维护 loading/error/空数据状态 → 本组件按优先级渲染
 *     loading > error > empty；三者皆不成立（即有数据）时本组件不渲染任何内容，
 *     数据行由调用方自己渲染 —— 这样表格里不会多出一行空白。
 *
 * 扩展（Extend）：
 *   表格内使用时传 colspan（此时以 <tr><td> 渲染，保证表格语义与列宽）；
 *   需要空态 CTA 时用 #action 插槽（如「创建第一个令牌」按钮）。
 */
import { computed } from 'vue'

import AppIcon from './AppIcon.vue'

const props = withDefaults(
  defineProps<{
    /** 是否加载中 */
    loading?: boolean
    /** 错误信息（空串表示无错误） */
    error?: string
    /** 是否为空数据 */
    empty?: boolean
    /** 加载中文案 */
    loadingText?: string
    /** 空态文案 */
    emptyText?: string
    /** 空态补充说明 */
    emptyHint?: string
    /** 错误态重试按钮文案（不传则不显示重试按钮） */
    retryText?: string
    /** 传入后以表格行形式渲染（值即 colSpan） */
    colspan?: number
    /** 紧凑模式：减小纵向留白（用于卡片内的小列表） */
    compact?: boolean
  }>(),
  {
    loading: false,
    error: '',
    empty: false,
    loadingText: '加载中…',
    emptyText: '暂无数据',
    emptyHint: '',
    retryText: '重新加载',
    compact: false,
  },
)

const emit = defineEmits<{ (e: 'retry'): void }>()

/** 是否以表格行渲染 */
const asRow = computed(() => typeof props.colspan === 'number' && props.colspan > 0)

/**
 * 是否需要渲染占位块。
 *
 * 关键：有数据时不渲染任何结构（否则表格里会多出一行空白），
 * 数据行由调用方自己渲染；只有调用方显式提供了默认插槽时才渲染其内容。
 */
const showState = computed(() => props.loading || Boolean(props.error) || props.empty)

/** 内层容器 class：控制纵向留白与对齐 */
const panelClass = computed(() => [
  'flex flex-col items-center justify-center gap-2 text-center',
  props.compact ? 'px-4 py-8' : 'px-4 py-14',
])
</script>

<template>
  <component v-if="showState" :is="asRow ? 'tr' : 'div'">
    <component
      :is="asRow ? 'td' : 'div'"
      :colspan="asRow ? colspan : undefined"
      :class="panelClass"
    >
      <!-- 加载态：骨架式转圈，文案说明「正在做什么」 -->
      <template v-if="loading">
        <span
          class="h-5 w-5 animate-spin rounded-full border-2 border-ink-600 border-t-brand-400"
          aria-hidden="true"
        />
        <p class="text-sm text-ink-400">{{ loadingText }}</p>
      </template>

      <!-- 错误态：展示后端返回的可读信息 + 重试入口 -->
      <template v-else-if="error">
        <span class="flex h-10 w-10 items-center justify-center rounded-xl bg-ink-850 text-red-700 ring-1 ring-inset ring-red-500/25">
          <AppIcon name="alert" :size="20" />
        </span>
        <p class="max-w-md text-sm leading-relaxed text-ink-200">{{ error }}</p>
        <button v-if="retryText" type="button" class="btn btn-secondary btn-sm mt-1" @click="emit('retry')">
          <AppIcon name="refresh" :size="14" />
          {{ retryText }}
        </button>
      </template>

      <!-- 空态：给出「下一步该做什么」的指引，而不是只写「暂无数据」 -->
      <template v-else-if="empty">
        <span class="flex h-10 w-10 items-center justify-center rounded-xl bg-ink-850 text-ink-400 ring-1 ring-inset ring-ink-700">
          <AppIcon name="layers" :size="20" />
        </span>
        <p class="text-sm font-medium text-ink-200">{{ emptyText }}</p>
        <p v-if="emptyHint" class="max-w-md text-xs leading-relaxed text-ink-400">{{ emptyHint }}</p>
        <div v-if="$slots.action" class="mt-1">
          <slot name="action" />
        </div>
      </template>
    </component>
  </component>
</template>
