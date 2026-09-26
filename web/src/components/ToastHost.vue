<script setup lang="ts">
/**
 * 全局提示宿主：渲染 useToast 队列。
 *
 * 意图（Why）：
 *   提示条必须浮在页面之上且不被 overflow/transform 裁剪，故挂 body 并固定右下角；
 *   右下角靠近表格操作列，用户点击后的反馈视线移动距离最短。
 *
 * 流转（Flow）：
 *   useToast.push() → items → 本组件渲染 → 点击 × 或超时移除
 *
 * 扩展（Extend）：
 *   增加「操作撤销」按钮时在此渲染，而不要改 useToast 的语义。
 */
import AppIcon from './AppIcon.vue'
import { useToastQueue, type ToastKind } from '@/composables/useToast'

const { items, dismiss } = useToastQueue()

/** 各类型的图标与配色（用 ring 描边而非纯色填充，避免提示条过于抢眼） */
const KIND_STYLE: Record<ToastKind, { icon: 'check' | 'alert' | 'info'; class: string }> = {
  success: { icon: 'check', class: 'text-emerald-700 ring-emerald-500/25' },
  error: { icon: 'alert', class: 'text-red-700 ring-red-500/25' },
  info: { icon: 'info', class: 'text-brand-700 ring-brand-500/25' },
}
</script>

<template>
  <Teleport to="body">
    <div
      class="pointer-events-none fixed bottom-5 right-5 z-[60] flex w-[min(92vw,380px)] flex-col gap-2"
      role="status"
      aria-live="polite"
    >
      <TransitionGroup
        enter-active-class="transition duration-200 ease-out"
        enter-from-class="translate-y-2 opacity-0"
        leave-active-class="transition duration-150 ease-in"
        leave-to-class="translate-x-2 opacity-0"
      >
        <div
          v-for="item in items"
          :key="item.id"
          class="pointer-events-auto flex items-start gap-2.5 rounded-xl border border-ink-700 bg-ink-850/95 px-3.5 py-3 shadow-pop backdrop-blur"
        >
          <span
            class="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-lg bg-ink-900 ring-1 ring-inset"
            :class="KIND_STYLE[item.kind].class"
          >
            <AppIcon :name="KIND_STYLE[item.kind].icon" :size="14" />
          </span>
          <p class="flex-1 break-words text-sm leading-relaxed text-ink-100">{{ item.message }}</p>
          <button
            type="button"
            class="btn btn-ghost btn-icon h-6 w-6 text-ink-400"
            :aria-label="$t('components.toast.dismiss')"
            @click="dismiss(item.id)"
          >
            <AppIcon name="close" :size="14" />
          </button>
        </div>
      </TransitionGroup>
    </div>
  </Teleport>
</template>
