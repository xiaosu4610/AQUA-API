/**
 * 管理后台接口（需登录且 role = 10）。
 *
 * 意图（Why）：
 *   仪表盘、渠道、令牌、用户、日志五个管理页面共用这组接口；
 *   与契约「四、管理后台接口」表格一一对应，便于审计「哪些接口已被前端使用」。
 *
 * 流转（Flow）：
 *   views/admin/* → 本文件 → /api/admin/*
 *
 * 扩展（Extend）：
 *   新增管理接口：在此加函数 + 更新 types.ts。
 *   注意：渠道的 update 用 PUT（全量字段）；令牌/用户用 PUT 提交需变更字段（后端应支持部分更新）。
 */
import { api, UPSTREAM_TIMEOUT_MS } from './client'
import type {
  AccessToken,
  AdminUpdateTokenPayload,
  AdminUser,
  Channel,
  ChannelKey,
  ChannelModelMapping,
  ChannelModelMappingItem,
  ChannelPayload,
  ChannelTestResult,
  ChannelTypesResponse,
  CreateRedeemCodesPayload,
  CreateRedeemCodesResult,
  CreateTokenPayload,
  CreateTokenResult,
  CreateUserPayload,
  DashboardStats,
  FetchModelsPayload,
  FetchModelsResult,
  KeyStrategyCatalog,
  KeyFailurePolicyCatalog,
  LimitItem,
  LogQuery,
  ModelGroup,
  ModelGroupPayload,
  ModelMappingOverview,
  ModelPrice,
  ModelPricePayload,
  OAuthProvider,
  OAuthProviderPayload,
  OrderQuery,
  Paged,
  PaymentChannel,
  PaymentOrder,
  QuotePreview,
  RedeemCode,
  RedeemCodeQuery,
  SiteSettings,
  SMTPSettings,
  SpeedTestRunResult,
  SMTPSettingsPayload,
  Task,
  TaskProvider,
  TaskQuery,
  TrialGrantPayload,
  TrialGrantResult,
  UpdateChannelKeyPayload,
  UpdateLimitsPayload,
  UpdateRedeemCodePayload,
  UpdateSiteSettingsPayload,
  UpdateUserPayload,
  UsageLog,
} from './types'

/* ── 仪表盘 ─────────────────────────────────────────────── */

/** GET /api/admin/dashboard：汇总卡片 + 趋势 + Top 模型 */
export function fetchDashboard(): Promise<DashboardStats> {
  return api.get<DashboardStats>('/admin/dashboard')
}

/* ── 渠道 ───────────────────────────────────────────────── */

/** GET /api/admin/channels：渠道列表（响应只有 masked_key） */
export function listChannels(paged: { page?: number; size?: number } = {}): Promise<Paged<Channel>> {
  return api.get<Paged<Channel>>('/admin/channels', paged)
}

/** GET /api/admin/channels/{id}：渠道详情 */
export function getChannel(id: number): Promise<Channel> {
  return api.get<Channel>(`/admin/channels/${id}`)
}

/**
 * GET /api/admin/channel-types：上游渠道类型目录。
 *
 * 返回每种类型的默认地址、鉴权方式、能力位与额外参数定义，
 * 供新建/编辑渠道时做「选类型 → 展开该类型必填项」的触发式渲染。
 * 单独开接口而不是塞进渠道详情：它会被多处复用（类型筛选、批量导入模板等）。
 */
export function fetchChannelTypes(): Promise<ChannelTypesResponse> {
  return api.get<ChannelTypesResponse>('/admin/channel-types')
}

/** POST /api/admin/channels：新建渠道（api_key 为明文，仅存在于请求体） */
export function createChannel(payload: ChannelPayload): Promise<Channel> {
  return api.post<Channel>('/admin/channels', payload)
}

/** PUT /api/admin/channels/{id}：更新渠道（api_key 留空表示不修改） */
export function updateChannel(id: number, payload: Partial<ChannelPayload>): Promise<Channel> {
  return api.put<Channel>(`/admin/channels/${id}`, payload)
}

