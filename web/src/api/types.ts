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
  /**
   * 1 元可兑换的额度数（充值兑换比例，默认 100）。
   *
   * 额度是站内计费单位（1 元 = 100 额度，即 1 额度 ≈ 1 分），用户只认人民币。
   * 用户门户据此把余额、消费、模型单价折算成 ¥ 展示；
   * 为 0 表示后端未提供，此时退回显示原始额度。
   */
  quota_per_yuan: number

  /**
   * 合规信息（对用户公示）。
   *
   * 这些值展示在全站页脚与协议页：页脚是"任何页面都能看到服务由谁提供"的地方，
   * 也是浏览器、微信/QQ、支付通道核验站点时最常看的位置。
   * 各字段可能为空（站长尚未填写），为空时前端不展示该项。
   */
  operator_name: string
  icp_license: string
  police_license: string
  contact_email: string
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
  /** 代理分组名（空/缺省 = 普通用户） */
  agent_group?: string
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
  /**
   * 是否已阅读并同意《用户协议》与《隐私政策》。
   *
   * 后端强制校验：收集邮箱属于个人信息处理，必须先取得同意；
   * 前端把按钮置灰只是体验，真正拦住的是服务端这一道。
   */
  agreed_terms: boolean
  /** 邀请码（由邀请链接带出，可选） */
  invite_code?: string
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
  /**
   * 令牌所属分组标识；空串表示"使用网关默认分组"。
   *
   * 契约未定义该字段，但后端 DTO 已固定下发（见 internal/server/dto.go）。
   * 声明为可选是为了兼容早期后端响应，展示时按「空 = 默认」处理。
   */
  group_name?: string
  /**
   * ── 周期预算（滚动窗口，迁移 0043）────────────────────────
   *
   * 与 remain_quota（总量墙）并列的「周期墙」：每个周期内最多消耗这么多额度，
   * 周期一到自动翻篇。本窗口已消耗 = used_quota − budget_window_base（负数按 0）。
   *
   * 注意：后端 tokenDTO 当前【未下发】这几个字段（见 internal/server/dto.go），
   * 故声明为可选：有则展示周期预算进度，无则显示"未设置"（后端补字段后自动生效）。
   */
  /** 周期预算额度（站内单位）；0/缺失表示未启用预算 */
  budget_quota?: number
  /** 预算周期：daily / weekly / monthly；空/缺失表示未启用 */
  budget_period?: string
  /** 当前预算窗口起点（Unix 秒；0 = 尚未锚定） */
  budget_window_start?: number
  /** 窗口起点时刻的 used_quota 快照（窗口基线） */
  budget_window_base?: number
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
  /**
   * 令牌所属分组标识；空串表示"使用网关默认分组"。
   *
   * 后端会校验「格式合法 + 分组存在」，指向不存在的分组会返回 400。
   */
  group_name?: string
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
  /**
   * 令牌所属分组标识。
   *
   * 更新语义与创建不同：留空或空串表示"保持原分组不变"（后端如此约定），
   * 因此不能用空串把已有分组改回默认。
   */
  group_name?: string
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

/* ── 用量排行榜（GET /api/user/leaderboard）──────────────── */

/** 排行榜中一行的对外表示。 */
export interface LeaderboardEntry {
  /** 名次（1 起） */
  rank: number
  user_id: number
  /** 账号 ID（用户名） */
  username: string
  /** 窗口内请求总数（含成功与失败） */
  requests: number
  /** 窗口内 token 消耗总数 */
  tokens: number
  /**
   * 综合使用量分数（0~100 封顶）。
   *
   * = 50 分 × 请求数归一 + 50 分 × token 归一（榜内相对刻度），
   * 衡量「用得多不多」；与 success_rate（用得稳不稳）是两个独立维度。
   */
  score: number
  /** 请求成功率（0~1）：成功数 / 总数，衡量稳定性 */
  success_rate: number
  /** 平均请求耗时（毫秒，仅成功请求） */
  avg_latency_ms: number
  /** 窗口内峰值并发请求数（差分扫描估算） */
  peak_concurrency: number
  /** 是否当前登录用户（前端据此高亮并标注"我"） */
  is_me: boolean
}

/** 单个榜单（付费榜 / 免费榜）的响应。 */
export interface LeaderboardSection {
  items: LeaderboardEntry[]
  /** 当前登录用户在该榜中的名次；未上榜为 0 */
  my_rank: number
}

/** 排行榜顶部的全站汇总（与榜单同一时间窗、同一数据源） */
export interface LeaderboardTotals {
  /** 窗口内总请求数（含成功与失败） */
  requests: number
  /** 窗口内总 Token 消耗 */
  tokens: number
  /** 总成功率（0~1） */
  success_rate: number
  /** 窗口内有用量的用户数 */
  users: number
}

/** GET /api/user/leaderboard?days=30 响应 */
export interface LeaderboardStats {
  range_days: number
  totals: LeaderboardTotals
  paid: LeaderboardSection
  free: LeaderboardSection
  updated_at: number
}

/* ── 模型实时指标与连通性测试（模型详情页）────────────────── */

/** GET /api/user/models/stats?model=...&minutes=15 响应 */
export interface ModelStats {
  model: string
  /** 是否至少有一个启用渠道支持该模型（false = 离线） */
  available: boolean
  /** 支持该模型的启用渠道数量（0 = 离线） */
  channel_count: number
  /** 窗口内该模型的成功请求数 */
  requests: number
  /** 窗口内平均输出速率（tokens/s；0 = 无样本） */
  avg_tokens_per_second: number
  /** 窗口内平均总耗时（毫秒；0 = 无请求） */
  avg_latency_ms: number
  /** 窗口内平均首字延迟 TTFB（毫秒；0 = 无样本） */
  avg_first_token_ms: number
}

