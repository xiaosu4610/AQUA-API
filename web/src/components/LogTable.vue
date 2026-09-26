<script setup lang="ts">
/**
 * 调用日志表格（门户与管理端共用）。
 *
 * 意图（Why）：
 *   门户「我的调用记录」与管理「全站调用日志」是同一张表的两种视图（后者多两列）；
 *   共用组件可保证列宽、对齐、状态徽标与错误提示的呈现完全一致。
 *
 * 流转（Flow）：
 *   页面请求 /api/user/logs 或 /api/admin/logs → 传入 logs → 本组件渲染
 *   状态渲染优先级：loading > error > empty > 数据行
 *
 * 扩展（Extend）：
 *   新增列（如 request_id）时：在表头与数据行同时添加，并同步 bumpColumnCount 的基数，
 *   否则空态行的 colSpan 会对不齐。
 */
import { computed } from 'vue'

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
    emptyText?: string
    emptyHint?: string
  }>(),
  {
    loading: false,
    error: '',
    showUser: false,
    showChannel: false,
    emptyText: '暂无调用记录',
    emptyHint: '使用访问令牌发起一次模型请求后，这里会出现对应的调用明细。',
  },
)

const emit = defineEmits<{ (e: 'retry'): void }>()

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
  <div class="table-wrap">
    <table class="data-table min-w-[1080px]">
      <thead>
        <tr>
          <th class="w-[9rem]">时间</th>
          <th v-if="showUser">用户</th>
          <th>模型</th>
          <th v-if="showChannel">渠道</th>
          <th>令牌</th>
          <th class="text-right">入 / 出 Token</th>
          <th class="text-right">合计</th>
          <th class="text-right">配额</th>
          <th class="text-right">延迟</th>
          <th>类型</th>
          <th>状态</th>
        </tr>
      </thead>

      <tbody>
        <DataState
          :loading="loading"
          :error="error"
          :empty="!logs.length"
          :colspan="columnCount"
          :empty-text="emptyText"
          :empty-hint="emptyHint"
          loading-text="正在加载调用记录…"
          @retry="emit('retry')"
        />

        <template v-if="!loading && !error && logs.length">
          <tr v-for="log in logs" :key="log.id">
            <td class="cell-muted whitespace-nowrap" :title="formatDateTime(log.created_at)">
              {{ formatRelative(log.created_at) }}
            </td>

            <td v-if="showUser" class="whitespace-nowrap">
              <span class="text-ink-100">{{ log.username || `#${log.user_id}` }}</span>
              <span class="ml-1 text-[11px] text-ink-500">#{{ log.user_id }}</span>
            </td>

            <td class="whitespace-nowrap">
              <span class="chip">{{ log.model || '—' }}</span>
            </td>

            <td v-if="showChannel" class="whitespace-nowrap text-ink-200">
              {{ log.channel_name || (log.channel_id ? `#${log.channel_id}` : '—') }}
            </td>

            <td class="max-w-[10rem] truncate text-ink-200" :title="log.token_name">
              {{ log.token_name || '—' }}
            </td>

            <td class="cell-num text-ink-300">
              {{ formatNumber(log.prompt_tokens) }} / {{ formatNumber(log.completion_tokens) }}
            </td>

            <td class="cell-num">{{ formatNumber(log.total_tokens) }}</td>
            <td class="cell-num">{{ formatNumber(log.quota) }}</td>
            <td class="cell-num text-ink-300">{{ formatLatency(log.latency_ms) }}</td>

            <td>
              <span class="badge badge-off">{{ log.is_stream ? '流式' : '非流式' }}</span>
            </td>

            <td>
              <div class="flex items-center gap-1.5">
                <span :class="httpStatusBadgeClass(log.status_code)">
                  <span class="dot" />
                  {{ log.status_code || '—' }}
                </span>
                <!-- 失败原因优先用 title 提示，避免长错误信息破坏表格密度 -->
                <span v-if="log.error" class="text-red-400" :title="log.error">
                  <AppIcon name="alert" :size="14" />
                </span>
                <span v-if="!isOk(log) && !log.error" class="text-ink-500">失败</span>
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
