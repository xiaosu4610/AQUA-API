<script setup lang="ts">
/**
 * 用户门户 · 生成任务：我提交的图像/视频等异步任务。
 *
 * 意图（Why）：
 *   任务类请求不是"一问一答"，用户在浏览器里最需要确认的是
 *   「提交成功了没」「现在到哪一步了」「结果在哪」。
 *   因此本页以任务为核心：进度条 + 状态 + 结果入口，并支持手动刷新
 *   （后台有轮询器兜底推进，用户手动刷新只是让自己更快看到结果）。
 *
 * 为什么没有"取消"：门户不暴露取消入口——取消会退还额度，
 *   属于计费敏感的副作用，统一由管理员在后台操作，避免被滥用。
 *
 * 流转（Flow）：
 *   进入页面 → listMyTasks({page,size}) → TaskTable（只读）
 *   客户端提交任务走 POST /v1/tasks（使用访问令牌，不在浏览器里发起）
 *
 * 扩展（Extend）：
 *   需要"在网页里直接提交任务"时，必须走服务端代理（浏览器不该持有访问令牌），
 *   不要在此直接调用 /v1/tasks。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import Pagination from '@/components/Pagination.vue'
import TaskTable from '@/components/TaskTable.vue'
import { ApiError } from '@/api/client'
import { listMyTasks } from '@/api/portal'
import type { Task } from '@/api/types'

const tasks = ref<Task[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')
const kindFilter = ref('')

const hasFilter = computed(() => kindFilter.value !== '')

/** 对外 base_url 前缀（含 /v1），用于示例命令 */
const baseUrl = computed(() => `${window.location.origin}/v1`)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listMyTasks({
      page: page.value,
      size: size.value,
      kind: kindFilter.value || undefined,
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

function changePage(next: number): void {
  page.value = next
  void load()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void load()
}

function applyFilter(): void {
  page.value = 1
  void load()
}

const emptyText = computed(() => (hasFilter.value ? '该类别下暂无任务' : '还没有生成任务'))
const emptyHint = computed(() =>
  hasFilter.value
    ? '换个类别看看，或清空筛选。'
    : '用访问令牌调用 POST /v1/tasks 提交图像/视频生成任务后，这里会显示进度与结果。',
)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">生成任务</h2>
        <p class="page-desc">
          通过 <code class="chip">POST /v1/tasks</code> 提交的图像、视频等异步任务。
          任务在提交时扣减额度，失败或取消会自动退还。
        </p>
      </div>
      <div class="toolbar">
        <select v-model="kindFilter" class="input min-w-[9rem]" @change="applyFilter">
          <option value="">全部类别</option>
          <option value="image">图像生成</option>
          <option value="video">视频生成</option>
          <option value="music">音乐生成</option>
        </select>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
      </div>
    </div>

    <TaskTable
      :tasks="tasks"
      :loading="loading"
      :error="error"
      :empty-text="emptyText"
      :empty-hint="emptyHint"
      @retry="load"
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

    <section class="mt-5 card card-pad">
      <h3 class="section-title flex items-center gap-2">
        <AppIcon name="bolt" :size="16" class="text-brand-700" />
        如何提交任务
      </h3>
      <div class="code-block mt-3">
        <pre>curl -X POST {{ baseUrl }}/tasks \
  -H "Authorization: Bearer &lt;你的访问令牌&gt;" \
  -H "Content-Type: application/json" \
  -d '{"kind":"image","model":"&lt;模型名&gt;","prompt":"一只在雨里的猫","n":1}'

# 查询结果
curl {{ baseUrl }}/tasks/&lt;任务号&gt; -H "Authorization: Bearer &lt;你的访问令牌&gt;"</pre>
      </div>
      <p class="hint">
        提示：模型名可在「模型广场」查看；任务在提交时即扣费，失败会自动退还，不会白花额度。
      </p>
    </section>
  </div>
</template>