/** 模型连通性测试结果（前端直连 /v1/chat/completions 测得） */
export interface ModelTestResult {
  /** 是否连通（成功拿到首个数据块） */
  connected: boolean
  /** 首字延迟 TTFB（毫秒；未连通为 0） */
  ttfb_ms: number
  /** 未连通时的错误信息 */
  error?: string
  /** 测试实际使用的模型名 */
  model: string
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
  /** 本次实际发给上游的模型名（经渠道映射改写）；空串/缺省表示与 model 相同 */
  upstream_model?: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  /**
   * 缓存命中的输入 token（0 = 上游未回报）。
   *
   * 为什么单列：缓存命中的提示词通常按更低价计费，是核对账单与评估
   * "提示词复用率"的直接依据；混在 prompt_tokens 里会看不清钱花在哪。
   */
  cached_tokens: number
  /** 推理 token（0 = 上游未回报）。它计入输出但使用者看不到，是出账争议的主要来源 */
  reasoning_tokens: number
  /** 首 token 延迟（毫秒，0 = 非流式或上游未采集） */
  first_token_ms: number
  /** 输出速率（tokens/s，0 = 无法计算）；已扣除首包时间，反映真实生成速度 */
  tokens_per_second: number
  quota: number
  latency_ms: number
  is_stream: boolean
  status_code: number
  error?: string
  created_at: number
}

/* ────────────────────── 邮件通道（SMTP）────────────────────── */

/**
 * 邮件通道配置状态（GET/PUT /api/admin/smtp）。
 *
 * 注意：这里【没有】password 字段——口令不对外输出是后端的硬约束，
 * 界面只能"重填"，不能"查看"。
 */
export interface SMTPSettings {
  host: string
  port: number
  username: string
  from: string
  from_name: string
  /** 后台这条配置是否启用（启用则优先于环境变量） */
  enabled: boolean
  updated_at: number
  /** 库里是否已保存口令（用于提示"留空即沿用"） */
  password_set: boolean
  /** 上面这组参数是否来自环境变量回填（而非后台保存过的配置） */
  values_from_env: boolean
  /** 环境变量里是否提供了口令（口令无法回显，只能给"有没有"） */
  env_password_set: boolean
  /** 当前实际生效的来源：database / env / none */
  source: 'database' | 'env' | 'none' | string
  /** 此刻是否真的能发信 */
  ready: boolean
  /** 当前生效的端点（不含口令），用于展示"到底在用哪一套" */
  effective_host: string
  effective_port: number
  effective_from: string
  effective_sender: string
}

/** PUT /api/admin/smtp 的请求体；password 留空表示沿用已保存的口令 */
export interface SMTPSettingsPayload {
  host: string
  port: number
  username: string
  from: string
  from_name: string
  enabled: boolean
  password: string
}

/* ────────────────────────── 渠道 ────────────────────────── */

/**
 * 渠道级「模型重试覆盖规则」。
 *
 * 语义：站长的配置视角是"我对外卖的这个模型"，因此这里写的是平台模型名
 * （public_model），与映射后的上游模型名无关；支持尾部通配符 *（如 "gpt-4*"）。
 *
 * 解析次序为「精确匹配 → 最长前缀 → 渠道级配置」，
 * 与计价规则的匹配约定保持一致（同一份模型名在价格与重试上不会出现两套解释）。
 */
export interface ChannelModelRetryRule {
  /** 平台模型名，支持尾部通配符 * */
  model: string
  /** 该模型是否允许站内重试 */
  enabled: boolean
  /** 该模型的渠道级重试次数；0 表示用默认值 */
  max_attempts: number
}

/** 渠道对象（响应中只有 masked_key，绝不出现明文） */
export interface Channel {
  id: number
  name: string
  /** 渠道类型编号（契约仅示例了 1，具体枚举待与后端对齐） */
  type: number
  /** 渠道类型标识（与 channeltype 目录 Key 对应，如 azure_openai / anthropic）；空串表示历史数据 */
  type_key?: string
  base_url: string
  masked_key: string
  /** 空数组表示「支持全部模型」（M2 过渡约定） */
  models: string[]
  /** 主分组（= groups 的第一项），用于展示与分组统计 */
  group: string
  /**
   * 本渠道可服务的全部分组（多选；后端保证非空，第一项即主分组）。
   *
   * 路由按令牌分组匹配渠道：列在这里的分组都能路由到本渠道，
   * 因此"一个渠道同时服务免费组与自营组"无需建两个渠道。
   */
  groups: string[]
  priority: number
  weight: number
  /**
   * 凭据池调度策略标识（sequential / round_robin / weighted_random /
   * least_recent / least_in_flight）。后端保证恒为合法值。
   */
  key_strategy: string
  /**
   * 密钥失败处置策略标识（cooldown_only / auto_remove）。后端保证恒为合法值。
   *
   * cooldown_only 表示密钥失败后只进冷却池、到期自动恢复，永不自动摘除；
   * 这是默认值，适合"密钥不会死"的上游（如免费额度池）。
   */
  key_failure_policy: string
  /** 密钥失败后的统一冷却时长（秒）；0 表示使用系统内置的分级退避 */
  key_cooldown_seconds: number
  /**
   * 上游错误重试总开关。
   *
   * true（默认）= 上游报错时在站内换密钥/换渠道再试，降低下游看到的错误率；
   * false = 只尝试一次，上游一报错就按本站定制错误码回给下游
   * （适合"重复请求会重复扣费"的上游）。
   */
  retry_enabled: boolean
  /** 渠道级重试次数上限（含首次尝试）；后端保证已归一为 1~10 */
  retry_max_attempts: number
  /** 模型级重试覆盖规则；空数组表示全部沿用渠道级配置 */
  model_retry_rules: ChannelModelRetryRule[]
  status: number
  status_text?: string
  last_test_at?: number
  last_test_ok?: boolean
  created_at?: number
  updated_at?: number
  /** 密钥池概览；total 为 0 表示该渠道未配置密钥池（走单密钥模式） */
  key_pool?: KeyPoolSummary
}

/**
 * 渠道级「模型 ID 映射」（GET/PUT /api/admin/channels/{id}/mappings 的 items 项）。
 *
 * 语义（这是本功能唯一需要理解的一句话）：
 *   平台模型 ID（public_model）——客户端调用时使用的名字，出现在模型广场与 /v1/models；
 *   上游模型 ID（upstream_model）——网关转发时真正发给上游的名字。
 * 请求方向用 public 匹配、改写为 upstream；响应方向把上游回包的名字改回 public。
 * 两侧都支持尾部通配符 *（前缀族），精确匹配优先、最长前缀次之。
 */
