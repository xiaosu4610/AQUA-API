<script setup lang="ts">
/**
 * 管理后台 · 渠道管理：列表 + 新建/编辑抽屉 + 测活 + 启停 + 删除。
 *
 * 意图（Why）：
 *   渠道是网关的「上游能力来源」，配置项最多（协议、地址、密钥、模型、分组、优先级、权重），
 *   因此用右侧抽屉承载表单，让管理员在填表时仍能看到列表上下文；
 *   测活则直接内联在表格里展示延迟与结果，避免「点一下、跳个提示、不知道成功没」。
 *
 * 流转（Flow）：
 *   列表：listChannels({page,size}) → 表格
 *   新建：抽屉表单 → createChannel(payload) → 重新拉取列表
 *   编辑：打开抽屉时先 GET /channels/{id} 取详情（列表里没有明文密钥，但需最新配置）→ updateChannel
 *   测活：testChannel(id) → 结果写入 testResults 映射（含延迟）→ 就地更新该行状态
 *   启停：updateChannel(id, { status })（契约中更新使用 PUT，这里提交完整对象以免字段被清空）
 *
 * 扩展（Extend）：
 *   新增渠道字段：同步 api/types.ts 的 ChannelPayload 与此处表单（两处必须一致）。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Drawer from '@/components/Drawer.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import {
  createChannel,
  deleteChannel,
  fetchUpstreamModels,
  getChannel,
  listChannelKeys,
  listChannels,
  testChannel,
  updateChannel,
  updateChannelKeyStatus,
} from '@/api/admin'
import {
  KEY_STATUS_AUTO_REMOVED,
  KEY_STATUS_DISABLED,
  KEY_STATUS_ENABLED,
  STATUS_DISABLED,
  STATUS_ENABLED,
  type Channel,
  type ChannelKey,
  type ChannelPayload,
  type ChannelTestResult,
  type FetchModelsPayload,
} from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { useSiteStore } from '@/stores/site'
import { channelTypeLabel, statusBadgeClass } from '@/utils/display'
import { formatDateTime, joinModelList, parseModelList } from '@/utils/format'

const site = useSiteStore()

const channels = ref<Channel[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')

/** 行级操作忙碌标记（避免同一行被重复点击） */
const busyId = ref<number | null>(null)
/** 测活结果：按渠道 id 缓存，用于在表格内展示延迟与结论 */
const testResults = ref<Record<number, ChannelTestResult>>({})
const testingId = ref<number | null>(null)

/* ── 上游模型拉取状态 ─────────────────────────────────── */
/** 是否正在拉取模型清单 */
const fetchingModels = ref(false)
/** 上游返回的模型清单（用于勾选） */
const upstreamModels = ref<string[]>([])
/** 拉取失败原因 */
const upstreamError = ref('')

/* ── 密钥池明细状态 ───────────────────────────────────── */
const keysDrawerOpen = ref(false)
/** 当前查看密钥池的渠道 */
const keysOfChannel = ref<Channel | null>(null)
const channelKeys = ref<ChannelKey[]>([])
const keysLoading = ref(false)
const keysError = ref('')
/** 正在切换状态的密钥 id（避免重复点击） */
const keyBusyId = ref<number | null>(null)

/* ── 列表 ─────────────────────────────────────────────── */

