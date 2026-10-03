/** 登录回跳地址的白名单校验（只放行站内路径）。
 *
 * 意图（Why）：
 *   redirect 取自查询串，任何人都能拼成 `https://evil.example` 或 `//evil.example`，
 *   直接喂给 router.replace 就会把刚登录的用户送到站外——这是"登录后钓鱼"的经典手法，
 *   管理员登录尤其危险：跳转目标是攻击者指定的页面，紧接着的伪造后台更容易得手。
 *   校验必须放在【读取侧】（跳转前），因为写入侧只能控制自己的 pathname，
 *   改不了别人构造出来的 URL。
 *
 * 流转（Flow）：
 *   /login 与 /admin/login 读 searchParams.redirect
 *     → safeRedirect(redirect, 默认页) → router.replace(结果)
 *
 * 扩展（Extend）：
 *   新增任何"跳转到用户指定地址"的位置（退出登录回跳、SSO 回调等）都必须走本函数，
 *   不要各自再写一遍 startsWith 判断——漏掉任一处就等于漏洞仍在。
 */
export function safeRedirect(raw: string | null | undefined, fallback: string): string {
  if (!raw) return fallback
  // 必须以单个 `/` 开头：一次挡掉绝对 URL、协议相对地址（//host）与空值
  if (!raw.startsWith('/') || raw.startsWith('//')) return fallback
  // 反斜杠开头的串会被部分浏览器规范化成 //host，同样能跳出站
  if (raw.includes('\\')) return fallback
  // 控制字符（C0 与 DEL）会绕过浏览器的地址归一化；用 charCodeAt 判断而不是
  // 正则字面量，免得在源码里写入不可见的字符
  for (let i = 0; i < raw.length; i++) {
    const code = raw.charCodeAt(i)
    if (code < 0x20 || code === 0x7f) return fallback
  }
  return raw
}