/** DELETE /api/admin/channels/{id}：删除渠道 */
export function deleteChannel(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/channels/${id}`)
}

/** POST /api/admin/channels/{id}/test：测活（后端会真实发起一次请求，可能较慢） */
export function testChannel(id: number): Promise<ChannelTestResult> {
  // 后端会等上游最多 300 秒（部分平台排队很久），前端必须比它更有耐心，
  // 否则后端还在等、前端已经报"请求超时"，用户得到的是错误结论。
  return api.post<ChannelTestResult>(`/admin/channels/${id}/test`, undefined, {
    timeout: UPSTREAM_TIMEOUT_MS,
  })
}

/**
 * POST /api/admin/channels/{id}/speedtest：模型测速（逐模型测首字延迟）。
 *
 * 每次对上游产生真实但极小的消耗（提示词 "ping" + max_tokens=1 ≈ 2~3 token/模型）。
 * 前端为拿到实时进度，通常一次只传一个模型并循环调用；后端串行探测。
 */
export function speedTestChannel(id: number, models: string[]): Promise<SpeedTestRunResult> {
  return api.post<SpeedTestRunResult>(`/admin/channels/${id}/speedtest`, { models }, {
    timeout: UPSTREAM_TIMEOUT_MS,
  })
}

/* ── 令牌 ───────────────────────────────────────────────── */

/** GET /api/admin/tokens：全部令牌 */
export function listAllTokens(paged: { page?: number; size?: number } = {}): Promise<Paged<AccessToken>> {
  return api.get<Paged<AccessToken>>('/admin/tokens', paged)
}

/** POST /api/admin/tokens：为指定用户创建令牌（响应含一次性明文 key） */
export function createTokenForUser(payload: CreateTokenPayload): Promise<CreateTokenResult> {
  return api.post<CreateTokenResult>('/admin/tokens', payload)
}

/** PUT /api/admin/tokens/{id}：更新令牌（改名 / 启停 / 额度 / 到期） */
export function updateToken(id: number, payload: AdminUpdateTokenPayload): Promise<AccessToken> {
  return api.put<AccessToken>(`/admin/tokens/${id}`, payload)
}

/** DELETE /api/admin/tokens/{id}：删除令牌 */
export function deleteToken(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/tokens/${id}`)
}

/**
 * GET /api/admin/tokens/{id}/key：取该令牌的明文密钥（管理员专用）。
 *
 * 用于后台"代客户复制/找回密钥"。该接口挂在 RequireAdmin 之后，天然是管理员专属。
 */
export function getTokenKey(id: number): Promise<{ id: number; key: string }> {
  return api.get<{ id: number; key: string }>(`/admin/tokens/${id}/key`)
}

/* ── 用户 ───────────────────────────────────────────────── */

/** GET /api/admin/users：用户列表 */
export function listUsers(paged: { page?: number; size?: number } = {}): Promise<Paged<AdminUser>> {
  return api.get<Paged<AdminUser>>('/admin/users', paged)
}

/** POST /api/admin/users：新建用户 */
export function createUser(payload: CreateUserPayload): Promise<AdminUser> {
  return api.post<AdminUser>('/admin/users', payload)
}

/** PUT /api/admin/users/{id}：更新用户（含调整额度） */
export function updateUser(id: number, payload: UpdateUserPayload): Promise<AdminUser> {
  return api.put<AdminUser>(`/admin/users/${id}`, payload)
}

