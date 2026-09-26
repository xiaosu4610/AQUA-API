<script setup lang="ts">
/**
 * 异步任务表格（后台与门户共用）。
 *
 * 意图（Why）：
 *   任务列表在管理后台与用户门户里几乎完全一致（列差异只有「归属用户」），
 *   因此把表格、进度条与详情弹窗收敛成一个组件，避免两份会逐渐分叉的实现。
 *   组件只负责展示与"请求取消"，数据加载由页面负责——这样页面能统一控制
 *   筛选与分页，而不是把请求逻辑藏进组件内部。
 *
 * 关键的展示取舍：
 *   - 进度用进度条而不是数字：任务类场景下"还剩多少"比精确百分比更重要；
 *   - 失败的终态必须把原因显示在行内（而不是只放在弹窗里）——
 *     失败原因往往是可操作的（如"上游拒绝任务"）；
 *   - 结果图直接以缩略图预览，点击可跳原图。
 *
 * 流转（Flow）：
 *   页面 → <TaskTable :tasks :loading :error @retry @cancel />
 *   → 行内展示 + 详情弹窗 → 取消任务由页面调用接口后刷新
 *
 * 扩展（Extend）：
 *   新增任务类别（音乐等）时无需改动本组件（kind_text 由后端给出）；
 *   新增可选列（渠道、额度）时加 prop 并在表头/单元格各补一处。
 */
import { computed, ref } from 'vue'

import AppIcon from './AppIcon.vue'
import DataState from './DataState.vue'
import Modal from './Modal.vue'
import type { Task } from '@/api/types'
import {
  TASK_STATUS_CANCELED,
  TASK_STATUS_FAILED,
  TASK_STATUS_RUNNING,
  TASK_STATUS_SUCCEEDED,
} from '@/api/types'
import { formatDateTime, formatNumber } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    tasks: Task[]
    loading?: boolean
    error?: string
    emptyText?: string
    emptyHint?: string
    /** 是否显示「归属用户」列（管理后台使用） */
    showUser?: boolean
    /** 是否允许取消任务（终态任务不可取消） */
    allowCancel?: boolean
    /** 取消中的任务号（用于禁用按钮） */
    cancelling?: string
  }>(),
  {
    loading: false,
    error: '',
    emptyText: '暂无任务',
    emptyHint: '',
    showUser: false,
    allowCancel: false,
    cancelling: '',
  },
)

const emit = defineEmits<{
  (e: 'retry'): void
  (e: 'cancel', task: Task): void
}>()

/** 详情弹窗当前展示的任务 */
const detail = ref<Task | null>(null)

/** 列数：用于 DataState 的 colspan，必须与表头列数严格一致 */
const columnCount = computed(() => 6 + (props.showUser ? 1 : 0) + (props.allowCancel ? 1 : 0))

function isTerminal(task: Task): boolean {
  return (
    task.status === TASK_STATUS_SUCCEEDED ||
    task.status === TASK_STATUS_FAILED ||
    task.status === TASK_STATUS_CANCELED
  )
}

/** 状态徽标：进行中与排队中用中性/品牌色，避免"未完成"看着像失败 */
function statusBadgeClass(task: Task): string {
  switch (task.status) {
    case TASK_STATUS_SUCCEEDED:
      return 'badge badge-ok'
    case TASK_STATUS_FAILED:
      return 'badge badge-err'
    case TASK_STATUS_CANCELED:
      return 'badge badge-off'
    case TASK_STATUS_RUNNING:
      return 'badge badge-info'
    default:
      return 'badge badge-warn'
  }
}

/** 进度条颜色：失败用红色，其余用品牌色 */
function progressClass(task: Task): string {
  if (task.status === TASK_STATUS_FAILED) return 'bg-red-500'
  if (task.status === TASK_STATUS_SUCCEEDED) return 'bg-emerald-500'
  return 'bg-brand-500'
}

