<script setup lang="ts">
/**
 * 管理后台 · 异步任务：全站生成类任务（图像/视频/音乐）。
 *
 * 意图（Why）：
 *   任务类接口是"提交后异步完成"的，站长最关心两件事：
 *     1) 有没有大量任务卡在"进行中"（说明上游或轮询器有问题）；
 *     2) 失败的任务是因为什么（额度已扣但结果没拿到，属于必须解释清楚的情形）。
 *   因此本页默认按创建时间倒序，并提供状态与类别筛选。
 *
 * 为什么提供"取消"：任务在提交时就已扣费，若上游长时间无响应，
 *   站长需要能主动取消并退还额度，而不是让它永久挂着。
 *
 * 流转（Flow）：
 *   进入页面 → listAllTasks({page,size}) → TaskTable
 *   取消 → cancelTask(task_ref) → 重新加载
 *
 * 扩展（Extend）：
 *   新增筛选维度：在 types.ts 的 TaskQuery 加字段 → 本页加控件 → 传入接口。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import Pagination from '@/components/Pagination.vue'
import TaskTable from '@/components/TaskTable.vue'
import { ApiError } from '@/api/client'
import { cancelTask, listAllTasks } from '@/api/admin'
import type { Task } from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'

const tasks = ref<Task[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')
const cancelling = ref('')

const kindFilter = ref('')
const statusFilter = ref<'' | number>('')

const hasFilter = computed(() => kindFilter.value !== '' || statusFilter.value !== '')

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listAllTasks({
      page: page.value,
      size: size.value,
      kind: kindFilter.value || undefined,
      status: statusFilter.value === '' ? undefined : Number(statusFilter.value),
    })
    tasks.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    tasks.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '任务加载失败'
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
  kindFilter.value = ''
  statusFilter.value = ''
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

async function handleCancel(task: Task): Promise<void> {
  const ok = await confirmDialog({
    title: '取消任务',
    message: `任务 ${task.task_ref} 将被标记为已取消，已扣减的额度（${task.quota}）会退还给用户。`,
    confirmText: '取消任务',
    danger: true,
  })
  if (!ok) return

  cancelling.value = task.task_ref
  try {
    await cancelTask(task.task_ref)
    toastSuccess('任务已取消，额度已退还')
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '取消失败')
  } finally {
    cancelling.value = ''
  }
}

const emptyText = computed(() => (hasFilter.value ? '没有符合筛选条件的任务' : '暂无异步任务'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '试试放宽筛选条件，或点击「重置」查看全部任务。'
    : '当客户端调用 POST /v1/tasks 提交图像/视频生成任务后，这里会出现任务明细。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">异步任务</h2>
        <p class="page-desc">
          图像、视频等生成类任务。任务在<strong>提交时即扣费</strong>，失败或取消会自动退还额度。
          点击任务号可查看提交参数与上游返回原文。
        </p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
        <AppIcon name="refresh" :size="14" />
        刷新
      </button>
    </div>

    <div class="filter-bar mb-4">
      <div>
        <label class="label" for="task-kind">类别</label>
        <select id="task-kind" v-model="kindFilter" class="input min-w-[9rem]">
          <option value="">全部类别</option>
          <option value="image">图像生成</option>
          <option value="video">视频生成</option>
          <option value="music">音乐生成</option>
        </select>
      </div>

      <div>
        <label class="label" for="task-status">状态</label>
        <select id="task-status" v-model="statusFilter" class="input min-w-[9rem]">
          <option value="">全部状态</option>
          <option :value="1">排队中</option>
          <option :value="2">进行中</option>
          <option :value="3">已完成</option>
          <option :value="4">已失败</option>
          <option :value="5">已取消</option>
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

      <div class="ml-auto flex items-center gap-2 text-xs text-ink-400">
        <span class="badge badge-info">{{ total }} 条</span>
      </div>
    </div>

    <TaskTable
      :tasks="tasks"
      :loading="loading"
      :error="error"
      :empty-text="emptyText"
      :empty-hint="emptyHint"
      :cancelling="cancelling"
      show-user
      allow-cancel
      @retry="load"
      @cancel="handleCancel"
    />

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

    <!-- 状态说明：把"哪些状态是终态、失败会不会退钱"讲清楚，减少误判 -->
    <section class="mt-5 card card-pad">
      <h3 class="section-title flex items-center gap-2">
        <AppIcon name="info" :size="16" class="text-brand-700" />
        关于任务状态
      </h3>
      <ul class="mt-3 space-y-1.5 text-xs leading-relaxed text-ink-400">
        <li>
          <span class="badge badge-warn">排队中</span>
          <span class="badge badge-info">进行中</span>
          为非终态，后台轮询器会持续向上游查询进度（客户端不查询也不会卡住）。
        </li>
        <li>
          <span class="badge badge-ok">已完成</span> 结果可取；
          <span class="badge badge-err">已失败</span> 与
          <span class="badge badge-off">已取消</span> 会退还提交时扣减的额度。
        </li>
        <li>
          若大量任务长期停在「进行中」，通常说明上游接口异常或渠道密钥失效，
          请到「渠道管理」测活并查看密钥池状态。
        </li>
      </ul>
    </section>
  </div>
</template>
