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
 * 流转（Flow）：
 *   ModelPlazaBoard → 点击卡片 → 传入 model → 本弹窗
 *
 * 扩展（Extend）：
 *   需要展示上下文长度、能力标签等更多属性时，在后端模型广场接口补字段
 *   并在此追加一段（不要在前端硬编码模型元数据）。
 */
import { computed } from 'vue'

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

const vendor = computed(() => (props.model ? vendorOf(props.model.model) : ''))

/**
 * 是否为嵌入类模型。
 *
 * 为什么要判断：嵌入模型（embed / rerank / clip）不提供 /v1/chat/completions，
 * 给它发对话请求只会得到 404。示例必须按模型类型给出正确的端点，
 * 否则用户复制过去就是失败 —— 网关也因此单独开放了 /v1/embeddings。
 */
const isEmbedding = computed(() => /embed|rerank|clip/i.test(props.model?.model ?? ''))

/** 调用示例：可直接粘贴运行的 cURL */
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
  return value > 0 ? formatNumber(value) : '未定价'
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
    subtitle="模型详情与调用方式。价格均为每 100 万 token 的额度消耗，按次计费的模型单独标注。"
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
          <p class="truncate font-mono text-sm font-semibold text-ink-50">{{ model.model }}</p>
          <p class="mt-0.5 text-xs text-ink-400">
            上游厂商：{{ vendorLabel(vendor) }}
            <template v-if="model.channel_count > 0"> · {{ model.channel_count }} 个渠道支持</template>
          </p>
        </div>
        <span class="badge" :class="model.available ? 'badge-ok' : 'badge-off'">
          <span class="dot" />
          {{ model.available ? '当前可用' : '未接入渠道' }}
        </span>
      </div>

      <!-- 状态说明：把"不可用"到底意味着什么讲清楚，避免误判 -->
      <p
        v-if="!model.available"
        class="mt-3 flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/10 px-3 py-2 text-xs leading-relaxed text-amber-800"
      >
        <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
        该模型暂无启用中的上游渠道，调用会立即返回 503。请在后台「渠道管理」里为它配置渠道。
      </p>

      <!-- 分组与价格 -->
      <h3 class="mt-5 flex items-center gap-1.5 text-sm font-semibold text-ink-100">
        <AppIcon name="tag" :size="15" class="text-brand-700" />
        分组与价格
      </h3>
      <div class="mt-2 table-wrap">
        <table class="data-table min-w-[520px]">
          <thead>
            <tr>
              <th>分组</th>
              <th>倍率</th>
              <th class="text-right">输入</th>
              <th class="text-right">输出</th>
              <th class="text-right">按次</th>
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
              <td colspan="5" class="cell-muted text-center">未配置价格（调用不计费）</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-[11px] leading-relaxed text-ink-500">
        倍率来自模型分组（100 = 1.0 倍）；实际扣费 = 基础额度 × 倍率 ÷ 100。
      </p>

      <!-- 调用示例 -->
      <div class="mt-5 flex flex-wrap items-center justify-between gap-2">
        <h3 class="flex items-center gap-1.5 text-sm font-semibold text-ink-100">
          <AppIcon name="terminal" :size="15" class="text-brand-700" />
          调用示例
        </h3>
        <CopyButton :value="curlSample" label="复制命令" success-text="调用命令已复制" small outline />
      </div>
      <div class="code-block mt-2">
        <pre>{{ curlSample }}</pre>
      </div>
      <p class="mt-2 flex items-start gap-1.5 text-[11px] leading-relaxed text-ink-500">
        <AppIcon name="info" :size="12" class="mt-0.5 shrink-0" />
        <span v-if="isEmbedding">
          这是嵌入类模型，必须使用 <code class="chip">/v1/embeddings</code> 端点；用对话端点调用会返回 404。
        </span>
        <span v-else>
          把 <code class="chip">$AQUA_API_KEY</code> 换成你在「访问令牌」里创建的密钥；需要流式输出时在请求体里加
          <code class="chip">"stream": true</code>。
        </span>
      </p>
    </template>

    <template #footer>
      <CopyButton :value="model?.model || ''" label="复制模型名" success-text="模型名已复制" outline />
      <button type="button" class="btn btn-primary" @click="emit('close')">关闭</button>
    </template>
  </Modal>
</template>
