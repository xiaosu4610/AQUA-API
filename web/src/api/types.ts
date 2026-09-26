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
  /** 注册是否必须填写邮箱验证码（由后台开关控制） */
  email_code_required: boolean
  /** 邮件发送通道是否已就绪；false 时即使开启校验也收不到验证码 */
  email_service_ready: boolean
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
  /** 邮箱：站点开启"注册必须邮箱验证码"时为必填 */
  email?: string
  /** 邮箱验证码，与 email 成对出现 */
  code?: string
}

/** POST /api/auth/email-code 请求体 */
export interface EmailCodePayload {
  email: string
}

/** POST /api/auth/email-code 响应 */
export interface EmailCodeResult {
  ok: boolean
  /** 验证码有效期（秒） */
  expires_in: number
  /** 重发冷却时间（秒），前端据此启动倒计时 */
  cooldown: number
  /** 后端给出的提示文案（如"验证码已发送，请查收邮件"） */
  message: string
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
  /**
   * 凭据池调度策略标识（sequential / round_robin / weighted_random /
   * least_recent / least_in_flight）。后端保证恒为合法值。
   */
  key_strategy: string
  status: number
  status_text?: string
  last_test_at?: number
  last_test_ok?: boolean
  created_at?: number
  updated_at?: number
  /** 密钥池概览；total 为 0 表示该渠道未配置密钥池（走单密钥模式） */
  key_pool?: KeyPoolSummary
}

/** 渠道密钥池概览 */
export interface KeyPoolSummary {
  total: number
  enabled: number
  disabled: number
  auto_removed: number
}

/** 密钥池内的单把密钥（只含掩码，明文永不返回） */
export interface ChannelKey {
  id: number
  label: string
  masked_key: string
  status: number
  status_text: string
  fail_count: number
  last_used_at: number
  last_error: string
  created_at: number
  /** 权重（weighted_random 使用；0 表示不参与加权，全为 0 时退化为等概率） */
  weight: number
  /** 优先级（sequential 使用；数值越大越优先） */
  priority: number
  /** 每分钟请求上限（0 = 不限速） */
  rpm_limit: number
  /** 当前在途请求数（least_in_flight 使用）；只读展示 */
  in_flight: number
  /** 冷却截止时间的 Unix 秒（0 = 无冷却）；只读，由前端换算剩余时间 */
  cooldown_until: number
}

/** PUT /api/admin/keys/{keyId} 请求体：状态与调度参数均可选，仅提交变更项 */
export interface UpdateChannelKeyPayload {
  status?: number
  /**
   * 调度参数（weight / priority / rpm_limit）。
   *
   * 三项必须同时提供：后端底层是一次性整组覆盖，缺项会被写成 0。
   */
  weight?: number
  priority?: number
  rpm_limit?: number
}

/** 一种凭据调度策略（GET /api/admin/key-strategies 的 items 项） */
export interface KeyStrategyOption {
  /** 策略标识（稳定不变，写入渠道记录） */
  key: string
  /** 中文名（后端下发，前端不硬编码） */
  label: string
  /** 一句话说明（后端下发，用于下拉下方的帮助文案） */
  description: string
}

/** GET /api/admin/key-strategies 响应 */
export interface KeyStrategyCatalog {
  items: KeyStrategyOption[]
  total: number
}

/** 密钥状态：与后端 model.ChannelKeyStatus 一一对应 */
export const KEY_STATUS_ENABLED = 1
export const KEY_STATUS_DISABLED = 2
export const KEY_STATUS_AUTO_REMOVED = 3

/** POST /api/admin/fetch-models 请求体 */
export interface FetchModelsPayload {
  /** > 0 时用该渠道已保存的地址与密钥；否则使用下面的 base_url / api_key */
  channel_id?: number
  base_url?: string
  api_key?: string
}

/** POST /api/admin/fetch-models 响应 */
export interface FetchModelsResult {
  models: string[]
  count: number
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
  /**
   * 凭据池调度策略标识。留空表示「不修改」（更新时后端保留原策略）。
   * 取值来自 GET /api/admin/key-strategies。
   */
  key_strategy?: string
  /**
   * 批量密钥文本：每行一把，行内可用空格或逗号附加备注。
   *
   * 留空表示"不修改密钥池"（避免只改个名字就把几百把密钥清空）。
   */
  keys_text?: string
}

