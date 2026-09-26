<script setup lang="ts">
/**
 * 管理后台 · 站点公告：发布/编辑/删除公告，支持语气、置顶、定时发布与过期。
 *
 * 意图（Why）：
 *   公告是站点对全体用户的单向通知（维护窗口、活动、计费调整、故障说明）。
 *   本页围绕管理员最关心的三件事设计：
 *     1) 一条公告"现在对用户可见吗"——由启用状态 + 发布/过期时间共同决定，
 *        列表必须把这三者显式展示出来，避免"发了等于没发"；
 *     2) 草稿与历史公告要能被找到——列表包含停用与已过期记录，并支持状态筛选；
 *     3) 误发代价高（全员可见）——删除需二次确认，内容长度由前后端共同约束。
 *
 * 流转（Flow）：
 *   列表：listAnnouncements({page,size,keyword,enabled}) → 表格
 *   新建/编辑：Modal 表单 → createAnnouncement / updateAnnouncement → 重新加载
 *   删除：confirmDialog → deleteAnnouncement(id)
 *
 * 扩展（Extend）：
 *   新增公告字段时：本页表单与表格各补一处，并同步 api/announcement.ts 的类型。
 *   新增语气时：在 LEVEL_OPTIONS 追加一项，并同步后端白名单与前台配色。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import {
  createAnnouncement,
  deleteAnnouncement,
  listAnnouncements,
  updateAnnouncement,
  type Announcement,
  type AnnouncementLevel,
} from '@/api/announcement'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatDateTime } from '@/utils/format'

/** 语气选项：与后端 model.AnnouncementLevel 白名单一一对应 */
const LEVEL_OPTIONS: { value: AnnouncementLevel; label: string }[] = [
  { value: 'info', label: '普通' },
  { value: 'success', label: '喜报' },
  { value: 'warning', label: '警告' },
  { value: 'danger', label: '故障' },
]

