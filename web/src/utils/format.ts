/**
 * 展示格式化工具：数字 / 时间 / 耗时 / 百分比。
 *
 * 意图（Why）：
 *   契约里所有时间字段都是 Unix 秒、所有数量字段都是裸数字，
 *   若各页面各写一套 toLocaleString，会出现「有的带千分位、有的不带」的不一致；
 *   统一在此处理，同时也保证空值统一显示为 “—”（而不是 undefined）。
 *
 * 流转（Flow）：
 *   .vue / LogTable 等组件 → 本文件 → 字符串
 *
 * 扩展（Extend）：
 *   新增格式化需求先加到这里；涉及货币换算时（契约未定义 quota 单位）需先与后端确认单位。
 */

/** 空值占位符：统一用长破折号，视觉上比 “-” 更清晰 */
export const EMPTY = '—'

/** 千分位数字（用于配额、Token 数等需要精确读数的场景） */
export function formatNumber(value: number | null | undefined): string {
  if (value === null || value === undefined || Number.isNaN(value)) return EMPTY
  return value.toLocaleString('zh-CN')
}

/**
 * 紧凑数字（1.2万 / 3.5亿）：仅用于仪表盘大卡片，避免长数字撑破布局。
 * 需要精确值时请用 formatNumber，并把完整值放进 title 属性。
 */
export function formatCompact(value: number | null | undefined): string {
  if (value === null || value === undefined || Number.isNaN(value)) return EMPTY
  if (Math.abs(value) < 10000) return value.toLocaleString('zh-CN')
  return new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 }).format(value)
}

/** 百分比：0.987 → 98.7% */
export function formatPercent(ratio: number | null | undefined, digits = 1): string {
  if (ratio === null || ratio === undefined || Number.isNaN(ratio)) return EMPTY
  return `${(ratio * 100).toFixed(digits)}%`
}

function pad(n: number): string {
  return n < 10 ? `0${n}` : String(n)
}

/** Unix 秒 → 本地时间 YYYY-MM-DD HH:mm:ss（0/空 视为「无」） */
export function formatDateTime(ts: number | null | undefined): string {
  if (!ts) return EMPTY
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes(),
  )}:${pad(d.getSeconds())}`
}

/** Unix 秒 → 本地日期 YYYY-MM-DD */
export function formatDate(ts: number | null | undefined): string {
  if (!ts) return EMPTY
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/**
 * Unix 秒 → 相对时间（如「3 分钟前」）。
 * 为什么需要：日志列表里「刚刚发生的错误」比绝对时间更易扫读。
 */
export function formatRelative(ts: number | null | undefined): string {
  if (!ts) return EMPTY
  const diff = Math.floor(Date.now() / 1000) - ts
  if (diff < 0) return formatDateTime(ts)
  if (diff < 60) return '刚刚'
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  if (diff < 86400 * 30) return `${Math.floor(diff / 86400)} 天前`
  return formatDate(ts)
}

/** 毫秒耗时：>1s 用秒表示，便于快速判断慢请求 */
export function formatLatency(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || Number.isNaN(ms)) return EMPTY
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(2)} s`
}

/**
 * 到期时间展示：0 表示永不过期（契约约定），其中文文案比 “—” 更明确。
 */
export function formatExpiry(ts: number | null | undefined): string {
  if (!ts) return '永不过期'
  return formatDateTime(ts)
}

/**
 * 剩余额度展示：unlimited_quota 为 true 时忽略 remain_quota（契约明确该字段无意义）。
 */
export function formatQuota(remain: number | null | undefined, unlimited?: boolean): string {
  if (unlimited) return '不限额度'
  if (remain === null || remain === undefined) return EMPTY
  // 负数在契约里未被定义为「无限」，这里如实展示，避免误读为可用
  return formatNumber(remain)
}

/** 解析输入框里的模型列表：支持中英文逗号、空格、换行分隔 */
export function parseModelList(input: string): string[] {
  return input
    .split(/[,，\s]+/)
    .map((item) => item.trim())
    .filter(Boolean)
}

/** 模型数组 → 输入框文本（parseModelList 的反向操作） */
export function joinModelList(models: string[] | undefined | null): string {
  return Array.isArray(models) ? models.join(', ') : ''
}
