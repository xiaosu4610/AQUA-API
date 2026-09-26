<script setup lang="ts">
/**
 * 管理后台 · 兑换码：批量生成 + 列表筛选 + 改状态/备注 + 删除 + 清理失效。
 *
 * 意图（Why）：
 *   兑换码是「先发码、后兑换」的运营工具，管理员用它做限时活动与小额发放。
 *   本页围绕三个关键动作设计：
 *     1) 批量生成：生成后必须把本批【全部明文码】摊开，并给出一键复制——
 *        管理员的下一步一定是"导出并分发"，只回一个数量等于让人白跑一趟；
 *     2) 精确筛选：状态 / 关键词 / 批次三个维度，覆盖"查某批活动的码用了多少"
 *        与"找某张码"两类最常见的核对诉求；
 *     3) 清理：失效码（已使用 / 已过期）会有大量历史数据，需一键清理并二次确认。
 *
 * 流转（Flow）：
 *   列表：listRedeemCodes({page,size,status,keyword,batch_no}) → 表格
 *   生成：Modal（数量/额度/有效期/备注）→ createRedeemCodes → 结果弹窗（明文 + 复制全部）
 *   改状态/备注：Modal → updateRedeemCode(id, {status?, remark?})
 *   删除单张：confirmDialog → deleteRedeemCode(id)
 *   清理失效：confirmDialog（危险操作）→ deleteInvalidRedeemCodes() → 显示清理条数
 *
 * 扩展（Extend）：
 *   新增筛选维度时：types.ts 的 RedeemCodeQuery 加字段 → 本页筛选条加控件 → listRedeemCodes 传入。
 *   新增字段（如"限定分组"）时：表格与表单各补一处，并同步后端 DTO。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import CopyButton from '@/components/CopyButton.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import {
  createRedeemCodes,
  deleteInvalidRedeemCodes,
  deleteRedeemCode,
  listRedeemCodes,
  updateRedeemCode,
} from '@/api/admin'
import {
  REDEEM_STATUS_UNUSED,
  REDEEM_STATUS_USED,
  REDEEM_STATUS_VOID,
  type RedeemCode,
} from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatDateTime, formatExpiry, formatNumber } from '@/utils/format'

const codes = ref<RedeemCode[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')
const busyId = ref<number | null>(null)
const clearing = ref(false)

/** 筛选条件：状态为空串表示"全部" */
const statusFilter = ref<'' | number>('')
const keywordFilter = ref('')
const batchFilter = ref('')

const hasFilter = computed(
  () => statusFilter.value !== '' || keywordFilter.value.trim() !== '' || batchFilter.value.trim() !== '',
)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listRedeemCodes({
      page: page.value,
      size: size.value,
      // 只把"真正选了"的条件传给后端，避免发出 ?status= 这类无意义查询
      status: statusFilter.value === '' ? undefined : Number(statusFilter.value),
      keyword: keywordFilter.value.trim() || undefined,
      batch_no: batchFilter.value.trim() || undefined,
    })
    codes.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    codes.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '兑换码列表加载失败'
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
  statusFilter.value = ''
  keywordFilter.value = ''
  batchFilter.value = ''
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

/* ── 批量生成 ─────────────────────────────────────────── */

const createOpen = ref(false)
const creating = ref(false)
const createError = ref('')
const createForm = ref({ count: 10, quota: 100, expiresDays: 0, remark: '' })

/** 生成结果（弹窗展示 + 复制全部）；不落库到页面其他地方，关闭即清空 */
const createdBatchNo = ref('')
const createdCodes = ref<string[]>([])
const resultOpen = ref(false)

/** 结果文本：每行一张码，便于粘贴到表格或聊天工具分发 */
const createdText = computed(() => createdCodes.value.join('\n'))

function openCreate(): void {
  createForm.value = { count: 10, quota: 100, expiresDays: 0, remark: '' }
  createError.value = ''
  createOpen.value = true
}

async function submitCreate(): Promise<void> {
  const count = Number(createForm.value.count)
  const quota = Number(createForm.value.quota)
  const expiresDays = Number(createForm.value.expiresDays)

  if (!Number.isInteger(count) || count <= 0) {
    createError.value = '生成数量必须是大于 0 的整数'
    return
  }
  if (!Number.isFinite(quota) || quota <= 0) {
    createError.value = '可兑换额度必须大于 0'
    return
  }
  if (!Number.isFinite(expiresDays) || expiresDays < 0) {
    createError.value = '有效期天数不能为负数（0 表示永不过期）'
    return
  }

  creating.value = true
  createError.value = ''
  try {
    const result = await createRedeemCodes({
      count,
      quota: Math.round(quota),
      expires_days: Math.round(expiresDays),
      remark: createForm.value.remark.trim() || undefined,
    })
    createdBatchNo.value = result.batch_no
    createdCodes.value = (result.items ?? []).map((item) => item.code)
    createOpen.value = false
    resultOpen.value = true
    page.value = 1
    await load()
  } catch (err) {
    createError.value = err instanceof ApiError ? err.message : '生成失败，请稍后重试'
  } finally {
    creating.value = false
  }
}

