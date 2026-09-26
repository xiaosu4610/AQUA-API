<script setup lang="ts">
/**
 * 调用日志表格（门户与管理端共用）。
 *
 * 意图（Why）：
 *   门户「我的调用记录」与管理「全站调用日志」是同一张表的两种视图（后者多两列）；
 *   共用组件可保证列宽、对齐、状态徽标与错误提示的呈现完全一致。
 *   表头与单元格标签走词条（components.logTable.*），列标签同时用于窄屏卡片视图。
 *
 * 流转（Flow）：
 *   页面请求 /api/user/logs 或 /api/admin/logs → 传入 logs → 本组件渲染
 *   状态渲染优先级：loading > error > empty > 数据行
 *
 * 扩展（Extend）：
 *   新增列（如 request_id）时：在表头与数据行同时添加，并同步 bump columnCount 的基数，
 *   否则空态行的 colSpan 会对不齐；同时别忘了给新增的 <td> 补 data-label
 *   （窄屏卡片视图的「标签：值」标签即来自该属性，与表头文案保持一致），并补词条键。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import AppIcon from './AppIcon.vue'
import DataState from './DataState.vue'
import type { UsageLog } from '@/api/types'
import { formatDateTime, formatLatency, formatNumber, formatRelative } from '@/utils/format'
import { httpStatusBadgeClass } from '@/utils/display'

const props = withDefaults(
  defineProps<{
    logs: UsageLog[]
    loading?: boolean
    error?: string
    /** 是否显示「用户」列（管理端） */
    showUser?: boolean
    /** 是否显示「渠道」列（管理端） */
    showChannel?: boolean
    /** 空态文案（不传则用当前语言默认词条） */
    emptyText?: string
    /** 空态补充说明（不传则用当前语言默认词条） */
    emptyHint?: string
  }>(),
  {
    loading: false,
    error: '',
    showUser: false,
    showChannel: false,
  },
)

const emit = defineEmits<{ (e: 'retry'): void }>()

const { t } = useI18n()

const emptyMessage = computed(() => props.emptyText ?? t('components.logTable.empty'))
const emptyDescription = computed(() => props.emptyHint ?? t('components.logTable.emptyHint'))

/**
 * 总列数：用于空态/加载态行的 colSpan。
 * 基数 9 对应「时间/模型/令牌/入出/合计/配额/延迟/流式/状态」，两列可选列在此叠加。
 */
const columnCount = computed(() => 9 + (props.showUser ? 1 : 0) + (props.showChannel ? 1 : 0))

/** 请求是否成功（HTTP 2xx 视为成功，与仪表盘 success_rate 口径一致） */
function isOk(log: UsageLog): boolean {
  return log.status_code >= 200 && log.status_code < 300
}
</script>

<template>
  <!-- 滑动提示只在桌面端出现：窄屏已切换为卡片视图（.table-cards），不需要左右滑动 -->
  <p class="mb-2 hidden text-xs text-ink-400 lg:block">{{ $t('components.logTable.scrollHint') }}</p>

  <div class="table-wrap table-cards">
    <table class="data-table min-w-[1080px]">
      <thead>
        <tr>
          <th class="w-[9rem]">{{ $t('components.logTable.col.time') }}</th>
          <th v-if="showUser">{{ $t('components.logTable.col.user') }}</th>
          <th>{{ $t('components.logTable.col.model') }}</th>
          <th v-if="showChannel">{{ $t('components.logTable.col.channel') }}</th>
          <th>{{ $t('components.logTable.col.token') }}</th>
          <th class="text-right">{{ $t('components.logTable.col.tokens') }}</th>
          <th class="text-right">{{ $t('components.logTable.col.total') }}</th>
          <th class="text-right">{{ $t('components.logTable.col.quota') }}</th>
          <th class="text-right">{{ $t('components.logTable.col.latency') }}</th>
          <th>{{ $t('components.logTable.col.kind') }}</th>
          <th>{{ $t('components.logTable.col.status') }}</th>
        </tr>
      </thead>

      <tbody>
        <DataState
          :loading="loading"
          :error="error"
          :empty="!logs.length"
          :colspan="columnCount"
          :empty-text="emptyMessage"
          :empty-hint="emptyDescription"
          :loading-text="$t('components.logTable.loading')"
          @retry="emit('retry')"
        />

        <template v-if="!loading && !error && logs.length">
          <tr v-for="log in logs" :key="log.id">
            <td class="cell-muted whitespace-nowrap" :title="formatDateTime(log.created_at)" :data-label="$t('components.logTable.col.time')">
              {{ formatRelative(log.created_at) }}
            </td>

            <td v-if="showUser" class="whitespace-nowrap" :data-label="$t('components.logTable.col.user')">
              <span class="text-ink-100">{{ log.username || `#${log.user_id}` }}</span>
              <span class="ms-1 text-[11px] text-ink-500">#{{ log.user_id }}</span>
            </td>

            <td class="whitespace-nowrap" :data-label="$t('components.logTable.col.model')">
              <span class="chip" dir="ltr">{{ log.model || '—' }}</span>
            </td>

            <td v-if="showChannel" class="whitespace-nowrap text-ink-200" :data-label="$t('components.logTable.col.channel')">
              {{ log.channel_name || (log.channel_id ? `#${log.channel_id}` : '—') }}
            </td>

            <td class="max-w-[10rem] truncate text-ink-200" :title="log.token_name" :data-label="$t('components.logTable.col.token')">
              {{ log.token_name || '—' }}
            </td>

            <td class="cell-num text-ink-300" :data-label="$t('components.logTable.col.tokens')">
              {{ formatNumber(log.prompt_tokens) }} / {{ formatNumber(log.completion_tokens) }}
            </td>

            <td class="cell-num" :data-label="$t('components.logTable.col.total')">{{ formatNumber(log.total_tokens) }}</td>
            <td class="cell-num" :data-label="$t('components.logTable.col.quota')">{{ formatNumber(log.quota) }}</td>
            <td class="cell-num text-ink-300" :data-label="$t('components.logTable.col.latency')">{{ formatLatency(log.latency_ms) }}</td>

            <td :data-label="$t('components.logTable.col.kind')">
              <span class="badge badge-off">{{ log.is_stream ? $t('components.logTable.stream') : $t('components.logTable.nonStream') }}</span>
            </td>

            <td :data-label="$t('components.logTable.col.status')">
              <div class="flex items-center gap-1.5">
                <span :class="httpStatusBadgeClass(log.status_code)">
                  <span class="dot" />
                  {{ log.status_code || '—' }}
                </span>
                <!-- 失败原因优先用 title 提示，避免长错误信息破坏表格密度 -->
                <span v-if="log.error" class="text-red-600" :title="log.error">
                  <AppIcon name="alert" :size="14" />
                </span>
                <span v-if="!isOk(log) && !log.error" class="text-ink-500">{{ $t('components.logTable.failed') }}</span>
              </div>
            </td>
          </tr>
        </template>
      </tbody>
    </table>

    <!-- 底部插槽：用于放置分页控件，让它与表格共用一个边框容器，视觉上更整体 -->
    <slot name="footer" />
  </div>
</template>
