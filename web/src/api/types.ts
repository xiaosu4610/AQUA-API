/**
 * 接口类型定义：与《前后端接口契约 v1》(docs/06-前后端接口契约.md) 逐字段对齐。
 *
 * 意图（Why）：
 *   把契约固化成 TS 类型，让字段名写错在编译期就暴露，
 *   避免前后端联调时才发现「字段叫 quota 还是 remain_quota」这类低级问题。
 *
 * 流转（Flow）：
 *   client.ts 的请求方法 → 各 api 模块 → 视图层（.vue）
 *   契约变更顺序：先改本文档 → 再改本文件 → 最后改调用处。
 *
 * 扩展（Extend）：
 *   新增接口时，先在此处加请求/响应类型，再到对应 api 模块加函数。
 *   契约里未明确的字段一律声明为可选（?），并在注释里标注「契约未定义」，
 *   便于后续与后端对齐时快速定位。
 */

/* ────────────────────────── 通用 ────────────────────────── */

/** 统一列表响应（契约 1.4：page 从 1 开始，size 上限 100） */
export interface Paged<T> {
  items: T[]
  total: number
  page: number
  size: number
}

/** 统一错误体（契约 1.2，沿用 OpenAI 兼容格式） */
export interface ApiErrorBody {
  error: {
    message: string
    type?: string
    code?: string
  }
}

/** 角色（契约 1.5）：1 = 普通用户，10 = 管理员 */
export const ROLE_USER = 1
export const ROLE_ADMIN = 10

/** 通用启用/禁用状态：1 启用，其余视为停用 */
export const STATUS_ENABLED = 1
export const STATUS_DISABLED = 2

/* ────────────────────────── 公开接口 ────────────────────────── */

/** GET /api/status 响应 */
export interface SiteStatus {
  name: string
  version: string
  registration_enabled: boolean
  site_description: string
  /** 当前对外提供的模型（所有启用渠道声明模型的并集） */
  models: string[]
}

/** 登录 / 注册返回的用户摘要（契约二、三节） */
export interface AuthUser {
  id: number
  username: string
  role: number
  /** 额度（契约未给出单位，前端仅按数值展示，不做货币换算） */
  quota?: number
  email?: string
  status?: number
  used_quota?: number
  created_at?: number
}

/** POST /api/auth/login、/api/auth/register 响应 */
export interface AuthResult {
  session_token: string
  /** Unix 秒；契约未说明 0 的含义，前端不据此做过期判断（由 401 兜底） */
  expires_at: number
  user: AuthUser
}

export interface LoginPayload {
  username: string
  password: string
}

export interface RegisterPayload {
  username: string
  password: string
  /** 可选字段，留空时不发送 */
  email?: string
}

/* ────────────────────────── 访问令牌 ────────────────────────── */

/** 访问令牌对象（契约三节） */
export interface AccessToken {
  id: number
  name: string
  /** 掩码后的密钥，如 sk-abc****wxyz（明文永不返回） */
  masked_key: string
  status: number
  status_text?: string
  /** Unix 秒；0 表示永不过期 */
  expires_at: number
  /** unlimited_quota 为 true 时本字段无意义 */
  remain_quota: number
  unlimited_quota: boolean
  used_quota: number
  /** 空数组表示不限制模型 */
  models: string[]
  created_at: number
  last_used_at?: number
  /** 契约未定义：管理端列表可能带出的归属信息，前端有则展示 */
  user_id?: number
  username?: string
}

/** POST /api/user/tokens、/api/admin/tokens 请求体 */
export interface CreateTokenPayload {
  name: string
  /** 0 表示永不过期 */
  expires_in_days: number
  /** 空数组表示不限制模型 */
  models: string[]
  unlimited_quota: boolean
  remain_quota: number
  /** 仅管理端：为指定用户创建令牌 */
  user_id?: number
}

/** 创建令牌响应：额外返回一次性明文 key */
export interface CreateTokenResult extends AccessToken {
  /** 明文密钥，仅此一次返回，前端必须显著提示保存 */
  key: string
}

/** PATCH /api/user/tokens/{id}、PUT /api/admin/tokens/{id} 请求体（字段可选，按需提交） */
export interface UpdateTokenPayload {
  name?: string
  status?: number
}

