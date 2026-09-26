/**
 * 上游厂商（模型名前缀）的解析与配色。
 *
 * 意图（Why）：
 *   站点的模型名统一是「厂商/模型」形式（nvidia/nemotron-…、z-ai/glm-5.3）。
 *   广场的左侧栏目要按厂商分组，卡片要显示厂商头像，两处必须用同一套
 *   解析与配色规则 —— 否则同一个厂商在两处会显示成不同名字/不同颜色。
 *   因此把「取前缀 → 展示名 → 配色」收敛到这里。
 *
 * 为什么用哈希配色而不是维护一张映射表：
 *   上游厂商会不断增加（现在就有 20+ 个），维护颜色表必然滞后；
 *   哈希配色保证「同一个厂商永远同色、不同厂商尽量不同色」，
 *   新增厂商无需改代码。
 *
 * 流转（Flow）：
 *   ModelPlazaBoard（左栏厂商筛选）、ModelCard（头像）、ModelDetailModal → 本文件
 *
 * 扩展（Extend）：
 *   想在侧栏显示中文厂商名时，在 VENDOR_LABELS 里补一条即可（缺失则回退前缀本身）。
 */

/** 独立厂商（模型名不含斜杠时使用）：多为自研或单模型仓库 */
export const VENDOR_OTHER = 'other'

/** 已知厂商的展示名；未登记的前缀会回退为前缀本身 */
const VENDOR_LABELS: Record<string, string> = {
  nvidia: 'NVIDIA',
  'nv-mistralai': 'NV Mistral',
  meta: 'Meta',
  google: 'Google',
  microsoft: 'Microsoft',
  openai: 'OpenAI',
  mistralai: 'Mistral AI',
  'z-ai': 'Z.ai',
  moonshotai: 'Moonshot',
  'deepseek-ai': 'DeepSeek',
  deepseek: 'DeepSeek',
  ibm: 'IBM',
  snowflake: 'Snowflake',
  writer: 'Writer',
  databricks: 'Databricks',
  'ai21labs': 'AI21 Labs',
  aisingapore: 'AI Singapore',
  bigcode: 'BigCode',
  poolside: 'Poolside',
  zyphra: 'Zyphra',
  adept: 'Adept',
  '01-ai': '01.AI',
}

/**
 * 取模型所属厂商：取 "/" 之前的前缀。
 * 为什么不区分大小写：上游偶尔会写成 NVIDIA/xxx，归一后再比对。
 */
export function vendorOf(model: string): string {
  const slash = model.indexOf('/')
  if (slash <= 0) return VENDOR_OTHER
  return model.slice(0, slash).toLowerCase()
}

/** 厂商展示名 */
export function vendorLabel(vendor: string): string {
  if (vendor === VENDOR_OTHER) return '其他'
  return VENDOR_LABELS[vendor] || vendor
}

/**
 * 头像文字：优先用展示名的首字符。
 * 为什么不是首字母大写缩写：厂商名长短不一（Z.ai / AI21 Labs / NVIDIA），
 * 单字符在 36px 的头像里最稳定，不会出现两行或溢出。
 */
export function vendorInitial(vendor: string): string {
  const label = vendorLabel(vendor)
  return label.slice(0, 1).toUpperCase()
}

/** 头像配色候选：统一走「浅色底 + 深色字 + 内描边」，保证在亮色主题下可读 */
const VENDOR_TONES = [
  'bg-cyan-500/10 text-cyan-700 ring-cyan-500/25',
  'bg-indigo-500/10 text-indigo-700 ring-indigo-500/25',
  'bg-emerald-500/10 text-emerald-700 ring-emerald-500/25',
  'bg-amber-500/10 text-amber-700 ring-amber-500/25',
  'bg-rose-500/10 text-rose-700 ring-rose-500/25',
  'bg-violet-500/10 text-violet-700 ring-violet-500/25',
  'bg-sky-500/10 text-sky-700 ring-sky-500/25',
  'bg-teal-500/10 text-teal-700 ring-teal-500/25',
]

/**
 * 厂商配色：对厂商名做简单稳定哈希后取模。
 * 用 djb2 的变体而非 Math.random —— 必须在多次渲染、多个组件间保持一致。
 */
export function vendorTone(vendor: string): string {
  let hash = 5381
  for (let index = 0; index < vendor.length; index += 1) {
    hash = (hash * 33 + vendor.charCodeAt(index)) % 1_000_003
  }
  return VENDOR_TONES[hash % VENDOR_TONES.length]
}
