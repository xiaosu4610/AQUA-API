<script setup lang="ts">
/**
 * 用户门户 · 游乐场（在线试聊）。
 *
 * 意图（Why）：
 *   用户拿到访问令牌后，第一件事往往是"它到底通不通、上游答不答得上来"。
 *   把这一步放进浏览器，可以省掉"先去装 SDK、写脚本、再看报错"的往返；
 *   出现错误时直接展示上游透传的真实错误，比一句"调用失败"有用得多。
 *
 * 两个刻意的设计约束：
 *   1) 令牌明文拿不回来——后端只给掩码，明文仅在创建时展示一次。
 *      所以这里让用户从自己的令牌列表里【对照】选一把（确认用的是哪把），
 *      再【手动粘贴】一次明文。明文只存在内存（ref），绝不写 localStorage：
 *      凭据一旦落盘，就成了"清理浏览器数据也带不走、xss 一读就走"的风险敞口。
 *   2) 不走 api/client.ts：它带的是门户会话令牌，而这里必须带用户自己的【访问令牌】，
 *      且要手工解析 SSE 流。因此用原生 fetch + ReadableStream，不引第三方库。
 *
 * 流转（Flow）：
 *   进入页面 → listMyTokens()（对照用）→ 用户粘贴令牌 → 加载模型（GET /v1/models）
 *   → 发送 → POST /v1/chat/completions（stream 时手工解析 SSE 逐字追加）
 *   → 出错则原样展示上游错误 → 「复制 cURL」把当前参数拼成可运行命令
 *
 * 扩展（Extend）：
 *   新增参数（top_p / stop 等）时：在参数卡片加控件 → 在 buildRequestBody 与 buildCurl 各补一处，
 *   两处必须同源（否则"复制的命令"与"实际请求"会对不上）。
 */
