<script setup lang="ts">
/**
 * 一次性明文密钥展示弹窗。
 *
 * 意图（Why）：
 *   契约明确「明文 key 仅此一次返回」，一旦关闭就无法再次获取；
 *   若用户漏看就永久丢密钥，所以这里必须做到：显著提示 + 强复制入口 + 明确的确认动作。
 *
 * 流转（Flow）：
 *   创建令牌成功 → 父页面把 result.key 传入 → 本弹窗展示
 *   → 用户复制/关闭 → 父页面清空 apiKey（避免明文长期驻留内存）
 *
 * 扩展（Extend）：
 *   若将来支持「下载 .env 配置」，在下方补充按钮即可。
 */
import { computed } from 'vue'

import AppIcon from './AppIcon.vue'
import CopyButton from './CopyButton.vue'
import Modal from './Modal.vue'

const props = withDefaults(
  defineProps<{
    open: boolean
    /** 明文访问令牌 */
    apiKey: string
    /** 令牌名称，用于提示「这是哪一个令牌」 */
    tokenName?: string
  }>(),
  { tokenName: '' },
)

const emit = defineEmits<{ (e: 'close'): void }>()

/** 当前站点的 API 基地址：优先用浏览器地址，便于用户直接复制到客户端配置 */
const baseUrl = computed(() => `${window.location.origin}/v1`)
</script>

<template>
  <Modal
    :open="open"
    title="请立即保存访问令牌"
    subtitle="出于安全考虑，明文密钥只在此处展示一次，关闭后将无法再次查看。"
    width="max-w-2xl"
    :close-on-backdrop="false"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <!-- 风险提示：用醒目的警示色，避免用户直接点关闭 -->
      <div class="flex gap-3 rounded-xl border border-amber-500/25 bg-amber-500/10 px-4 py-3">
        <AppIcon name="alert" :size="18" class="mt-0.5 text-amber-300" />
        <p class="text-sm leading-relaxed text-amber-100">
          明文密钥<strong class="font-semibold">不会再显示</strong>，请立刻复制并妥善保存。
          若已丢失，只能删除该令牌后重新创建。
        </p>
      </div>

      <div>
        <p v-if="tokenName" class="mb-1.5 text-xs text-ink-400">
          令牌名称：<span class="text-ink-200">{{ tokenName }}</span>
        </p>
        <div class="code-block">
          <div class="flex items-center justify-between gap-3 border-b border-ink-800 px-4 py-2">
            <span class="font-mono text-xs text-ink-300">访问令牌</span>
            <CopyButton :value="apiKey" label="复制密钥" small success-text="访问令牌已复制" />
          </div>
          <pre class="whitespace-pre-wrap break-all">{{ apiKey }}</pre>
        </div>
      </div>

      <div class="rounded-xl border border-ink-800 bg-ink-850/50 px-4 py-3">
        <p class="mb-2 text-xs font-medium text-ink-300">接下来怎么用</p>
        <div class="space-y-2 text-xs leading-relaxed text-ink-400">
          <p>
            API 基地址：<code class="chip">{{ baseUrl }}</code>
            <CopyButton :value="baseUrl" small class="ml-1 inline-flex align-middle" success-text="基地址已复制" />
          </p>
          <p>在客户端请求头中携带：<code class="chip">Authorization: Bearer {{ apiKey.slice(0, 7) }}…</code></p>
        </div>
      </div>
    </div>

    <template #footer>
      <CopyButton :value="apiKey" label="复制并关闭" success-text="访问令牌已复制" />
      <button type="button" class="btn btn-primary" @click="emit('close')">
        <AppIcon name="check" :size="16" />
        我已保存
      </button>
    </template>
  </Modal>
</template>
