/**
 * 站点公告接口（公开端读取 + 后台维护）。
 *
 * 意图（Why）：
 *   1) 公开端（AnnouncementBanner）只需"当前可见"的公告列表，且无需登录；
 *   2) 后台（AnnouncementsView）需要完整 CRUD，并可见停用/过期公告。
 *   两类调用差异较大，统一收敛在本文件，视图层不关心路径与字段转换。
 *
 * 流转（Flow）：
 *   AnnouncementBanner → fetchPublicAnnouncements() → GET /api/announcements
 *   AnnouncementsView → listAnnouncements / createAnnouncement / updateAnnouncement / deleteAnnouncement
 *                     → /api/admin/announcements[...]
 *
 * 扩展（Extend）：
 *   新增字段时同步下方类型定义与后端 DTO；新增接口时在 api/admin.ts 之外
 *   单独放这里（公告是独立域，避免 admin.ts 继续膨胀）。
 *   注意：路径不含 /api 前缀，前缀由 client.ts 的 baseURL 统一拼接。
 */
import { api } from './client'
import type { Paged } from './types'

/** 公告展示语气（决定前台配色与后台徽标） */
export type AnnouncementLevel = 'info' | 'success' | 'warning' | 'danger'

/** 后台公告视图（含启用状态与更新时间） */
export interface Announcement {
  id: number
  title: string
  content: string
  level: AnnouncementLevel
  /** 语气的中文描述（由后端下发，避免前端各写一份映射） */
  level_text: string
  pinned: boolean
  enabled: boolean
  /** 开始展示时间（Unix 秒，0 = 立即发布） */
  publish_at: number
  /** 停止展示时间（Unix 秒，0 = 永不过期） */
  expire_at: number
  created_at: number
  updated_at: number
}

/** 公开端公告视图（仅当前可见的公告，字段更少） */
export interface PublicAnnouncement {
  id: number
  title: string
  content: string
  level: AnnouncementLevel
  pinned: boolean
  publish_at: number
  expire_at: number
  created_at: number
}

/** 新建/更新公告的请求体（更新时只需提供要改的字段） */
export interface AnnouncementPayload {
  title?: string
  content?: string
  level?: AnnouncementLevel
  pinned?: boolean
  enabled?: boolean
  publish_at?: number
  expire_at?: number
}

/** 后台列表筛选条件 */
export interface AnnouncementQuery {
  page?: number
  size?: number
  keyword?: string
  /** true 只看已发布 / false 只看草稿 / 不传为全部 */
  enabled?: boolean
  level?: AnnouncementLevel
}

/**
 * GET /api/announcements：读取当前可见公告（公开接口，无需登录）。
 *
 * 后端已按时间窗口与启用状态过滤并排序，前端无需再做筛选。
 */
export function fetchPublicAnnouncements(): Promise<{ items: PublicAnnouncement[] }> {
  return api.get<{ items: PublicAnnouncement[] }>('/announcements')
}

/** GET /api/admin/announcements：后台分页列表（含停用与已过期） */
export function listAnnouncements(query: AnnouncementQuery = {}): Promise<Paged<Announcement>> {
  return api.get<Paged<Announcement>>('/admin/announcements', { ...query })
}

/** POST /api/admin/announcements：新建公告 */
export function createAnnouncement(payload: AnnouncementPayload): Promise<Announcement> {
  return api.post<Announcement>('/admin/announcements', payload)
}

/** PUT /api/admin/announcements/{id}：更新公告（只需提交要修改的字段） */
export function updateAnnouncement(id: number, payload: AnnouncementPayload): Promise<Announcement> {
  return api.put<Announcement>(`/admin/announcements/${id}`, payload)
}

/** DELETE /api/admin/announcements/{id}：删除公告 */
export function deleteAnnouncement(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/announcements/${id}`)
}
