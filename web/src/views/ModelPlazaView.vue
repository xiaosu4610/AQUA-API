<script setup lang="ts">
/**
 * 模型广场：把网关当前"能用什么、什么价"摊开给访客看（无需登录）。
 *
 * 意图（Why）：
 *   落地页只列了模型名，无法回答"这个模型多少钱、在哪个分组、现在有没有渠道"。
 *   模型广场承担这件事：按分组筛选 + 关键词搜索 + 模型卡片（可用状态与价格）。
 *   它是"这个网关能做什么"的唯一权威清单，因此数据全部来自公开接口
 *   GET /api/models，不依赖登录态。
 *
 * 为什么过滤放在服务端：模型数量可达上百，服务端过滤能显著减少传输量；
 * 关键词输入做了防抖，避免每敲一个字发一次请求。
 *
 * 流转（Flow）：
 *   进入页面 → fetchModelPlaza() → 分组筛选条 + 卡片网格
 *   点击分组 / 输入关键词 → 重新请求（带参数）→ 刷新卡片与分组计数
 *
 * 扩展（Extend）：
 *   新增卡片信息请改 components/ModelCard.vue；
 *   新增筛选维度（能力标签、上游厂商）时在此加控件并传给 fetchModelPlaza。
 */
import { computed, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import ModelCard from '@/components/ModelCard.vue'
import { fetchModelPlaza } from '@/api/site'
import type { ModelPlaza } from '@/api/types'
import { toastSuccess } from '@/composables/useToast'
import { copyText } from '@/utils/clipboard'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const site = useSiteStore()
const auth = useAuthStore()

const plaza = ref<ModelPlaza | null>(null)
const loading = ref(false)
const error = ref('')

/** 当前选中的分组（空串表示全部） */
const activeGroup = ref('')
/** 关键词输入值与生效值分开：输入即更新输入框，防抖后才真正请求 */
const keywordInput = ref('')
const keyword = ref('')

let debounceTimer = 0

const groups = computed(() => plaza.value?.groups ?? [])
const items = computed(() => plaza.value?.items ?? [])

/** 分组名 → 展示名，供卡片显示中文标签 */
const groupLabels = computed(() => {
  const map: Record<string, string> = {}
  for (const group of groups.value) map[group.name] = group.label
  return map
})

/** 可用模型数：给访客一个"现在能调多少"的直接答案 */
const availableCount = computed(() => items.value.filter((item) => item.available).length)

const consoleTarget = computed(() => (auth.isAdmin ? { name: 'admin-dashboard' } : { name: 'console-overview' }))

/** 对外 base_url：用浏览器地址而非硬编码域名，任何部署环境复制出来都能直接用 */
const baseUrl = computed(() => `${window.location.origin}/v1`)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    plaza.value = await fetchModelPlaza({ group: activeGroup.value, keyword: keyword.value })
  } catch (err) {
    error.value = err instanceof Error ? err.message : '加载模型广场失败'
  } finally {
    loading.value = false
  }
}

/** 切换分组：立即请求（点击是明确意图，不需要防抖） */
function selectGroup(name: string): void {
  if (activeGroup.value === name) return
  activeGroup.value = name
  void load()
}

/** 关键词输入：300ms 防抖，避免逐字请求 */
function onKeywordInput(): void {
  window.clearTimeout(debounceTimer)
  debounceTimer = window.setTimeout(() => {
    keyword.value = keywordInput.value.trim()
    void load()
  }, 300)
}

async function handleCopy(value: string): Promise<void> {
  const ok = await copyText(value)
  if (ok) toastSuccess(`已复制模型名 ${value}`)
}

onMounted(async () => {
  // 站点信息用于页头站点名；失败不阻断广场（广场有自己的错误态）
  void site.load()
  await load()
})
</script>