export interface ChannelModelMapping {
  id: number
  channel_id: number
  /** 实际发给上游的模型 ID */
  upstream_model: string
  /** 平台模型 ID（用户调用时使用） */
  public_model: string
  /** 优先级，数值越大越优先（同精度竞争时使用） */
  priority: number
  enabled: boolean
  remark: string
  created_at: number
  updated_at: number
}

/** 提交映射时的单条数据（id/时间戳由后端生成，不需要提交） */
export interface ChannelModelMappingItem {
  upstream_model: string
  public_model: string
  priority?: number
  enabled?: boolean
  remark?: string
}

/** 全站模型 ID 映射总览里的一行（已带上渠道信息） */
export interface ModelMappingOverviewItem {
  id: number
  channel_id: number
  channel_name: string
  /** 渠道状态：1 启用 / 2 停用 / 3 自动停用；非 1 时映射不会生效 */
  channel_status: number
  group: string
  /** 平台模型 ID（用户调用时使用） */
  public_model: string
  /** 上游模型 ID（实际发给上游） */
  upstream_model: string
  priority: number
  enabled: boolean
  remark: string
}

/** 未配置任何映射的渠道摘要（模型名原样透传给上游） */
export interface ModelMappingPlainChannel {
  channel_id: number
  channel_name: string
  channel_status: number
  group: string
  /** 该渠道声明的模型数；0 表示声明"支持全部模型" */
  model_count: number
}

/** GET /api/admin/model-mappings 的响应 */
export interface ModelMappingOverview {
  items: ModelMappingOverviewItem[]
  channels_without_mapping: ModelMappingPlainChannel[]
  total: number
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
  /** 凭据类型：api_key（静态密钥）/ oauth（订阅账号）；后端 channelKeyDTO.kind */
  kind: string
  /** 凭据类型的中文名（"API Key" / "订阅账号"），由后端下发，前端不硬编码 */
  kind_text: string
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
  /**
   * 以下为订阅账号（如 ChatGPT/Codex）专有字段（迁移 0028）。
   *
   * 对 API Key 型凭据一律为空/未知：account_id 为空串、quota_used_percent 为 -1。
   */
  account_id: string
  /** 套餐标识（plus / pro / team…）；空串表示未知 */
  plan_type: string
  /** 订阅账号邮箱（空串 = 未知或非订阅账号）；用于界面辨认"这是谁的账号" */
  email: string
  /** 上游【主】额度窗口（5 小时）已用百分比；-1 表示尚未探测 */
  quota_used_percent: number
  /** 主窗口重置时间的 Unix 秒（0 = 未知） */
  quota_reset_at: number
  /** 上次探测额度的 Unix 秒（0 = 从未探测） */
  quota_checked_at: number
  /** 是否已探测过主窗口额度（后端派生，避免前端自己实现 -1 的规则） */
  quota_known: boolean
  /** 主窗口额度是否已用满（后端已考虑"重置时间已过视为已恢复"） */
  quota_exhausted: boolean
  /**
   * 以下为【次】额度窗口（每周）字段（迁移 0048）。
   *
   * 与主窗口同构：-1 表示尚未探测。次窗口【不】参与调度判定，
   * 仅用于界面预警（避免"没怎么用账号却突然全满"）。
   */
  quota_secondary_used_percent: number
  /** 次窗口重置时间的 Unix 秒（0 = 未知） */
  quota_secondary_reset_at: number
  /** 是否已探测过次窗口额度（后端派生） */
  quota_secondary_known: boolean
  /** 主窗口时长（秒；0 = 上游未提供，前端退回默认文案） */
  quota_primary_window_seconds: number
  /** 次窗口时长（秒；0 = 上游未提供） */
  quota_secondary_window_seconds: number
  /**
   * 路由分叉（迁移 0038）：本凭据可服务的分组与模型。
   *
   * 空数组 = 不限（继承渠道级路由），这是默认值；
   * models 支持尾部通配符 *（如 "gpt-4*"）。
   */
  groups: string[]
  models: string[]
}