import { computed, nextTick, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import CopyButton from '@/components/CopyButton.vue'
import { ApiError } from '@/api/client'
import { listMyTokens } from '@/api/portal'
import type { AccessToken } from '@/api/types'
import { toastError } from '@/composables/useToast'

/** 对话消息：仅用于本地渲染，字段与 OpenAI 的 messages 结构一致，便于直接提交 */
interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

/* ── 调用参数 ─────────────────────────────────────────── */

/**
 * 访问令牌明文：仅存内存，不落任何持久化存储。
 * 页面刷新 / 关闭即丢失，用户需重新粘贴——这是安全与便利之间的明确取舍。
 */
const tokenInput = ref('')
/** 用户自己的令牌列表（只有掩码），用于"对照确认我粘贴的是哪一把" */
const tokens = ref<AccessToken[]>([])

const models = ref<string[]>([])
const modelsLoading = ref(false)
const modelInput = ref('')
const temperature = ref(0.7)
const maxTokens = ref(512)
const useStream = ref(true)

/* ── 对话状态 ─────────────────────────────────────────── */

const messages = ref<ChatMessage[]>([])
const prompt = ref('')
const sending = ref(false)
const requestError = ref('')

const chatURL = `${window.location.origin}/v1/chat/completions`
const modelsURL = `${window.location.origin}/v1/models`

const scrollArea = ref<HTMLElement | null>(null)

onMounted(async () => {
  try {
    const result = await listMyTokens({ page: 1, size: 100 })
    tokens.value = result.items ?? []
  } catch (err) {
    // 令牌列表仅作对照参考，失败不阻断试聊
    if (err instanceof ApiError) toastError(err.message)
  }
})

/** 发送前把本地消息整理成接口格式（跳过流式占位的空内容与失败中断的空消息） */
function buildRequestMessages(): { role: string; content: string }[] {
  return messages.value
    .filter((message) => message.content.trim() !== '')
    .map((message) => ({ role: message.role, content: message.content }))
}

/** 当前请求体：实际发送与"复制 cURL"共用，保证二者永远一致 */
function buildRequestBody(): Record<string, unknown> {
  const history = buildRequestMessages()
  return {
    model: modelInput.value.trim(),
    messages: history.length ? history : [{ role: 'user', content: '你好' }],
    temperature: Number(temperature.value),
    max_tokens: Math.round(Number(maxTokens.value)),
    stream: useStream.value,
  }
}

const curlText = computed(() => {
  // 令牌优先用用户粘贴的真实值，便于命令"可直接运行"；未粘贴时用占位符提示替换
  const token = tokenInput.value.trim() || '<你的访问令牌>'
  const body = JSON.stringify(buildRequestBody())
  return `curl ${chatURL} \\
  -H "Authorization: Bearer ${token}" \\
  -H "Content-Type: application/json" \\
  -d '${body}'`
})

/* ── 加载模型 ─────────────────────────────────────────── */

async function loadModels(): Promise<void> {
  const token = tokenInput.value.trim()
  if (!token) {
    toastError('请先粘贴你的访问令牌，模型列表需要鉴权后才能获取')
    return
  }
  modelsLoading.value = true
  try {
    const resp = await fetch(modelsURL, { headers: { Authorization: `Bearer ${token}` } })
    const raw = await resp.text()
    if (!resp.ok) throw new Error(extractError(raw, resp.status))
    const json = JSON.parse(raw) as { data?: { id?: string }[] }
    models.value = (json.data ?? []).map((item) => item.id ?? '').filter(Boolean)
    if (!modelInput.value && models.value.length) modelInput.value = models.value[0]
    if (!models.value.length) toastError('该令牌下没有可用模型（可能被白名单限制）')
  } catch (err) {
    toastError(err instanceof Error ? err.message : '模型列表加载失败')
  } finally {
    modelsLoading.value = false
  }
}

/* ── 发送 ─────────────────────────────────────────────── */

async function scrollToBottom(): Promise<void> {
  await nextTick()
  const el = scrollArea.value
  if (el) el.scrollTop = el.scrollHeight
}

function clearConversation(): void {
  messages.value = []
  requestError.value = ''
}

async function send(): Promise<void> {
  const text = prompt.value.trim()
  if (!text) return
  if (!tokenInput.value.trim()) {
    toastError('请先粘贴你的访问令牌')
    return
  }
  if (!modelInput.value.trim()) {
    toastError('请先选择或填写模型名')
    return
  }

  messages.value.push({ role: 'user', content: text })
  // 先插入空的助手占位，流式内容直接往里追加（视觉上就是"正在逐字生成"）
  const assistantIndex = messages.value.length
  messages.value.push({ role: 'assistant', content: '' })
  prompt.value = ''
  sending.value = true
  requestError.value = ''
  await scrollToBottom()

  const append = (delta: string): void => {
    messages.value[assistantIndex].content += delta
    void scrollToBottom()
  }

  try {
    if (useStream.value) {
      await streamCompletion(append)
    } else {
      const content = await plainCompletion()
      messages.value[assistantIndex].content = content
    }
  } catch (err) {
    requestError.value = err instanceof Error ? err.message : String(err)
    // 一个字都没生成时移除占位气泡，避免留下空气泡
    if (!messages.value[assistantIndex].content) messages.value.splice(assistantIndex, 1)
  } finally {
    sending.value = false
    await scrollToBottom()
  }
}

/** 非流式：一次性拿到完整回复 */
async function plainCompletion(): Promise<string> {
  const resp = await fetch(chatURL, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${tokenInput.value.trim()}`,
    },
    body: JSON.stringify(buildRequestBody()),
  })
  const raw = await resp.text()
  if (!resp.ok) throw new Error(extractError(raw, resp.status))
  const json = JSON.parse(raw) as { choices?: { message?: { content?: string } }[] }
  const content = json.choices?.[0]?.message?.content
  return typeof content === 'string' ? content : '(上游未返回内容)'
}

/**
 * 流式：手工解析 SSE。
 *
 * 为什么不引第三方库：这里只需要"逐行取 data: 前缀、忽略 [DONE]"这一条最小逻辑，
 * 为此增加一个依赖不划算；且自托管环境下依赖越少越好。
 *
 * 关键细节：网络分片不保证按行到达，必须维护跨 chunk 的缓冲区，
 * 用 split('\n') 后把最后一段（可能是不完整行）留到下次拼接，否则会丢字符。
 */
async function streamCompletion(append: (delta: string) => void): Promise<void> {
  const resp = await fetch(chatURL, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${tokenInput.value.trim()}`,
    },
    body: JSON.stringify(buildRequestBody()),
  })
  if (!resp.ok) throw new Error(extractError(await resp.text(), resp.status))
  if (!resp.body) throw new Error('响应没有可读的流内容')

  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })

    const lines = buffer.split('\n')
    // 最后一段可能是不完整行，留到下一轮再拼
    buffer = lines.pop() ?? ''

    for (const line of lines) {
      const trimmed = line.trim()
      if (!trimmed.startsWith('data:')) continue
      const data = trimmed.slice(5).trim()
      if (data === '[DONE]') {
        await reader.cancel().catch(() => undefined)
        return
      }
      try {
        const json = JSON.parse(data) as { choices?: { delta?: { content?: string } }[] }
        const delta = json.choices?.[0]?.delta?.content
        if (typeof delta === 'string' && delta) append(delta)
      } catch {
        // 上游偶发非 JSON 心跳行：跳过即可，不影响后续内容
      }
    }
  }
}