<template>
  <div class="min-h-screen bg-ink-950">
    <!-- ── 页头 ─────────────────────────────────────────── -->
    <header class="sticky top-0 z-20 border-b border-ink-800/70 bg-white/70 backdrop-blur-xl">
      <div class="mx-auto flex max-w-6xl flex-wrap items-center gap-3 px-5 py-3 lg:px-8">
        <RouterLink to="/" class="flex items-center gap-2.5">
          <img src="/favicon.ico" alt="" class="h-8 w-8 rounded-lg" />
          <span class="text-sm font-semibold text-ink-50">{{ site.siteName }}</span>
        </RouterLink>

        <nav class="ml-3 hidden items-center gap-1 md:flex">
          <RouterLink to="/" class="btn btn-ghost btn-sm">首页</RouterLink>
          <RouterLink to="/models" class="btn btn-ghost btn-sm text-brand-700">模型广场</RouterLink>
          <RouterLink v-if="auth.isLoggedIn" :to="consoleTarget" class="btn btn-ghost btn-sm">控制台</RouterLink>
        </nav>

        <div class="ml-auto flex items-center gap-2">
          <RouterLink v-if="!auth.isLoggedIn" to="/login" class="btn btn-ghost btn-sm">登录</RouterLink>
          <RouterLink v-else :to="consoleTarget" class="btn btn-primary btn-sm">
            <AppIcon :name="auth.isAdmin ? 'shield' : 'home'" :size="15" />
            {{ auth.isAdmin ? '管理后台' : '用户门户' }}
          </RouterLink>
        </div>
      </div>
    </header>

    <main class="mx-auto max-w-6xl px-5 py-8 lg:px-8">
      <!-- ── 标题与统计 ─────────────────────────────────── -->
      <div class="page-head">
        <div>
          <h1 class="page-title">模型广场</h1>
          <p class="page-desc">
            本站当前对外提供的全部模型与价格。调用方式与 OpenAI 接口一致，
            只需把 <code class="chip">base_url</code> 指向本站并用访问令牌鉴权。
          </p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <span class="badge badge-ok">
            <span class="dot" />
            {{ availableCount }} 个可用
          </span>
          <span class="badge badge-off">共 {{ items.length }} 个模型</span>
        </div>
      </div>

      <!-- ── 筛选条 ─────────────────────────────────────── -->
      <div class="filter-bar mb-6">
        <div class="min-w-[200px] flex-1">
          <label class="label" for="plaza-keyword">
            <AppIcon name="search" :size="13" />
            搜索模型
          </label>
          <input
            id="plaza-keyword"
            v-model="keywordInput"
            class="input"
            type="search"
            placeholder="输入模型名片段，如 gpt / glm / llama"
            @input="onKeywordInput"
          />
        </div>

        <div class="min-w-0">
          <p class="label">按分组筛选</p>
          <div class="flex flex-wrap gap-1.5">
            <button
              type="button"
              class="btn btn-sm"
              :class="activeGroup === '' ? 'btn-primary' : 'btn-secondary'"
              @click="selectGroup('')"
            >
              全部
            </button>
            <button
              v-for="group in groups"
              :key="group.name"
              type="button"
              class="btn btn-sm"
              :class="activeGroup === group.name ? 'btn-primary' : 'btn-secondary'"
              :title="group.description || group.name"
              @click="selectGroup(group.name)"
            >
              {{ group.label }}
              <span class="text-[11px] opacity-70">{{ group.model_count }}</span>
            </button>
          </div>
        </div>
      </div>

      <!-- ── 卡片网格 ───────────────────────────────────── -->
      <DataState
        :loading="loading"
        :error="error"
        :empty="!loading && !error && items.length === 0"
        loading-text="正在读取模型清单…"
        empty-text="没有匹配的模型"
        empty-hint="换个关键词，或清空分组筛选后重试。若刚部署完成，请先在后台配置渠道并声明模型。"
        @retry="load"
      />

      <div v-if="!loading && !error && items.length" class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
        <ModelCard
          v-for="item in items"
          :key="item.model"
          :model="item"
          :group-labels="groupLabels"
          @copy="handleCopy"
        />
      </div>

      <!-- ── 接入提示 ───────────────────────────────────── -->
      <section v-if="!loading && !error && items.length" class="mt-10 card card-pad">
        <h2 class="section-title flex items-center gap-2">
          <AppIcon name="bolt" :size="16" class="text-brand-700" />
          如何调用
        </h2>
        <ol class="mt-3 space-y-2 text-sm leading-relaxed text-ink-300">
          <li>
            1. 登录后在「访问令牌」页创建一个令牌（令牌明文只展示一次，请立即保存）。
          </li>
          <li>
            2. 把 <code class="chip">{{ baseUrl }}</code> 作为 base_url，
            模型名填卡片上的名称。
          </li>
          <li>
            3. 需要图像/视频等生成类能力时，使用
            <code class="chip">POST /v1/tasks</code> 提交任务，再用
            <code class="chip">GET /v1/tasks/{任务号}</code> 查询结果。
          </li>
        </ol>
        <div class="mt-4 flex flex-wrap gap-2">
          <RouterLink to="/" class="btn btn-secondary btn-sm">
            <AppIcon name="home" :size="14" />
            查看接入示例
          </RouterLink>
          <RouterLink v-if="auth.isLoggedIn" :to="consoleTarget" class="btn btn-primary btn-sm">
            <AppIcon name="key" :size="14" />
            去创建令牌
          </RouterLink>
          <RouterLink v-else to="/register" class="btn btn-primary btn-sm">
            免费注册
          </RouterLink>
        </div>
      </section>
    </main>

    <footer class="border-t border-ink-800/70 py-8">
      <div class="mx-auto flex max-w-6xl flex-wrap items-center justify-between gap-3 px-5 text-xs text-ink-500 lg:px-8">
        <p>{{ site.siteName }} · 模型广场</p>
        <p v-if="site.version">版本 {{ site.version }}</p>
      </div>
    </footer>
  </div>
</template>
