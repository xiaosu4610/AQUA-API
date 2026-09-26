/**
 * 轻量全局提示（Toast）——不引入 UI 组件库，避免为一个提示功能增加依赖体积。
 *
 * 意图（Why）：
 *   操作结果必须可反馈（创建成功、复制成功、接口报错），否则用户会反复点击；
 *   用模块级单例状态 + 一个宿主组件（ToastHost）即可覆盖全站，无需状态库。
 *
 * 流转（Flow）：
 *   任意 .vue / router 守卫 → toastSuccess()/toastError() → 写入 items
 *   → ToastHost.vue（挂载在 App.vue）渲染 → 定时自动移除
 *
 * 扩展（Extend）：
 *   需要「确认后执行」的交互请用 composables/useConfirm.ts；
 *   本文件只做单向通知，不要在这里加交互逻辑。
 *   文案：这里不硬编码中文 —— 兜底文案取自 i18n 词条，调用方传入的 message 应已是译文。
 */
import { ref } from 'vue'

import { i18n } from '@/i18n'

export type ToastKind = 'success' | 'error' | 'info'

export interface ToastItem {
  id: number
  kind: ToastKind
  message: string
}

/** 模块级单例：全站共享同一队列 */
const items = ref<ToastItem[]>([])
let seq = 0

/** 各类提示的默认停留时长（毫秒）：错误留得久一点，便于阅读 */
const DURATION: Record<ToastKind, number> = {
  success: 2800,
  info: 3200,
  error: 5000,
}

function push(kind: ToastKind, message: string, duration = DURATION[kind]): void {
  const id = ++seq
  items.value.push({ id, kind, message })
  window.setTimeout(() => dismiss(id), duration)
}

/** 手动关闭（用户点击 × 或自动过期调用） */
export function dismiss(id: number): void {
  items.value = items.value.filter((item) => item.id !== id)
}

/** 成功提示 */
export function toastSuccess(message: string): void {
  push('success', message)
}

/** 失败提示：message 为空时用当前语言的兜底文案，避免出现「空提示框」 */
export function toastError(message: string | undefined): void {
  push('error', message || i18n.global.t('common.toast.operationFailed'))
}

/** 中性提示 */
export function toastInfo(message: string): void {
  push('info', message)
}

/** 供 ToastHost.vue 读取队列 */
export function useToastQueue() {
  return { items, dismiss }
}