/** DELETE /api/admin/users/{id}：删除用户 */
export function deleteUser(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/users/${id}`)
}

/* ── 限时试用额度 ───────────────────────────────────────── */

/**
 * POST /api/admin/trial-grants：给全站用户批量发放限时试用额。
 *
 * 为什么单独成一个函数而不是混进 updateUser：
 *   这是一次「给所有人加钱」的批量写操作，只对「启用且额度非不限」的用户生效，
 *   金额按分计（amount_cents），换算由后端按充值比例完成。
 *   后端要求 confirm=true 且批次唯一（同一批次只发一次），
 *   前端务必先做二次确认弹层再调用，并提示用户记住批次名。
 */
export function grantTrialQuota(payload: TrialGrantPayload): Promise<TrialGrantResult> {
  return api.post<TrialGrantResult>('/admin/trial-grants', payload)
}

/* ── 调用日志 ───────────────────────────────────────────── */

/** GET /api/admin/logs：全站调用日志（分页 + 多条件筛选） */
export function listAllLogs(query: LogQuery): Promise<Paged<UsageLog>> {
  return api.get<Paged<UsageLog>>('/admin/logs', { ...query })
}

/* ── 系统设置 ───────────────────────────────────────────── */

/** GET /api/admin/settings：读取系统设置（含邮件通道是否就绪） */
export function fetchSettings(): Promise<SiteSettings> {
  return api.get<SiteSettings>('/admin/settings')
}

/**
 * PUT /api/admin/settings：更新系统设置。
 *
 * 只提交变更字段：后端对未提交项保持原值，避免"改一项清空其他项"。
 */
export function updateSettings(payload: UpdateSiteSettingsPayload): Promise<unknown> {
  return api.put<unknown>('/admin/settings', payload)
}

/**
 * 读取支付通道清单（字段描述 + 当前值 + 密钥就绪状态）。
 *
 * 不单开接口：后端已把它放在 /admin/settings 的 payment_channels 字段里，
 * 这里只是从同一份响应里取出，避免页面为了通道清单再发一次请求
 * （设置页本来就要拉 settings，多一次往返纯属浪费）。
 */
export async function fetchPaymentChannels(): Promise<PaymentChannel[]> {
  const settings = await fetchSettings()
  return settings.payment_channels ?? []
}

/* ── 邮件通道（SMTP）─────────────────────────────────────── */

/**
 * GET /api/admin/smtp：读取邮件通道配置状态。
 *
 * 响应不含口令（后端永不回传），只给 password_set 表示"是否已配置"。
 */
export function fetchSMTP(): Promise<SMTPSettings> {
  return api.get<SMTPSettings>('/admin/smtp')
}

/**
 * PUT /api/admin/smtp：保存邮件通道配置。
 *
 * 保存成功后后端会立即热加载发送器（无需重启进程）；
 * password 留空表示沿用已保存的口令（避免"改端口却清空口令"）。
 */
export function updateSMTP(payload: SMTPSettingsPayload): Promise<SMTPSettings> {
  return api.put<SMTPSettings>('/admin/smtp', payload)
}

/**
 * POST /api/admin/smtp/test：发送测试邮件。
 *
 * 失败时后端返回 502 与具体原因（认证失败/端口被拒等），前端应原样展示，
 * 这正是站长排查发信问题最需要的信息。to 留空则发给发件地址。
 */
export function testSMTP(to = ''): Promise<{ ok: boolean; to: string }> {
  return api.post<{ ok: boolean; to: string }>('/admin/smtp/test', { to })
}

/* ── 运行上限（超管可调）────────────────────────────────── */

/**
 * GET /api/admin/limits：读取运行上限（当前值 + 默认值 + 合法区间）。
 *
 * 与 /admin/settings 分开：这里只放数值型"保护性上限"，每项都带默认值与区间，
 * 前端据此渲染表单并限制输入，不硬编码任何数值。
 */
export function fetchLimits(): Promise<{ items: LimitItem[] }> {
  return api.get<{ items: LimitItem[] }>('/admin/limits')
}

/**
 * PUT /api/admin/limits：更新运行上限（只提交变更字段）。
 *
 * 后端对越界值返回 400 与明确原因（含合法区间），前端应原样展示给管理员。
 */
export function updateLimits(payload: UpdateLimitsPayload): Promise<unknown> {
  return api.put<unknown>('/admin/limits', payload)
}

/* ── 渠道模型 ID 映射 ───────────────────────────────────── */

/**
 * GET /api/admin/channels/{id}/mappings：读取某渠道的模型 ID 映射。
 *
 * 映射语义：public_model（平台模型 ID，用户调用时用）↔ upstream_model（实际发给上游的名字）。
 * 不配置映射时模型名原样透传。
 */
export function listChannelMappings(channelId: number): Promise<{ items: ChannelModelMapping[]; total: number }> {
  return api.get<{ items: ChannelModelMapping[]; total: number }>(`/admin/channels/${channelId}/mappings`)
}

/**
 * PUT /api/admin/channels/{id}/mappings：整组替换该渠道的映射。
 *
 * 为什么是整组替换而不是逐条增删：界面是一张表一次性提交，
 * 整组替换能保证「界面所见 = 落库结果」，不会留下中间态或孤儿行。
 */
export function replaceChannelMappings(
  channelId: number,
  items: ChannelModelMappingItem[],
): Promise<{ items: ChannelModelMapping[]; total: number }> {
  return api.put<{ items: ChannelModelMapping[]; total: number }>(`/admin/channels/${channelId}/mappings`, { items })
}

/* ── 上游模型列表 ───────────────────────────────────────── */

/**
 * GET /api/admin/model-mappings：全站模型 ID 映射总览。
 *
 * 跨渠道聚合，回答「我的平台有哪些模型 ID、分别映射到哪个上游 ID」；
 * 同时给出没有配置映射的渠道（它们把模型名原样透传给上游）。
 */
export function fetchModelMappingOverview(): Promise<ModelMappingOverview> {
  return api.get<ModelMappingOverview>('/admin/model-mappings')
}

/**
 * POST /api/admin/fetch-models：向上游查询可用模型列表。
 *
 * 两种用法：
 *   - 传 channel_id：用该渠道已保存的地址与密钥（适用于已配好的渠道同步清单）；
 *   - 传 base_url + api_key：用于"还没保存就想先看看有哪些模型"。
 *
 * 为什么不挂在 /channels 下：后端路由树中 `/channels/fetch-models` 会与
 * `/channels/:id` 冲突，因此放在管理端顶层路径。
 */
export function fetchUpstreamModels(payload: FetchModelsPayload): Promise<FetchModelsResult> {
  // 与测活同理：拉取模型列表要等上游回答（部分平台很慢），
  // 前端超时必须放宽到与后端一致的量级。
  return api.post<FetchModelsResult>('/admin/fetch-models', payload, {
    timeout: UPSTREAM_TIMEOUT_MS,
  })
}

/* ── 渠道密钥池 ─────────────────────────────────────────── */

/** GET /api/admin/channels/{id}/keys：读取某渠道的密钥池明细（只含掩码） */
export function listChannelKeys(channelId: number): Promise<{ items: ChannelKey[]; total: number }> {
  return api.get<{ items: ChannelKey[]; total: number }>(`/admin/channels/${channelId}/keys`)
}

/**
 * PUT /api/admin/keys/{keyId}：修改单把密钥的状态与调度参数。
 *
 * 路径沿用原有的状态更新接口（未破坏既有调用），只是扩展了可接受的字段：
 *   - 仅传 status：启用 / 禁用 / 恢复；
 *   - 传 weight + priority + rpm_limit：更新调度参数（三项必须同时提供）。
 */
export function updateChannelKey(keyId: number, payload: UpdateChannelKeyPayload): Promise<unknown> {
  return api.put<unknown>(`/admin/keys/${keyId}`, payload)
}

/**
 * GET /api/admin/key-strategies：凭据调度策略目录。
 *
 * 返回每种策略的标识、中文名与一句话说明，供渠道表单渲染下拉与帮助文案
 * （文案由后端下发，前端不硬编码，新增策略时前端无需改动）。
 */
export function fetchKeyStrategies(): Promise<KeyStrategyCatalog> {
  return api.get<KeyStrategyCatalog>('/admin/key-strategies')
}

/**
 * GET /api/admin/key-failure-policies：密钥失败处置策略目录。
 *
 * 同时返回冷却时长上限与默认策略，供表单限制输入并给出准确提示
 * （避免站长填一个过长时长，把"只冷却"变成事实上的摘除）。
 */
export function fetchKeyFailurePolicies(): Promise<KeyFailurePolicyCatalog> {
  return api.get<KeyFailurePolicyCatalog>('/admin/key-failure-policies')
}

/* ── 模型分组 ───────────────────────────────────────────── */

/**
 * GET /api/admin/groups：分组列表（含引用统计）。
 *
 * 后端不分页（分组数量极少），返回 { items, total }。
 */
export function listGroups(): Promise<{ items: ModelGroup[]; total: number }> {
  return api.get<{ items: ModelGroup[]; total: number }>('/admin/groups')
}

/** POST /api/admin/groups：新建分组（标识强制小写） */
export function createGroup(payload: ModelGroupPayload): Promise<ModelGroup> {
  return api.post<ModelGroup>('/admin/groups', payload)
}

/** PUT /api/admin/groups/{id}：更新分组（标识不可改，倍率改完立即生效） */
export function updateGroup(id: number, payload: Partial<ModelGroupPayload>): Promise<ModelGroup> {
  return api.put<ModelGroup>(`/admin/groups/${id}`, payload)
}

/** DELETE /api/admin/groups/{id}：删除分组（仍被渠道/价格引用时会被拒绝） */
export function deleteGroup(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/groups/${id}`)
}

