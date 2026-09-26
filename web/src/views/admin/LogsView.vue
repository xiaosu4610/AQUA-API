<script setup lang="ts">
/**
 * 管理后台 · 调用日志：全站调用记录（分页 + 多条件筛选）。
 *
 * 意图（Why）：
 *   排查线上问题时，管理员通常按「谁在用」（用户）、「用的什么」（模型/渠道）、
 *   「有没有报错」（状态）三个维度定位，因此筛选条同时提供这四个条件。
 *
 * 流转（Flow）：
 *   进入页面 → listAllLogs({page,size}) + listChannels()（为渠道筛选准备选项）
 *   → LogTable（含用户/渠道列）→ Pagination 翻页
 *   筛选变化 → 回到第 1 页重新请求
 *
 * 扩展（Extend）：
 *   新增筛选条件：在 api/types.ts 的 LogQuery 加字段 → 本页加控件 → 传入 listAllLogs。
 *   注意：user / channel_id 的具体查询参数名待与后端联调确认（见接口契约对齐项）。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import LogTable from '@/components/LogTable.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import { listAllLogs, listChannels } from '@/api/admin'
import type { Channel, UsageLog } from '@/api/types'
import { useSiteStore } from '@/stores/site'

const site = useSiteStore()

const logs = ref<UsageLog[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')

/** 渠道筛选选项（用于把 channel_id 变成可读的下拉选择） */
const channels = ref<Channel[]>([])

const userFilter = ref('')
const modelFilter = ref('')
const channelFilter = ref<number | ''>('')
const statusFilter = ref<'' | 'success' | 'error'>('')

const hasFilter = computed(
  () => Boolean(userFilter.value.trim() || modelFilter.value.trim() || channelFilter.value !== '' || statusFilter.value),
)

async function loadLogs(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listAllLogs({
      page: page.value,
      size: size.value,
      user: userFilter.value.trim() || undefined,
      model: modelFilter.value.trim() || undefined,
      channel_id: channelFilter.value === '' ? undefined : Number(channelFilter.value),
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

/** 渠道下拉只在首次进入时加载一次（渠道数量有限，且变更不频繁） */
async function loadChannelOptions(): Promise<void> {
  try {
    const result = await listChannels({ page: 1, size: 100 })
    channels.value = result.items ?? []
  } catch {
    // 渠道选项加载失败只影响筛选便利性，不影响日志主流程，故不打断用户
    channels.value = []
  }
}

onMounted(async () => {
  await Promise.all([loadLogs(), loadChannelOptions()])
})

function applyFilter(): void {
  page.value = 1
  void loadLogs()
}

function resetFilter(): void {
  userFilter.value = ''
  modelFilter.value = ''
  channelFilter.value = ''
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

const emptyText = computed(() => (hasFilter.value ? '没有符合筛选条件的记录' : '暂无调用记录'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '试试放宽筛选条件，或点击「重置」查看全部记录。'
    : '当有客户端通过访问令牌调用模型后，这里会出现全站调用明细。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">调用日志</h2>
        <p class="page-desc">全站调用明细，可按用户、模型、渠道与状态组合筛选。</p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadLogs">
        <AppIcon name="refresh" :size="14" />
        刷新
      </button>
    </div>

    <!-- 筛选条：条件较多，采用 flex 换行，窄屏下自动堆叠 -->
    <div class="filter-bar mb-4">
      <div class="min-w-[10rem] flex-1">
        <label class="label" for="admin-log-user">用户</label>
        <div class="relative">
          <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-ink-500">
            <AppIcon name="search" :size="15" />
          </span>
          <input
            id="admin-log-user"
            v-model="userFilter"
            class="input pl-9"
            type="text"
            placeholder="用户名或用户 ID"
            @keydown.enter="applyFilter"
          />
        </div>
      </div>

      <div class="min-w-[10rem] flex-1">
        <label class="label" for="admin-log-model">模型</label>
        <div class="relative">
          <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-ink-500">
            <AppIcon name="layers" :size="15" />
          </span>
          <input
            id="admin-log-model"
            v-model="modelFilter"
            class="input pl-9 input-mono"
            type="text"
            list="admin-log-models"
            placeholder="模型名"
            @keydown.enter="applyFilter"
          />
          <datalist id="admin-log-models">
            <option v-for="model in site.models" :key="model" :value="model" />
          </datalist>
        </div>
      </div>

      <div class="min-w-[10rem]">
        <label class="label" for="admin-log-channel">渠道</label>
        <select id="admin-log-channel" v-model="channelFilter" class="input min-w-[10rem]">
          <option value="">全部渠道</option>
          <option v-for="channel in channels" :key="channel.id" :value="channel.id">
            {{ channel.name }}
          </option>
        </select>
      </div>

      <div>
        <label class="label" for="admin-log-status">状态</label>
        <select id="admin-log-status" v-model="statusFilter" class="input min-w-[8rem]">
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
      show-user
      show-channel
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
