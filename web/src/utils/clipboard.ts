/**
 * 剪贴板工具：一键复制的统一实现。
 *
 * 意图（Why）：
 *   落地页/令牌页都有「一键复制」，而自托管部署常见于内网的 http 环境，
 *   此时 navigator.clipboard 不可用（需要安全上下文）；
 *   故提供 textarea + execCommand 的回退实现，保证复制在所有部署形态下都可用。
 *
 * 流转（Flow）：
 *   CopyButton.vue → copyText(text) → 返回是否成功 → 页面提示「已复制」或「复制失败，请手动选择」
 *
 * 扩展（Extend）：
 *   若将来需要复制富文本，另加 writeRichText()，不要改本函数的语义。
 */

/** 复制纯文本；返回是否成功（调用方据此决定提示文案） */
export async function copyText(text: string): Promise<boolean> {
  if (!text) return false

  // 首选现代 API（https / localhost 下可用）
  if (typeof navigator !== 'undefined' && navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // 用户拒绝授权或 API 异常：继续走回退方案，不直接失败
    }
  }

  // 回退：临时 textarea + execCommand（http 内网环境可用）
  try {
    const textarea = document.createElement('textarea')
    textarea.value = text
    textarea.setAttribute('readonly', '')
    textarea.style.position = 'fixed'
    textarea.style.top = '-1000px'
    textarea.style.opacity = '0'
    document.body.appendChild(textarea)
    textarea.select()
    textarea.setSelectionRange(0, textarea.value.length)
    const ok = document.execCommand('copy')
    document.body.removeChild(textarea)
    return ok
  } catch {
    return false
  }
}