/** POST /api/admin/channels/{id}/test 响应 */
export interface ChannelTestResult {
  ok: boolean
  latency_ms: number
  model: string
  message: string
  status_code: number
}

/* ────────────────────────── 上游渠道类型目录 ────────────────────────── */

/**
 * 上游渠道类型的一个额外参数（供新建/编辑渠道时做条件表单）。
 *
 * ExtraField 只属于少数类型（如 Azure 的部署名、Vertex 的项目/区域），
 * 因此由后端声明、前端按选中类型动态展开，而不是平铺给所有渠道。
 */
export interface ChannelTypeField {
  key: string
  label: string
  placeholder: string
  help: string
  default: string
  /** 必填：该类型没有它必然连不通 */
  required: boolean
  /** 敏感值（如 AK/SK、服务账号 JSON），应按密码框渲染并提示走环境变量 */
  secret: boolean
}

/** 一种上游渠道类型（GET /api/admin/channel-types 的 items 项） */
export interface ChannelType {
  /** 类型标识（稳定不变，写入渠道记录） */
  key: string
  label: string
  /** 所属大类标识（用于分组下拉） */
  category: string
  /** 大类中文名（后端下发，避免前端硬编码中英映射） */
  category_label: string
  protocol: string
  /** 鉴权方式的稳定枚举值 */
  auth_mode: string
  /** 鉴权方式的中文说明（只读提示） */
  auth_label: string
  /** 鉴权所需的自定义请求头名（部分模式使用） */
  auth_header: string
  /** 默认上游地址；为空表示必须由使用者填写 */
  default_base_url: string
  /** 是否允许使用者覆盖默认地址 */
  base_url_editable: boolean
  /** 必须携带的固定请求头（如某些版本的版本头） */
  default_headers?: Record<string, string>
  /** 能力位的中文名列表（如 ["对话","流式","工具调用"]） */
  capabilities: string[]
  extra_fields: ChannelTypeField[]
  supports_model_list: boolean
  /** false 表示适配器尚未实现：显示"即将支持"且不允许选中 */
  available: boolean
  notes: string
}

/** 渠道类型大类汇总（供分组渲染与计数） */
export interface ChannelTypeCategory {
  key: string
  label: string
  count: number
}

