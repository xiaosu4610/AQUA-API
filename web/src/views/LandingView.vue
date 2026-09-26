<script setup lang="ts">
/**
 * 落地页：站点定位 + 核心特性 + 可用模型 + 快速接入（无需登录）。
 *
 * 意图（Why）：
 *   这是对外展示的「门面」：访客在 10 秒内要能回答三个问题——
 *   「这是什么」「现在能用哪些模型」「我该怎么接」。
 *   因此结构固定为：Hero（定位 + 可复制的接入片段）→ 特性 → 模型列表 → 接入示例 → 页脚。
 *
 * 流转（Flow）：
 *   main.ts 预取站点信息 → 本页读取 stores/site（名称/描述/版本/模型列表）
 *   → 失败时显示可重试的提示条，不阻断页面其余内容
 *
 * 扩展（Extend）：
 *   新增展示区块：在 <main> 内按「先价值后细节」的顺序插入；
 *   新增接入语言示例：在 CODE_SAMPLES 追加一项（会自动多出一个 Tab）。
 *   注意：本页不使用任何占位图片，视觉全部由 CSS 渐变/网格与内联 SVG 构成。
 */
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import CopyButton from '@/components/CopyButton.vue'
import { type IconName } from '@/components/icons'
import { useAuthStore } from '@/stores/auth'
import { useSiteStore } from '@/stores/site'

const site = useSiteStore()
const auth = useAuthStore()

/** 站点根地址：用浏览器地址而非硬编码域名，任何部署环境复制出来都能直接用 */
const baseUrl = computed(() => window.location.origin)
/** 示例里优先用站点真实开放的模型，空则给一个通用占位（避免出现 undefined） */
const sampleModel = computed(() => site.models[0] || 'gpt-4o')

/** 特性列表：内容与后端能力一一对应，避免出现「宣传了但没有」的功能 */
interface Feature {
  icon: IconName
  title: string
  desc: string
}

const features: Feature[] = [
  {
    icon: 'server',
    title: '统一上游入口',
    desc: '把 API Key、订阅账号与自托管模型收敛成一个入口，客户端只需要认一套地址与协议。',
  },
  {
    icon: 'layers',
    title: 'OpenAI 协议兼容',
    desc: '对外提供 /v1/chat/completions 兼容接口，既有的 SDK、IDE 插件与脚本无需改造。',
  },
  {
    icon: 'globe',
    title: '渠道调度与容错',
    desc: '按分组、优先级与权重分配请求；渠道异常会被自动标记，降低故障对业务的影响。',
  },
  {
    icon: 'key',
    title: '令牌即权限边界',
    desc: '按人、按用途签发访问令牌，可限定可用模型、有效期与额度，随时停用或删除。',
  },
  {
    icon: 'quota',
    title: '用量可对账',
    desc: '每次调用都记录 Token 与额度消耗，后台可按天、按模型、按令牌逐条核查。',
  },
  {
    icon: 'lock',
    title: '自托管更可控',
    desc: '单二进制 + 本地数据库部署，密钥只走环境变量，请求与账目数据不出自己的机器。',
  },
]

/** 接入示例：三种最常见的方式，Tab 切换 */
interface CodeSample {
  key: string
  label: string
  code: string
}

const codeSamples = computed<CodeSample[]>(() => [
  {
    key: 'curl',
    label: 'cURL',
    code: `curl ${baseUrl.value}/v1/chat/completions \\
  -H "Authorization: Bearer $AQUA_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${sampleModel.value}",
    "messages": [{"role": "user", "content": "你好，介绍一下你自己"}],
    "stream": false
  }'`,
  },
  {
    key: 'python',
    label: 'Python SDK',
    code: `from openai import OpenAI

client = OpenAI(
    api_key="sk-...",              # 在「访问令牌」页创建
    base_url="${baseUrl.value}/v1",
)

resp = client.chat.completions.create(
    model="${sampleModel.value}",
    messages=[{"role": "user", "content": "你好，介绍一下你自己"}],
)
print(resp.choices[0].message.content)`,
  },
  {
    key: 'node',
    label: 'Node / fetch',
    code: `const res = await fetch("${baseUrl.value}/v1/chat/completions", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    Authorization: \`Bearer \${process.env.AQUA_API_KEY}\`,
  },
  body: JSON.stringify({
    model: "${sampleModel.value}",
    messages: [{ role: "user", content: "你好，介绍一下你自己" }],
  }),
})

const data = await res.json()
console.log(data.choices[0].message.content)`,
  },
])