async function loadChannels(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listChannels({ page: page.value, size: size.value })
    channels.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    channels.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '渠道列表加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(loadChannels)

function changePage(next: number): void {
  page.value = next
  void loadChannels()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void loadChannels()
}

/* ── 表单（新建 / 编辑共用抽屉）───────────────────────── */

interface ChannelForm {
  name: string
  type: number
  base_url: string
  /** 明文密钥：新建时必填；编辑时留空表示不修改 */
  api_key: string
  /**
   * 批量密钥文本：每行一把，支持行内备注（空格或逗号分隔）。
   *
   * 与 api_key 的关系：两者都填时以密钥池为准（池化优先）；
   * 只填 api_key 走单密钥模式；只填批量密钥走池化轮询模式。
   */
  keysText: string
  modelText: string
  group: string
  priority: number
  weight: number
  status: number
}

/** 表单默认值：优先级 10、权重 1、启用，符合常见「默认可用」预期 */
function emptyChannelForm(): ChannelForm {
  return {
    name: '',
    type: 1,
    base_url: '',
    api_key: '',
    keysText: '',
    modelText: '',
    group: 'default',
    priority: 10,
    weight: 1,
    status: STATUS_ENABLED,
  }
}

const drawerOpen = ref(false)
const editing = ref<Channel | null>(null)
const form = ref<ChannelForm>(emptyChannelForm())
const formError = ref('')
const saving = ref(false)

const drawerTitle = computed(() => (editing.value ? `编辑渠道 · ${editing.value.name}` : '新建渠道'))

/**
 * 已选模型的集合（用于勾选清单的高亮判断）。
 *
 * 用 computed + Set 而不是在模板里每次 parseModelList：
 * 上游可能有几百个模型，若每次渲染都对文本框做一次解析，
 * 勾选时会明显卡顿。
 */
const selectedModelSet = computed(() => new Set(parseModelList(form.value.modelText)))

/** 打开新建抽屉 */
function openCreate(): void {
  editing.value = null
  form.value = emptyChannelForm()
  formError.value = ''
  // 清空上一次的上游模型缓存，避免把 A 上游的模型误选到 B 渠道
  upstreamModels.value = []
  upstreamError.value = ''
  drawerOpen.value = true
}

/**
 * 打开编辑抽屉：先取详情再填充。
 * 为什么不用列表行数据：列表可能被其他管理员改过，取详情能确保编辑的是最新配置。
 */
async function openEdit(channel: Channel): Promise<void> {
  editing.value = channel
  form.value = {
    name: channel.name,
    type: channel.type,
    base_url: channel.base_url,
    api_key: '',
    keysText: '',
    modelText: joinModelList(channel.models),
    group: channel.group || 'default',
    priority: channel.priority,
    weight: channel.weight,
    status: channel.status,
  }
  formError.value = ''
  drawerOpen.value = true

  try {
    const detail = await getChannel(channel.id)
    editing.value = detail
    form.value = {
      name: detail.name,
      type: detail.type,
      base_url: detail.base_url,
      api_key: '',
      keysText: '',
      modelText: joinModelList(detail.models),
      group: detail.group || 'default',
      priority: detail.priority,
      weight: detail.weight,
      status: detail.status,
    }
  } catch (err) {
    // 取详情失败不阻断编辑：至少列表数据可用，但提示用户
    toastError(err instanceof ApiError ? err.message : '渠道详情加载失败，已使用列表数据')
  }
}

function validateForm(): string | null {
  if (!form.value.name.trim()) return '请填写渠道名称'
  if (!form.value.base_url.trim()) return '请填写上游 Base URL'
  if (!/^https?:\/\//i.test(form.value.base_url.trim())) return 'Base URL 需以 http:// 或 https:// 开头'
  // 新建时必须至少提供一种密钥：单密钥或批量密钥池
  if (!editing.value && !form.value.api_key.trim() && !form.value.keysText.trim()) {
    return '新建渠道必须填写密钥（单密钥或批量密钥至少填一项）'
  }
  if (form.value.priority < 0) return '优先级不能为负数'
  if (form.value.weight <= 0) return '权重必须大于 0'
  return null
}

/**
 * 从表单里推测一把可用于上游鉴权的密钥。
 *
 * 为什么要"推测"：拉取模型清单需要真实密钥，而用户可能只填了批量密钥框
 * （还没保存渠道）。这里取第一行有效密钥的首个字段作为探测用密钥。
 */
function firstKeyFromText(text: string): string {
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    // 与后端解析规则保持一致：优先按逗号/制表符切分，其次按空格
    const [head] = trimmed.split(/[,，\t\s]/, 1)
    if (head) return head
  }
  return ''
}

/** 从上游拉取模型列表，并展示为可勾选清单 */
async function pullModels(): Promise<void> {
  upstreamError.value = ''
  const baseURL = form.value.base_url.trim()
  const apiKey = form.value.api_key.trim() || firstKeyFromText(form.value.keysText)

  const payload: FetchModelsPayload = {}
  if (editing.value && !apiKey) {
    // 编辑已有渠道且表单里没有新密钥：让后端用库里保存的地址与密钥池
    payload.channel_id = editing.value.id
  } else {
    if (!baseURL) {
      upstreamError.value = '请先填写上游 Base URL'
      return
    }
    payload.base_url = baseURL
    if (apiKey) payload.api_key = apiKey
  }

  fetchingModels.value = true
  try {
    const result = await fetchUpstreamModels(payload)
    upstreamModels.value = result.models ?? []
    toastSuccess(`已从上游拉取 ${result.count} 个模型`)
  } catch (err) {
    upstreamModels.value = []
    upstreamError.value = err instanceof ApiError ? err.message : '拉取模型列表失败'
  } finally {
    fetchingModels.value = false
  }
}

/** 勾选/取消勾选某个模型 */
function toggleModel(name: string): void {
  const current = parseModelList(form.value.modelText)
  form.value.modelText = current.includes(name)
    ? current.filter((item) => item !== name).join(', ')
    : [...current, name].join(', ')
}

/** 一键选中上游返回的全部模型 */
function selectAllModels(): void {
  form.value.modelText = upstreamModels.value.join(', ')
}

/** 清空模型声明（等价于"支持全部模型"） */
function clearModels(): void {
  form.value.modelText = ''
}

/* ── 密钥池明细 ───────────────────────────────────────── */

/** 打开某渠道的密钥池抽屉 */
async function openKeys(channel: Channel): Promise<void> {
  keysOfChannel.value = channel
  keysDrawerOpen.value = true
  await loadChannelKeys(channel.id)
}

/** 读取密钥池明细（只含掩码） */
async function loadChannelKeys(channelId: number): Promise<void> {
  keysLoading.value = true
  keysError.value = ''
  try {
    const result = await listChannelKeys(channelId)
    channelKeys.value = result.items ?? []
  } catch (err) {
    channelKeys.value = []
    keysError.value = err instanceof ApiError ? err.message : '密钥列表加载失败'
  } finally {
    keysLoading.value = false
  }
}

/** 启用 / 禁用 / 恢复某把密钥 */
async function setKeyStatus(key: ChannelKey, status: number): Promise<void> {
  keyBusyId.value = key.id
  try {
    await updateChannelKeyStatus(key.id, status)
    toastSuccess(`密钥已${status === KEY_STATUS_ENABLED ? '启用' : '禁用'}`)
    if (keysOfChannel.value) await loadChannelKeys(keysOfChannel.value.id)
    // 池内可用密钥数会影响列表展示，一并刷新
    await loadChannels()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    keyBusyId.value = null
  }
}

/** 密钥状态样式 */
function keyStatusClass(status: number): string {
  if (status === KEY_STATUS_ENABLED) return 'badge badge-ok'
  if (status === KEY_STATUS_AUTO_REMOVED) return 'badge badge-warn'
  return 'badge badge-off'
}

/** 密钥池按状态统计（抽屉顶部概览用） */
const keyStats = computed(() => {
  const stats = { enabled: 0, disabled: 0, removed: 0 }
  for (const key of channelKeys.value) {
    if (key.status === KEY_STATUS_ENABLED) stats.enabled += 1
    else if (key.status === KEY_STATUS_AUTO_REMOVED) stats.removed += 1
    else stats.disabled += 1
  }
  return stats
})

/** 渠道在表格"密钥"列展示的文案 */
function keyColumnText(channel: Channel): string {
  const pool = channel.key_pool
  if (pool && pool.total > 0) {
    return `池 ${pool.total} 把（可用 ${pool.enabled}）`
  }
  return channel.masked_key || '未配置'
}

async function submitForm(): Promise<void> {
  const invalid = validateForm()
  if (invalid) {
    formError.value = invalid
    return
  }

  const payload: ChannelPayload = {
    name: form.value.name.trim(),
    type: Number(form.value.type) || 1,
    base_url: form.value.base_url.trim(),
    models: parseModelList(form.value.modelText),
    group: form.value.group.trim() || 'default',
    priority: Number(form.value.priority) || 0,
    weight: Number(form.value.weight) || 1,
    status: form.value.status,
  }
  // 编辑时密钥留空表示「不修改」，因此不发送该字段（避免把密钥覆盖为空）
  if (form.value.api_key.trim()) payload.api_key = form.value.api_key.trim()
  // 批量密钥同理：留空即不动密钥池，防止"只改个名字却清空了 500 把密钥"
  if (form.value.keysText.trim()) payload.keys_text = form.value.keysText

  saving.value = true
  formError.value = ''
  try {
    if (editing.value) {
      await updateChannel(editing.value.id, payload)
      toastSuccess('渠道已更新')
    } else {
      await createChannel(payload)
      toastSuccess('渠道已创建')
    }
    drawerOpen.value = false
    await loadChannels()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败，请稍后重试'
  } finally {
    saving.value = false
  }
}

/* ── 测活 / 启停 / 删除 ───────────────────────────────── */

async function runTest(channel: Channel): Promise<void> {
  testingId.value = channel.id
  try {
    const result = await testChannel(channel.id)
    testResults.value = { ...testResults.value, [channel.id]: result }
    if (result.ok) {
      toastSuccess(`「${channel.name}」连通正常，延迟 ${result.latency_ms} ms`)
    } else {
      toastError(`「${channel.name}」测活失败：${result.message || `HTTP ${result.status_code}`}`)
    }
    // 测活会更新渠道的测试时间与状态，重新拉取以保证表格与后端一致
    await loadChannels()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '测活请求失败')
  } finally {
    testingId.value = null
  }
}

