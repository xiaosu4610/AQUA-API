/**
 * 国际化（i18n）基础设施：React Context 版（替代旧 vue-i18n）。
 *
 * 意图（Why）：
 *   React 生态没有与 vue-i18n 完全对等的轻量实现（next-intl 偏重、且需要服务端配置），
 *   而本前端是纯客户端渲染（数据全走 Go API），因此用「模块级状态 + React Context」
 *   实现等价能力：词条合并、语言检测、持久化与 <html lang/dir> 同步。
 *
 * 流转（Flow）：
 *   providers.tsx 挂载 I18nProvider → useI18n()/t() 生效
 *   语言优先级：localStorage → 浏览器 Accept-Language（只匹配语言主码）→ 回退 zh-CN
 *   setLocale(lang) → 更新状态 → 写 localStorage → 同步 <html lang> 与 <html dir>
 *   api/client.ts 通过 getLocale() 取当前语言，写入请求头 Accept-Language。
 *
 * 扩展（Extend）：
 *   新增语言：在 locales/<code>/ 建 common/components/portal/admin 四个词条文件，
 *   在 SUPPORTED_LOCALES 与 messages 各加一项即可（键集合由 TS 类型约束与中文一致）。
 */
'use client'

import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'

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
const STORAGE_KEY = 'ltzy.locale'

/** 兜底语言：所有匹配失败时使用 */
export const FALLBACK_LOCALE: LocaleCode = 'zh-CN'

/** 词条合并表：以简体中文的结构为基准，其余语言缺键会在 type-check 阶段暴露 */
const messages = {
  'zh-CN': zhCN,
  en,
  fr,
  ru,
  es,
  ar,
} satisfies Record<LocaleCode, typeof zhCN>

/** 词条对象深层取值：common.action.save → '保存'；未命中返回键名（便于发现漏词条） */
type Messages = typeof zhCN
type Dictionary = { [key: string]: unknown }

function resolvePath(obj: Dictionary, path: string): string | undefined {
  const value = path.split('.').reduce<unknown>((acc, key) => {
    if (acc && typeof acc === 'object') return (acc as Dictionary)[key]
    return undefined
  }, obj)
  return typeof value === 'string' ? value : undefined
}

/* ── 模块级状态（非组件环境也可读取，供 api 层取语言）───────────── */
let currentLocale: LocaleCode = FALLBACK_LOCALE

/** 判断某语言是否为从右向左（RTL）书写 */
export function isRtl(code: LocaleCode): boolean {
  return code === 'ar'
}

/** 把任意语言标签规整为受支持的语言代码（精确 → 语言主码 → null） */
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
    return normalizeLocale(window.localStorage.getItem(STORAGE_KEY))
  } catch {
    return null
  }
}

/** 写入语言选择；失败时静默忽略 */
function persistLocale(code: LocaleCode): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, code)
  } catch {
    /* 忽略 */
  }
}

/** 按优先级检测初始语言：localStorage → navigator.languages → FALLBACK_LOCALE */
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
function applyDocumentLocale(code: LocaleCode): void {
  if (typeof document === 'undefined') return
  document.documentElement.setAttribute('lang', code)
  document.documentElement.setAttribute('dir', isRtl(code) ? 'rtl' : 'ltr')
}

/** 当前语言代码（供格式化工具与网络层读取） */
export function getLocale(): LocaleCode {
  return currentLocale
}

/**
 * 切换语言：更新模块状态 + 持久化 + 同步 <html lang/dir>。
 * 返回最终生效的语言代码（无法识别时回退为 FALLBACK_LOCALE）。
 */
export function setLocale(input: string | null | undefined): LocaleCode {
  const code = normalizeLocale(input) ?? FALLBACK_LOCALE
  currentLocale = code
  persistLocale(code)
  applyDocumentLocale(code)
  return code
}

/* ── React Context 层 ───────────────────────────────────────── */

interface I18nContextValue {
  /** 当前语言代码 */
  locale: LocaleCode
  /** 翻译：t('common.action.save')；支持 {var} 插值（第二个参数传 { name: 'A' } 即可） */
  t: (key: string, vars?: Record<string, string | number>) => string
  /** 语言切换 */
  setLocale: (input: string | null | undefined) => void
}

const I18nContext = createContext<I18nContextValue | null>(null)

/** 全局 Provider：应用根节点挂载一次 */
export function I18nProvider({ children }: { children: ReactNode }) {
  // 首帧固定为模块级或兜底语言：SSG（Node 无 navigator）与浏览器若要在这里
  // 读 navigator.languages 会得到不同结果，进而触发 React hydration 不一致
  // （#412/#418）。语言检测延迟到挂载后执行——首帧保持一致，再静默切换到用户语言。
  const [locale, setLocaleState] = useState<LocaleCode>(() => {
    currentLocale = FALLBACK_LOCALE
    return FALLBACK_LOCALE
  })

  // 挂载后：同步 <html lang/dir>（SSG 阶段没有 document），并检测用户语言
  useEffect(() => {
    const detected = detectLocale()
    if (detected !== currentLocale) {
      currentLocale = detected
      setLocaleState(detected)
    }
    applyDocumentLocale(detected)
  }, [])

  const changeLocale = useCallback((input: string | null | undefined) => {
    const code = setLocale(input)
    setLocaleState(code)
  }, [])

  const t = useCallback(
    (key: string, vars?: Record<string, string | number>): string => {
      const dict = messages[locale]
      const template = resolvePath(dict as unknown as Dictionary, key) ?? key
      return interpolate(template, vars)
    },
    [locale],
  )

  const value = useMemo(
    () => ({ locale, t, setLocale: changeLocale }),
    [locale, t, changeLocale],
  )

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>
}

/** 在组件中读取翻译与语言（必须在 I18nProvider 内使用） */
export function useI18n(): I18nContextValue {
  const ctx = useContext(I18nContext)
  if (!ctx) throw new Error('useI18n 必须在 I18nProvider 内使用')
  return ctx
}

/** 模板插值：把 {name} 替换为 vars[name]，缺失的占位符原样保留（便于发现漏传） */
function interpolate(template: string, vars?: Record<string, string | number>): string {
  if (!vars) return template
  return template.replace(/\{(\w+)\}/g, (_, name: string) =>
    vars[name] !== undefined ? String(vars[name]) : `{${name}}`,
  )
}

/**
 * 非组件环境的翻译单例（如工具函数、事件回调里格式化文案）。
 *
 * 组件内请优先用 useI18n().t —— 它能随语言切换触发重渲染；
 * 本函数读取模块级 currentLocale，切换语言后调用方若未重渲染不会自动更新，
 * 故仅用于「每次调用即时取当前语言」的短文案（如时长格式化）。
 */
export function translate(
  key: string,
  vars?: Record<string, string | number>,
  locale: LocaleCode = currentLocale,
): string {
  const dict = messages[locale]
  return interpolate(resolvePath(dict as unknown as Dictionary, key) ?? key, vars)
}