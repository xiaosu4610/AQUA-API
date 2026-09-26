<script setup lang="ts">
/**
 * 通用弹窗（Modal）。
 *
 * 意图（Why）：
 *   新建/编辑/一次性密钥展示等场景都需要「聚焦的浮层」；
 *   统一实现遮罩、ESC 关闭、滚动锁定与动画，避免各页面重复处理这些细节。
 *
 * 流转（Flow）：
 *   页面 v-model:open → Teleport 到 body → 内部插槽渲染内容 / footer 插槽渲染按钮
 *
 * 扩展（Extend）：
 *   需要更宽的表单请传 width="max-w-2xl"；
 *   表单类弹窗建议 closeOnBackdrop=false，避免误点遮罩丢失输入。
 */
import { onBeforeUnmount, watch } from 'vue'

import AppIcon from './AppIcon.vue'

const props = withDefaults(
  defineProps<{
    open: boolean
    title: string
    /** 标题下的补充说明（建议写清影响范围） */
    subtitle?: string
    /** 面板最大宽度类名 */
    width?: string
    /** 是否允许点击遮罩关闭 */
    closeOnBackdrop?: boolean
    /** 是否显示右上角关闭按钮（一次性密钥弹窗建议保留） */
    showClose?: boolean
  }>(),
  { width: 'max-w-lg', closeOnBackdrop: true, showClose: true },
)

const emit = defineEmits<{ (e: 'close'): void }>()

/** ESC 关闭：键盘用户的操作预期 */
function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') emit('close')
}

/**
 * 打开时锁定 body 滚动：否则长表单滚动会「带着」背后的页面一起滚。
 * 关闭时恢复为空串（而不是恢复原值），因为本应用默认不需要 body 滚动锁的组合场景。
 */
watch(
  () => props.open,
  (open) => {
    if (open) {
      document.addEventListener('keydown', onKeydown)
      document.body.style.overflow = 'hidden'
    } else {
      document.removeEventListener('keydown', onKeydown)
      document.body.style.overflow = ''
    }
  },
)

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  document.body.style.overflow = ''
})
</script>

<template>
  <Teleport to="body">
    <Transition
      enter-active-class="transition duration-150 ease-out"
      enter-from-class="opacity-0"
      enter-to-class="opacity-100"
      leave-active-class="transition duration-100 ease-in"
      leave-from-class="opacity-100"
      leave-to-class="opacity-0"
    >
      <div
        v-if="open"
        class="fixed inset-0 z-50 overflow-y-auto overscroll-contain p-4 sm:p-6"
        role="dialog"
        aria-modal="true"
        :aria-label="title"
      >
        <div
          class="fixed inset-0 bg-ink-950/80 backdrop-blur-[2px]"
          @click="closeOnBackdrop && emit('close')"
        />

        <div
          class="relative z-10 mx-auto my-6 w-full rounded-2xl border border-ink-700 bg-ink-900 shadow-pop"
          :class="width"
        >
          <header class="flex items-start justify-between gap-4 border-b border-ink-800 px-5 py-4">
            <div class="min-w-0">
              <h2 class="text-base font-semibold text-white">{{ title }}</h2>
              <p v-if="subtitle" class="mt-1 text-xs leading-relaxed text-ink-400">{{ subtitle }}</p>
            </div>
            <button
              v-if="showClose"
              type="button"
              class="btn btn-ghost btn-icon -mr-1.5 -mt-0.5"
              aria-label="关闭"
              @click="emit('close')"
            >
              <AppIcon name="close" :size="18" />
            </button>
          </header>

          <div class="px-5 py-4">
            <slot />
          </div>

          <footer v-if="$slots.footer" class="flex flex-wrap items-center justify-end gap-2 border-t border-ink-800 px-5 py-4">
            <slot name="footer" />
          </footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
