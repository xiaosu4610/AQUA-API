<script setup lang="ts">
/**
 * 用户门户 · 调用日志：我的调用记录（分页 + 按模型/状态筛选）。
 *
 * 意图（Why）：
 *   排查「某次调用为什么失败」「这个月哪个模型用得最多」都依赖这张表；
 *   因此筛选条件要能一眼看清当前生效条件，并提供一键重置。
 *
 * 流转（Flow）：
 *   进入页面 → listMyLogs({page,size,model,status}) → LogTable 渲染 → Pagination 翻页
 *   筛选条件变化 → 重置到第 1 页后重新请求（避免停留在越界页码）
 *
 * 扩展（Extend）：
 *   新增筛选条件时：在 api/types.ts 的 LogQuery 加字段 → 本页加控件 → 传参。
 *   注意：status 取值 success/error 为前端约定，后端字段名待联调确认（见接口契约对齐项）。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import LogTable from '@/components/LogTable.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import { listMyLogs } from '@/api/portal'
import type { UsageLog } from '@/api/types'
import { useSiteStore } from '@/stores/site'

const site = useSiteStore()

const logs = ref<UsageLog[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')

/** 筛选条件（与 LogQuery 对齐） */
const modelFilter = ref('')
const statusFilter = ref<'' | 'success' | 'error'>('')

/** 是否有生效的筛选条件（用于展示「重置」与空态文案） */
const hasFilter = computed(() => Boolean(modelFilter.value.trim() || statusFilter.value))

async function loadLogs(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listMyLogs({
      page: page.value,
      size: size.value,
      model: modelFilter.value.trim() || undefined,
      status: statusFilter.value || undefined,
    })
    logs.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    logs.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '日志加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(loadLogs)

/** 筛选条件变化后必须回到第 1 页：否则可能请求到越界页码，出现「空列表 + 有总数」的怪状态 */
function applyFilter(): void {
  page.value = 1
  void loadLogs()
}

function resetFilter(): void {
  modelFilter.value = ''
  statusFilter.value = ''
  applyFilter()
}

function changePage(next: number): void {
  page.value = next
  void loadLogs()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void loadLogs()
}

/** 空态文案随筛选状态变化，避免用户误以为「系统里没有数据」 */
const emptyText = computed(() => (hasFilter.value ? '没有符合筛选条件的记录' : '暂无调用记录'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '试试放宽筛选条件，或点击「重置」查看全部记录。'
    : '创建访问令牌并发起一次模型请求后，这里会显示调用明细。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">调用日志</h2>
        <p class="page-desc">按时间倒序展示你的每一次模型调用，可核对 Token 消耗与失败原因。</p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadLogs">
        <AppIcon name="refresh" :size="14" />
        刷新
      </button>
    </div>

    <!-- 筛选条 -->
    <div class="filter-bar mb-4">
      <div class="min-w-[12rem] flex-1">
        <label class="label" for="console-log-model">模型</label>
        <div class="relative">
          <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-ink-500">
            <AppIcon name="search" :size="15" />
          </span>
          <input
            id="console-log-model"
            v-model="modelFilter"
            class="input pl-9 input-mono"
            type="text"
            list="console-log-models"
            placeholder="按模型名筛选，回车应用"
            @keydown.enter="applyFilter"
          />
          <datalist id="console-log-models">
            <option v-for="model in site.models" :key="model" :value="model" />
          </datalist>
        </div>
      </div>

      <div>
        <label class="label" for="console-log-status">状态</label>
        <select id="console-log-status" v-model="statusFilter" class="input min-w-[8rem]">
          <option value="">全部</option>
          <option value="success">仅成功</option>
          <option value="error">仅失败</option>
        </select>
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

    <LogTable
      :logs="logs"
      :loading="loading"
      :error="error"
      :empty-text="emptyText"
      :empty-hint="emptyHint"
      @retry="loadLogs"
    >
      <template #footer>
        <Pagination
          v-if="total > 0"
          :page="page"
          :size="size"
          :total="total"
          :disabled="loading"
          @update:page="changePage"
          @update:size="changeSize"
        />
      </template>
    </LogTable>
  </div>
</template>
