/**
 * 全局确认弹窗（Promise 风格）。
 *
 * 意图（Why）：
 *   删除渠道/令牌/用户等破坏性操作必须先确认；若各页面各写一个弹窗组件，
 *   会出现文案与按钮顺序不一致的问题。用 `await confirmDialog({...})` 的写法，
 *   调用处可读性最好（if (!ok) return）。
 *
 * 流转（Flow）：
 *   页面 → confirmDialog(options) → 写入单例状态 → ConfirmHost.vue 展示
 *   → 用户点击 → answerConfirm(true/false) → Promise 兑现 → 页面继续/中止
 *
 * 扩展（Extend）：
 *   需要输入后再确认（如「输入用户名以确认删除」）时，扩展 ConfirmRequest 增加字段，
 *   并在 ConfirmHost.vue 中渲染对应输入框。
 *   文案：默认按钮文案不在此硬编码 —— 由 ConfirmHost.vue 用词条兜底（common.action.*）。
 */
import { reactive } from 'vue'

export interface ConfirmRequest {
  /** 标题：一句话说明将要执行的动作，如「删除渠道」 */
  title: string
  /** 补充说明：写清影响范围（如「该渠道的令牌将无法调用」） */
  message?: string
  /** 确认按钮文案；不传时由 ConfirmHost 用当前语言的默认词条兜底 */
  confirmText?: string
  /** 取消按钮文案；不传时由 ConfirmHost 用当前语言的默认词条兜底 */
  cancelText?: string
  /** 是否为破坏性操作（红色按钮）；默认 false */
  danger?: boolean
}

interface ConfirmState {
  open: boolean
  request: ConfirmRequest
}

// 注意：这里【不写死中文】。按钮文案的默认值由 ConfirmHost.vue 在渲染时用 $t 兜底，
// 这样 composables 层与语言解耦，调用方也可以自由传入已翻译的文案。
const state = reactive<ConfirmState>({
  open: false,
  request: { title: '', message: '', confirmText: '', cancelText: '', danger: false },
})

/** 等待用户答复的 resolver；同一时刻只允许一个确认框（后开覆盖前开，前一个自动视为取消） */
let resolver: ((value: boolean) => void) | null = null

/** 打开确认框，返回用户是否确认 */
export function confirmDialog(request: ConfirmRequest): Promise<boolean> {
  // 上一个未决的确认框按「取消」处理，避免 Promise 永久悬挂
  resolver?.(false)
  state.request = {
    confirmText: '',
    cancelText: '',
    danger: false,
    ...request,
  }
  state.open = true
  return new Promise<boolean>((resolve) => {
    resolver = resolve
  })
}

/** 由 ConfirmHost.vue 调用：关闭并兑现 Promise */
export function answerConfirm(value: boolean): void {
  state.open = false
  const fn = resolver
  resolver = null
  fn?.(value)
}

/** 供 ConfirmHost.vue 读取状态 */
export function useConfirmState() {
  return state
}
