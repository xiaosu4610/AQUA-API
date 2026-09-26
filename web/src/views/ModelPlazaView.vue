<script setup lang="ts">
/**
 * 模型广场（公开页，无需登录）。
 *
 * 意图（Why）：
 *   落地页只列了模型名，无法回答"这个模型多少钱、在哪个分组、现在有没有渠道"。
 *   本页承担这件事，并且是"这个网关能做什么"的唯一权威清单。
 *   筛选、排序、视图、详情弹窗全部复用 components/ModelPlazaBoard.vue，
 *   与登录后的控制台版本保持完全一致的交互 —— 用户先看再登录，不需要重新学一遍。
 *
 * 流转（Flow）：
 *   进入页面 → site.load()（站点名/版本）+ ModelPlazaBoard 自行拉取模型清单
 *   → 访客可直接浏览与复制模型名；调用则需要登录后创建访问令牌
 *
 * 扩展（Extend）：
 *   广场的交互改动请改 ModelPlazaBoard，不要在本页实现第二套筛选逻辑。
 */
import { computed, onMounted } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import ModelPlazaBoard from '@/components/ModelPlazaBoard.vue'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const site = useSiteStore()
const auth = useAuthStore()

const consoleTarget = computed(() => (auth.isAdmin ? { name: 'admin-dashboard' } : { name: 'console-overview' }))

/** 对外 base_url：用浏览器地址而非硬编码域名 */
const baseUrl = computed(() => `${window.location.origin}/v1`)

onMounted(() => {
  void site.load()
})
</script>

<template>
  <div class="app-ambient min-h-screen">
    <!-- ── 页头 ─────────────────────────────────────────── -->
    <header class="sticky top-0 z-20 border-b border-ink-800/70 bg-white/90">
      <div class="mx-auto flex max-w-[1400px] flex-wrap items-center gap-3 px-5 py-3 lg:px-8">
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

    <main class="mx-auto max-w-[1400px] px-5 py-8 lg:px-8">
      <!-- ── 标题区 ─────────────────────────────────────── -->
      <div class="page-head">
        <div class="max-w-2xl">
          <h1 class="page-title text-2xl">
            模型<span class="text-gradient-brand">广场</span>
          </h1>
          <p class="page-desc">
            本站当前对外提供的全部模型、分组与价格。调用方式与 OpenAI 接口一致，
            只需把 <code class="chip">base_url</code> 指向 <code class="chip">{{ baseUrl }}</code>
            并用访问令牌鉴权。
          </p>
        </div>
        <div class="toolbar">
          <RouterLink v-if="auth.isLoggedIn" :to="consoleTarget" class="btn btn-primary btn-sm">
            <AppIcon name="key" :size="15" />
            去创建令牌
          </RouterLink>
          <RouterLink v-else to="/register" class="btn btn-primary btn-sm">
            <AppIcon name="bolt" :size="15" />
            免费注册
          </RouterLink>
        </div>
      </div>

      <!-- ── 广场主体 ───────────────────────────────────── -->
      <ModelPlazaBoard />

      <!-- ── 接入提示 ───────────────────────────────────── -->
      <section class="mt-10 card card-pad">
        <h2 class="section-title flex items-center gap-2">
          <AppIcon name="bolt" :size="16" class="text-brand-700" />
          如何调用
        </h2>
        <ol class="mt-3 space-y-2 text-sm leading-relaxed text-ink-300">
          <li>1. 登录后在「访问令牌」页创建一个令牌（明文只展示一次，请立即保存）。</li>
          <li>
            2. 把 <code class="chip">{{ baseUrl }}</code> 作为 base_url，模型名填广场里的名称；
            嵌入类模型请用 <code class="chip">/v1/embeddings</code> 端点。
          </li>
          <li>
            3. 需要图像/视频等生成类能力时，使用 <code class="chip">POST /v1/tasks</code> 提交任务，
            再用 <code class="chip">GET /v1/tasks/{任务号}</code> 查询结果。
          </li>
        </ol>
        <p class="mt-3 text-xs text-ink-500">
          提示：点击任意模型卡片可以就地查看分组价格与可直接粘贴运行的调用命令。
        </p>
      </section>
    </main>

    <footer class="border-t border-ink-800/70 py-8">
      <div class="mx-auto flex max-w-[1400px] flex-wrap items-center justify-between gap-3 px-5 text-xs text-ink-500 lg:px-8">
        <p>{{ site.siteName }} · 模型广场</p>
        <p v-if="site.version">版本 {{ site.version }}</p>
      </div>
    </footer>
  </div>
</template>