/** 结果是否为图片（用于决定是否渲染缩略图） */
function isImageResult(task: Task): boolean {
  const url = task.result_url.toLowerCase()
  return /\.(png|jpe?g|webp|gif|avif)(\?|$)/.test(url)
}

/** 结果 JSON 美化展示；解析失败时原样显示，便于排查上游返回了什么 */
const detailResultText = computed(() => {
  const raw = detail.value?.result_data
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
})

/** 提交参数美化展示 */
const detailParamsText = computed(() => {
  const raw = detail.value?.params
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
})

function openDetail(task: Task): void {
  detail.value = task
}
</script>

<template>
  <div class="table-wrap">
    <table class="data-table">
      <thead>
        <tr>
          <th>任务</th>
          <th v-if="showUser">用户</th>
          <th>模型 / 适配器</th>
          <th>进度</th>
          <th>状态</th>
          <th class="text-right">额度</th>
          <th>提交时间</th>
          <th v-if="allowCancel" class="cell-actions">操作</th>
        </tr>
      </thead>
      <tbody>
        <DataState
          :loading="loading"
          :error="error"
          :empty="!loading && !error && tasks.length === 0"
          :colspan="columnCount"
          loading-text="正在读取任务…"
          :empty-text="emptyText"
          :empty-hint="emptyHint"
          @retry="emit('retry')"
        />

        <tr v-for="task in tasks" :key="task.task_ref">
          <td>
            <button type="button" class="text-left" @click="openDetail(task)">
              <span class="block font-mono text-[12px] text-ink-200">{{ task.task_ref }}</span>
              <span class="mt-0.5 line-clamp-1 block max-w-[18rem] text-xs text-ink-400">
                {{ task.prompt || '（无提示词）' }}
              </span>
            </button>
          </td>

          <td v-if="showUser" class="cell-muted">{{ task.user_id }}</td>

          <td>
            <span class="block font-mono text-xs text-ink-200">{{ task.model || '—' }}</span>
            <span class="text-[11px] text-ink-500">{{ task.provider }} · {{ task.kind_text }}</span>
          </td>

          <td class="min-w-[7rem]">
            <div class="flex items-center gap-2">
              <span class="h-1.5 w-16 overflow-hidden rounded-full bg-ink-800">
                <span
                  class="block h-full rounded-full transition-all"
                  :class="progressClass(task)"
                  :style="{ width: `${Math.min(100, Math.max(0, task.progress))}%` }"
                />
              </span>
              <span class="text-xs tabular-nums text-ink-400">{{ task.progress }}%</span>
            </div>
          </td>

          <td>
            <span :class="statusBadgeClass(task)">{{ task.status_text }}</span>
            <span v-if="task.error" class="mt-0.5 line-clamp-1 block max-w-[14rem] text-[11px] text-red-600">
              {{ task.error }}
            </span>
          </td>

          <td class="cell-num">{{ task.quota > 0 ? formatNumber(task.quota) : '—' }}</td>

          <td class="cell-muted">{{ formatDateTime(task.created_at) }}</td>

          <td v-if="allowCancel" class="cell-actions">
            <div class="flex items-center justify-end gap-1">
              <a
                v-if="task.result_url"
                class="btn-row"
                :href="task.result_url"
                target="_blank"
                rel="noopener noreferrer"
                title="打开结果"
              >
                <AppIcon name="external" :size="14" />
              </a>
              <button type="button" class="btn-row" title="详情" @click="openDetail(task)">
                <AppIcon name="eye" :size="14" />
              </button>
              <button
                v-if="!isTerminal(task)"
                type="button"
                class="btn-row"
                :disabled="cancelling === task.task_ref"
                title="取消任务（会退还已扣额度）"
                @click="emit('cancel', task)"
              >
                <AppIcon name="close" :size="14" />
              </button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>

    <!-- 详情弹窗：把"提交了什么、上游返回了什么"完整摊开，便于排查 -->
    <Modal
      :open="Boolean(detail)"
      :title="detail ? `任务 ${detail.task_ref}` : ''"
      subtitle="任务类接口的价值之一是可复现：这里保留提交参数与上游返回原文。"
      width="max-w-3xl"
      @close="detail = null"
    >
      <div v-if="detail" class="space-y-4">
        <div class="flex flex-wrap items-center gap-2">
          <span :class="statusBadgeClass(detail)">{{ detail.status_text }}</span>
          <span class="chip">{{ detail.kind_text }}</span>
          <span class="chip">{{ detail.provider }}</span>
          <span class="chip">{{ detail.model || '—' }}</span>
          <span v-if="detail.quota > 0" class="chip">额度 {{ formatNumber(detail.quota) }}</span>
        </div>

        <div v-if="detail.error" class="rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs text-red-700">
          {{ detail.error }}
        </div>

        <!-- 结果预览：图片直接看，其它类型给链接 -->
        <div v-if="detail.result_url">
          <p class="label">结果</p>
          <a
            :href="detail.result_url"
            target="_blank"
            rel="noopener noreferrer"
            class="block overflow-hidden rounded-lg border border-ink-800"
          >
            <img
              v-if="isImageResult(detail)"
              :src="detail.result_url"
              alt="任务结果"
              class="max-h-72 w-full bg-ink-900 object-contain"
              loading="lazy"
            />
            <span v-else class="block px-3 py-2 font-mono text-xs text-brand-700">{{ detail.result_url }}</span>
          </a>
        </div>

        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <p class="label">提交参数</p>
            <pre class="code-block max-h-56 overflow-auto p-3 font-mono text-[12px] leading-relaxed text-ink-200">{{ detailParamsText || '（空）' }}</pre>
          </div>
          <div>
            <p class="label">上游返回</p>
            <pre class="code-block max-h-56 overflow-auto p-3 font-mono text-[12px] leading-relaxed text-ink-200">{{ detailResultText || '（空）' }}</pre>
          </div>
        </div>

        <dl class="grid grid-cols-2 gap-3 text-xs text-ink-400 sm:grid-cols-4">
          <div>
            <dt>提交时间</dt>
            <dd class="mt-0.5 text-ink-200">{{ formatDateTime(detail.created_at) }}</dd>
          </div>
          <div>
            <dt>更新时间</dt>
            <dd class="mt-0.5 text-ink-200">{{ formatDateTime(detail.updated_at) }}</dd>
          </div>
          <div>
            <dt>完成时间</dt>
            <dd class="mt-0.5 text-ink-200">{{ detail.finished_at ? formatDateTime(detail.finished_at) : '—' }}</dd>
          </div>
          <div>
            <dt>渠道 / 用户</dt>
            <dd class="mt-0.5 text-ink-200">#{{ detail.channel_id }} / #{{ detail.user_id }}</dd>
          </div>
        </dl>

        <div>
          <p class="label">提示词</p>
          <p class="whitespace-pre-wrap break-words rounded-lg border border-ink-800 bg-ink-950 px-3 py-2 text-sm text-ink-200">
            {{ detail.prompt || '（无）' }}
          </p>
        </div>
      </div>

      <template #footer>
        <a
          v-if="detail?.result_url"
          class="btn btn-secondary"
          :href="detail.result_url"
          target="_blank"
          rel="noopener noreferrer"
        >
          <AppIcon name="external" :size="14" />
          打开结果
        </a>
        <button type="button" class="btn btn-secondary" @click="detail = null">关闭</button>
        <button
          v-if="detail && allowCancel && !isTerminal(detail)"
          type="button"
          class="btn btn-danger"
          @click="emit('cancel', detail); detail = null"
        >
          <AppIcon name="close" :size="14" />
          取消任务
        </button>
      </template>
    </Modal>
  </div>
</template>
