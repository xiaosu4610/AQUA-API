/**
 * 运维监控与备份接口（需管理员权限）。
 *
 * 意图（Why）：
 *   后台运维页需要回答三个问题：「现在健不健康」「怎么留一份数据」「这份备份能不能用」。
 *   本模块把这三个诉求对应的接口收在一处，供 MaintenanceView 使用；
 *   路径不含 /api 前缀（由 client.ts 的 baseURL 统一拼接）。
 *
 * 流转（Flow）：
 *   views/admin/MaintenanceView.vue → 本文件 → /api/admin/maintenance/*
 *
 * 扩展（Extend）：
 *   新增运维接口：在此加函数，并在 MaintenanceView 中调用。
 *   注意备份下载：它需要携带会话令牌的 Authorization 头，浏览器无法直接打开链接下载，
 *   因此这里用 fetch + client 导出的 getSessionToken 显式发起（复用同一套令牌读写）。
 */
import { api, getSessionToken } from './client'

/** 数据库驱动与体积 */
export interface MaintenanceDatabaseInfo {
  /** 驱动名（sqlite 等） */
  driver: string
  /** 数据体积（字节） */
  size_bytes: number
  /** 是否成功取到体积（非 SQLite 或取不到时为 false） */
  size_available: boolean
}

/** 数据目录所在分区的磁盘水位 */
export interface MaintenanceDiskInfo {
  /** 是否可用（Windows 开发环境为 false） */
  available: boolean
  total_bytes: number
  free_bytes: number
  used_bytes: number
  /** 使用比例（0~1） */
  used_ratio: number
}

/** 单张表的行数 */
export interface MaintenanceTableRow {
  name: string
  rows: number
}

/** 单个时间窗口内的调用健康度 */
export interface MaintenanceUsageWindow {
  requests: number
  failures: number
  /** 失败率（0~1） */
  failure_rate: number
  avg_latency_ms: number
}

/** 调用健康度（近 24 小时 / 近 7 天） */
export interface MaintenanceUsage {
  last_24h: MaintenanceUsageWindow
  last_7d: MaintenanceUsageWindow
}

/** 运维概览响应 */
export interface MaintenanceOverview {
  version: string
  build_time: string
  git_commit: string
  started_at: number
  uptime_seconds: number
  database: MaintenanceDatabaseInfo
  disk: MaintenanceDiskInfo
  tables: MaintenanceTableRow[]
  usage: MaintenanceUsage
}

/** 备份与当前库的单表对比行 */
export interface MaintenanceCompareRow {
  name: string
  /** 备份中是否存在该表（false 表示缺失，此时 backup_rows 无意义） */
  in_backup: boolean
  backup_rows: number
  current_rows: number
}

/** 备份只读校验结果 */
export interface MaintenanceInspectResult {
  valid: boolean
  /** 备份的 schema 版本 */
  schema_version: number
  /** 备份是否含 schema_migrations 表 */
  schema_table_present: boolean
  /** 当前运行库的 schema 版本 */
  current_schema_version: number
  tables: MaintenanceCompareRow[]
  /** 人工恢复步骤（后端刻意不提供在线恢复） */
  restore_steps: string[]
  /** 为什么不提供在线恢复的说明 */
  note: string
}

/** GET /api/admin/maintenance/overview：运维概览 */
export function fetchMaintenanceOverview(): Promise<MaintenanceOverview> {
  return api.get<MaintenanceOverview>('/admin/maintenance/overview')
}

/**
 * POST /api/admin/maintenance/backup/inspect：只读校验上传的备份文件。
 *
 * 用 FormData 上传；不手动设置 Content-Type，交给浏览器自动带上 multipart 边界。
 */
export function inspectMaintenanceBackup(file: File): Promise<MaintenanceInspectResult> {
  const form = new FormData()
  form.append('backup', file)
  return api.post<MaintenanceInspectResult>('/admin/maintenance/backup/inspect', form)
}

/** 从 Content-Disposition 中解析下载文件名（拿不到时返回空串） */
function parseFilename(disposition: string | null): string {
  if (!disposition) return ''
  const match = /filename="?([^";]+)"?/i.exec(disposition)
  return match ? match[1].trim() : ''
}

/**
 * GET /api/admin/maintenance/backup：下载数据库一致性快照。
 *
 * 为什么不用 api.get：下载需要拿到二进制体并触发浏览器保存，
 * 而 api 客户端默认把响应按 JSON 解析、也不暴露 responseType；
 * 这里改用 fetch 显式请求，并复用 client 的令牌读取函数附加鉴权头。
 */
export async function downloadMaintenanceBackup(): Promise<void> {
  const token = getSessionToken()
  const headers: Record<string, string> = {}
  if (token) headers.Authorization = `Bearer ${token}`
  const base = import.meta.env.VITE_API_BASE || '/api'

  const response = await fetch(`${base}/admin/maintenance/backup`, { headers })
  if (!response.ok) {
    // 后端错误体统一为 { error: { message } }，尽量取出可读提示
    let message = `下载失败（HTTP ${response.status}）`
    try {
      const data = await response.json()
      if (data?.error?.message) message = data.error.message
    } catch {
      /* 非 JSON 错误体，沿用兜底提示 */
    }
    throw new Error(message)
  }

  const blob = await response.blob()
  const filename = parseFilename(response.headers.get('Content-Disposition')) || 'aqua-backup.db'
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.appendChild(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
}
