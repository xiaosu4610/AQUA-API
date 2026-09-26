<script setup lang="ts">
/**
 * 模型详情弹窗：在广场就地展开一个模型的全部信息，不跳页。
 *
 * 意图（Why）：
 *   用户在广场上想确认的是三件事——「能不能用」「多贵」「怎么调」。
 *   这三件事都在当前上下文里有答案，因此用弹窗就地展开，
 *   而不是跳到另一个页面（跳页会让用户丢失当前的筛选条件与滚动位置，
 *   这正是"点一个按钮就换一个页面"最让人烦躁的地方）。
 *
 *   为什么要给 cURL 示例：很多用户是拿着模型名来找调用方式的；
 *   把可直接粘贴运行的命令放在同一屏，省掉"再去文档页翻一遍"的往返。
 *
 *   文案走词条（components.modelDetail.*）；模型名、端点、cURL 一律 dir="ltr"。
 *
 * 流转（Flow）：
 *   ModelPlazaBoard → 点击卡片 → 传入 model → 本弹窗
 *
 * 扩展（Extend）：
 *   需要展示上下文长度、能力标签等更多属性时，在后端模型广场接口补字段
 *   并在此追加一段（不要在前端硬编码模型元数据）。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import AppIcon from './AppIcon.vue'
import CopyButton from './CopyButton.vue'
import Modal from './Modal.vue'
import type { PlazaModel, PlazaPrice } from '@/api/types'
import { formatNumber } from '@/utils/format'
import { vendorInitial, vendorOf, vendorLabel, vendorTone } from '@/utils/vendor'

const props = withDefaults(
  defineProps<{
    open: boolean
    /** 当前查看的模型；为 null 时不渲染内容 */
    model: PlazaModel | null
    groupLabels?: Record<string, string>
    /** 对外 base_url（用于生成调用示例） */
    baseUrl: string
  }>(),
  { groupLabels: () => ({}) },
)

const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()

const vendor = computed(() => (props.model ? vendorOf(props.model.model) : ''))

/**
 * 是否为嵌入类模型。
 *
 * 为什么要判断：嵌入模型（embed / rerank / clip）不提供 /v1/chat/completions，
 * 给它发对话请求只会得到 404。示例必须按模型类型给出正确的端点，
 * 否则用户复制过去就是失败 —— 网关也因此单独开放了 /v1/embeddings。
 */
const isEmbedding = computed(() => /embed|rerank|clip/i.test(props.model?.model ?? ''))

/** 调用示例：可直接粘贴运行的 cURL（英文命令，始终 LTR） */
const curlSample = computed(() => {
  if (!props.model) return ''
  const endpoint = isEmbedding.value ? '/embeddings' : '/chat/completions'
  const payload = isEmbedding.value
    ? `{\n    "model": "${props.model.model}",\n    "input": "hello world"\n  }`
    : `{\n    "model": "${props.model.model}",\n    "messages": [{"role": "user", "content": "你好"}]\n  }`
  return [
    `curl ${props.baseUrl}${endpoint} \\`,
    '  -H "Authorization: Bearer $AQUA_API_KEY" \\',
    '  -H "Content-Type: application/json" \\',
    `  -d '${payload}'`,
  ].join('\n')
})

function isPerCall(price: PlazaPrice): boolean {
  return price.per_call_price > 0
}

function priceText(value: number): string {
  return value > 0 ? formatNumber(value) : t('components.modelDetail.unpriced')
}

function ratioText(ratio: number): string {
  return `${(ratio / 100).toFixed(ratio % 100 === 0 ? 1 : 2)}x`
}

function groupLabel(name: string): string {
  return props.groupLabels[name] || name
}
</script>