/** GET /api/admin/channel-types 响应 */
export interface ChannelTypesResponse {
  items: ChannelType[]
  categories: ChannelTypeCategory[]
  total: number
  /** 适配器已实现、可直接接入的类型数量 */
  available_count: number
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

/* ────────────────────────── 系统设置 ────────────────────────── */

/** GET /api/admin/settings 响应 */
export interface SiteSettings {
  site_name: string
  site_description: string
  /** 是否开放自助注册 */
  registration_enabled: boolean
  /** 注册是否必须通过邮箱验证码校验 */
  registration_require_email_code: boolean
  /** 新用户默认额度（-1 表示不限） */
  default_user_quota: number
  default_group: string
  /** 邮件通道是否已就绪（SMTP 配置完整）；只读，由服务端环境变量决定 */
  email_service_ready: boolean
  /** 发件地址；未配置时为空字符串。只读 */
  email_from: string
  /** 充值 / 支付运营参数（非密钥，可修改并即时生效） */
  payment: PaymentSettings
  /**
   * 支付通道清单：由后端注册表下发的「字段描述 + 当前值 + 密钥就绪状态」。
   *
   * 前端据此做触发式渲染（勾选哪个通道才展开它的字段），
   * 因此后端新增支付通道时前端无需改动。
   */
  payment_channels: PaymentChannel[]
  /** 各支付通道的密钥是否已通过环境变量就绪；只读 */
  payment_secrets: PaymentSecretStatus
}

/** PUT /api/admin/settings 请求体：只提交需要变更的字段 */
export type UpdateSiteSettingsPayload = Partial<{
  site_name: string
  site_description: string
  registration_enabled: boolean
  registration_require_email_code: boolean
  default_user_quota: number
  default_group: string
  payment: PaymentSettings
}>

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

/* ────────────────────────── 模型计价规则 ────────────────────────── */

/**
 * 模型计价规则（GET /api/admin/prices）。
 *
 * 价格口径：字段表示「每 100 万 token 消耗的站点额度」，
 * per_call_price 表示「每调用一次消耗的额度」（异步任务/图像等按次计费的能力）。
 * 三处必须与后端 model.ModelPrice 保持一致：改动时同步本文件与价格页文案。
 */
export interface ModelPrice {
  id: number
  /** 模型名或通配模式（"gpt-4*" 前缀、"*" 全局） */
  model: string
  prompt_price: number
  completion_price: number
  per_call_price: number
  group: string
  enabled: boolean
  remark: string
  created_at: number
  updated_at: number
}

/** 新增/更新计价规则请求体 */
export interface ModelPricePayload {
  model: string
  prompt_price?: number
  completion_price?: number
  per_call_price?: number
  group?: string
  enabled?: boolean
  remark?: string
}

/** GET /api/admin/prices/quote 响应：费用试算结果 */
export interface QuotePreview {
  model: string
  prompt_tokens: number
  completion_tokens: number
  quota: number
  priced: boolean
}

/* ────────────────────────── 模型分组 ────────────────────────── */

/**
 * 模型分组（GET /api/admin/groups）。
 *
 * ratio 是计费倍率的百分比整数：100 = 1.0 倍、150 = 1.5 倍。
 * 实际扣费 = 基础额度 × ratio / 100（向下取整）。
 */
export interface ModelGroup {
  id: number
  name: string
  display_name: string
  /** 展示名（未设置 display_name 时等于 name） */
  label: string
  ratio: number
  description: string
  enabled: boolean
  /** 引用统计：界面上据此提示「该分组正在被使用，删除会影响 N 个渠道」 */
  channel_count: number
  price_count: number
  created_at: number
  updated_at: number
}

export interface ModelGroupPayload {
  name: string
  display_name?: string
  ratio?: number
  description?: string
  enabled?: boolean
}

/* ────────────────────────── 模型广场（公开） ────────────────────────── */

/** 模型卡片上的某分组价格 */
export interface PlazaPrice {
  group: string
  prompt_price: number
  completion_price: number
  per_call_price: number
  /** 该分组的计费倍率（百分比） */
  ratio: number
}

/** 模型广场里的一张模型卡片 */
export interface PlazaModel {
  model: string
  /** 当前可用的分组（来自渠道声明） */
  groups: string[]
  /** 是否至少有一个启用渠道支持它 */
  available: boolean
  /** 支持该模型的启用渠道数量 */
  channel_count: number
  prices: PlazaPrice[]
}

/** 模型广场的分组视图 */
export interface PlazaGroup {
  name: string
  label: string
  ratio: number
  description: string
  model_count: number
}

/** GET /api/models 响应 */
export interface ModelPlaza {
  items: PlazaModel[]
  groups: PlazaGroup[]
  total: number
}

/* ────────────────────────── 异步任务 ────────────────────────── */

/** 任务类别：与后端 model.TaskKind 一一对应 */
export const TASK_KIND_IMAGE = 'image'
export const TASK_KIND_VIDEO = 'video'
export const TASK_KIND_MUSIC = 'music'

/** 任务状态：与后端 model.TaskStatus 一一对应（3/4/5 为终态） */
export const TASK_STATUS_QUEUED = 1
export const TASK_STATUS_RUNNING = 2
export const TASK_STATUS_SUCCEEDED = 3
export const TASK_STATUS_FAILED = 4
export const TASK_STATUS_CANCELED = 5

/** 异步任务对象 */
export interface Task {
  task_ref: string
  kind: string
  kind_text: string
  provider: string
  model: string
  prompt: string
  /** 原始请求参数（JSON 字符串，原样交给上游适配器） */
  params: string
  status: number
  status_text: string
  progress: number
  result_url: string
  result_data: string
  error: string
  quota: number
  user_id: number
  channel_id: number
  created_at: number
  updated_at: number
  finished_at: number
}

/** 已注册的上游任务适配器（GET /api/admin/task-providers） */
export interface TaskProvider {
  name: string
  kinds: { kind: string; text: string }[]
}

/** 查询任务列表的参数 */
export interface TaskQuery {
  page?: number
  size?: number
  kind?: string
  status?: number
}

/* ────────────────────────── 充值 / 支付 ────────────────────────── */

/** 订单状态：与后端 model.PaymentStatus 一一对应 */
export const ORDER_STATUS_PENDING = 1
export const ORDER_STATUS_PAID = 2
export const ORDER_STATUS_CLOSED = 3
export const ORDER_STATUS_REFUNDED = 4

/** 充值订单 */
export interface PaymentOrder {
  trade_no: string
  amount_cents: number
  /** 金额的可读形式（如 "10.50"），由后端格式化，避免前端浮点换算 */
  amount_text: string
  currency: string
  quota: number
  method: string
  sub_method: string
  status: number
  status_text: string
  /** 第三方收银台地址；人工确认通道为空 */
  pay_url: string
  remark: string
  user_id: number
  credited: boolean
  created_at: number
  paid_at: number
  expires_at: number
}

/** 下单请求体：只传金额与通道，额度由服务端按当前汇率计算 */
export interface CreateOrderPayload {
  amount_cents: number
  method: string
  sub_method?: string
  remark?: string
}

/** 公开的充值参数（GET /api/payment/public） */
export interface PublicPaymentInfo {
  enabled: boolean
  methods: { name: string; label: string; ready: boolean }[]
  exchange_rate: number
  currency: string
  min_cents: number
  /** 0 表示不限 */
  max_cents: number
}

/** 管理端支付运营参数（非密钥） */
export interface PaymentSettings {
  enabled: boolean
  /** 已启用的支付通道标识集合（即通道清单里被勾选的通道 key） */
  methods: string[]
  exchange_rate: number
  currency: string
  min_cents: number
  max_cents: number
  order_ttl_minutes: number
  notify_base: string
  /**
   * 各通道的通道级参数，键为 `setting_key`（形如 "epay.gateway"）。
   *
   * 这是「任意多种支付通道」的承载：通道与字段由后端注册表声明，
   * 前端按字段描述渲染表单并原样回传，后端不再为每个通道定义专用字段。
   */
  params: Record<string, string>
  /**
   * 以下四个是旧版专用字段，仅为兼容老前端保留；新前端一律改用 params。
   * @deprecated 请使用 params（键如 "epay.gateway" / "stripe.note"）
   */
  epay_gateway: string
  epay_pid: string
  epay_types: string[]
  stripe_note: string
}

/** 各支付通道的密钥是否已通过环境变量就绪（只读，不回传密钥本身） */
export type PaymentSecretStatus = Record<string, boolean>

/** 支付通道字段的控件类型（与后端 payment.FieldKind 一一对应） */
export type PaymentFieldKind = 'text' | 'number' | 'list' | 'select' | 'switch'

/**
 * 支付通道字段的取值来源（与后端 payment.FieldSource 一一对应）。
 * - setting：值存于设置表，可在后台编辑，随 payment.params 提交；
 * - secret：密钥，只从环境变量读取，后台仅展示"是否就绪"，永不回显内容。
 */
export type PaymentFieldSource = 'setting' | 'secret'

/** select 类型字段的候选项 */
export interface PaymentChannelFieldOption {
  value: string
  label: string
}

/**
 * 支付通道的一个配置字段（由后端下发，前端据 kind 触发式渲染控件）。
 *
 * 为什么字段由后端描述：字段名、是否必填、密钥对应哪个环境变量，
 * 都是"该通道需要什么"的知识，放前端会出现"后端加了字段、前端忘了加输入框"的漏配。
 */
export interface PaymentChannelField {
  key: string
  label: string
  kind: PaymentFieldKind | string
  source: PaymentFieldSource | string
  /** source=secret 时对应的环境变量名（如 AQUA_EPAY_KEY） */
  env_var: string
  placeholder: string
  help: string
  default: string
  required: boolean
  /** 仅 select 类型有候选项 */
  options?: PaymentChannelFieldOption[]
  /** setting 字段的当前值（secret 字段恒为空，值不出服务端） */
  value: string
  /** setting：值非空即为就绪；secret：环境变量已注入即为就绪 */
  ready: boolean
  /** 提交 payment.params 时使用的键（形如 "epay.gateway"） */
  setting_key: string
}

/** 一个支付通道（含字段定义、密钥就绪状态与回调路径） */
export interface PaymentChannel {
  key: string
  label: string
  description: string
  /** false 表示适配器尚未实现：界面显示"即将支持"且不允许勾选 */
  available: boolean
  /** 该通道当前是否启用 */
  enabled: boolean
  /** 回调地址的路径部分；后台拼上站点地址展示，供站长复制到支付平台 */
  notify_path: string
  /** 尚未注入的环境变量名列表；非空表示密钥未就绪，启用会被后端拒绝 */
  missing_env: string[]
  fields: PaymentChannelField[]
}

/** 订单查询参数 */
export interface OrderQuery {
  page?: number
  size?: number
  status?: number
  method?: string
  user_id?: number
}

/* ────────────────────────── OAuth 提供方 ────────────────────────── */

/**
 * OAuth 提供方配置（订阅账号池刷新令牌时使用）。
 *
 * 注意：client_secret 只写入不读出（后端只返回掩码），
 * 因此编辑表单留空表示「不修改」。
 */
export interface OAuthProvider {
  id: number
  name: string
  token_url: string
  client_id: string
  masked_client_secret: string
  /** 授权范围，空格分隔（OAuth2 约定的 scope 字符串形式） */
  scope: string
  remark: string
  enabled: boolean
  created_at: number
  updated_at: number
}

export interface OAuthProviderPayload {
  name: string
  token_url: string
  client_id: string
  /** 留空表示不修改（后端只返回掩码，不回传明文） */
  client_secret?: string
  scope?: string
  remark?: string
  enabled?: boolean
}

/* ────────────────────────── 兑换码（后台） ────────────────────────── */

/** 兑换码状态：与后端 model.RedeemStatus 一一对应（1 未使用 / 2 已使用 / 3 已作废） */
export const REDEEM_STATUS_UNUSED = 1
export const REDEEM_STATUS_USED = 2
export const REDEEM_STATUS_VOID = 3

/**
 * 兑换码对象（GET /api/admin/redeem-codes 的 items 项）。
 *
 * 与令牌不同，兑换码明文对后台是可见的——管理员的下一步操作必然是"把码导出分发"，
 * 因此后端返回明文 code（这也是它必须靠状态与有效期来约束使用的原因）。
 */
export interface RedeemCode {
  id: number
  /** 兑换码明文（大写字母数字） */
  code: string
  /** 可兑换额度 */
  quota: number
  status: number
  status_text: string
  /** Unix 秒；0 表示永不过期 */
  expires_at: number
  /** 后端按当前时刻判定的过期状态（不依赖前端时钟），与 status 正交 */
  expired: boolean
  /** 领取用户 id；未使用为 0 */
  used_by: number
  /** Unix 秒；0 表示未使用 */
  used_at: number
  batch_no: string
  remark: string
  created_at: number
}

/** GET /api/admin/redeem-codes 查询参数（空值由 client 自动剔除） */
export interface RedeemCodeQuery {
  page?: number
  size?: number
  /** 状态过滤（不传表示全部） */
  status?: number
  /** 按兑换码或备注模糊匹配 */
  keyword?: string
  /** 仅返回该批次 */
  batch_no?: string
}

/** POST /api/admin/redeem-codes 请求体（批量生成） */
export interface CreateRedeemCodesPayload {
  count: number
  quota: number
  /** 有效期（天）；0 表示永不过期 */
  expires_days: number
  remark?: string
  /** 批次号；留空由服务端按时间自动生成 */
  batch_no?: string
}

/** POST /api/admin/redeem-codes 响应：本批生成的全部兑换码（供导出分发） */
export interface CreateRedeemCodesResult {
  batch_no: string
  count: number
  items: RedeemCode[]
}

/** PUT /api/admin/redeem-codes/{id} 请求体：只提交需要变更的字段 */
export interface UpdateRedeemCodePayload {
  status?: number
  remark?: string
}

