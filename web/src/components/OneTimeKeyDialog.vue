<script setup lang="ts">
/**
 * 一次性明文密钥展示弹窗。
 *
 * 意图（Why）：
 *   契约明确「明文 key 仅此一次返回」，一旦关闭就无法再次获取；
 *   若用户漏看就永久丢密钥，所以这里必须做到：显著提示 + 强复制入口 + 明确的确认动作。
 *   文案走词条（components.oneTimeKey.*）；密钥、URL 等一律 dir="ltr"，避免阿拉伯语下倒排。
 *
 * 流转（Flow）：
 *   创建令牌成功 → 父页面把 result.key 传入 → 本弹窗展示
 *   → 用户复制/关闭 → 父页面清空 apiKey（避免明文长期驻留内存）
 *
 * 扩展（Extend）：
 *   若将来支持「下载 .env 配置」，在下方补充按钮即可。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

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

const { t } = useI18n()

/** 当前站点的 API 基地址：优先用浏览器地址，便于用户直接复制到客户端配置 */
const baseUrl = computed(() => `${window.location.origin}/v1`)
</script>

<template>
  <Modal
    :open="open"
    :title="t('components.oneTimeKey.title')"
    :subtitle="t('components.oneTimeKey.subtitle')"
    width="max-w-2xl"
    :close-on-backdrop="false"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <!-- 风险提示：用醒目的警示色，避免用户直接点关闭 -->
      <div class="flex gap-3 rounded-xl border border-amber-500/25 bg-amber-500/10 px-4 py-3">
        <AppIcon name="alert" :size="18" class="mt-0.5 text-amber-700" />
        <i18n-t keypath="components.oneTimeKey.warning" tag="p" class="text-sm leading-relaxed text-amber-800">
          <template #strong>
            <strong class="font-semibold">{{ t('components.oneTimeKey.warningStrong') }}</strong>
          </template>
        </i18n-t>
      </div>

      <div>
        <p v-if="tokenName" class="mb-1.5 text-xs text-ink-400">
          {{ t('components.oneTimeKey.tokenName') }}<span class="text-ink-200">{{ tokenName }}</span>
        </p>
        <div class="code-block">
          <div class="flex items-center justify-between gap-3 border-b border-ink-800 px-4 py-2">
            <span class="font-mono text-xs text-ink-300">{{ t('components.oneTimeKey.accessToken') }}</span>
            <CopyButton
              :value="apiKey"
              :label="t('components.oneTimeKey.copyKey')"
              small
              :success-text="t('components.oneTimeKey.tokenCopied')"
            />
          </div>
          <pre class="whitespace-pre-wrap break-all" dir="ltr">{{ apiKey }}</pre>
        </div>
      </div>

      <div class="rounded-xl border border-ink-800 bg-ink-850/50 px-4 py-3">
        <p class="mb-2 text-xs font-medium text-ink-300">{{ t('components.oneTimeKey.nextSteps') }}</p>
        <div class="space-y-2 text-xs leading-relaxed text-ink-400">
          <p>
            {{ t('components.oneTimeKey.apiBase') }}<code class="chip" dir="ltr">{{ baseUrl }}</code>
            <CopyButton
              :value="baseUrl"
              small
              class="ms-1 inline-flex align-middle"
              :success-text="t('components.oneTimeKey.baseCopied')"
            />
          </p>
          <p>
            {{ t('components.oneTimeKey.authHeader') }}<code class="chip" dir="ltr">Authorization: Bearer {{ apiKey.slice(0, 7) }}…</code>
          </p>
        </div>
      </div>
    </div>

    <template #footer>
      <CopyButton
        :value="apiKey"
        :label="t('components.oneTimeKey.copyAndClose')"
        :success-text="t('components.oneTimeKey.tokenCopied')"
      />
      <button type="button" class="btn btn-primary" @click="emit('close')">
        <AppIcon name="check" :size="16" />
        {{ t('components.oneTimeKey.saved') }}
      </button>
    </template>
  </Modal>
</template>