async function toggleStatus(channel: Channel): Promise<void> {
  const nextStatus = channel.status === STATUS_ENABLED ? STATUS_DISABLED : STATUS_ENABLED
  busyId.value = channel.id
  try {
    // 契约中渠道更新为 PUT（全量），因此提交当前渠道的完整配置，仅替换 status
    await updateChannel(channel.id, {
      name: channel.name,
      type: channel.type,
      base_url: channel.base_url,
      models: channel.models,
      group: channel.group,
      priority: channel.priority,
      weight: channel.weight,
      status: nextStatus,
    })
    toastSuccess(nextStatus === STATUS_ENABLED ? '渠道已启用' : '渠道已停用')
    await loadChannels()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    busyId.value = null
  }
}

async function removeChannel(channel: Channel): Promise<void> {
  const ok = await confirmDialog({
    title: '删除渠道',
    message: `删除「${channel.name}」后，该渠道不再参与请求调度；依赖该渠道的模型可能因此不可用。`,
    confirmText: '删除渠道',
    danger: true,
  })
  if (!ok) return

  busyId.value = channel.id
  try {
    await deleteChannel(channel.id)
    toastSuccess('渠道已删除')
    if (channels.value.length === 1 && page.value > 1) page.value -= 1
    await loadChannels()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    busyId.value = null
  }
}