/** POST /api/admin/channels/{id}/keys/{keyId}/quota 响应 */
export interface ChannelKeyQuota {
  key_id: number
  plan_type: string
  /** 订阅账号邮箱（空串 = 上游未提供） */
  email: string
  /** 主窗口（5 小时）已用百分比；-1 表示上游未提供额度窗口 */
  used_percent: number
  /** 主窗口重置时间（Unix 秒，0 = 未知） */
  reset_at: number
  /** 主窗口时长（秒；0 = 未知） */
  primary_window_seconds: number
  /** 次窗口（每周）已用百分比；-1 表示上游未提供 */
  secondary_used_percent: number
  /** 次窗口重置时间（Unix 秒，0 = 未知） */
  secondary_reset_at: number
  /** 次窗口时长（秒；0 = 未知） */
  secondary_window_seconds: number
  /** 上游是否明确告知"当前已触顶" */
  limit_reached: boolean
  /** 后端给出的说明文案（成功/未提供额度窗口），前端直接展示 */
  note: string
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
  /**
   * 路由分叉（迁移 0038）：本凭据可服务的分组与模型。
   *
   * 两项必须同时提供（后端整组覆盖，缺项会被误写成"不限"）。
   * 空数组 = 不限，即继承渠道级路由。
   */
  groups?: string[]
  models?: string[]
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

/**
 * 密钥失败处置策略目录（GET /api/admin/key-failure-policies）。
 *
 * 除策略列表外还下发上限与默认值：前端据此限制冷却时长输入，
 * 避免站长填一个天文数字然后困惑"密钥怎么再也不回来了"。
 */
export interface KeyFailurePolicyCatalog {
  items: KeyStrategyOption[]
  total: number
  /** 冷却时长上限（秒）；超过它会被后端夹到上限，等价于事实摘除 */
  max_cooldown_seconds: number
  /** 后端默认策略标识（只冷却不摘除） */
  default_failure_policy: string
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
  /** 渠道类型标识（与 channeltype 目录 Key 对应）；留空 = OpenAI 兼容 / 更新时不修改 */
  type_key?: string
  base_url: string
  api_key?: string
  models: string[]
  /**
   * 服务分组清单（多选，至少一项；第一项为主分组）。
   *
   * 提交它即以它为准，后端会把 group 归一等同于 groups[0]。
   * 未提交时后端按单分组 group 处理（兼容旧客户端）。
   */
  groups?: string[]
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
   * 密钥失败处置策略标识：cooldown_only（只冷却不摘除，默认）/ auto_remove（失败自动摘除）。
   *
   * 留空表示「不修改」。它决定"密钥失败后是回到池子还是永久退出"，
   * 对"密钥不会死"的上游（如免费额度池）必须选 cooldown_only，
   * 否则好密钥会被瞬时故障逐批误杀，最终表现为大面积 503。
   */
  key_failure_policy?: string
  /**
   * 密钥失败后的统一冷却时长（秒）。
   *
   * 0 表示使用系统内置的分级指数退避；>0 表示统一按该时长冷却。
   * 注意：更新接口用指针语义区分"未提交"与"显式改为 0"，
   * 因此前端必须始终提交该字段（不要因为值为 0 就省略）。
   */
  key_cooldown_seconds?: number
  /**
   * 上游错误重试总开关。
   *
   * 省略（字段缺失）表示「不修改」；显式提交 false 表示关闭重试。
   * 关闭是一个明确诉求（按次计费的上游怕重复扣费），因此不能用"缺省"表达。
   */
  retry_enabled?: boolean
  /**
   * 渠道级重试次数上限（含首次尝试）。
   *
   * 显式提交 0 表示「恢复内置默认次数」（3 次）；省略表示「不修改」。
   */
  retry_max_attempts?: number
  /**
   * 模型级重试覆盖规则。
   *
   * 省略表示「不修改」；显式提交 [] 表示「清空所有模型级规则」。
   * 命中规则的模型不再沿用渠道级配置（数组里没有该模型即等于继承）。
   */
  model_retry_rules?: ChannelModelRetryRule[]
  /**
   * 批量密钥文本：每行一把，行内可用空格或逗号附加备注。
   *
   * 留空表示"不修改密钥池"（避免只改个名字就把几百把密钥清空）。
   */
  keys_text?: string
  /**
   * 批量订阅账号凭据文本（ChatGPT/Codex 等 OAuth 账号）。
   *
   * 可直接粘贴 Codex CLI 导出的 auth.json（JSON 数组 / JSONL 也支持），
   * 或每行一条 refresh_token。留空表示"不修改订阅账号池"。
   * 与 keys_text 分开提交：两类凭据的增删互不影响。
   */
  oauth_tokens_text?: string
  /**
   * 订阅账号的 OAuth 提供方名称；留空时由后端按渠道类型自动选择内置预设。
   *
   * 站长通常不需要填——刷新所需的 token 端点与 client_id 是内置的。
   */
  oauth_provider?: string
}

/** POST /api/admin/channels/{id}/test 响应 */
export interface ChannelTestResult {
  ok: boolean
  latency_ms: number
  model: string
  message: string
  status_code: number
  /** 上游响应片段（已截断）；失败时是最有价值的排查线索 */
  upstream_body?: string
  /** 本次真正发给上游的模型名（可能被渠道级模型映射改写） */
  upstream_model?: string
  /** 本次探测使用的凭据掩码；空表示无凭据可用 */
  key_masked?: string
  /** 凭据来源：pool（密钥池）/ single（渠道单密钥） */
  key_source?: string
  pool_total?: number
  pool_available?: number
  pool_cooling?: number
  pool_disabled?: number
  pool_removed?: number
  pool_exhausted?: number
}

/** POST /api/admin/channels/{id}/speedtest 的单个模型结果 */
export interface SpeedTestItem {
  model: string
  /** 实际发给上游的模型名（可能被渠道级映射改写） */
  upstream_model?: string
  ok: boolean
  status_code: number
  /** 首字延迟（毫秒）——测速主指标；失败时为 0 */
  ttfb_ms: number
  total_ms: number
  message?: string
  /** 该模型因无权限（403/404）被自动从渠道清单移除 */
  blocked?: boolean
}

/** POST /api/admin/channels/{id}/speedtest 响应 */
export interface SpeedTestRunResult {
  items: SpeedTestItem[]
  tested: number
  ok_count: number
  elapsed_ms: number
  /** 整批级失败说明（如凭据不可用）；为空表示逐模型结果可信 */
  message?: string
  /** 本次被自动屏蔽（无权限 403/404）并从渠道清单移除的模型 */
  blocked_models?: string[]
  key_masked?: string
  key_source?: string
  pool_total?: number
  pool_available?: number
  pool_cooling?: number
  pool_disabled?: number
  pool_removed?: number
  pool_exhausted?: number
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
  /** 代理分组名（空/缺省 = 普通用户）；决定该账号在模型广场看到的模型与价格 */
  agent_group?: string
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
  /** 代理分组名（空串 = 普通用户） */
  agent_group?: string
}

/** 更新用户（含调整额度）：只提交需要变更的字段 */
export interface UpdateUserPayload {
  email?: string
  role?: number
  status?: number
  quota?: number
  /**
   * 代理分组名：传空串 = 取消代理资格；不传 = 保持不变。
   *
   * 后端的区分依据是"字段是否存在"（指针语义），因此这里用可选属性表达。
   */
  agent_group?: string
  /** 契约未定义：是否允许管理员重置密码，待确认后再启用 */
  password?: string
}

/* ────────────────────────── 仪表盘 ────────────────────────── */

/** GET /api/admin/dashboard 响应 */
export interface DashboardStats {
  channels: { total: number; enabled: number; auto_disabled: number }
  users: { total: number; active: number }
  tokens: { total: number; enabled: number }
  /**
   * 今日汇总。
   *
   * 除总量外还给出"质量维度"：入/出 token 拆分、缓存命中率、平均延迟与
   * 平均输出速率。这些是判断"贵不贵、快不快、缓存有没有生效"的依据，
   * 只有总量时站长无法定位问题（例如总延迟高是慢在首包还是慢在生成）。
   */
  today: {
    requests: number
    tokens: number
    quota: number
    success_rate: number
    prompt_tokens: number
    completion_tokens: number
    cached_tokens: number
    reasoning_tokens: number
    /** 缓存命中率（0~1 的比值）；无输入 token 时为 0 */
    cache_hit_rate: number
    /** 平均总耗时（毫秒，0 = 无样本） */
    avg_latency_ms: number
    /** 平均首 token 延迟（毫秒，0 = 无样本） */
    avg_first_token_ms: number
    /** 平均输出速率（tokens/s，0 = 无样本） */
    avg_tokens_per_second: number
  }
  recent_days: { date: string; requests: number; tokens: number; cached_tokens: number }[]
  top_models: { model: string; requests: number }[]
}

/* ────────────────────────── 系统设置 ────────────────────────── */

/**
 * SEO 与站点收录配置（GET /api/admin/settings 响应中的 seo 对象）。
 *
 * 这些值由后端在响应时注入页面 <head>（Next 静态导出产物没有预留锚点，
 * 由 internal/server/seo.go 的 injectSEOMeta 直接插入），
 * 并据此生成 sitemap.xml / robots.txt；前端的职责只是采集与展示，不做任何拼接。
 */
export interface SeoSettings {
  /** 站点公开访问地址（如 https://api.example.com）；留空时后端按访问请求推导 */
  site_url: string
  /** SEO 关键词 */
  keywords: string[]
  /** 必应站长验证码（msvalidate.01） */
  bing_verification: string
  /** Google Search Console 验证码 */
  google_verification: string
  /** 百度站长验证码 */
  baidu_verification: string
  /** 地域代码，如 CN-44 */
  geo_region: string
  /** 地名，如 Shenzhen */
  geo_placename: string
  /** 经纬度，格式「纬度;经度」，如 22.5431;114.0579 */
  geo_position: string
  /** 是否输出 sitemap.xml 与 robots.txt */
  sitemap_enabled: boolean
  /** 额外的公开路径（以 / 开头）；只填公开页面，登录后才能看的页面不要写 */
  sitemap_paths: string[]
  /** 只读：完整 sitemap 地址（后端算好）；未启用时为空 */
  sitemap_url: string
  /** 只读：完整 robots 地址（后端算好）；未启用时为空 */
  robots_url: string
}

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
  /** SEO 与站点收录配置 */
  seo: SeoSettings
  /** 内容安全（合规过滤）配置 */
  safeguard: SafeguardSettings
  /** 模型测速配置 */
  speedtest: SpeedTestSettings
  /**
   * 合规信息（对用户公示）：经营主体、备案号与客服邮箱。
   *
   * 这些值会出现在全站页脚与协议页，是浏览器、微信/QQ 与支付通道核验站点时
   * 最常检查的一组信息；未填写时页脚不展示对应项。
   */
  compliance: ComplianceSettings
}