/** 关闭结果弹窗：清空明文列表，避免长期驻留 DOM */
function closeResult(): void {
  resultOpen.value = false
  createdBatchNo.value = ''
  createdCodes.value = []
}

/* ── 改状态 / 备注 ────────────────────────────────────── */

const editOpen = ref(false)
const editTarget = ref<RedeemCode | null>(null)
const editForm = ref({ status: REDEEM_STATUS_UNUSED, remark: '' })
const editing = ref(false)
const editError = ref('')

function openEdit(code: RedeemCode): void {
  editTarget.value = code
  editForm.value = { status: code.status, remark: code.remark }
  editError.value = ''
  editOpen.value = true
}

async function submitEdit(): Promise<void> {
  if (!editTarget.value) return
  editing.value = true
  editError.value = ''
  try {
    await updateRedeemCode(editTarget.value.id, {
      status: editForm.value.status,
      remark: editForm.value.remark.trim(),
    })
    editOpen.value = false
    toastSuccess('兑换码已更新')
    await load()
  } catch (err) {
    editError.value = err instanceof ApiError ? err.message : '更新失败，请稍后重试'
  } finally {
    editing.value = false
  }
}

/* ── 删除 / 清理失效 ──────────────────────────────────── */

async function removeCode(code: RedeemCode): Promise<void> {
  const ok = await confirmDialog({
    title: '删除兑换码',
    message: `删除「${code.code}」后该码立即失效，用户无法再兑换。${
      code.status === REDEEM_STATUS_USED ? '该码已被领取，删除不影响已发放的额度。' : ''
    }`,
    confirmText: '删除',
    danger: true,
  })
  if (!ok) return

  busyId.value = code.id
  try {
    await deleteRedeemCode(code.id)
    toastSuccess('兑换码已删除')
    if (codes.value.length === 1 && page.value > 1) page.value -= 1
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    busyId.value = null
  }
}

async function clearInvalid(): Promise<void> {
  const ok = await confirmDialog({
    title: '清理失效兑换码',
    message: '将永久删除全部「已使用」与「已过期」的兑换码，操作不可恢复。未使用且未过期的码不受影响。',
    confirmText: '确认清理',
    danger: true,
  })
  if (!ok) return

  clearing.value = true
  try {
    const result = await deleteInvalidRedeemCodes()
    toastSuccess(`已清理 ${result.deleted ?? 0} 张失效兑换码`)
    page.value = 1
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '清理失败')
  } finally {
    clearing.value = false
  }
}

/* ── 展示辅助 ─────────────────────────────────────────── */

/**
 * 状态徽标：把「过期」单独表达。
 *
 * status 与 expired 是两个正交维度（业务状态 vs 时间），二者不一致时
 * 展示"已过期"更贴近管理员的实际判断依据——一张状态为"未使用"的码，
 * 若已过期，它在用户眼里就是废码，列表里必须一眼看出来。
 */
function statusBadgeClass(code: RedeemCode): string {
  if (code.status === REDEEM_STATUS_UNUSED && code.expired) return 'badge badge-warn'
  switch (code.status) {
    case REDEEM_STATUS_UNUSED:
      return 'badge badge-ok'
    case REDEEM_STATUS_VOID:
      return 'badge badge-err'
    default:
      return 'badge badge-off'
  }
}

function statusText(code: RedeemCode): string {
  if (code.status === REDEEM_STATUS_UNUSED && code.expired) return '已过期'
  return code.status_text || '—'
}

/** 领取情况：未领取显示"未领取"，已领取显示用户与时间 */
function usedText(code: RedeemCode): string {
  if (!code.used_by) return '未领取'
  const time = code.used_at ? formatDateTime(code.used_at) : ''
  return time ? `#${code.used_by} · ${time}` : `#${code.used_by}`
}

