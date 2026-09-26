/**
 * 管理后台 · 操作审计接口。
 *
 * 意图（Why）：
 *   后台的写操作（建渠道、改设置、删用户、退款……）由后端审计中间件自动落库，
 *   本模块只负责"按条件把记录查出来"，供审计页展示。
 *
 * 流转（Flow）：
 *   views/admin/AuditView.vue → listAuditLogs(query) → GET /api/admin/audit-logs
 *
 * 扩展（Extend）：
 *   新增筛选条件：在 AuditLogQuery 加字段（后端 handler_audit.go 同步解析）。
 *   路径统一写 "/admin/audit-logs"（不含 /api 前缀，前缀由 client 的 baseURL 拼接）。
 */
import { api } from './client'

/** 一条后台写操作的审计记录（字段与后端 auditLogDTO 对齐） */
export interface AuditLog {
  id: number
  /** 操作管理员 ID（0 表示未识别） */
  admin_id: number
  /** 操作管理员用户名（冗余保存，保证历史可读） */
  admin_username: string
  /** HTTP 方法（POST / PUT / PATCH / DELETE） */
  method: string
  /** 实际请求路径 */
  path: string
  /** 可读动作描述（如"更新渠道"） */
  action: string
  /** 路径参数取值（如 "id=12"） */
  target: string
  /** 请求体摘要（已脱敏、已截断） */
  detail: string
  /** 响应状态码 */
  status_code: number
  /** 处理耗时（毫秒） */
  latency_ms: number
  /** 客户端 IP */
  client_ip: string
  /** User-Agent（已截断） */
  user_agent: string
  /** 记录时间（Unix 秒） */
  created_at: number
}

/** 审计日志查询条件（全部可选） */
export interface AuditLogQuery {
  page?: number
  /** 每页条数（后端主参数名；同时兼容 size） */
  page_size?: number
  admin_id?: number
  /** HTTP 方法（大写） */
  method?: string
  /** 请求路径前缀 */
  path?: string
  status_code?: number
  /** 起始时间：Unix 秒 / RFC3339 / YYYY-MM-DDTHH:mm / YYYY-MM-DD */
  start?: string
  /** 结束时间：同上（闭区间，含该时刻） */
  end?: string
}

/** 审计日志分页响应 */
export interface AuditLogPage {
  items: AuditLog[]
  total: number
  page: number
  page_size: number
}

/** GET /api/admin/audit-logs：查询后台写操作审计日志（分页 + 多条件筛选） */
export function listAuditLogs(query: AuditLogQuery = {}): Promise<AuditLogPage> {
  return api.get<AuditLogPage>('/admin/audit-logs', { ...query })
}