/** 合规信息（公开公示用） */
export interface ComplianceSettings {
  /** 经营主体名称（如「XX 科技有限公司」）；为空时页脚回退显示站点名 */
  operator_name: string
  /** ICP 备案号（如 京ICP备00000000号-1）；为空时不展示 */
  icp_license: string
  /** 公安联网备案号；为空时不展示 */
  police_license: string
  /** 客服/投诉邮箱；为空时不展示 */
  contact_email: string
}

/** 内容安全（合规过滤）配置 */
export interface SafeguardSettings {
  /**
   * 敏感词过滤总开关。
   *
   * 关闭时 /v1 入口不扫描请求正文；开启后命中词表即拒绝请求。
   * 默认关闭：拦截会直接影响用户可用性，应由站长显式开启。
   */
  sensitive_filter_enabled: boolean
}

/** 模型测速配置 */
export interface SpeedTestSettings {
  /** 测速总开关：关闭时管理端测速接口拒绝、广场不下发延迟 */
  enabled: boolean
  /** 是否在模型广场向用户展示测得的延迟 */
  public: boolean
  /** 单个模型一次测速的超时（秒），后端限定 5~120 */
  timeout_seconds: number
  /** 单次测速请求允许测的模型数上限，后端限定 1~500 */
  max_models: number
  /** 测速后自动屏蔽无权限模型（上游明确回 403/404 的从渠道清单移除） */
  auto_block: boolean
}

/**
 * PUT /api/admin/settings 的 seo 字段：只包含可写项。
 *
 * sitemap_url / robots_url 由后端按 site_url 计算，属于只读产物，
 * 前端一律不回传，避免"用旧值覆盖后端算好的地址"。
 */
export type UpdateSeoSettingsPayload = Partial<{
  site_url: string
  keywords: string[]
  bing_verification: string
  google_verification: string
  baidu_verification: string
  geo_region: string
  geo_placename: string
  geo_position: string
  sitemap_enabled: boolean
  sitemap_paths: string[]
}>

/** PUT /api/admin/settings 请求体：只提交需要变更的字段 */
export type UpdateSiteSettingsPayload = Partial<{
  site_name: string
  site_description: string
  registration_enabled: boolean
  registration_require_email_code: boolean
  default_user_quota: number
  default_group: string
  payment: PaymentSettings
  seo: UpdateSeoSettingsPayload
  safeguard: Partial<SafeguardSettings>
  speedtest: Partial<SpeedTestSettings>
  /**
   * 合规信息：允许提交空串以清空某一项（如备案号填错要删掉），
   * 未提交的字段由后端保持原值。
   */
  compliance: Partial<ComplianceSettings>
}>

/* ────────────────────────── 运行上限（超管可调） ────────────────────────── */

/**
 * 一项运行上限：当前值 + 默认值 + 合法区间。
 *
 * 后端一次性给出三样信息，前端据此限制输入范围并展示"当前 vs 默认"，
 * 因此前端不硬编码任何数值（改默认值只改后端一处）。
 */
export interface LimitItem {
  /** 字段标识，与 PUT 请求体的键一致 */
  field: string
  /** 当前生效值 */
  value: number
  /** 默认值（与改动前的硬编码常量一致） */
  default: number
  /** 合法区间下界（含） */
  min: number
  /** 合法区间上界（含） */
  max: number
}