const isEmpty = computed(() => !loading.value && !error.value && codes.value.length === 0)
const emptyText = computed(() => (hasFilter.value ? '没有符合筛选条件的兑换码' : '还没有兑换码'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '试试放宽筛选条件，或点击「重置」查看全部。'
    : '点击「批量生成」创建一批兑换码，导出后即可分发给用户兑换额度。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">兑换码</h2>
        <p class="page-desc">
          批量生成兑换码并导出分发，用户在门户输入即可领取额度。一码一用，可设置有效期。
        </p>
      </div>
      <div class="toolbar">
        <button
          type="button"
          class="btn btn-secondary btn-sm"
          :disabled="clearing || loading"
          @click="clearInvalid"
        >
          <AppIcon name="trash" :size="14" />
          清理失效
        </button>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="15" />
          批量生成
        </button>
      </div>
    </div>

    <!-- 筛选条：状态 / 关键词 / 批次三个维度，覆盖"核对某批活动"与"找某张码" -->
    <div class="filter-bar mb-4">
      <div>
        <label class="label" for="redeem-status">状态</label>
        <select id="redeem-status" v-model="statusFilter" class="input min-w-[8rem]">
          <option value="">全部状态</option>
          <option :value="REDEEM_STATUS_UNUSED">未使用</option>
          <option :value="REDEEM_STATUS_USED">已使用</option>
          <option :value="REDEEM_STATUS_VOID">已作废</option>
        </select>
      </div>

      <div class="min-w-[12rem] flex-1">
        <label class="label" for="redeem-keyword">关键词</label>
        <input
          id="redeem-keyword"
          v-model="keywordFilter"
          class="input"
          type="text"
          placeholder="兑换码或备注"
          @keydown.enter="applyFilter"
        />
      </div>

      <div class="min-w-[12rem] flex-1">
        <label class="label" for="redeem-batch">批次号</label>
        <input
          id="redeem-batch"
          v-model="batchFilter"
          class="input input-mono"
          type="text"
          placeholder="如 R20260927…"
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

    <!-- 滑动提示只在桌面端出现：窄屏已切换为卡片视图（.table-cards），不需要左右滑动 -->
    <p class="mb-2 hidden text-xs text-ink-400 lg:block">表格列较多，可左右滑动查看完整内容。</p>

    <div class="table-wrap table-cards">
      <table class="data-table min-w-[1080px]">
        <thead>
          <tr>
            <th>兑换码</th>
            <th class="text-right">额度</th>
            <th>状态</th>
            <th>批次</th>
            <th>有效期</th>
            <th>领取情况</th>
            <th>备注</th>
            <th>创建时间</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>

        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="isEmpty"
            :colspan="9"
            loading-text="正在加载兑换码…"
            :empty-text="emptyText"
            :empty-hint="emptyHint"
            @retry="load"
          >
            <template #action>
              <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
                <AppIcon name="plus" :size="14" />
                批量生成
              </button>
            </template>
          </DataState>

          <template v-if="!loading && !error && codes.length">
            <tr v-for="code in codes" :key="code.id">
              <td data-label="兑换码">
                <span class="flex items-center gap-1.5">
                  <code class="chip">{{ code.code }}</code>
                  <CopyButton :value="code.code" small class="btn-row" success-text="兑换码已复制" />
                </span>
              </td>

              <td class="cell-num" data-label="额度">{{ formatNumber(code.quota) }}</td>

              <td data-label="状态">
                <span :class="statusBadgeClass(code)">
                  <span class="dot" />
                  {{ statusText(code) }}
                </span>
              </td>

              <td class="cell-muted" data-label="批次">
                <code class="font-mono text-[12px]">{{ code.batch_no || '—' }}</code>
              </td>

              <td class="cell-muted whitespace-nowrap" data-label="有效期">{{ formatExpiry(code.expires_at) }}</td>
              <td class="cell-muted" data-label="领取情况">{{ usedText(code) }}</td>
              <td class="cell-muted max-w-[14rem]" data-label="备注">
                <span class="line-clamp-1">{{ code.remark || '—' }}</span>
              </td>
              <td class="cell-muted whitespace-nowrap" data-label="创建时间">{{ formatDateTime(code.created_at) }}</td>

              <td class="cell-actions" data-label="操作">
                <div class="flex items-center justify-end gap-1">
                  <button type="button" class="btn btn-row" title="改状态 / 备注" @click="openEdit(code)">
                    <AppIcon name="edit" :size="14" />
                  </button>
                  <button
                    type="button"
                    class="btn btn-row text-ink-400 hover:text-red-700"
                    title="删除"
                    :disabled="busyId === code.id"
                    @click="removeCode(code)"
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

    <!-- 批量生成 -->
    <Modal
      :open="createOpen"
      title="批量生成兑换码"
      subtitle="生成后请立即导出分发给用户；明文码只在本次生成结果中展示。"
      width="max-w-lg"
      :close-on-backdrop="false"
      @close="createOpen = false"
    >
      <div class="space-y-4">
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label" for="redeem-count">生成数量 <span class="text-red-600">*</span></label>
            <input id="redeem-count" v-model.number="createForm.count" class="input text-right tabular-nums" type="number" min="1" max="500" />
            <p class="hint">单次最多 500 张，需要更多请分批生成。</p>
          </div>
          <div>
            <label class="label" for="redeem-quota">每张额度 <span class="text-red-600">*</span></label>
            <input id="redeem-quota" v-model.number="createForm.quota" class="input text-right tabular-nums" type="number" min="1" />
            <p class="hint">每张码可兑换的额度，须大于 0。</p>
          </div>
        </div>

        <div>
          <label class="label" for="redeem-expires">有效期（天）</label>
          <input id="redeem-expires" v-model.number="createForm.expiresDays" class="input max-w-[12rem] text-right tabular-nums" type="number" min="0" />
          <p class="hint">填 0 表示永不过期；过期后用户无法再兑换。</p>
        </div>

        <div>
          <label class="label" for="redeem-remark">备注</label>
          <input
            id="redeem-remark"
            v-model="createForm.remark"
            class="input"
            type="text"
            placeholder="如「双十一活动」「社群福利」"
          />
          <p class="hint">备注会同时写入本批全部兑换码，便于日后核对来源。</p>
        </div>

        <p v-if="createError" class="field-error">{{ createError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="creating" @click="createOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="creating" @click="submitCreate">
          <span
            v-if="creating"
            class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
            aria-hidden="true"
          />
          <AppIcon v-else name="plus" :size="16" />
          {{ creating ? '生成中…' : '生成' }}
        </button>
      </template>
    </Modal>

    <!-- 生成结果：一次性展示本批全部明文码 + 一键复制全部 -->
    <Modal
      :open="resultOpen"
      title="生成成功"
      :subtitle="`本批共 ${createdCodes.length} 张，批次号 ${createdBatchNo}。请立即复制保存，关闭后仍可在列表查看。`"
      width="max-w-2xl"
      :close-on-backdrop="false"
      @close="closeResult"
    >
      <div class="space-y-3">
        <div class="code-block">
          <div class="flex items-center justify-between gap-3 border-b border-ink-800 px-4 py-2">
            <span class="font-mono text-xs text-ink-300">批次号 {{ createdBatchNo }}</span>
            <CopyButton :value="createdText" label="复制全部" small success-text="本批兑换码已全部复制" />
          </div>
          <pre class="max-h-72 whitespace-pre-wrap break-all">{{ createdText }}</pre>
        </div>
        <p class="text-xs leading-relaxed text-ink-400">
          每行一张码，可直接粘贴到表格或社群分发。用户兑换入口在门户，一码只能使用一次。
        </p>
      </div>

      <template #footer>
        <CopyButton :value="createdText" label="复制全部" success-text="本批兑换码已全部复制" />
        <button type="button" class="btn btn-primary" @click="closeResult">
          <AppIcon name="check" :size="16" />
          完成
        </button>
      </template>
    </Modal>

    <!-- 改状态 / 备注 -->
    <Modal
      :open="editOpen"
      title="编辑兑换码"
      :subtitle="editTarget ? `兑换码：${editTarget.code}` : ''"
      width="max-w-md"
      :close-on-backdrop="false"
      @close="editOpen = false"
    >
      <div class="space-y-4">
        <div>
          <label class="label" for="redeem-edit-status">状态</label>
          <select id="redeem-edit-status" v-model.number="editForm.status" class="input">
            <option :value="REDEEM_STATUS_UNUSED">未使用</option>
            <option :value="REDEEM_STATUS_USED">已使用</option>
            <option :value="REDEEM_STATUS_VOID">已作废</option>
          </select>
          <p class="hint">置为「已作废」后该码不可再兑换；「已使用」一般无需手工设置。</p>
        </div>

        <div>
          <label class="label" for="redeem-edit-remark">备注</label>
          <input id="redeem-edit-remark" v-model="editForm.remark" class="input" type="text" placeholder="留空表示不写备注" />
        </div>

        <p v-if="editError" class="field-error">{{ editError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="editing" @click="editOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="editing" @click="submitEdit">
          {{ editing ? '保存中…' : '保存' }}
        </button>
      </template>
    </Modal>
  </div>
</template>
