/**
 * 安装向导与站点引导接口。
 *
 * 意图（Why）：
 *   新部署的实例里一个账号都没有，必须有一个"登录之前就能访问"的入口把第一个
 *   管理员建出来。本模块把这两个动作（查状态 / 提交安装）收敛在一处，
 *   视图层只按 status.installed 决定"引导安装"还是"引导登录"。
 *
 * 流转（Flow）：
 *   InstallView.vue → fetchInstallStatus() → GET /api/install/status
 *                   → submitInstall()     → POST /api/install → 成功跳 /admin/login
 *
 * 扩展（Extend）：
 *   向导增加步骤时，在 InstallStatus 里加字段（如 needs_database=true），
 *   并在视图里按字段决定展示哪一步；不要在前端硬编码"装过了没有"的判断。
 */
import { api } from './client'

/** 安装状态 */
export interface InstallStatus {
  /** 是否已完成安装（存在管理员即为已安装） */
  installed: boolean
  /** 站点名称（未安装时为默认名） */
  site_name: string
  /** 后端版本号 */
  version: string
  /** 数据库驱动（仅未安装时返回，如 sqlite） */
  database_driver?: string
  /** 是否为免配置的 SQLite（仅未安装时返回） */
  sqlite_zero_config?: boolean
}

/** 安装请求体 */
export interface InstallPayload {
  /** 管理员用户名；留空时后端使用 admin */
  username?: string
  password: string
  confirm_password?: string
  /** 可选：站点名称 */
  site_name?: string
}

/** 安装结果 */
export interface InstallResult {
  ok: boolean
  admin_username: string
  /** 安装完成后应前往的后台登录地址 */
  admin_login_path: string
}

/** GET /api/install/status：查询安装状态（无需登录） */
export function fetchInstallStatus(): Promise<InstallStatus> {
  return api.get<InstallStatus>('/install/status')
}

/** POST /api/install：提交首次安装（已安装时后端返回 409） */
export function submitInstall(payload: InstallPayload): Promise<InstallResult> {
  return api.post<InstallResult>('/install', payload)
}