/** PUT /api/admin/limits 请求体：只提交需要变更的字段（未提交项保持原值） */
export type UpdateLimitsPayload = Partial<{
  /** 普通 JSON 接口的请求体上限（字节） */
  body_max_bytes: number
  /** 敏感词单次批量导入的条数上限 */
  sensitive_import_max_words: number
  /** 排行榜允许的最大统计窗口（天） */
  leaderboard_max_days: number
  /** 模型实时指标允许的最大窗口（分钟） */
  model_stats_max_minutes: number
  /** 限时试用额的时长上限（小时） */
  trial_grant_max_hours: number
  /** 公开端一次返回的公告条数上限 */
  announcement_active_max: number
}>

/* ────────────────────────── 上游进价与密钥余额核算 ────────────────────────── */

/**
 * 上游进价规则（渠道 × 模型）。
 *
 * 口径与模型售价完全一致（每 1M token 的额度单位），因此毛利就是一次减法。
 * 四个价格全为 0 表示「上游免费」这一明确结论；未录入则是根本没有这条规则。
 */
export interface ChannelModelCost {
  id: number
  model: string
  prompt_price: number
  cache_price: number
  completion_price: number
  per_call_price: number
  remark: string
  /** 是否表示上游免费（四个价格全为 0） */
  is_free: boolean
  updated_at: number
}

/** 单条进价的可写入参（保存时整批提交） */
export interface ChannelModelCostPayload {
  model: string
  prompt_price: number
  cache_price: number
  completion_price: number
  per_call_price: number
  remark: string
}

/** GET /api/admin/channels/{id}/costs 响应 */
export interface ChannelCostListResult {
  items: ChannelModelCost[]
  total: number
  /** 该渠道声明的模型清单，供「按渠道模型预填」使用 */
  declared_models: string[]
}

/** 密钥在某个模型上的用量与成本 */
export interface ChannelKeyModelUsage {
  model: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  cached_tokens: number
  /** 按上游进价估算的成本（额度单位） */
  cost: number
  /** 是否匹配到了进价规则；false 表示「未录进价」（不是零成本） */
  priced: boolean
}

/** 一把密钥的用量、估算消耗与剩余余额 */
export interface ChannelKeyUsage {
  channel_key_id: number
  label: string
  status: number
  account_hint: string
  balance: number
  balance_unknown: boolean
  balance_exhausted: boolean
  balance_updated_at: number
  requests: number
  prompt_tokens: number
  completion_tokens: number
  cached_tokens: number
  /** 按上游进价估算的累计消耗（只统计有进价的模型） */
  estimated_cost: number
  /** 能/不能估算成本的请求数 */
  priced_requests: number
  unpriced_requests: number
  /** 估算剩余 = 录入余额 − 估算消耗；仅在 balance_known 为 true 时有意义，可为负 */
  remaining: number
  /** 站长是否录入过余额 */
  balance_known: boolean
  models: ChannelKeyModelUsage[]
  /** 没有进价规则的模型清单（提示站长去「上游计费」补录） */
  unpriced_models: string[]
}

/** GET /api/admin/channels/{id}/key-usage 响应 */
export interface ChannelKeyUsageResult {
  items: ChannelKeyUsage[]
  total: number
}

/* ────────────────────────── 敏感词（内容合规） ────────────────────────── */

/**
 * 敏感词条（GET /api/admin/sensitive-words）。
 *
 * 说明：word 落库时统一为「去首尾空白 + 小写」，匹配也不区分大小写；
 * 因此界面上展示的即是实际参与匹配的形式。
 */
export interface SensitiveWord {
  id: number
  /** 词条（小写归一后的形式） */
  word: string
  /** 分类（如「违法违规」），可为空 */
  category: string
  /** 是否启用；停用即不参与匹配 */
  enabled: boolean
  remark: string
  created_at: number
  updated_at: number
}

/** POST/PUT /api/admin/sensitive-words 请求体 */
export interface SensitiveWordPayload {
  word?: string
  category?: string
  enabled?: boolean
  remark?: string
}

/** GET /api/admin/sensitive-words 响应 */
export interface SensitiveWordListResult {
  items: SensitiveWord[]
  total: number
  /** 其中启用状态的条数（界面上直接显示"生效中 N 条"） */
  enabled_total: number
}

