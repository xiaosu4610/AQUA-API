<script setup lang="ts">
/**
 * 管理后台 · 操作审计：后台写操作的可追溯记录（分页 + 多条件筛选）。
 *
 * 意图（Why）：
 *   出了误操作或越权时，管理员需要回答"谁、什么时候、对什么、做了什么、结果如何"。
 *   后端审计中间件已把所有写操作（POST/PUT/PATCH/DELETE）落库，本页负责按
 *   管理员、方法、路径、状态码与时间范围组合查出来。
 *
 * 流转（Flow）：
 *   进入页面 → listAuditLogs({page, page_size}) → 表格 → Pagination 翻页
 *   筛选变化 → 回到第 1 页重新请求
 *
 * 扩展（Extend）：
 *   新增筛选条件：在 src/api/audit.ts 的 AuditLogQuery 加字段 → 本页加控件 → 传入请求。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import { listAuditLogs } from '@/api/audit'
import type { AuditLog } from '@/api/audit'
import { formatDateTime, formatLatency } from '@/utils/format'

const logs = ref<AuditLog[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')

const adminFilter = ref('')
const methodFilter = ref('')
const pathFilter = ref('')
const statusFilter = ref('')
const startFilter = ref('')
const endFilter = ref('')

const hasFilter = computed(
  () =>
    Boolean(
      adminFilter.value.trim() ||
        methodFilter.value ||
        pathFilter.value.trim() ||
        statusFilter.value.trim() ||
        startFilter.value ||
        endFilter.value,
    ),
)

/** detail 在表格里截断展示，完整内容放在 title 提示中 */
function previewDetail(detail: string): string {
  if (!detail) return '—'
  const runes = Array.from(detail)
  return runes.length > 80 ? runes.slice(0, 80).join('') + '…' : detail
}

/** 状态码徽标样式：2xx 视为成功，其余视为异常 */
function statusClass(code: number): string {
  if (code >= 200 && code < 300) return 'badge badge-ok'
  if (code >= 400) return 'badge badge-off'
  return 'badge badge-warn'
}

/** 方法徽标样式：删除类操作醒目提示 */
function methodClass(method: string): string {
  return method === 'DELETE' ? 'badge badge-off' : 'badge badge-info'
}

function timeParam(value: string): string | undefined {
  // datetime-local（"YYYY-MM-DDTHH:mm"）直接交给后端解析（后端支持该格式），
  // 因此这里原样返回，只是在为空时统一成 undefined 以便被 cleanParams 剔除。
  return value ? value : undefined
}

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listAuditLogs({
      page: page.value,
      page_size: size.value,
      admin_id: adminFilter.value.trim() ? Number(adminFilter.value.trim()) : undefined,
      method: methodFilter.value || undefined,
      path: pathFilter.value.trim() || undefined,
      status_code: statusFilter.value.trim() ? Number(statusFilter.value.trim()) : undefined,
      start: timeParam(startFilter.value),
      end: timeParam(endFilter.value),
    })
    logs.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    logs.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '审计日志加载失败'
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
  adminFilter.value = ''
  methodFilter.value = ''
  pathFilter.value = ''
  statusFilter.value = ''
  startFilter.value = ''
  endFilter.value = ''
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

