<script setup lang="ts">
/**
 * 用户门户 · 接入示例：把"怎么调"集中在一页，且留在控制台外壳内。
 *
 * 意图（Why）：
 *   原来门户侧栏的「接入示例」指向落地页的锚点（/#quickstart），
 *   点一下就被甩出控制台、落到营销页上，用户还得自己滚回去找。
 *   接入文档属于"用起来之后"的需求，理应是控制台的一部分，
 *   所以它在控制台里就地展开，外壳、侧边栏、身份都不变。
 *
 *   覆盖三种协议（OpenAI 兼容 / Anthropic / Gemini）是因为网关本身在这三种
 *   协议之间做转换，用户带着自己的客户端来，第一件事就是确认"我这种协议怎么填"。
 *
 * 流转（Flow）：
 *   进入页面 → 读 auth.user（展示当前身份）+ 拼接 baseUrl → 切换协议页签 → 复制示例
 *
 * 扩展（Extend）：
 *   新增协议支持时，在 TABS 里加一项并在模板追加一个示例块；
 *   新增端点时同步补充接口一览表。
 */
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import CopyButton from '@/components/CopyButton.vue'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()

/** 协议页签：与网关实际支持的三条入口一一对应 */
const TABS = [
  { key: 'openai', label: 'OpenAI 兼容' },
  { key: 'anthropic', label: 'Anthropic' },
  { key: 'gemini', label: 'Gemini' },
] as const

type TabKey = (typeof TABS)[number]['key']

const activeTab = ref<TabKey>('openai')

/** 对外 base_url：用浏览器地址而非硬编码域名，任何部署环境复制出来都能直接用 */
const origin = computed(() => window.location.origin)
const baseUrl = computed(() => `${origin.value}/v1`)

/** 示例里统一用占位符，避免把真实密钥写进可复制的文本 */
const KEY_PLACEHOLDER = 'sk-你的访问令牌'

const SAMPLES: Record<TabKey, { hint: string; code: string }> = {
  openai: {
    hint: '把 base_url 指向本站即可，任何 OpenAI 兼容客户端（SDK、第三方前端）都能直接使用。',
    code: `curl ${baseUrl.value}/chat/completions \\
  -H "Authorization: Bearer ${KEY_PLACEHOLDER}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "z-ai/glm-5.3",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'`,
  },
  anthropic: {
    hint: '网关同时接受 Anthropic Messages 协议的请求体，便于直接接入 Claude 生态的客户端。',
    code: `curl ${baseUrl.value}/messages \\
  -H "x-api-key: ${KEY_PLACEHOLDER}" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "z-ai/glm-5.3",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "你好"}]
  }'`,
  },
  gemini: {
    hint: 'Gemini 原生协议走 /v1beta 前缀，请求体沿用 Google 的 contents 结构。',
    code: `curl ${origin.value}/v1beta/models/z-ai/glm-5.3:generateContent \\
  -H "x-goog-api-key: ${KEY_PLACEHOLDER}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "contents": [{"parts": [{"text": "你好"}]}]
  }'`,
  },
}

/** Python SDK 示例：最贴近国内开发者的接入方式 */
const pythonSample = computed(() => `from openai import OpenAI

client = OpenAI(
    api_key="${KEY_PLACEHOLDER}",
    base_url="${baseUrl.value}",
)

resp = client.chat.completions.create(
    model="z-ai/glm-5.3",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)`)

/** 端点一览：与后端路由表逐条对齐，避免写了不存在的路径 */
const ENDPOINTS = [
  { method: 'GET', path: '/v1/models', desc: '列出当前可用的模型' },
  { method: 'POST', path: '/v1/chat/completions', desc: '对话补全（支持流式）' },
  { method: 'POST', path: '/v1/embeddings', desc: '文本嵌入（嵌入类模型必须用这个端点）' },
  { method: 'POST', path: '/v1/messages', desc: 'Anthropic Messages 协议' },
  { method: 'POST', path: '/v1/tasks', desc: '提交异步生成任务（图像 / 视频等）' },
  { method: 'GET', path: '/v1/tasks/{ref}', desc: '查询异步任务结果' },
]

