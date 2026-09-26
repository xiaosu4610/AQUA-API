<script setup lang="ts">
/**
 * 右侧抽屉（Drawer）：用于渠道新建/编辑这类字段较多的表单。
 *
 * 意图（Why）：
 *   渠道表单有 9 个字段，用居中弹窗会把背景页完全遮住，用户在填表时看不到列表上下文；
 *   抽屉从右侧滑出、保留左侧列表可见，适合「边看列表边改配置」的场景。
 *
 * 流转（Flow）：
 *   ChannelsView（open 状态）→ Teleport 到 body → 右侧面板 → footer 插槽放「保存/取消」
 *
 * 扩展（Extend）：
 *   需要更宽的表单请传 width="max-w-2xl"。
 */
import { onBeforeUnmount, watch } from 'vue'

import AppIcon from './AppIcon.vue'

const props = withDefaults(
  defineProps<{
    open: boolean
    title: string
    subtitle?: string
    /** 面板宽度类名 */
    width?: string
    /** 是否允许点击遮罩关闭 */
    closeOnBackdrop?: boolean
  }>(),
  { width: 'max-w-xl', closeOnBackdrop: true },
)

const emit = defineEmits<{ (e: 'close'): void }>()

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') emit('close')
}

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
    <div v-if="open" class="fixed inset-0 z-50" role="dialog" aria-modal="true" :aria-label="title">
      <div
        class="absolute inset-0 bg-ink-50/30 backdrop-blur-[2px] animate-fade-in"
        @click="closeOnBackdrop && emit('close')"
      />

      <aside
        class="absolute right-0 top-0 flex h-full w-full flex-col border-l border-ink-700 bg-ink-900 shadow-pop animate-slide-in-right"
        :class="width"
      >
        <header class="flex items-start justify-between gap-4 border-b border-ink-800 px-5 py-4">
          <div class="min-w-0">
            <h2 class="text-base font-semibold text-ink-50">{{ title }}</h2>
            <p v-if="subtitle" class="mt-1 text-xs leading-relaxed text-ink-400">{{ subtitle }}</p>
          </div>
          <button type="button" class="btn btn-ghost btn-icon -mr-1.5" aria-label="关闭" @click="emit('close')">
            <AppIcon name="close" :size="18" />
          </button>
        </header>

        <div class="flex-1 overflow-y-auto px-5 py-4">
          <slot />
        </div>

        <footer
          v-if="$slots.footer"
          class="flex flex-wrap items-center justify-end gap-2 border-t border-ink-800 bg-ink-900 px-5 py-4"
        >
          <slot name="footer" />
        </footer>
      </aside>
    </div>
  </Teleport>
</template>