const isEmpty = computed(() => !loading.value && !error.value && channels.value.length === 0)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">渠道管理</h2>
        <p class="page-desc">渠道决定请求可以转发到哪些上游。密钥仅在提交时发送，列表只显示掩码。</p>
      </div>
      <div class="toolbar">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadChannels">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="15" />
          新建渠道
        </button>
      </div>
    </div>

    <div class="table-wrap">
      <table class="data-table min-w-[1220px]">
        <thead>
          <tr>
            <th>名称</th>
            <th>类型</th>
            <th>Base URL</th>
            <th>密钥</th>
            <th class="text-right">模型</th>
            <th>分组</th>
            <th class="text-right">优先级</th>
            <th class="text-right">权重</th>
            <th>状态</th>
            <th>最近测活</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>

        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="isEmpty"
            :colspan="11"
            loading-text="正在加载渠道列表…"
            empty-text="还没有配置渠道"
            empty-hint="添加第一个上游渠道后，平台才能对外提供模型服务。"
            @retry="loadChannels"
          >
            <template #action>
              <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
                <AppIcon name="plus" :size="14" />
                新建渠道
              </button>
            </template>
          </DataState>

          <template v-if="!loading && !error && channels.length">
            <tr v-for="channel in channels" :key="channel.id">
              <td class="font-medium text-ink-100">
                <span class="flex items-center gap-2">
                  {{ channel.name }}
                  <span v-if="channel.last_test_ok === false" class="text-amber-700" title="上次测活失败">
                    <AppIcon name="alert" :size="14" />
                  </span>
                </span>
              </td>

              <td class="whitespace-nowrap text-ink-300">{{ channelTypeLabel(channel.type) }}</td>

              <td class="max-w-[16rem]">
                <span class="block truncate font-mono text-xs text-ink-300" :title="channel.base_url">
                  {{ channel.base_url }}
                </span>
              </td>

              <td>
                <!-- 配了密钥池的渠道：显示池概况并可点开看明细（含失效密钥） -->
                <button
                  v-if="channel.key_pool && channel.key_pool.total > 0"
                  type="button"
                  class="chip transition hover:border-brand-500/40 hover:text-brand-700"
                  title="查看密钥池明细"
                  @click="openKeys(channel)"
                >
                  {{ keyColumnText(channel) }}
                </button>
                <code v-else class="chip">{{ channel.masked_key || '未配置' }}</code>
              </td>

              <td class="cell-num">
                <span v-if="channel.models && channel.models.length" :title="channel.models.join(', ')">
                  {{ channel.models.length }}
                </span>
                <span v-else class="text-xs text-ink-400">全部</span>
              </td>

              <td class="whitespace-nowrap text-ink-300">{{ channel.group || '—' }}</td>
              <td class="cell-num">{{ channel.priority }}</td>
              <td class="cell-num">{{ channel.weight }}</td>

              <td>
                <span :class="statusBadgeClass(channel.status)">
                  <span class="dot" />
                  {{ channel.status_text || (channel.status === STATUS_ENABLED ? '启用' : '停用') }}
                </span>
              </td>

              <td class="whitespace-nowrap">
                <!-- 测活完成后即时展示延迟与结论，无需去日志里找 -->
                <span v-if="testResults[channel.id]" class="flex items-center gap-1.5">
                  <span :class="testResults[channel.id].ok ? 'badge badge-ok' : 'badge badge-err'">
                    {{ testResults[channel.id].ok ? '通过' : '失败' }}
                  </span>
                  <span class="font-mono text-[11px] text-ink-300">{{ testResults[channel.id].latency_ms }} ms</span>
                </span>
                <span v-else-if="channel.last_test_at" class="cell-muted">
                  {{ channel.last_test_ok === false ? '上次失败' : '上次通过' }}
                </span>
                <span v-else class="cell-muted">未测活</span>
              </td>

              <td class="cell-actions">
                <div class="flex items-center justify-end gap-1">
                  <button
                    type="button"
                    class="btn btn-row"
                    title="测活（真实发起一次请求）"
                    :disabled="testingId === channel.id"
                    @click="runTest(channel)"
                  >
                    <span
                      v-if="testingId === channel.id"
                      class="mx-auto block h-3.5 w-3.5 animate-spin rounded-full border-2 border-ink-600 border-t-brand-400"
                    />
                    <AppIcon v-else name="play" :size="14" />
                  </button>

                  <button type="button" class="btn btn-row" title="编辑" @click="openEdit(channel)">
                    <AppIcon name="edit" :size="14" />
                  </button>

                  <button
                    type="button"
                    class="btn btn-row"
                    :title="channel.status === STATUS_ENABLED ? '停用' : '启用'"
                    :disabled="busyId === channel.id"
                    @click="toggleStatus(channel)"
                  >
                    <AppIcon :name="channel.status === STATUS_ENABLED ? 'lock' : 'bolt'" :size="14" />
                  </button>

                  <button
                    type="button"
                    class="btn btn-row text-ink-400 hover:text-red-700"
                    title="删除"
                    :disabled="busyId === channel.id"
                    @click="removeChannel(channel)"
                  >
                    <AppIcon name="trash" :size="14" />
                  </button>
                </div>
              </td>
            </tr>
          </template>
        </tbody>
      </table>

      <Pagination
        v-if="total > 0"
        :page="page"
        :size="size"
        :total="total"
        :disabled="loading"
        @update:page="changePage"
        @update:size="changeSize"
      />
    </div>

    <!-- 新建 / 编辑抽屉 -->
    <Drawer
      :open="drawerOpen"
      :title="drawerTitle"
      subtitle="密钥仅用于上游鉴权，保存后接口只返回掩码。"
      @close="drawerOpen = false"
    >
      <div class="space-y-5">
        <div>
          <label class="label" for="channel-name">渠道名称 <span class="text-red-600">*</span></label>
          <input id="channel-name" v-model="form.name" class="input" type="text" placeholder="例如：OpenAI 官方" />
        </div>

        <div class="grid gap-5 sm:grid-cols-2">
          <div>
            <label class="label" for="channel-type">渠道类型 <span class="text-red-600">*</span></label>
            <input
              id="channel-type"
              v-model.number="form.type"
              class="input tabular-nums"
              type="number"
              min="1"
              list="channel-type-options"
            />
            <datalist id="channel-type-options">
              <option value="1">OpenAI 兼容</option>
            </datalist>
            <p class="hint">当前版本仅支持 1（OpenAI 兼容协议），其它类型待后端支持。</p>
          </div>

          <div>
            <label class="label" for="channel-group">分组</label>
            <input id="channel-group" v-model="form.group" class="input input-mono" type="text" placeholder="default" />
            <p class="hint">用于按业务线隔离渠道；不确定时保持 default。</p>
          </div>
        </div>

        <div>
          <label class="label" for="channel-base-url">Base URL <span class="text-red-600">*</span></label>
          <input
            id="channel-base-url"
            v-model="form.base_url"
            class="input input-mono"
            type="url"
            placeholder="https://api.openai.com"
          />
          <p class="hint">上游接口根地址，不要包含 /v1/chat/completions 等具体路径。</p>
        </div>

        <div>
          <label class="label" for="channel-key">
            上游密钥
            <span v-if="!editing" class="text-red-600">*</span>
          </label>
          <input
            id="channel-key"
            v-model="form.api_key"
            class="input input-mono"
            type="password"
            autocomplete="new-password"
            :placeholder="editing ? '留空表示不修改现有密钥' : 'sk-...'"
          />
          <p class="hint">
            <template v-if="editing">
              当前密钥：<code class="chip">{{ editing.masked_key || '未配置' }}</code>。为避免误改，留空即保持原密钥不变。
            </template>
            <template v-else>单个密钥。需要多把密钥轮询时用下面的「批量密钥」。</template>
          </p>
        </div>

        <!-- 批量密钥池：支持一次粘贴几百把密钥并轮询使用 -->
        <div>
          <label class="label" for="channel-keys">批量密钥（密钥池）</label>
          <textarea
            id="channel-keys"
            v-model="form.keysText"
            class="input input-mono h-32 resize-y"
            placeholder="每行一把密钥，可粘贴数百行。&#10;例：&#10;nvapi-xxxxxxxxxxxx&#10;nvapi-yyyyyyyyyyyy 备注文字"
          />
          <p class="hint">
            每行一把，行内可用空格或逗号附加备注；以 <code>#</code> 开头的行会被忽略。
            填了本项即启用池化轮询：请求会在池内轮换，某把失效会被自动摘除并换下一把。
            <span v-if="editing" class="text-amber-700">编辑时留空表示不修改现有密钥池。</span>
          </p>
          <p v-if="editing && editing.key_pool && editing.key_pool.total > 0" class="mt-1 text-xs text-ink-300">
            当前池：
            共 {{ editing.key_pool.total }} 把 ·
            可用 {{ editing.key_pool.enabled }} ·
            已禁用 {{ editing.key_pool.disabled }} ·
            已摘除 {{ editing.key_pool.auto_removed }}
            <button type="button" class="ml-1 text-brand-700 underline hover:text-brand-800" @click="openKeys(editing)">
              查看明细
            </button>
          </p>
        </div>

        <div>
          <label class="label" for="channel-models">声明支持的模型</label>
          <div class="flex gap-2">
            <input
              id="channel-models"
              v-model="form.modelText"
              class="input input-mono flex-1"
              type="text"
              placeholder="留空表示支持全部模型"
            />
            <button type="button" class="btn btn-secondary shrink-0" :disabled="fetchingModels" @click="pullModels">
              <span
                v-if="fetchingModels"
                class="h-3.5 w-3.5 animate-spin rounded-full border-2 border-ink-500/40 border-t-ink-500"
                aria-hidden="true"
              />
              <AppIcon v-else name="refresh" :size="15" />
              {{ fetchingModels ? '拉取中…' : '从上游拉取' }}
            </button>
          </div>

          <p v-if="upstreamError" class="field-error">{{ upstreamError }}</p>

          <!-- 上游模型勾选清单：把真实模型名一键勾进来，避免手抄出错 -->
          <div v-if="upstreamModels.length" class="mt-2 rounded-lg border border-ink-800 bg-ink-950/60 p-2.5">
            <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
              <span class="text-xs text-ink-400">上游共 {{ upstreamModels.length }} 个模型，点击即可勾选</span>
              <span class="flex gap-2">
                <button type="button" class="text-xs text-brand-700 hover:text-brand-800" @click="selectAllModels">
                  全选
                </button>
                <button type="button" class="text-xs text-ink-400 hover:text-ink-200" @click="clearModels">
                  清空
                </button>
              </span>
            </div>
            <div class="flex max-h-56 flex-wrap gap-1.5 overflow-y-auto">
              <button
                v-for="model in upstreamModels"
                :key="model"
                type="button"
                class="chip transition"
                :class="selectedModelSet.has(model) ? 'border-brand-500/50 text-brand-700' : 'hover:border-brand-500/40'"
                @click="toggleModel(model)"
              >
                {{ model }}
              </button>
            </div>
          </div>

          <!-- 未拉取上游时的快捷补全：用站点已有模型 -->
          <div v-else-if="site.models.length" class="mt-2 flex flex-wrap gap-1.5">
            <button
              v-for="model in site.models.slice(0, 8)"
              :key="model"
              type="button"
              class="chip transition hover:border-brand-500/40 hover:text-brand-700"
              @click="toggleModel(model)"
            >
              {{ model }}
            </button>
          </div>

          <p class="hint">
            留空表示该渠道支持全部模型；多个模型用英文逗号分隔。
            点击「从上游拉取」可自动获取该上游支持的全部模型（如 NIM 平台的全部模型）。
          </p>
        </div>

        <div class="grid gap-5 sm:grid-cols-2">
          <div>
            <label class="label" for="channel-priority">优先级</label>
            <input id="channel-priority" v-model.number="form.priority" class="input tabular-nums" type="number" min="0" />
            <p class="hint">数值越大越优先被选中（路由会先用高优先级层）。</p>
          </div>

          <div>
            <label class="label" for="channel-weight">权重</label>
            <input id="channel-weight" v-model.number="form.weight" class="input tabular-nums" type="number" min="1" />
            <p class="hint">同优先级下按权重分配流量。</p>
          </div>
        </div>

        <div>
          <label class="label" for="channel-status">状态</label>
          <select id="channel-status" v-model.number="form.status" class="input max-w-[12rem]">
            <option :value="STATUS_ENABLED">启用</option>
            <option :value="STATUS_DISABLED">停用</option>
          </select>
        </div>

        <p
          v-if="formError"
          class="flex items-start gap-2 rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs leading-relaxed text-red-800"
        >
          <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
          {{ formError }}
        </p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="drawerOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="saving" @click="submitForm">
          <span
            v-if="saving"
            class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
            aria-hidden="true"
          />
          <AppIcon v-else name="check" :size="16" />
          {{ saving ? '保存中…' : editing ? '保存修改' : '创建渠道' }}
        </button>
      </template>
    </Drawer>

    <!-- 密钥池明细抽屉 -->
    <Drawer
      :open="keysDrawerOpen"
      :title="`密钥池 · ${keysOfChannel?.name ?? ''}`"
      subtitle="仅显示掩码。连续失败达阈值的密钥会被自动摘除，可在此手动恢复。"
      @close="keysDrawerOpen = false"
    >
      <DataState
        :loading="keysLoading"
        :error="keysError"
        :empty="!keysLoading && !keysError && channelKeys.length === 0"
        loading-text="正在读取密钥池…"
        empty-text="该渠道没有配置密钥池"
        empty-hint="在渠道表单的「批量密钥」里粘贴密钥即可启用池化轮询。"
        @retry="keysOfChannel && loadChannelKeys(keysOfChannel.id)"
      />

      <div v-if="!keysLoading && !keysError && channelKeys.length" class="space-y-3">
        <p class="text-xs text-ink-400">
          共 {{ channelKeys.length }} 把 ·
          <span class="text-emerald-700">可用 {{ keyStats.enabled }}</span> ·
          已禁用 {{ keyStats.disabled }} ·
          <span class="text-amber-700">已自动摘除 {{ keyStats.removed }}</span>
        </p>

        <div class="table-wrap">
          <table class="data-table">
            <thead>
              <tr>
                <th>密钥</th>
                <th>备注</th>
                <th>状态</th>
                <th class="text-right">连续失败</th>
                <th>最近使用</th>
                <th class="cell-actions">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="key in channelKeys" :key="key.id">
                <td><code class="chip">{{ key.masked_key }}</code></td>
                <td class="cell-muted">{{ key.label || '—' }}</td>
                <td>
                  <span :class="keyStatusClass(key.status)" :title="key.last_error || undefined">
                    {{ key.status_text }}
                  </span>
                </td>
                <td class="cell-num">{{ key.fail_count }}</td>
                <td class="cell-muted">
                  {{ key.last_used_at ? formatDateTime(key.last_used_at) : '未使用' }}
                </td>
                <td class="cell-actions">
                  <button
                    v-if="key.status !== KEY_STATUS_ENABLED"
                    type="button"
                    class="btn btn-row"
                    title="启用 / 恢复该密钥"
                    :disabled="keyBusyId === key.id"
                    @click="setKeyStatus(key, KEY_STATUS_ENABLED)"
                  >
                    <AppIcon name="bolt" :size="14" />
                  </button>
                  <button
                    v-else
                    type="button"
                    class="btn btn-row"
                    title="禁用该密钥"
                    :disabled="keyBusyId === key.id"
                    @click="setKeyStatus(key, KEY_STATUS_DISABLED)"
                  >
                    <AppIcon name="lock" :size="14" />
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </Drawer>
  </div>
</template>