/** HTTP 方法 → 徽标配色（让表格能一眼区分读操作与写操作） */
function methodClass(method: string): string {
  return method === 'GET' ? 'badge-info' : 'badge-ok'
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div class="max-w-2xl">
        <h2 class="page-title">接入示例</h2>
        <p class="page-desc">
          本站与 OpenAI 接口兼容，同时接受 Anthropic 与 Gemini 协议。
          把 <code class="chip">{{ baseUrl }}</code> 填成客户端的 base_url，再用访问令牌鉴权即可。
        </p>
      </div>
      <div class="toolbar">
        <RouterLink to="/console/tokens" class="btn btn-primary btn-sm">
          <AppIcon name="key" :size="15" />
          创建访问令牌
        </RouterLink>
      </div>
    </div>

    <!-- 关键参数：两行搞定，避免用户到处找 -->
    <section class="grid gap-4 lg:grid-cols-2">
      <div class="card card-pad">
        <p class="label !mb-2">接入地址（base_url）</p>
        <div class="flex items-center gap-2">
          <code class="chip min-w-0 flex-1 truncate !py-1.5">{{ baseUrl }}</code>
          <CopyButton :value="baseUrl" label="复制" small outline />
        </div>
        <p class="hint">所有模型共用同一个地址，路径各不相同。</p>
      </div>

      <div class="card card-pad">
        <p class="label !mb-2">鉴权方式</p>
        <div class="flex items-center gap-2">
          <code class="chip min-w-0 flex-1 truncate !py-1.5">Authorization: Bearer {{ KEY_PLACEHOLDER }}</code>
        </div>
        <p class="hint">
          当前登录身份：{{ auth.displayName }}。令牌明文只在创建时展示一次，请及时保存。
        </p>
      </div>
    </section>

    <!-- 协议示例 -->
    <section class="mt-6 card">
      <div class="card-head">
        <div class="seg" role="group" aria-label="协议切换">
          <button
            v-for="tab in TABS"
            :key="tab.key"
            type="button"
            class="seg-item"
            :class="activeTab === tab.key ? 'seg-item-active' : ''"
            @click="activeTab = tab.key"
          >
            {{ tab.label }}
          </button>
        </div>
        <CopyButton :value="SAMPLES[activeTab].code" label="复制示例" success-text="示例已复制" small outline />
      </div>

      <div class="card-pad">
        <p class="mb-3 text-xs leading-relaxed text-ink-400">{{ SAMPLES[activeTab].hint }}</p>
        <div class="code-block">
          <pre>{{ SAMPLES[activeTab].code }}</pre>
        </div>
      </div>
    </section>

    <!-- Python SDK -->
    <section class="mt-6 card">
      <div class="card-head">
        <div>
          <h3 class="section-title">Python SDK</h3>
          <p class="mt-1 text-xs text-ink-400">直接复用 openai 官方 SDK，只改 base_url 与 api_key。</p>
        </div>
        <CopyButton :value="pythonSample" label="复制示例" success-text="示例已复制" small outline />
      </div>
      <div class="card-pad">
        <div class="code-block">
          <pre>{{ pythonSample }}</pre>
        </div>
      </div>
    </section>

    <!-- 端点一览 -->
    <section class="mt-6">
      <div class="mb-3 flex items-center gap-2">
        <AppIcon name="list" :size="16" class="text-brand-700" />
        <h3 class="section-title">接口一览</h3>
      </div>
      <div class="table-wrap">
        <table class="data-table min-w-[560px]">
          <thead>
            <tr>
              <th>方法</th>
              <th>路径</th>
              <th>说明</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in ENDPOINTS" :key="item.method + item.path">
              <td>
                <span class="badge" :class="methodClass(item.method)">{{ item.method }}</span>
              </td>
              <td class="font-mono text-[13px] text-ink-100">{{ item.path }}</td>
              <td class="cell-muted">{{ item.desc }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 注意事项：把最常踩的坑前置说明 -->
    <section class="mt-6 card card-pad">
      <h3 class="section-title flex items-center gap-2">
        <AppIcon name="info" :size="16" class="text-brand-700" />
        注意事项
      </h3>
      <ul class="mt-3 space-y-2 text-sm leading-relaxed text-ink-300">
        <li>
          · 上游排队与首字节延迟本就较大，网关最长等待 <strong>300 秒</strong>；
          客户端超时应设置得比它更宽松，否则会先于网关放弃。
        </li>
        <li>
          · 嵌入类模型（名称含 embed / rerank / clip）不提供对话端点，
          必须调用 <code class="chip">/v1/embeddings</code>。
        </li>
        <li>
          · 调用未接入渠道的模型会立即返回 <code class="chip">503</code>（而不是长时间等待），
          可在「模型广场」确认模型当前是否可用。
        </li>
        <li>
          · 上游返回的真实错误会被原样透传（含 HTTP 状态码与 error.message），
          便于定位是账号无权访问还是上游故障。
        </li>
      </ul>
    </section>
  </div>
</template>