/* ── 计价规则 ───────────────────────────────────────────── */

/** GET /api/admin/prices：计价规则列表（可按分组过滤） */
export function listPrices(group = ''): Promise<{ items: ModelPrice[]; total: number }> {
  return api.get<{ items: ModelPrice[]; total: number }>('/admin/prices', { group })
}

/** POST /api/admin/prices：新增计价规则 */
export function createPrice(payload: ModelPricePayload): Promise<ModelPrice> {
  return api.post<ModelPrice>('/admin/prices', payload)
}

/** PUT /api/admin/prices/{id}：更新计价规则 */
export function updatePrice(id: number, payload: ModelPricePayload): Promise<ModelPrice> {
  return api.put<ModelPrice>(`/admin/prices/${id}`, payload)
}

/** DELETE /api/admin/prices/{id}：删除计价规则（删除后该模型变为不计费） */
export function deletePrice(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/prices/${id}`)
}

/** GET /api/admin/prices/quote：费用试算（核对定价是否合理） */
export function quotePrice(
  model: string,
  promptTokens: number,
  completionTokens: number,
  cachedTokens = 0,
): Promise<QuotePreview> {
  return api.get<QuotePreview>('/admin/prices/quote', {
    model,
    prompt_tokens: promptTokens,
    completion_tokens: completionTokens,
    cached_tokens: cachedTokens,
  })
}

/* ── 异步任务 ───────────────────────────────────────────── */

/** GET /api/admin/tasks：全站异步任务（可按用户 / 类别 / 状态过滤） */
export function listAllTasks(
  query: TaskQuery & { user_id?: number } = {},
): Promise<Paged<Task>> {
  return api.get<Paged<Task>>('/admin/tasks', { ...query })
}

/** POST /api/admin/tasks/{ref}/cancel：取消任务并退还额度 */
export function cancelTask(taskRef: string): Promise<Task> {
  return api.post<Task>(`/admin/tasks/${encodeURIComponent(taskRef)}/cancel`)
}

/** GET /api/admin/task-providers：已注册的上游任务适配器（供表单提示可选 provider） */
export function listTaskProviders(): Promise<{ items: TaskProvider[] }> {
  return api.get<{ items: TaskProvider[] }>('/admin/task-providers')
}

/* ── 充值订单 ───────────────────────────────────────────── */

/** GET /api/admin/orders：全站充值订单 */
export function listAllOrders(query: OrderQuery = {}): Promise<Paged<PaymentOrder>> {
  return api.get<Paged<PaymentOrder>>('/admin/orders', { ...query })
}

/**
 * POST /api/admin/orders/{tradeNo}/mark-paid：人工确认入账。
 *
 * 两种用途：人工确认通道的核心动作；以及"用户已付款但回调丢失"的补救。
 * 后端是幂等的，重复点击不会重复加额度。
 */
export function markOrderPaid(tradeNo: string): Promise<PaymentOrder> {
  return api.post<PaymentOrder>(`/admin/orders/${encodeURIComponent(tradeNo)}/mark-paid`)
}

/** POST /api/admin/orders/{tradeNo}/close：关闭待支付订单 */
export function closeOrder(tradeNo: string): Promise<PaymentOrder> {
  return api.post<PaymentOrder>(`/admin/orders/${encodeURIComponent(tradeNo)}/close`)
}

/* ── OAuth 提供方 ───────────────────────────────────────── */

/** GET /api/admin/oauth-providers：订阅账号刷新令牌所需的提供方配置 */
export function listOAuthProviders(): Promise<{ items: OAuthProvider[]; total: number }> {
  return api.get<{ items: OAuthProvider[]; total: number }>('/admin/oauth-providers')
}

/** POST /api/admin/oauth-providers：新增提供方配置 */
export function createOAuthProvider(payload: OAuthProviderPayload): Promise<OAuthProvider> {
  return api.post<OAuthProvider>('/admin/oauth-providers', payload)
}

/** PUT /api/admin/oauth-providers/{id}：更新提供方配置（client_secret 留空表示不修改） */
export function updateOAuthProvider(
  id: number,
  payload: Partial<OAuthProviderPayload>,
): Promise<OAuthProvider> {
  return api.put<OAuthProvider>(`/admin/oauth-providers/${id}`, payload)
}

/** DELETE /api/admin/oauth-providers/{id}：删除提供方配置 */
export function deleteOAuthProvider(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/oauth-providers/${id}`)
}

