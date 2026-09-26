/**
 * 国际化（i18n）基础设施：实例创建、语言检测、持久化与 <html lang/dir> 同步。
 *
 * 意图（Why）：
 *   全站界面文案要支持联合国六种官方语言（zh-CN / en / fr / ru / es / ar），
 *   其中阿拉伯语为 RTL（从右向左）。把「词条合并 + 语言检测 + 方向切换」
 *   集中在一个入口，组件只关心 $t()，不关心语言从哪来、往哪存。
 *
 * 流转（Flow）：
 *   main.ts → app.use(i18n)（useI18n/$t 生效）
 *   语言优先级：localStorage → 浏览器 Accept-Language（只匹配语言主码）→ 回退 zh-CN
 *   setLocale(lang) → 更新 i18n.locale → 写 localStorage → 同步 <html lang> 与 <html dir>
 *   api/client.ts 通过 getLocale() 取当前语言，写入请求头 Accept-Language。
 *
 * 扩展（Extend）：
 *   新增语言：在 locales/<code>/ 建 common/components/portal/admin 四个词条文件，
 *   在 SUPPORTED_LOCALES 与下方 messages 各加一项即可；
 *   词条键集合由 `messages` 的类型约束（Record<LocaleCode, typeof zhCN>）强制与中文一致，
 *   漏键会在 `npm run type-check` 阶段直接报错。
 */
import { createI18n } from 'vue-i18n'

import ar from './locales/ar'
import en from './locales/en'
import es from './locales/es'
import fr from './locales/fr'
import ru from './locales/ru'
import zhCN from './locales/zh-CN'

/** 支持的语言及其「母语自称」（语言切换器直接用母语名展示，不随界面语言变化） */
export const SUPPORTED_LOCALES = [
  { code: 'zh-CN', name: '简体中文' },
  { code: 'en', name: 'English' },
  { code: 'fr', name: 'Français' },
  { code: 'ru', name: 'Русский' },
  { code: 'es', name: 'Español' },
  { code: 'ar', name: 'العربية' },
] as const

/** 受支持的语言代码联合类型 */
export type LocaleCode = (typeof SUPPORTED_LOCALES)[number]['code']

/** 语言选择的 localStorage 键名（与 api/client.ts 的会话键风格保持一致） */
const STORAGE_KEY = 'aqua.locale'

/** 兜底语言：所有匹配失败时使用，同时作为 vue-i18n 的 fallbackLocale */
export const FALLBACK_LOCALE: LocaleCode = 'zh-CN'

/**
 * 六种语言的词条合并表。
 *
 * 这里的类型是 Record<LocaleCode, typeof zhCN>：以简体中文的词条结构为基准，
 * 其余语言若缺少任何一条同名键，`vue-tsc` 会直接报错 —— 用类型系统保证键集合一致，
 * 而不是靠人工核对（缺键时界面会直接显示键名，必须在编译期拦住）。
 */
const messages = {
  'zh-CN': zhCN,
  en,
  fr,
  ru,
  es,
  ar,
} satisfies Record<LocaleCode, typeof zhCN>

/** 判断某语言是否为从右向左（RTL）书写 */
export function isRtl(code: LocaleCode): boolean {
  return code === 'ar'
}

/**
 * 把任意语言标签规整为受支持的语言代码。
 *
 * 规则：先精确匹配（大小写不敏感），再按「语言主码」匹配，
 * 因此 `fr-CA` → `fr`、`zh-Hant` → `zh-CN`、`ar-EG` → `ar`。
 * 无法匹配时返回 null（由调用方决定回退策略）。
 */
export function normalizeLocale(input: string | null | undefined): LocaleCode | null {
  if (!input) return null
  const lower = input.trim().toLowerCase()
  if (!lower) return null
  const exact = SUPPORTED_LOCALES.find((item) => item.code.toLowerCase() === lower)
  if (exact) return exact.code
  const primary = lower.split('-')[0]
  const byPrimary = SUPPORTED_LOCALES.find((item) => item.code.toLowerCase().split('-')[0] === primary)
  return byPrimary ? byPrimary.code : null
}

/** 读取已持久化的语言选择（隐私模式下 localStorage 不可用时返回 null） */
function readStoredLocale(): LocaleCode | null {
  try {
    return normalizeLocale(localStorage.getItem(STORAGE_KEY))
  } catch {
    return null
  }
}

/** 写入语言选择；失败（不可写）时静默忽略，不影响当前会话内的切换 */
function persistLocale(code: LocaleCode): void {
  try {
    localStorage.setItem(STORAGE_KEY, code)
  } catch {
    /* 忽略 */
  }
}

/**
 * 按优先级检测初始语言：
 *   localStorage → navigator.languages / navigator.language → FALLBACK_LOCALE。
 */
export function detectLocale(): LocaleCode {
  const stored = readStoredLocale()
  if (stored) return stored

  const candidates =
    typeof navigator !== 'undefined' && navigator.languages?.length
      ? navigator.languages
      : typeof navigator !== 'undefined' && navigator.language
        ? [navigator.language]
        : []
  for (const candidate of candidates) {
    const hit = normalizeLocale(candidate)
    if (hit) return hit
  }
  return FALLBACK_LOCALE
}

/** 同步 <html lang> 与 <html dir>（ar → rtl，其余 ltr），保证 CSS 与无障碍语义一致 */
export function applyDocumentLocale(code: LocaleCode): void {
  if (typeof document === 'undefined') return
  document.documentElement.setAttribute('lang', code)
  document.documentElement.setAttribute('dir', isRtl(code) ? 'rtl' : 'ltr')
}

/** vue-i18n 实例（legacy: false 组合式 API；globalInjection 让模板可直接用 $t） */
export const i18n = createI18n({
  legacy: false,
  globalInjection: true,
  locale: detectLocale(),
  fallbackLocale: FALLBACK_LOCALE,
  messages,
})

/** 当前语言代码（供格式化工具与网络层读取） */
export function getLocale(): LocaleCode {
  return i18n.global.locale.value as LocaleCode
}

/**
 * 切换语言：更新实例语言 + 持久化 + 同步 <html lang/dir>。
 * 返回最终生效的语言代码（无法识别时回退为 FALLBACK_LOCALE）。
 */
export function setLocale(input: string | null | undefined): LocaleCode {
  const code = normalizeLocale(input) ?? FALLBACK_LOCALE
  i18n.global.locale.value = code
  persistLocale(code)
  applyDocumentLocale(code)
  return code
}

// 首次加载即把当前语言写到 <html> 上，避免出现「界面是 RTL、根节点仍是 ltr」的闪跳
applyDocumentLocale(getLocale())