/** POST /api/admin/sensitive-words/import 响应 */
export interface SensitiveWordImportResult {
  /** 实际新增的条数（重复的会被跳过） */
  imported: number
  /** 因长度不合法被跳过的行数 */
  skipped_invalid: number
  /** 解析出的候选词条总数 */
  total: number
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

/* ────────────────────────── 模型计价规则 ────────────────────────── */

/**
 * 模型计价规则（GET /api/admin/prices）。
 *
 * 价格口径：字段表示「每 100 万 token 消耗的站点额度」，
 * per_call_price 表示「每调用一次消耗的额度」（异步任务/图像等按次计费的能力）。
 * cache_price 表示「每 100 万命中缓存的输入 token 消耗的额度」，0 = 未配置，
 * 此时命中缓存的输入仍按 prompt_price 计费。
 * 四处必须与后端 model.ModelPrice 保持一致：改动时同步本文件、价格页文案与后端。
 */
export interface ModelPrice {
  id: number
  /** 模型名或通配模式（"gpt-4*" 前缀、"*" 全局） */
  model: string
  prompt_price: number
  /** 缓存命中价（每 1M 命中缓存的输入 token 额度）；0 = 按 prompt_price 计 */
  cache_price: number
  completion_price: number
  per_call_price: number
  /**
   * 站长显式选择的计费方式；空串表示「自动判定」（历史数据）。
   *
   * 'free' 免费 / 'token' 按量 / 'per_call' 按次。
   */
  billing_mode: BillingMode | ''
  /** 实际生效的计费方式（把「自动」解释成具体口径），只读 */
  effective_billing_mode: 'free' | 'token' | 'per_call'
  /** 是否显式免费（只读派生值；与「未定价」是两回事） */
  is_free: boolean
  group: string
  enabled: boolean
  remark: string
  created_at: number
  updated_at: number
}

/** 计费方式 */
export type BillingMode = 'free' | 'token' | 'per_call'

/** 新增/更新计价规则请求体 */
export interface ModelPricePayload {
  model: string
  prompt_price?: number
  cache_price?: number
  completion_price?: number
  per_call_price?: number
  billing_mode?: BillingMode | ''
  group?: string
  enabled?: boolean
  remark?: string
}

/** GET /api/admin/prices/quote 响应：费用试算结果 */
export interface QuotePreview {
  model: string
  prompt_tokens: number
  /** 其中命中上游缓存的输入 token 数（按 cache_price 计费） */
  cached_tokens: number
  completion_tokens: number
  quota: number
  /**
   * 是否命中了计价规则。
   *
   * 注意语义是「有规则」而不是「金额大于 0」：显式免费的规则金额也是 0，
   * 若按金额判定，界面会把「免费」误显示成「未定价」。
   */
  priced: boolean
  /** 生效的计费方式；空串表示未定价 */
  billing_mode: '' | 'free' | 'token' | 'per_call'
  is_free: boolean
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
  /**
   * 解锁本分组所需的累计充值（单位：分，0 = 无门槛）。
   *
   * 后端以「分」为整数存储与传输，避免浮点比较出现 99.99999 < 100 的假性未达标；
   * 展示与输入由前端换算成元（见 formatCents / yuanToCents）。
   */
  unlock_min_recharge_cents: number
  /**
   * 是否只能由管理员分发（批发价分组）。
   *
   * 与 unlock_min_recharge_cents 是两件不同的事：前者管"谁来发"（分发授权），
   * 后者管"谁够格买"（客户资格）。开启后门户不下发该分组，
   * 普通用户直接调接口指定也会被 403，只有后台代建令牌才放行。
   */
  admin_only: boolean
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
  /** 解锁门槛（分）。不传 = 保持原值，传 0 = 清除门槛。 */
  unlock_min_recharge_cents?: number
  /** 仅后台分发。不传 = 保持原值（避免改名类的一次 PUT 意外放开批发价分组）。 */
  admin_only?: boolean
}

/* ────────────────────────── 模型广场（公开） ────────────────────────── */

/** 模型卡片上的某分组价格 */
export interface PlazaPrice {
  group: string
  prompt_price: number
  cache_price: number
  completion_price: number
  per_call_price: number
  /** 生效的计费方式 */
  billing_mode: 'free' | 'token' | 'per_call'
  /** 是否显式免费（广场据此显示「免费」标识） */
  is_free: boolean
  /** 该分组的计费倍率（百分比） */
  ratio: number
  /**
   * 价格生效时间 / 最近更新时间（Unix 秒；0 或缺失表示后端未下发）。
   *
   * 为什么需要它：价格随时会被站长调整，用户比对账单时最常问"这个价是什么时候的"。
   * 后端 plazaPriceDTO 当前【未下发】该字段（见 internal/server/handler_group.go），
   * 因此这里声明为可选：有则展示，无则不显示（后端补 updated_at/effective_at 后自动生效）。
   */
  updated_at?: number
  /** 价格生效时间（若后端区分"更新时间"与"生效时间"则优先用它） */
  effective_at?: number
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
  /**
   * 原价（未打折）。仅【代理视图】下发，用于渲染「划线原价」。
   *
   * 与 prices[0]（代理价）取自同一条价格规则：代理价 = 原价 × 分组倍率 ÷ 100。
   * 普通用户视图不下发该字段。
   */
  list_price?: PlazaPrice
  /**
   * 最近一次测速的最小首字延迟（毫秒）。后台「模型测速」的产物，
   * 每次测速对上游消耗约 2~3 token，因此是低频快照而非实时数据。
   * 未测过或站长关闭公示时整个字段不下发（前端不渲染延迟列）。
   */
  speed_ttfb_ms?: number
  /** 该延迟的测速时间（Unix 秒），让用户知道数字有多新鲜 */
  speed_tested_at?: number
}

/** 模型广场的分组视图 */
export interface PlazaGroup {
  name: string
  label: string
  ratio: number
  description: string
  model_count: number
  /** 该分组的每分钟请求上限（0/缺失 = 不限速）；后端 plazaGroupDTO 暂未下发 */
  rpm_limit?: number
}

/** 广场查看者身份（仅代理登录后下发） */
export interface PlazaViewer {
  /** 查看者所属的代理分组名 */
  agent_group: string
  /** 分组展示名（如「战略代理」） */
  label: string
  /** 分组计费倍率（百分比，60 = 拿货 6 折） */
  ratio: number
  /** 该分组的每分钟请求上限（0/缺失 = 不限速）；后端 plazaViewerDTO 暂未下发 */
  rpm_limit?: number
}

/** GET /api/models 响应 */
export interface ModelPlaza {
  items: PlazaModel[]
  groups: PlazaGroup[]
  total: number
  /** 代理视图标识：存在即表示"这次是代理在看广场" */
  viewer?: PlazaViewer
}

/**
 * GET /api/models/quote 响应（公开费用试算）。
 *
 * ⚠️ 契约见 docs/23 C5。该接口在后端【尚未落地】（当前仅有管理员接口
 * /api/admin/prices/quote）；此处先按约定契约声明，UI 优先调用、失败回退本地计算。
 *
 * 单位假设（需与后端核对）：price/cost 类字段按站内「额度」下发（与全站记账口径一致），
 * 展示时由前端按 quota_per_yuan 折算成人民币；currency 仅作标注。
 */
export interface ModelQuoteResult {
  model: string
  group: string
  /** 生效的计费方式 */
  billing_mode: 'free' | 'token' | 'per_call'
  /** 币种标注（如 CNY）；单位换算仍以额度为准 */
  currency: string
  /** 该分组的计费倍率（百分比） */
  ratio: number
  /** 折扣文案（后端翻译，如「6折」/「原价」） */
  discount_label: string
  /** 输入单价（每 1M token 额度） */
  input_unit_price: number
  /** 输出单价（每 1M token 额度） */
  output_unit_price: number
  /** 缓存命中单价（每 1M token 额度） */
  cached_unit_price: number
  /** 输入部分费用（额度） */
  input_cost: number
  /** 输出部分费用（额度） */
  output_cost: number
  /** 缓存命中部分费用（额度） */
  cached_cost: number
  /** 合计费用（额度） */
  total_cost: number
}

/* ─────────────────── 门户可选分组（GET /api/user/groups） ─────────────────── */

/**
 * 门户侧（创建令牌时「所属分组」下拉）看到的分组。
 *
 * 与广场的 PlazaGroup 的区别：这里带「当前用户是否已解锁」的判断，
 * 因此必须登录后才能拿到——未解锁的分组仍会下发（前端置灰并提示差多少），
 * 而不是直接隐藏：让用户看见"充值能拿到更低价格"本身就是转化引导。
 */
export interface PortalGroup {
  name: string
  label: string
  ratio: number
  description: string
  /** 解锁所需的累计充值（分，0 = 无门槛） */
  unlock_min_recharge_cents: number
  /** 当前用户是否已解锁（无门槛时恒为 true） */
  unlocked: boolean
  /** 当前用户的累计充值（分），用于显示"还差多少解锁" */
  paid_amount_cents: number
  /** 这是当前用户自己的代理拿货档（由管理员指派）；前端据此单独标注 */
  is_agent?: boolean
  /**
   * 该分组的每分钟请求上限（0/缺失 = 不限速）。
   *
   * 后端 portalGroupDTO 当前【未下发】该字段（见 internal/server/handler_group.go），
   * 故切记判空：>0 才展示提示，0/缺失一律不显示。
   */
  rpm_limit?: number
}

/** GET /api/user/groups 响应 */
export interface MyGroupsResponse {
  items: PortalGroup[]
  paid_amount_cents: number
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
  /** 通道中文名（后端翻译，如「在线支付」） */
  method_label?: string
  /** 子方式中文名（后端翻译，如「支付宝」「微信支付」） */
  sub_method_label?: string
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

/**
 * POST /api/user/redeem 响应：兑换码兑换结果。
 *
 * total_quota / remaining_quota 可选的原因：后端在"兑换已入账、但回显余额查询失败"时
 * 只返回 quota（少一个展示字段，不影响入账结果），前端不能把它们当作必有字段。
 */
export interface RedeemResult {
  /** 本次兑换获得的额度（站内整数单位） */
  quota: number
  /** 兑换后的总充值额度（尽力回显，可能缺失） */
  total_quota?: number
  /** 兑换后的剩余额度（尽力回显，可能缺失） */
  remaining_quota?: number
}

/** 公开的充值参数（GET /api/payment/public） */
export interface PublicPaymentInfo {
  enabled: boolean
  /**
   * 可用的支付通道。
   *
   * sub_methods 是通道下的"子支付方式"：易支付这类聚合通道只有一个通道名，
   * 但用户要选的是"支付宝还是微信"，因此由后端下发子方式（含中文名），
   * 前端把每个子方式渲染成一行独立选项。
   * 没有子方式概念的通道（Stripe、人工确认）下发空数组。
   */
  methods: { name: string; label: string; ready: boolean; sub_methods: { name: string; label: string }[] }[]
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

/* ────────────────────────── 财务记录 ────────────────────────── */

/**
 * GET /api/user/finance：财务板块顶部汇总。
 *
 * 所有金额字段都是**额度**（站内计费单位），由前端按后端配置的兑换比例
 * 折算成人民币展示（见 composables/useQuotaUnit.ts）——比例站长可改，
 * 因此后端不做展示换算。
 */
export interface FinanceSummary {
  /** 当前余额；-1 表示不限额度 */
  balance_quota: number
  /** 累计消费额度 */
  used_quota: number
  /** 累计有效充值额度（仅统计已支付的订单） */
  recharged_quota: number
  /** 已支付订单笔数 */
  recharge_count: number
  /** 累计返利额度（邀请注册奖 + 充值返利） */
  reward_quota: number
}

/**
 * GET /api/user/trial：当前用户的限时试用额状态。
 *
 * 恒为非 null（后端总是返回对象），前端据 active 决定要不要展示横幅。
 * remaining 是额度（站内单位），展示时交给 useQuotaUnit 折算成人民币。
 */
export interface TrialGrant {
  /** 是否处于生效中（未过期且未用完） */
  active: boolean
  /** 仍可用的试用额度（站内单位） */
  remaining: number
  /** 到期时间（unix 秒）；active=false 时为 0 */
  expires_at: number
  /** 距到期的剩余秒数，用于直接渲染倒计时 */
  expires_in_seconds: number
}

/**
 * POST /api/admin/trial-grants 请求体：给全站用户批量发放限时试用额。
 *
 * 金额刻意用「分」：站长按"1 毛钱"思考而不是按"100000 额度"，
 * 分 → 额度的换算由后端按充值比例完成，保证"发放 0.1 元"与"充值 0.1 元"同值。
 */
export interface TrialGrantPayload {
  /** 每位用户获得的金额（分）；0.1 元填 10 */
  amount_cents: number
  /** 自发放时刻起的有效小时数（1..720，上限 30 天） */
  hours: number
  /** 批次标识：同一批次只允许发放一次（防误发双份） */
  batch: string
  /** 必须显式为 true：发放即入用户余额，后端据此拦截误触 */
  confirm: boolean
}

/** POST /api/admin/trial-grants 响应：发放结果回执 */
export interface TrialGrantResult {
  batch: string
  /** 本次实际发放的用户数 */
  recipients: number
  /** 每人获得的额度（站内单位），用于核对与预期一致 */
  amount_quota: number
  /** 每人获得的金额（分），由请求原样回显 */
  amount_cents: number
  hours: number
  /** 到期时间（unix 秒） */
  expires_at: number
}

/** 一条返利明细（GET /api/user/referral/rewards） */
export interface ReferralReward {
  id: number
  /** 奖励类型代码：register / recharge */
  kind: string
  /** 奖励类型中文名（由后端翻译，前端不做代码到名字的映射） */
  kind_text: string
  /** 本次奖励额度 */
  quota: number
  /** 触发奖励的用户名；已由后端脱敏（如 13***@qq.com） */
  invitee: string
  /** 充值返利对应的订单号；注册奖为空串 */
  order_trade_no: string
  created_at: number
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