/* ── 兑换码 ─────────────────────────────────────────────── */

/**
 * GET /api/admin/redeem-codes：兑换码列表（分页 + 状态/关键词/批次筛选）。
 *
 * 筛选参数由后端逐条解析：status 为空串时不传（视为全部），
 * 因此这里把"未选择"统一表示为 undefined，交由 client 的 cleanParams 剔除。
 */
export function listRedeemCodes(query: RedeemCodeQuery = {}): Promise<Paged<RedeemCode>> {
  return api.get<Paged<RedeemCode>>('/admin/redeem-codes', { ...query })
}

/** POST /api/admin/redeem-codes：批量生成兑换码（响应含本批全部明文码，供导出分发） */
export function createRedeemCodes(payload: CreateRedeemCodesPayload): Promise<CreateRedeemCodesResult> {
  return api.post<CreateRedeemCodesResult>('/admin/redeem-codes', payload)
}

/** PUT /api/admin/redeem-codes/{id}：改状态 / 备注（只提交变更字段，避免误改另一项） */
export function updateRedeemCode(id: number, payload: UpdateRedeemCodePayload): Promise<unknown> {
  return api.put<unknown>(`/admin/redeem-codes/${id}`, payload)
}

/** DELETE /api/admin/redeem-codes/{id}：删除单张兑换码 */
export function deleteRedeemCode(id: number): Promise<unknown> {
  return api.delete<unknown>(`/admin/redeem-codes/${id}`)
}

/**
 * DELETE /api/admin/redeem-codes/invalid：清理失效兑换码（已使用 / 已过期）。
 *
 * 响应返回被清理的条数，前端据此给出"清理了 N 条"的明确反馈
 * （而不是笼统的"操作成功"，管理员无法判断是否真的删掉了东西）。
 */
export function deleteInvalidRedeemCodes(): Promise<{ deleted: number }> {
  return api.delete<{ deleted: number }>('/admin/redeem-codes/invalid')
}