/**
 * 管理端更新令牌请求体：在通用字段之外允许调整额度。
 * 契约只写了「更新令牌」，未逐一列出字段，故此处字段名沿用创建令牌的命名（remain_quota /
 * unlimited_quota），联调时若后端字段不同需以此为准调整。
 */
export interface AdminUpdateTokenPayload extends UpdateTokenPayload {
  unlimited_quota?: boolean
  remain_quota?: number
}

/* ────────────────────────── 用量与日志 ────────────────────────── */

/** GET /api/user/usage 的 series 项 */
export interface UsagePoint {
  date: string
  requests: number
  tokens: number
  quota: number
}

/** GET /api/user/usage 的 by_model 项 */
export interface UsageByModel {
  model: string
  requests: number
  tokens: number
}

/** GET /api/user/usage?days=7 响应 */
export interface UsageStats {
  range_days: number
  total_requests: number
  total_tokens: number
  total_quota: number
  series: UsagePoint[]
  by_model: UsageByModel[]
}

/** 调用日志对象（契约四节） */
export interface UsageLog {
  id: number
  user_id: number
  username?: string
  token_name?: string
  channel_id?: number
  channel_name?: string
  model: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  quota: number
  latency_ms: number
  is_stream: boolean
  status_code: number
  error?: string
  created_at: number
}

/* ────────────────────────── 渠道 ────────────────────────── */

/** 渠道对象（响应中只有 masked_key，绝不出现明文） */
export interface Channel {
  id: number
  name: string
  /** 渠道类型编号（契约仅示例了 1，具体枚举待与后端对齐） */
  type: number
  base_url: string
  masked_key: string
  /** 空数组表示「支持全部模型」（M2 过渡约定） */
  models: string[]
  group: string
  priority: number
  weight: number
  status: number
  status_text?: string
  last_test_at?: number
  last_test_ok?: boolean
  created_at?: number
  updated_at?: number
}

/** 新建 / 更新渠道请求体；更新时 api_key 留空表示不修改 */
export interface ChannelPayload {
  name: string
  type: number
  base_url: string
  api_key?: string
  models: string[]
  group: string
  priority: number
  weight: number
  status: number
}

/** POST /api/admin/channels/{id}/test 响应 */
export interface ChannelTestResult {
  ok: boolean
  latency_ms: number
  model: string
  message: string
  status_code: number
}

/* ────────────────────────── 用户 ────────────────────────── */

/** 管理端用户对象（契约数据模型 users 表） */
export interface AdminUser {
  id: number
  username: string
  email?: string
  role: number
  /** 1 启用 / 2 禁用 */
  status: number
  quota: number
  used_quota: number
  created_at?: number
  updated_at?: number
}

export interface CreateUserPayload {
  username: string
  password: string
  email?: string
  role: number
  quota?: number
  status?: number
}

/** 更新用户（含调整额度）：只提交需要变更的字段 */
export interface UpdateUserPayload {
  email?: string
  role?: number
  status?: number
  quota?: number
  /** 契约未定义：是否允许管理员重置密码，待确认后再启用 */
  password?: string
}

/* ────────────────────────── 仪表盘 ────────────────────────── */

/** GET /api/admin/dashboard 响应 */
export interface DashboardStats {
  channels: { total: number; enabled: number; auto_disabled: number }
  users: { total: number; active: number }
  tokens: { total: number; enabled: number }
  today: { requests: number; tokens: number; quota: number; success_rate: number }
  recent_days: { date: string; requests: number; tokens: number }[]
  top_models: { model: string; requests: number }[]
}

/* ────────────────────────── 查询参数 ────────────────────────── */

/** 日志查询条件（用户门户与全站日志共用；status 取值 success/error 为前端约定，待后端确认） */
export interface LogQuery {
  page?: number
  size?: number
  model?: string
  status?: 'success' | 'error' | ''
  /** 管理端专用：按用户名或用户 ID 过滤（字段名待后端确认） */
  user?: string
  /** 管理端专用：按渠道过滤（字段名待后端确认） */
  channel_id?: number | string
}