const emptyText = computed(() => (hasFilter.value ? '没有符合筛选条件的操作记录' : '暂无操作记录'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '试试放宽筛选条件，或点击「重置」查看全部记录。'
    : '管理员在后台进行新增 / 修改 / 删除等写操作后，这里会出现对应记录。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">操作审计</h2>
        <p class="page-desc">后台所有写操作（新增 / 修改 / 删除）的追溯记录，可按管理员、方法与时间筛选。</p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
        <AppIcon name="refresh" :size="14" />
        刷新
      </button>
    </div>

    <!-- 筛选条：条件较多，采用 flex 换行，窄屏下自动堆叠 -->
    <div class="filter-bar mb-4">
      <div class="min-w-[9rem] flex-1">
        <label class="label" for="audit-admin">管理员 ID</label>
        <input
          id="audit-admin"
          v-model="adminFilter"
          class="input"
          type="text"
          inputmode="numeric"
          placeholder="如 1"
          @keydown.enter="applyFilter"
        />
      </div>

      <div>
        <label class="label" for="audit-method">方法</label>
        <select id="audit-method" v-model="methodFilter" class="input min-w-[8rem]">
          <option value="">全部方法</option>
          <option value="POST">POST（新增）</option>
          <option value="PUT">PUT（更新）</option>
          <option value="PATCH">PATCH（部分更新）</option>
          <option value="DELETE">DELETE（删除）</option>
        </select>
      </div>

      <div class="min-w-[12rem] flex-1">
        <label class="label" for="audit-path">路径前缀</label>
        <input
          id="audit-path"
          v-model="pathFilter"
          class="input input-mono"
          type="text"
          placeholder="如 /api/admin/channels"
          @keydown.enter="applyFilter"
        />
      </div>

      <div>
        <label class="label" for="audit-status">状态码</label>
        <input
          id="audit-status"
          v-model="statusFilter"
          class="input min-w-[6rem]"
          type="text"
          inputmode="numeric"
          placeholder="如 200"
          @keydown.enter="applyFilter"
        />
      </div>

      <div>
        <label class="label" for="audit-start">开始时间</label>
        <input id="audit-start" v-model="startFilter" class="input" type="datetime-local" />
      </div>

      <div>
        <label class="label" for="audit-end">结束时间</label>
        <input id="audit-end" v-model="endFilter" class="input" type="datetime-local" />
      </div>

      <div class="flex items-center gap-2">
        <button type="button" class="btn btn-primary btn-sm" :disabled="loading" @click="applyFilter">
          <AppIcon name="filter" :size="14" />
          应用筛选
        </button>
        <button v-if="hasFilter" type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="resetFilter">
          重置
        </button>
      </div>
    </div>

    <div class="table-wrap table-cards">
      <table class="data-table">
        <thead>
          <tr>
            <th>时间</th>
            <th>管理员</th>
            <th>动作</th>
            <th>方法</th>
            <th>路径</th>
            <th>目标</th>
            <th class="text-right">状态码</th>
            <th class="text-right">耗时</th>
            <th>客户端 IP</th>
            <th>请求摘要</th>
          </tr>
        </thead>
        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="!loading && !error && logs.length === 0"
            :colspan="10"
            loading-text="正在读取审计记录…"
            :empty-text="emptyText"
            :empty-hint="emptyHint"
            @retry="load"
          />

          <tr v-for="log in logs" :key="log.id">
            <td class="cell-muted" data-label="时间">{{ formatDateTime(log.created_at) }}</td>
            <td data-label="管理员">
              <span class="text-ink-200">{{ log.admin_username || '—' }}</span>
              <span v-if="log.admin_id" class="ml-1 text-[11px] text-ink-500">#{{ log.admin_id }}</span>
            </td>
            <td data-label="动作">{{ log.action || '—' }}</td>
            <td data-label="方法"><span :class="methodClass(log.method)">{{ log.method }}</span></td>
            <td data-label="路径"><code class="font-mono text-[12px] text-ink-300">{{ log.path }}</code></td>
            <td class="cell-muted" data-label="目标">{{ log.target || '—' }}</td>
            <td class="cell-num" data-label="状态码">
              <span :class="statusClass(log.status_code)">{{ log.status_code }}</span>
            </td>
            <td class="cell-num" data-label="耗时">{{ formatLatency(log.latency_ms) }}</td>
            <td class="cell-muted" data-label="客户端 IP">{{ log.client_ip || '—' }}</td>
            <td data-label="请求摘要">
              <code class="font-mono text-[12px] text-ink-400" :title="log.detail">{{ previewDetail(log.detail) }}</code>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="total > 0" class="mt-3 card">
      <Pagination
        :page="page"
        :size="size"
        :total="total"
        :disabled="loading"
        @update:page="changePage"
        @update:size="changeSize"
      />
    </div>
  </div>
</template>