const items = ref<Announcement[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')
const busyId = ref<number | null>(null)

/** 筛选条件：enabledFilter 为空串表示"全部" */
const enabledFilter = ref<'' | 'true' | 'false'>('')
const keywordFilter = ref('')

const hasFilter = computed(() => enabledFilter.value !== '' || keywordFilter.value.trim() !== '')

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listAnnouncements({
      page: page.value,
      size: size.value,
      keyword: keywordFilter.value.trim() || undefined,
      enabled: enabledFilter.value === '' ? undefined : enabledFilter.value === 'true',
    })
    items.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    items.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '公告列表加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(load)

function applyFilter(): void {
  page.value = 1
  void load()
}

function resetFilter(): void {
  enabledFilter.value = ''
  keywordFilter.value = ''
  applyFilter()
}

function changePage(next: number): void {
  page.value = next
  void load()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void load()
}

/* ── 时间换算（datetime-local ↔ Unix 秒）────────────────── */

/**
 * datetime-local 输入值 → Unix 秒；空串返回 0（表示"立即发布/永不过期"）。
 * new Date('2026-09-27T10:00') 按本地时区解析，与管理员"我看到的就是本地时间"的直觉一致。
 */
function toUnix(value: string): number {
  if (!value) return 0
  const ms = new Date(value).getTime()
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : 0
}

/** Unix 秒 → datetime-local 输入值（YYYY-MM-DDTHH:mm，本地时区）；0 返回空串 */
function toLocalInput(ts: number): string {
  if (!ts) return ''
  const d = new Date(ts * 1000)
  const pad = (n: number): string => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/* ── 新建 / 编辑 ──────────────────────────────────────── */

const formOpen = ref(false)
const editing = ref<Announcement | null>(null)
const saving = ref(false)
const formError = ref('')
const form = ref({
  title: '',
  content: '',
  level: 'info' as AnnouncementLevel,
  pinned: false,
  enabled: true,
  publishAt: '',
  expireAt: '',
})

function openCreate(): void {
  editing.value = null
  formError.value = ''
  form.value = { title: '', content: '', level: 'info', pinned: false, enabled: true, publishAt: '', expireAt: '' }
  formOpen.value = true
}

function openEdit(item: Announcement): void {
  editing.value = item
  formError.value = ''
  form.value = {
    title: item.title,
    content: item.content,
    level: item.level,
    pinned: item.pinned,
    enabled: item.enabled,
    publishAt: toLocalInput(item.publish_at),
    expireAt: toLocalInput(item.expire_at),
  }
  formOpen.value = true
}

async function submit(): Promise<void> {
  const title = form.value.title.trim()
  if (!title) {
    formError.value = '公告标题不能为空'
    return
  }

  const publishAt = toUnix(form.value.publishAt)
  const expireAt = toUnix(form.value.expireAt)
  if (publishAt && expireAt && expireAt <= publishAt) {
    formError.value = '过期时间必须晚于发布时间'
    return
  }

  saving.value = true
  formError.value = ''
  const payload = {
    title,
    content: form.value.content,
    level: form.value.level,
    pinned: form.value.pinned,
    enabled: form.value.enabled,
    publish_at: publishAt,
    expire_at: expireAt,
  }
  try {
    if (editing.value) {
      await updateAnnouncement(editing.value.id, payload)
      toastSuccess('公告已更新')
    } else {
      await createAnnouncement(payload)
      toastSuccess('公告已发布')
    }
    formOpen.value = false
    page.value = 1
    await load()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败，请稍后重试'
  } finally {
    saving.value = false
  }
}

async function remove(item: Announcement): Promise<void> {
  const ok = await confirmDialog({
    title: '删除公告',
    message: `删除「${item.title}」后该公告立即从站点前台消失，操作不可恢复。`,
    confirmText: '删除',
    danger: true,
  })
  if (!ok) return

  busyId.value = item.id
  try {
    await deleteAnnouncement(item.id)
    toastSuccess('公告已删除')
    if (items.value.length === 1 && page.value > 1) page.value -= 1
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    busyId.value = null
  }
}

/* ── 展示辅助 ─────────────────────────────────────────── */

function levelLabel(level: AnnouncementLevel): string {
  return LEVEL_OPTIONS.find((option) => option.value === level)?.label ?? level
}

function levelBadgeClass(level: AnnouncementLevel): string {
  switch (level) {
    case 'success':
      return 'badge badge-ok'
    case 'warning':
      return 'badge badge-warn'
    case 'danger':
      return 'badge badge-err'
    default:
      return 'badge badge-off'
  }
}

/** 发布状态：把"启用 + 时间"合并成管理员一眼能懂的结论 */
function statusText(item: Announcement): string {
  if (!item.enabled) return '草稿'
  const now = Math.floor(Date.now() / 1000)
  if (item.publish_at && item.publish_at > now) return '待发布'
  if (item.expire_at && item.expire_at <= now) return '已过期'
  return '生效中'
}

function statusBadgeClass(item: Announcement): string {
  switch (statusText(item)) {
    case '生效中':
      return 'badge badge-ok'
    case '待发布':
      return 'badge badge-warn'
    case '已过期':
      return 'badge badge-off'
    default:
      return 'badge badge-off'
  }
}

function publishText(ts: number): string {
  return ts ? formatDateTime(ts) : '立即发布'
}

function expireText(ts: number): string {
  return ts ? formatDateTime(ts) : '永不过期'
}

const isEmpty = computed(() => !loading.value && !error.value && items.value.length === 0)
const emptyText = computed(() => (hasFilter.value ? '没有符合筛选条件的公告' : '还没有公告'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '试试放宽筛选条件，或点击「重置」查看全部。'
    : '点击「新建公告」发布站点通知，发布后会在前台横幅展示。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">站点公告</h2>
        <p class="page-desc">
          发布站点通知（维护窗口、活动、计费调整、故障说明）。支持置顶、定时发布与自动过期，
          公告会以横幅形式展示在用户前台。
        </p>
      </div>
      <div class="toolbar">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="15" />
          新建公告
        </button>
      </div>
    </div>

    <!-- 筛选条：状态 / 关键词两个维度，覆盖"找草稿"与"找某条"两类诉求 -->
    <div class="filter-bar mb-4">
      <div>
        <label class="label" for="announcement-status">状态</label>
        <select id="announcement-status" v-model="enabledFilter" class="input min-w-[8rem]">
          <option value="">全部</option>
          <option value="true">已发布</option>
          <option value="false">草稿</option>
        </select>
      </div>

      <div class="min-w-[12rem] flex-1">
        <label class="label" for="announcement-keyword">关键词</label>
        <input
          id="announcement-keyword"
          v-model="keywordFilter"
          class="input"
          type="text"
          placeholder="标题或正文"
          @keydown.enter="applyFilter"
        />
      </div>

      <div class="flex items-center gap-2">
        <button type="button" class="btn btn-primary btn-sm" :disabled="loading" @click="applyFilter">
          <AppIcon name="filter" :size="14" />
          查询
        </button>
        <button v-if="hasFilter" type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="resetFilter">
          重置
        </button>
      </div>
    </div>

    <div class="table-wrap table-cards">
      <table class="data-table min-w-[960px]">
        <thead>
          <tr>
            <th>标题</th>
            <th>语气</th>
            <th>置顶</th>
            <th>状态</th>
            <th>发布时间</th>
            <th>过期时间</th>
            <th>更新时间</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>

        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="isEmpty"
            :colspan="8"
            loading-text="正在加载公告…"
            :empty-text="emptyText"
            :empty-hint="emptyHint"
            @retry="load"
          >
            <template #action>
              <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
                <AppIcon name="plus" :size="14" />
                新建公告
              </button>
            </template>
          </DataState>

          <template v-if="!loading && !error && items.length">
            <tr v-for="item in items" :key="item.id">
              <td data-label="标题">
                <div class="flex items-center gap-2">
                  <span class="font-medium text-ink-100">{{ item.title }}</span>
                </div>
                <p v-if="item.content" class="mt-0.5 line-clamp-1 text-xs text-ink-400">{{ item.content }}</p>
              </td>

              <td data-label="语气">
                <span :class="levelBadgeClass(item.level)">{{ levelLabel(item.level) }}</span>
              </td>

              <td data-label="置顶">{{ item.pinned ? '是' : '否' }}</td>

              <td data-label="状态">
                <span :class="statusBadgeClass(item)">{{ statusText(item) }}</span>
              </td>

              <td class="cell-muted whitespace-nowrap" data-label="发布时间">{{ publishText(item.publish_at) }}</td>
              <td class="cell-muted whitespace-nowrap" data-label="过期时间">{{ expireText(item.expire_at) }}</td>
              <td class="cell-muted whitespace-nowrap" data-label="更新时间">{{ formatDateTime(item.updated_at) }}</td>

              <td class="cell-actions" data-label="操作">
                <div class="flex items-center justify-end gap-1">
                  <button type="button" class="btn btn-row" title="编辑" @click="openEdit(item)">
                    <AppIcon name="edit" :size="14" />
                  </button>
                  <button
                    type="button"
                    class="btn btn-row text-ink-400 hover:text-red-700"
                    title="删除"
                    :disabled="busyId === item.id"
                    @click="remove(item)"
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

    <Modal
      :open="formOpen"
      :title="editing ? '编辑公告' : '新建公告'"
      subtitle="留空发布时间表示立即展示；留空过期时间表示永不过期。"
      width="max-w-2xl"
      :close-on-backdrop="false"
      @close="formOpen = false"
    >
      <div class="space-y-4">
        <div>
          <label class="label" for="announcement-title">标题 <span class="text-red-600">*</span></label>
          <input
            id="announcement-title"
            v-model="form.title"
            class="input"
            type="text"
            maxlength="200"
            placeholder="如「今晚 02:00 - 04:00 系统维护」"
          />
          <p class="hint">最多 200 字符，会作为横幅主文案展示。</p>
        </div>

        <div>
          <label class="label" for="announcement-content">正文</label>
          <textarea
            id="announcement-content"
            v-model="form.content"
            class="input min-h-[8rem]"
            maxlength="20000"
            placeholder="补充说明，如影响范围与预计恢复时间"
          />
          <p class="hint">最多 20000 字符，支持换行。</p>
        </div>

        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label" for="announcement-level">语气</label>
            <select id="announcement-level" v-model="form.level" class="input">
              <option v-for="option in LEVEL_OPTIONS" :key="option.value" :value="option.value">
                {{ option.label }}
              </option>
            </select>
            <p class="hint">决定横幅配色：普通 / 喜报 / 警告 / 故障。</p>
          </div>

          <div class="space-y-2 pt-6">
            <label class="flex items-center gap-2 text-sm text-ink-200">
              <input v-model="form.pinned" type="checkbox" />
              置顶展示
            </label>
            <label class="flex items-center gap-2 text-sm text-ink-200">
              <input v-model="form.enabled" type="checkbox" />
              立即启用（取消则保存为草稿）
            </label>
          </div>
        </div>

        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label" for="announcement-publish">开始展示时间</label>
            <input id="announcement-publish" v-model="form.publishAt" class="input" type="datetime-local" />
            <p class="hint">留空表示立即展示。</p>
          </div>
          <div>
            <label class="label" for="announcement-expire">停止展示时间</label>
            <input id="announcement-expire" v-model="form.expireAt" class="input" type="datetime-local" />
            <p class="hint">留空表示永不过期。</p>
          </div>
        </div>

        <p v-if="formError" class="field-error">{{ formError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="formOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="saving" @click="submit">
          <span
            v-if="saving"
            class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
            aria-hidden="true"
          />
          <AppIcon v-else name="check" :size="16" />
          {{ saving ? '保存中…' : '保存' }}
        </button>
      </template>
    </Modal>
  </div>
</template>
