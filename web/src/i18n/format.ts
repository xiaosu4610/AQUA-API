/**
 * 本土化格式化工具：日期 / 时间 / 数字 / 货币 / 相对时间。
 *
 * 意图（Why）：
 *   同一份数据在不同语言下应呈现为当地习惯的写法（千分位、小数、日期顺序、
 *   阿拉伯语甚至使用阿拉伯-印度数字）。统一走原生 Intl，跟随当前 locale，
 *   避免再引 dayjs/moment 之类的重型依赖（本项目刻意只用标准库能力）。
 *
 * 流转（Flow）：
 *   .vue / utils/format.ts → 本文件 → Intl.* → 已本地化的字符串
 *   语言取自 i18n/index.ts 的 getLocale()，切换语言后再次调用即得到新写法。
 *
 * 扩展（Extend）：
 *   新增格式化需求先加到这里；纯「计算用」的格式化（如毫秒→秒）不要放这里，
 *   那是业务换算而非本地化展示。
 */
import { getLocale } from './index'

/** 空值占位符：统一用长破折号，视觉上比 “-” 更清晰（全站共用） */
export const EMPTY = '—'

/** 可接受的时间输入：Unix 秒（契约约定）或 Date 对象 */
type DateInput = number | Date | null | undefined

/**
 * 把 Unix 秒 / Date 统一成 Date。
 * 0、空与非法值一律返回 null（契约里 0 表示「无」，而非 1970 年）。
 */
function toDate(value: DateInput): Date | null {
  if (value === null || value === undefined) return null
  if (value instanceof Date) return Number.isNaN(value.getTime()) ? null : value
  if (!value) return null
  const date = new Date(value * 1000)
  return Number.isNaN(date.getTime()) ? null : date
}

/** 过滤非法数字（null / undefined / NaN 视为「无」） */
function toNumber(value: number | null | undefined): number | null {
  if (value === null || value === undefined || Number.isNaN(value)) return null
  return value
}

/** 千分位数字（配额、Token 数等需要精确读数的场景） */
export function formatNumber(value: number | null | undefined): string {
  const n = toNumber(value)
  if (n === null) return EMPTY
  return new Intl.NumberFormat(getLocale()).format(n)
}

/**
 * 紧凑数字（如中文的「1.2万」、英文的「12K」）：仅用于仪表盘大卡片，
 * 避免长数字撑破布局。需要精确值时请用 formatNumber。
 */
export function formatCompact(value: number | null | undefined): string {
  const n = toNumber(value)
  if (n === null) return EMPTY
  return new Intl.NumberFormat(getLocale(), { notation: 'compact', maximumFractionDigits: 1 }).format(n)
}

/** 百分比：0.987 → 98.7%（按当前语言的小数点与符号习惯呈现） */
export function formatPercent(ratio: number | null | undefined, digits = 1): string {
  const r = toNumber(ratio)
  if (r === null) return EMPTY
  return new Intl.NumberFormat(getLocale(), {
    style: 'percent',
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  }).format(r)
}

/**
 * 货币金额。
 * 注意：契约里的额度（quota）是平台内部计量单位、并非真实货币，
 * 该函数用于将来出现真实计价场景（如充值订单金额）时按语言格式展示。
 */
export function formatCurrency(value: number | null | undefined, currency = 'CNY'): string {
  const n = toNumber(value)
  if (n === null) return EMPTY
  return new Intl.NumberFormat(getLocale(), { style: 'currency', currency }).format(n)
}

/** Unix 秒 → 本地日期（不含时间） */
export function formatDate(value: DateInput): string {
  const date = toDate(value)
  if (!date) return EMPTY
  return new Intl.DateTimeFormat(getLocale(), { year: 'numeric', month: '2-digit', day: '2-digit' }).format(date)
}

/** Unix 秒 → 本地日期 + 时间 */
export function formatDateTime(value: DateInput): string {
  const date = toDate(value)
  if (!date) return EMPTY
  return new Intl.DateTimeFormat(getLocale(), {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(date)
}

/**
 * Unix 秒 → 相对时间（如中文「3 分钟前」、英文「3 minutes ago」、阿拉伯语按 RTL 习惯）。
 * 为什么需要：日志列表里「刚刚发生的错误」比绝对时间更易扫读。
 */
export function formatRelative(value: DateInput): string {
  const date = toDate(value)
  if (!date) return EMPTY
  const diffSeconds = Math.round((date.getTime() - Date.now()) / 1000)
  const absolute = Math.abs(diffSeconds)
  const formatter = new Intl.RelativeTimeFormat(getLocale(), { numeric: 'auto' })
  if (absolute < 45) return formatter.format(diffSeconds, 'second')
  if (absolute < 3600) return formatter.format(Math.round(diffSeconds / 60), 'minute')
  if (absolute < 86400) return formatter.format(Math.round(diffSeconds / 3600), 'hour')
  if (absolute < 86400 * 30) return formatter.format(Math.round(diffSeconds / 86400), 'day')
  if (absolute < 86400 * 365) return formatter.format(Math.round(diffSeconds / (86400 * 30)), 'month')
  return formatter.format(Math.round(diffSeconds / (86400 * 365)), 'year')
}