/* ── 错误与展示辅助 ───────────────────────────────────── */

/** 从错误响应体里尽量取出一句人话：后端/上游统一是 { error: { message } } */
function extractError(raw: string, status: number): string {
  try {
    const json = JSON.parse(raw) as { error?: { message?: string } }
    if (json.error?.message) return json.error.message
  } catch {
    /* 非 JSON：按原文展示 */
  }
  return raw.trim() || `请求失败（HTTP ${status}）`
}

/** 正在使用的令牌是否已在列表中出现（仅用于给出"这次用的是哪把"的对照提示） */
const knownTokenHint = computed(() => {
  if (!tokenInput.value.trim()) return ''
  return tokens.value.length ? '提示：令牌列表里只有掩码，无法直接选中；请确认你粘贴的正是想测试的那把。' : ''
})
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1 class="page-title">游乐场</h1>
        <p class="page-desc">
          用你的访问令牌直接调用 <code class="chip">/v1/chat/completions</code>，在线验证连通性与上游响应。
          令牌明文只存在本页内存中，刷新即清空。
        </p>
      </div>
      <div class="toolbar">
        <button
          type="button"
          class="btn btn-secondary btn-sm"
          :disabled="!messages.length"
          @click="clearConversation"
        >
          <AppIcon name="trash" :size="14" />
          清空对话
        </button>
        <CopyButton :value="curlText" label="复制 cURL" outline small success-text="cURL 已复制" />
      </div>
    </div>

    <!-- 左：调用参数（窄屏下排在对话上方，先配好再聊）；右：对话区 -->
    <div class="grid gap-5 lg:grid-cols-[20rem_minmax(0,1fr)]">
      <!-- ── 调用参数 ─────────────────────────────────── -->
      <section class="card self-start">
        <div class="card-head">
          <h2 class="section-title flex items-center gap-2">
            <AppIcon name="sliders" :size="16" class="text-brand-700" />
            调用参数
          </h2>
        </div>

        <div class="card-pad space-y-4">
          <!-- 令牌：粘贴明文（内存态） -->
          <div>
            <label class="label" for="pg-token">访问令牌 <span class="text-red-600">*</span></label>
            <input
              id="pg-token"
              v-model="tokenInput"
              class="input input-mono"
              type="password"
              autocomplete="off"
              placeholder="粘贴你保存的访问令牌"
            />
            <p class="hint">
              令牌明文只在创建时展示一次，请粘贴你保存的那把。仅保存在内存，不会写入浏览器存储。
            </p>
            <p v-if="knownTokenHint" class="mt-1 text-xs text-ink-400">{{ knownTokenHint }}</p>

            <!-- 令牌对照：只有掩码，帮助确认"我用的是哪一把" -->
            <div v-if="tokens.length" class="mt-2 space-y-1">
              <p class="text-[11px] text-ink-500">我的令牌（仅掩码，供对照）</p>
              <div class="flex flex-wrap gap-1">
                <code v-for="item in tokens" :key="item.id" class="chip">{{ item.name }} · {{ item.masked_key }}</code>
              </div>
            </div>
            <p v-else class="mt-2 text-[11px] text-ink-500">
              还没有令牌？先在「访问令牌」页创建一把并保存明文。
            </p>
          </div>

          <!-- 模型 -->
          <div>
            <label class="label" for="pg-model">模型 <span class="text-red-600">*</span></label>
            <div class="flex items-center gap-2">
              <input
                id="pg-model"
                v-model="modelInput"
                class="input input-mono"
                type="text"
                list="pg-model-options"
                placeholder="选择或手动填写模型名"
              />
              <datalist id="pg-model-options">
                <option v-for="name in models" :key="name" :value="name" />
              </datalist>
              <button
                type="button"
                class="btn btn-secondary btn-sm shrink-0"
                :disabled="modelsLoading"
                @click="loadModels"
              >
                <AppIcon name="refresh" :size="14" />
                {{ modelsLoading ? '加载中…' : '加载' }}
              </button>
            </div>
            <p class="hint">模型列表来自 <code class="chip">GET /v1/models</code>，只列出该令牌可用的模型。</p>
          </div>

          <!-- 温度与最大输出 -->
          <div class="grid grid-cols-2 gap-3">
            <div>
              <label class="label" for="pg-temp">温度</label>
              <input
                id="pg-temp"
                v-model.number="temperature"
                class="input text-right tabular-nums"
                type="number"
                min="0"
                max="2"
                step="0.1"
              />
            </div>
            <div>
              <label class="label" for="pg-max">最大输出</label>
              <input
                id="pg-max"
                v-model.number="maxTokens"
                class="input text-right tabular-nums"
                type="number"
                min="1"
              />
            </div>
          </div>

          <label class="flex items-center gap-2 text-sm text-ink-200">
            <input v-model="useStream" class="checkbox" type="checkbox" />
            流式输出（逐字显示）
          </label>
        </div>
      </section>

      <!-- ── 对话区 ───────────────────────────────────── -->
      <section class="card flex flex-col">
        <div class="card-head">
          <div class="min-w-0">
            <h2 class="section-title">对话</h2>
            <p class="mt-0.5 truncate text-xs text-ink-400">
              {{ modelInput || '未选择模型' }} · {{ useStream ? '流式' : '非流式' }}
            </p>
          </div>
          <span v-if="sending" class="badge badge-info">
            <span class="dot animate-pulse" />
            生成中
          </span>
        </div>

        <!-- 消息列表：限定高度使其内部滚动（否则多轮对话会把整页越撑越长） -->
        <div ref="scrollArea" class="min-h-[42vh] max-h-[60vh] space-y-3 overflow-y-auto px-5 py-4">
          <div v-if="!messages.length" class="flex flex-col items-center justify-center gap-2 py-14 text-center">
            <span class="flex h-10 w-10 items-center justify-center rounded-xl bg-ink-850 text-ink-400 ring-1 ring-inset ring-ink-700">
              <AppIcon name="send" :size="20" />
            </span>
            <p class="text-sm font-medium text-ink-200">粘贴令牌并选择模型后开始对话</p>
            <p class="max-w-md text-xs leading-relaxed text-ink-400">
              上游返回的真实错误会被原样展示，便于定位是令牌、模型还是上游的问题。
            </p>
          </div>

          <div
            v-for="(message, index) in messages"
            :key="index"
            class="flex"
            :class="message.role === 'user' ? 'justify-end' : 'justify-start'"
          >
            <div
              class="max-w-[85%] whitespace-pre-wrap break-words rounded-2xl px-3.5 py-2.5 text-sm leading-relaxed"
              :class="
                message.role === 'user'
                  ? 'bg-brand-600 text-white'
                  : 'border border-ink-800 bg-ink-850/70 text-ink-100'
              "
            >
              <span v-if="message.role === 'assistant' && !message.content" class="text-ink-400">…</span>
              <template v-else>{{ message.content }}</template>
            </div>
          </div>
        </div>

        <!-- 错误：原样展示上游/网关返回的信息 -->
        <p
          v-if="requestError"
          class="mx-5 mb-2 flex items-start gap-2 rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs leading-relaxed text-red-800"
        >
          <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
          <span class="break-all">{{ requestError }}</span>
        </p>

        <!-- 输入区 -->
        <div class="border-t border-ink-800 p-3.5">
          <textarea
            v-model="prompt"
            class="input min-h-[4.5rem] resize-y"
            rows="3"
            placeholder="输入消息，Ctrl / ⌘ + Enter 发送"
            :disabled="sending"
            @keydown.enter.meta.prevent="send"
            @keydown.enter.ctrl.prevent="send"
          />
          <div class="mt-2 flex items-center justify-between gap-2">
            <span class="text-[11px] text-ink-500">
              共 {{ messages.filter((m) => m.content.trim()).length }} 条消息
            </span>
            <button type="button" class="btn btn-primary btn-sm" :disabled="sending || !prompt.trim()" @click="send">
              <span
                v-if="sending"
                class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
                aria-hidden="true"
              />
              <AppIcon v-else name="send" :size="14" />
              {{ sending ? '生成中…' : '发送' }}
            </button>
          </div>
        </div>
      </section>
    </div>
  </div>
</template>