<template>
  <Modal
    :open="open && !!model"
    :title="model?.model || ''"
    :subtitle="t('components.modelDetail.subtitle')"
    width="max-w-2xl"
    @close="emit('close')"
  >
    <template v-if="model">
      <!-- 身份行：厂商 + 状态 + 渠道数 -->
      <div class="flex flex-wrap items-center gap-3 rounded-xl border border-ink-800/70 bg-ink-950/60 p-3.5">
        <span class="vendor-avatar" :class="vendorTone(vendor)" aria-hidden="true">
          {{ vendorInitial(vendor) }}
        </span>
        <div class="min-w-0 flex-1">
          <p class="truncate font-mono text-sm font-semibold text-ink-50" dir="ltr">{{ model.model }}</p>
          <p class="mt-0.5 text-xs text-ink-400">
            {{ t('components.modelDetail.upstreamVendor') }}{{ vendorLabel(vendor) }}
            <template v-if="model.channel_count > 0">{{ t('components.modelDetail.channelSupport', { count: model.channel_count }) }}</template>
          </p>
        </div>
        <span class="badge" :class="model.available ? 'badge-ok' : 'badge-off'">
          <span class="dot" />
          {{ model.available ? t('components.modelDetail.available') : t('components.modelDetail.unavailable') }}
        </span>
      </div>

      <!-- 状态说明：把"不可用"到底意味着什么讲清楚，避免误判 -->
      <p
        v-if="!model.available"
        class="mt-3 flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/10 px-3 py-2 text-xs leading-relaxed text-amber-800"
      >
        <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
        {{ t('components.modelDetail.unavailableHint') }}
      </p>

      <!-- 分组与价格 -->
      <h3 class="mt-5 flex items-center gap-1.5 text-sm font-semibold text-ink-100">
        <AppIcon name="tag" :size="15" class="text-brand-700" />
        {{ t('components.modelDetail.groupsAndPrices') }}
      </h3>
      <div class="mt-2 table-wrap">
        <table class="data-table min-w-[520px]">
          <thead>
            <tr>
              <th>{{ t('components.modelDetail.col.group') }}</th>
              <th>{{ t('components.modelDetail.col.ratio') }}</th>
              <th class="text-right">{{ t('components.modelDetail.col.input') }}</th>
              <th class="text-right">{{ t('components.modelDetail.col.output') }}</th>
              <th class="text-right">{{ t('components.modelDetail.col.perCall') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="price in model.prices" :key="price.group">
              <td class="font-medium text-ink-100">{{ groupLabel(price.group) }}</td>
              <td>
                <span class="badge" :class="price.ratio === 100 ? 'badge-off' : 'badge-warn'">
                  {{ ratioText(price.ratio) }}
                </span>
              </td>
              <td class="cell-num">{{ isPerCall(price) ? '—' : priceText(price.prompt_price) }}</td>
              <td class="cell-num">{{ isPerCall(price) ? '—' : priceText(price.completion_price) }}</td>
              <td class="cell-num">{{ isPerCall(price) ? formatNumber(price.per_call_price) : '—' }}</td>
            </tr>
            <tr v-if="!model.prices.length">
              <td colspan="5" class="cell-muted text-center">{{ t('components.modelDetail.noPrice') }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-[11px] leading-relaxed text-ink-500">
        {{ t('components.modelDetail.ratioHint') }}
      </p>

      <!-- 调用示例 -->
      <div class="mt-5 flex flex-wrap items-center justify-between gap-2">
        <h3 class="flex items-center gap-1.5 text-sm font-semibold text-ink-100">
          <AppIcon name="terminal" :size="15" class="text-brand-700" />
          {{ t('components.modelDetail.callExample') }}
        </h3>
        <CopyButton
          :value="curlSample"
          :label="t('components.modelDetail.copyCommand')"
          :success-text="t('components.modelDetail.commandCopied')"
          small
          outline
        />
      </div>
      <div class="code-block mt-2">
        <pre dir="ltr">{{ curlSample }}</pre>
      </div>
      <p class="mt-2 flex items-start gap-1.5 text-[11px] leading-relaxed text-ink-500">
        <AppIcon name="info" :size="12" class="mt-0.5 shrink-0" />
        <i18n-t v-if="isEmbedding" keypath="components.modelDetail.embeddingHint" tag="span">
          <template #endpoint><code class="chip" dir="ltr">/v1/embeddings</code></template>
        </i18n-t>
        <i18n-t v-else keypath="components.modelDetail.keyHint" tag="span">
          <template #key><code class="chip" dir="ltr">$AQUA_API_KEY</code></template>
          <template #stream><code class="chip" dir="ltr">"stream": true</code></template>
        </i18n-t>
      </p>
    </template>

    <template #footer>
      <CopyButton
        :value="model?.model || ''"
        :label="t('components.modelDetail.copyModelName')"
        :success-text="t('components.modelDetail.modelNameCopied')"
        outline
      />
      <button type="button" class="btn btn-primary" @click="emit('close')">{{ t('components.modelDetail.close') }}</button>
    </template>
  </Modal>
</template>