/** 当前选中的示例 Tab（默认 cURL：最少前置条件） */
const activeSample = ref('curl')
const currentCode = computed(() => codeSamples.value.find((item) => item.key === activeSample.value)?.code || '')

/** 登录后按钮的目标：管理员去后台，普通用户去门户 */
const consoleTarget = computed(() => (auth.isAdmin ? '/admin' : '/console'))
</script>

<template>
  <div class="min-h-screen bg-ink-950">
    <!-- ── 顶部导航 ───────────────────────────────────────── -->
    <header class="sticky top-0 z-30 border-b border-ink-800/80 bg-white/90">
      <div class="mx-auto flex max-w-6xl items-center gap-4 px-5 py-3.5 lg:px-8">
        <RouterLink to="/" class="flex items-center gap-2.5">
          <img src="/favicon.ico" alt="" class="h-8 w-8 rounded-lg" />
          <span class="text-sm font-semibold tracking-tight text-ink-50">{{ site.siteName }}</span>
        </RouterLink>

        <nav class="ml-4 hidden items-center gap-1 md:flex">
          <a href="#features" class="btn btn-ghost btn-sm">核心特性</a>
          <a href="#models" class="btn btn-ghost btn-sm">可用模型</a>
          <RouterLink to="/models" class="btn btn-ghost btn-sm">模型广场</RouterLink>
          <a href="#quickstart" class="btn btn-ghost btn-sm">快速接入</a>
        </nav>

        <div class="ml-auto flex items-center gap-2">
          <template v-if="auth.isLoggedIn">
            <RouterLink :to="consoleTarget" class="btn btn-primary btn-sm">
              <AppIcon :name="auth.isAdmin ? 'shield' : 'home'" :size="15" />
              {{ auth.isAdmin ? '管理后台' : '用户门户' }}
            </RouterLink>
          </template>
          <template v-else>
            <RouterLink to="/login" class="btn btn-ghost btn-sm">登录</RouterLink>
            <RouterLink v-if="site.registrationEnabled" to="/register" class="btn btn-primary btn-sm">
              免费注册
            </RouterLink>
          </template>
        </div>
      </div>
    </header>

    <main>
      <!-- ── Hero ─────────────────────────────────────────── -->
      <section class="relative overflow-hidden border-b border-ink-800/70">
        <!-- 背景：网格 + 两处低饱和光斑（纯 CSS，无图片资源） -->
        <div class="pointer-events-none absolute inset-0 bg-grid opacity-60" aria-hidden="true" />
        <div
          class="pointer-events-none absolute -left-24 -top-24 h-72 w-72 rounded-full bg-brand-500/20 blur-[90px]"
          aria-hidden="true"
        />
        <div
          class="pointer-events-none absolute -right-16 top-16 h-80 w-80 rounded-full bg-indigo-500/10 blur-[110px]"
          aria-hidden="true"
        />
        <div class="pointer-events-none absolute inset-x-0 bottom-0 h-32 bg-gradient-to-t from-ink-950 to-transparent" aria-hidden="true" />

        <div class="relative mx-auto grid max-w-6xl gap-12 px-5 py-16 lg:grid-cols-[1.05fr_1fr] lg:px-8 lg:py-24">
          <div>
            <div class="flex flex-wrap items-center gap-2">
              <span class="badge badge-info">
                <span class="dot" />
                自托管网关
              </span>
              <span v-if="site.version" class="chip">v{{ site.version }}</span>
              <span v-if="site.models.length" class="chip">{{ site.models.length }} 个模型在线</span>
            </div>

            <h1 class="mt-6 text-balance text-4xl font-semibold leading-tight tracking-tight text-ink-50 lg:text-5xl">
              一个入口，接管你所有的
              <!-- 渐变文字：亮色主题下必须用较深的品牌色，否则在浅底上几乎不可见 -->
              <span class="bg-gradient-to-r from-brand-600 to-brand-800 bg-clip-text text-transparent">大模型调用</span>
            </h1>

            <p class="mt-5 max-w-xl text-base leading-relaxed text-ink-300">
              {{ site.siteDescription || 'AQUA-API 是自托管的 LLM API 网关：统一上游渠道与下游协议，负责路由、计费与运营。' }}
            </p>

            <ul class="mt-6 flex flex-wrap gap-x-5 gap-y-2 text-sm text-ink-300">
              <li class="flex items-center gap-1.5">
                <AppIcon name="check" :size="15" class="text-brand-600" />
                兼容 OpenAI 接口
              </li>
              <li class="flex items-center gap-1.5">
                <AppIcon name="check" :size="15" class="text-brand-600" />
                令牌级额度控制
              </li>
              <li class="flex items-center gap-1.5">
                <AppIcon name="check" :size="15" class="text-brand-600" />
                逐次调用留痕
              </li>
            </ul>

            <div class="mt-8 flex flex-wrap items-center gap-3">
              <template v-if="auth.isLoggedIn">
                <RouterLink :to="consoleTarget" class="btn btn-primary">
                  <AppIcon :name="auth.isAdmin ? 'shield' : 'home'" :size="16" />
                  进入{{ auth.isAdmin ? '管理后台' : '用户门户' }}
                </RouterLink>
              </template>
              <template v-else>
                <RouterLink to="/login" class="btn btn-primary">
                  登录控制台
                  <AppIcon name="chevron-right" :size="16" />
                </RouterLink>
                <RouterLink v-if="site.registrationEnabled" to="/register" class="btn btn-secondary">
                  创建账号
                </RouterLink>
              </template>
              <a href="#quickstart" class="btn btn-ghost">
                <AppIcon name="bolt" :size="16" />
                查看接入示例
              </a>
            </div>
          </div>

          <!-- 请求示例卡片：让访客立刻看到「怎么用」 -->
          <div class="lg:pt-4">
            <div class="code-block shadow-panel">
              <div class="flex items-center justify-between gap-3 border-b border-ink-800 px-4 py-2.5">
                <div class="flex items-center gap-2">
                  <span class="flex gap-1.5" aria-hidden="true">
                    <span class="h-2.5 w-2.5 rounded-full bg-ink-700" />
                    <span class="h-2.5 w-2.5 rounded-full bg-ink-700" />
                    <span class="h-2.5 w-2.5 rounded-full bg-ink-700" />
                  </span>
                  <span class="font-mono text-xs text-ink-400">POST /v1/chat/completions</span>
                </div>
                <CopyButton :value="codeSamples[0].code" label="复制" small success-text="cURL 示例已复制" />
              </div>
              <pre>{{ codeSamples[0].code }}</pre>
            </div>

            <div class="mt-4 flex items-center gap-3 rounded-xl border border-ink-800 bg-ink-900/50 px-4 py-3">
              <AppIcon name="lock" :size="16" class="text-brand-700" />
              <p class="text-xs leading-relaxed text-ink-400">
                访问令牌仅创建时明文展示一次；服务端只保存摘要，可随时吊销。
              </p>
            </div>
          </div>
        </div>
      </section>

      <!-- ── 站点信息异常时的提示（不阻断页面）───────────────── -->
      <div v-if="site.error" class="mx-auto max-w-6xl px-5 pt-6 lg:px-8">
        <div class="flex flex-wrap items-center gap-3 rounded-xl border border-amber-500/25 bg-amber-500/10 px-4 py-3">
          <AppIcon name="alert" :size="16" class="text-amber-700" />
          <p class="flex-1 text-sm text-amber-800">站点信息加载失败：{{ site.error }}</p>
          <button type="button" class="btn btn-secondary btn-sm" @click="site.load(true)">
            <AppIcon name="refresh" :size="14" />
            重试
          </button>
        </div>
      </div>

      <!-- ── 核心特性 ─────────────────────────────────────── -->
      <section id="features" class="mx-auto max-w-6xl scroll-mt-20 px-5 py-16 lg:px-8 lg:py-20">
        <div class="max-w-2xl">
          <p class="text-xs font-medium uppercase tracking-widest text-brand-600">核心特性</p>
          <h2 class="mt-3 text-2xl font-semibold tracking-tight text-ink-50 lg:text-3xl">
            该管的都管住，该接的照旧接
          </h2>
          <p class="mt-3 text-sm leading-relaxed text-ink-300">
            把「账号、密钥、配额、账单」这类运维问题收敛到网关内部，
            业务侧继续用熟悉的 OpenAI 协议调用。
          </p>
        </div>

        <div class="mt-10 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <div
            v-for="feature in features"
            :key="feature.title"
            class="card card-pad transition-colors duration-200 hover:border-ink-700"
          >
            <span class="flex h-10 w-10 items-center justify-center rounded-xl bg-brand-500/10 text-brand-700 ring-1 ring-inset ring-brand-500/20">
              <AppIcon :name="feature.icon" :size="19" />
            </span>
            <h3 class="mt-4 text-sm font-semibold text-ink-50">{{ feature.title }}</h3>
            <p class="mt-2 text-sm leading-relaxed text-ink-400">{{ feature.desc }}</p>
          </div>
        </div>
      </section>

      <!-- ── 可用模型 ─────────────────────────────────────── -->
      <section id="models" class="scroll-mt-20 border-y border-ink-800/70 bg-ink-900/30">
        <div class="mx-auto max-w-6xl px-5 py-16 lg:px-8 lg:py-20">
          <div class="flex flex-wrap items-end justify-between gap-4">
            <div>
              <p class="text-xs font-medium uppercase tracking-widest text-brand-600">可用模型</p>
              <h2 class="mt-3 text-2xl font-semibold tracking-tight text-ink-50">当前对外提供的模型</h2>
              <p class="mt-2 text-sm text-ink-400">
                来自所有已启用渠道声明模型的并集，随渠道配置变化实时更新。
              </p>
            </div>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="site.loading" @click="site.load(true)">
              <AppIcon name="refresh" :size="14" />
              刷新列表
            </button>
          </div>

          <!-- 加载态：骨架块，避免布局跳动 -->
          <div v-if="site.loading && !site.models.length" class="mt-8 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <div v-for="index in 8" :key="index" class="h-12 rounded-xl border border-ink-800 bg-ink-900/40">
              <div class="m-3 h-5 w-2/3 skeleton" />
            </div>
          </div>

          <!-- 空态：说明「为什么为空」并给出下一步 -->
          <div
            v-else-if="!site.models.length"
            class="mt-8 flex flex-col items-center justify-center rounded-xl border border-dashed border-ink-700 bg-ink-900/40 px-6 py-12 text-center"
          >
            <span class="flex h-10 w-10 items-center justify-center rounded-xl bg-ink-850 text-ink-400 ring-1 ring-inset ring-ink-700">
              <AppIcon name="layers" :size="20" />
            </span>
            <p class="mt-3 text-sm font-medium text-ink-200">暂无可用模型</p>
            <p class="mt-1 max-w-md text-xs leading-relaxed text-ink-400">
              通常是还没有添加启用状态的渠道。管理员在「渠道管理」中添加渠道后，此处会自动展示可用模型。
            </p>
          </div>

          <!-- 模型列表：首页最多展示 24 个。
               上游（如 NIM）动辄上百个模型，全部铺开会把首页拉得极长，
               反而看不清别的信息；这里给出"还有 N 个"的提示，需要完整清单时去后台看。 -->
          <div v-else>
            <div class="mt-8 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <div
                v-for="model in site.models.slice(0, 24)"
                :key="model"
                class="flex items-center gap-2.5 rounded-xl border border-ink-800 bg-ink-900/50 px-3.5 py-3 transition-colors hover:border-brand-500/30"
              >
                <AppIcon name="layers" :size="16" class="text-brand-600/80" />
                <span class="truncate font-mono text-[13px] text-ink-100" :title="model">{{ model }}</span>
              </div>
            </div>
            <p v-if="site.models.length > 24" class="mt-3 text-sm text-ink-400">
              另有 {{ site.models.length - 24 }} 个模型同样可用（共 {{ site.models.length }} 个）。
            </p>
          </div>
        </div>
      </section>

      <!-- ── 快速接入 ─────────────────────────────────────── -->
      <section id="quickstart" class="mx-auto max-w-6xl scroll-mt-20 px-5 py-16 lg:px-8 lg:py-20">
        <div class="grid gap-10 lg:grid-cols-[0.9fr_1.1fr]">
          <div>
            <p class="text-xs font-medium uppercase tracking-widest text-brand-600">快速接入</p>
            <h2 class="mt-3 text-2xl font-semibold tracking-tight text-ink-50 lg:text-3xl">三步开始调用</h2>

            <ol class="mt-8 space-y-6">
              <li class="flex gap-4">
                <span class="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-brand-500/10 font-mono text-xs text-brand-700 ring-1 ring-inset ring-brand-500/20">
                  1
                </span>
                <div>
                  <p class="text-sm font-medium text-ink-100">登录并创建访问令牌</p>
                  <p class="mt-1 text-sm leading-relaxed text-ink-400">
                    在「访问令牌」页创建 <code class="chip">sk-</code> 开头的密钥，
                    可按用途分别签发，并限定模型或有效期。
                  </p>
                </div>
              </li>
              <li class="flex gap-4">
                <span class="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-brand-500/10 font-mono text-xs text-brand-700 ring-1 ring-inset ring-brand-500/20">
                  2
                </span>
                <div>
                  <p class="text-sm font-medium text-ink-100">把基地址指向本网关</p>
                  <p class="mt-1 text-sm leading-relaxed text-ink-400">
                    基地址使用 <code class="chip">{{ baseUrl }}/v1</code>，
                    请求头携带 <code class="chip">Authorization: Bearer sk-…</code>。
                  </p>
                </div>
              </li>
              <li class="flex gap-4">
                <span class="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-brand-500/10 font-mono text-xs text-brand-700 ring-1 ring-inset ring-brand-500/20">
                  3
                </span>
                <div>
                  <p class="text-sm font-medium text-ink-100">按模型名发起请求</p>
                  <p class="mt-1 text-sm leading-relaxed text-ink-400">
                    使用上方模型列表中的名称（如 <code class="chip">{{ sampleModel }}</code>），
                    调用记录与用量会实时出现在后台。
                  </p>
                </div>
              </li>
            </ol>

            <div class="mt-8 flex flex-wrap gap-3">
              <RouterLink v-if="!auth.isLoggedIn" to="/login" class="btn btn-primary">
                登录后创建令牌
                <AppIcon name="chevron-right" :size="16" />
              </RouterLink>
              <RouterLink v-else to="/console/tokens" class="btn btn-primary">
                <AppIcon name="key" :size="16" />
                我的访问令牌
              </RouterLink>
            </div>
          </div>

          <div>
            <div class="code-block shadow-panel">
              <div class="flex flex-wrap items-center gap-2 border-b border-ink-800 px-3 py-2.5">
                <button
                  v-for="sample in codeSamples"
                  :key="sample.key"
                  type="button"
                  class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors"
                  :class="
                    activeSample === sample.key
                      ? 'bg-brand-500/15 text-brand-700'
                      : 'text-ink-400 hover:bg-ink-850 hover:text-ink-200'
                  "
                  @click="activeSample = sample.key"
                >
                  {{ sample.label }}
                </button>
                <CopyButton :value="currentCode" label="复制" small class="ml-auto" success-text="示例已复制" />
              </div>
              <pre>{{ currentCode }}</pre>
            </div>
          </div>
        </div>
      </section>

      <!-- ── 结尾行动区 ───────────────────────────────────── -->
      <section class="border-t border-ink-800/70 bg-ink-900/30">
        <div class="mx-auto flex max-w-6xl flex-col items-start gap-6 px-5 py-14 lg:flex-row lg:items-center lg:justify-between lg:px-8">
          <div>
            <h2 class="text-xl font-semibold tracking-tight text-ink-50">准备好把模型调用收拢到一个入口了吗？</h2>
            <p class="mt-2 text-sm text-ink-400">
              {{ site.registrationEnabled ? '注册即可获得账号，登录后创建你的第一个访问令牌。' : '当前站点未开放自助注册，请联系管理员开通账号。' }}
            </p>
          </div>
          <div class="flex flex-wrap gap-3">
            <RouterLink v-if="site.registrationEnabled && !auth.isLoggedIn" to="/register" class="btn btn-primary">
              立即注册
            </RouterLink>
            <RouterLink v-else-if="!auth.isLoggedIn" to="/login" class="btn btn-primary">登录控制台</RouterLink>
            <RouterLink v-else :to="consoleTarget" class="btn btn-primary">进入控制台</RouterLink>
            <a href="#quickstart" class="btn btn-secondary">再看一遍示例</a>
          </div>
        </div>
      </section>
    </main>

    <!-- ── 页脚 ─────────────────────────────────────────── -->
    <footer class="border-t border-ink-800/70">
      <div class="mx-auto flex max-w-6xl flex-col gap-4 px-5 py-8 lg:flex-row lg:items-center lg:justify-between lg:px-8">
        <div class="flex items-center gap-2.5">
          <img src="/favicon.ico" alt="" class="h-6 w-6 rounded" />
          <span class="text-xs text-ink-400">
            {{ site.siteName }} <span v-if="site.version">· v{{ site.version }}</span> · 自托管 LLM API 网关
          </span>
        </div>
        <nav class="flex flex-wrap items-center gap-4 text-xs text-ink-400">
          <RouterLink to="/login" class="transition-colors hover:text-ink-200">登录</RouterLink>
          <RouterLink v-if="site.registrationEnabled" to="/register" class="transition-colors hover:text-ink-200">
            注册
          </RouterLink>
          <a href="#features" class="transition-colors hover:text-ink-200">核心特性</a>
          <a href="#models" class="transition-colors hover:text-ink-200">可用模型</a>
        </nav>
      </div>
    </footer>
  </div>
</template>
