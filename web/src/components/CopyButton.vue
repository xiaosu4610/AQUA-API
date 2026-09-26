<script setup lang="ts">
/**
 * 一键复制按钮。
 *
 * 意图（Why）：
 *   复制是落地页接入示例与令牌页的高频操作；
 *   统一按钮形态 + 2 秒成功态反馈（图标变对勾），让用户确信「已复制」。
 *
 * 流转（Flow）：
 *   点击 → utils/clipboard.copyText() → 成功/失败提示（toast + 按钮态）
 *
 * 扩展（Extend）：
 *   需要复制富文本或多段内容时，扩展 props 而不是复制一份新组件。
 */
import { onBeforeUnmount, ref } from 'vue'

import AppIcon from './AppIcon.vue'
import { toastError, toastSuccess } from '@/composables/useToast'
import { copyText } from '@/utils/clipboard'

const props = withDefaults(
  defineProps<{
    /** 要复制的文本 */
    value: string
    /** 按钮文案（不传则只显示图标） */
    label?: string
    /** 成功提示文案 */
    successText?: string
    /** 是否使用小尺寸 */
    small?: boolean
    /** 是否使用描边按钮样式（默认幽灵按钮） */
    outline?: boolean
  }>(),
  { label: '', successText: '已复制到剪贴板', small: false, outline: false },
)

/** 复制成功态：2 秒后自动复原，避免按钮长期停留在「已复制」 */
const copied = ref(false)
let timer: number | undefined

onBeforeUnmount(() => {
  if (timer) window.clearTimeout(timer)
})

async function handleCopy(): Promise<void> {
  const ok = await copyText(props.value)
  if (!ok) {
    toastError('复制失败，请手动选择文本后复制')
    return
  }
  copied.value = true
  toastSuccess(props.successText)
  if (timer) window.clearTimeout(timer)
  timer = window.setTimeout(() => (copied.value = false), 2000)
}
</script>

<template>
  <button
    type="button"
    class="btn"
    :class="[outline ? 'btn-secondary' : 'btn-ghost', small ? 'btn-sm' : '']"
    :title="label || '复制'"
    @click="handleCopy"
  >
    <AppIcon :name="copied ? 'check' : 'copy'" :size="small ? 14 : 16" />
    <span v-if="label">{{ copied ? '已复制' : label }}</span>
  </button>
</template>
