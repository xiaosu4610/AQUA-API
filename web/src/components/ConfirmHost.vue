<script setup lang="ts">
/**
 * 全局确认弹窗宿主。
 *
 * 意图（Why）：
 *   破坏性操作（删除渠道/令牌/用户）必须二次确认；
 *   用 Promise 风格的 API（confirmDialog）让调用处保持「线性」可读。
 *
 * 流转（Flow）：
 *   页面 await confirmDialog({...}) → state.open=true → 本组件渲染
 *   → 用户点击 → answerConfirm(true/false) → Promise 兑现
 *
 * 扩展（Extend）：
 *   若需要「输入名称以确认」，在 useConfirm.ts 扩展 request 字段后在此渲染输入框。
 *   文案：request 未提供按钮/说明文案时，用当前语言的词条兜底（common.action.* / common.confirm.*）。
 */
import AppIcon from './AppIcon.vue'
import Modal from './Modal.vue'
import { answerConfirm, useConfirmState } from '@/composables/useConfirm'

const state = useConfirmState()
</script>

<template>
  <Modal
    :open="state.open"
    :title="state.request.title"
    width="max-w-md"
    :close-on-backdrop="false"
    @close="answerConfirm(false)"
  >
    <div class="flex gap-3">
      <span
        class="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-ink-850 ring-1 ring-inset"
        :class="state.request.danger ? 'text-red-700 ring-red-500/25' : 'text-brand-700 ring-brand-500/25'"
      >
        <AppIcon :name="state.request.danger ? 'alert' : 'info'" :size="18" />
      </span>
      <p class="flex-1 text-sm leading-relaxed text-ink-200">
        {{ state.request.message || $t('common.confirm.defaultMessage') }}
      </p>
    </div>

    <template #footer>
      <button type="button" class="btn btn-secondary" @click="answerConfirm(false)">
        {{ state.request.cancelText || $t('common.action.cancel') }}
      </button>
      <button
        type="button"
        class="btn"
        :class="state.request.danger ? 'btn-danger' : 'btn-primary'"
        @click="answerConfirm(true)"
      >
        {{ state.request.confirmText || $t('common.action.confirm') }}
      </button>
    </template>
  </Modal>
</template>
