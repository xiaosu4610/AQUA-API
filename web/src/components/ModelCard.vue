<script setup lang="ts">
/**
 * 模型卡片：模型广场里"一个模型"的完整表达。
 *
 * 意图（Why）：
 *   广场的核心是"一眼看懂能用什么、什么价"。卡片把五件事固定下来：
 *     1) 厂商（头像 + 配色）—— 让人靠颜色/首字扫读，而不必逐字读全名；
 *     2) 模型名 + 是否当前可用（避免用户调用后才发现 503）；
 *     3) 归属哪些分组（不同分组价格不同）；
 *     4) 价格（按 token 或按次），并标出分组倍率；
 *     5) 明确的"点开看详情"入口。
 *
 *   为什么整块卡片可点、而不是只放一个"详情"按钮：
 *   广场是浏览型页面，用户的默认动作就是"点这个模型看看"；
 *   把点击热区放大到整张卡，比让用户去找按钮省一次视觉搜索。
 *   点击后由父组件开弹窗（就地展开），不做路由跳转 —— 跳页会打断浏览节奏。
 *
 * 流转（Flow）：
 *   ModelPlazaBoard → v-for → 本组件；emit('select') 交给父组件开详情弹窗
 *
 * 扩展（Extend）：
 *   新增模型属性（上下文长度、能力标签）时加 prop 并在模板中补一段即可；
 *   不要在本组件内请求接口，否则一张页面会发出几十个请求。
 */
import { computed } from 'vue'

import AppIcon from './AppIcon.vue'
import type { PlazaModel, PlazaPrice } from '@/api/types'
import { formatNumber } from '@/utils/format'
import { vendorInitial, vendorOf, vendorTone } from '@/utils/vendor'

const props = withDefaults(
  defineProps<{
    model: PlazaModel
    /** 分组标识 → 展示名；缺失时回退标识本身 */
    groupLabels?: Record<string, string>
    /** 是否显示"复制模型名"按钮 */
    showCopy?: boolean
  }>(),
  { groupLabels: () => ({}), showCopy: true },
)

const emit = defineEmits<{
  (e: 'copy', value: string): void
  (e: 'select', model: PlazaModel): void
}>()

/** 所属分组的展示名列表 */
const groupLabelsOfModel = computed(() =>
  props.model.groups.map((name) => props.groupLabels?.[name] || name),
)

const vendor = computed(() => vendorOf(props.model.model))

/** 是否为"按次计费"的能力（图像/视频类） */
function isPerCall(price: PlazaPrice): boolean {
  return price.per_call_price > 0
}

/** 格式化成 "每 1M" 的读数：价格为 0 表示未定价 */
function tokenPriceText(value: number): string {
  return value > 0 ? formatNumber(value) : '未定价'
}

/** 倍率展示：100 显示为 1.0x */
function ratioText(ratio: number): string {
  return `${(ratio / 100).toFixed(ratio % 100 === 0 ? 1 : 2)}x`
}

/** 复制按钮不能同时触发"打开详情"：单独拦住冒泡 */
function onCopy(): void {
  emit('copy', props.model.model)
}
</script>

<template>
  <article
    class="model-card group cursor-pointer"
    role="button"
    tabindex="0"
    :aria-label="`查看 ${model.model} 详情`"
    @click="emit('select', model)"
    @keydown.enter.prevent="emit('select', model)"
    @keydown.space.prevent="emit('select', model)"
  >
    <!-- 标题行：厂商头像 + 模型名 + 可用状态 -->
    <header class="flex items-start gap-3">
      <span class="vendor-avatar" :class="vendorTone(vendor)" aria-hidden="true">
        {{ vendorInitial(vendor) }}
      </span>

      <div class="min-w-0 flex-1">
        <h3 class="truncate font-mono text-sm font-semibold text-ink-50" :title="model.model">
          {{ model.model }}
        </h3>
        <p class="mt-1.5 flex flex-wrap items-center gap-1.5">
          <span class="badge" :class="model.available ? 'badge-ok' : 'badge-off'">
            <span class="dot" />
            {{ model.available ? '可用' : '未接入渠道' }}
          </span>
          <span v-if="model.channel_count > 0" class="text-xs text-ink-400">
            {{ model.channel_count }} 个渠道
          </span>
        </p>
      </div>

      <button
        v-if="showCopy"
        type="button"
        class="btn-row shrink-0"
        title="复制模型名"
        aria-label="复制模型名"
        @click.stop="onCopy"
      >
        <AppIcon name="copy" :size="14" />
      </button>
    </header>

    <!-- 分组标签 -->
    <div class="flex min-h-[22px] flex-wrap items-center gap-1.5">
      <template v-if="groupLabelsOfModel.length">
        <span v-for="label in groupLabelsOfModel" :key="label" class="chip">
          <AppIcon name="tag" :size="11" class="opacity-60" />
          {{ label }}
        </span>
      </template>
      <span v-else class="text-xs text-ink-500">暂无分组</span>
    </div>

    <!-- 价格：按分组逐条列出（这正是"分组"存在的意义） -->
    <div class="mt-auto space-y-1.5 border-t border-ink-800/60 pt-3">
      <template v-if="model.prices.length">
        <div
          v-for="price in model.prices"
          :key="price.group"
          class="flex items-center justify-between gap-2 text-xs"
        >
          <span class="flex min-w-0 items-center gap-1.5">
            <span class="truncate text-ink-300">{{ groupLabels[price.group] || price.group }}</span>
            <span v-if="price.ratio !== 100" class="badge badge-warn">{{ ratioText(price.ratio) }}</span>
          </span>

          <!-- 按次计费与按 token 计费是两种口径，展示上必须区分 -->
          <span v-if="isPerCall(price)" class="whitespace-nowrap font-mono text-ink-200">
            {{ formatNumber(price.per_call_price) }} <span class="text-ink-500">/ 次</span>
          </span>
          <span v-else class="whitespace-nowrap font-mono text-ink-200" title="每 100 万 token 的额度">
            {{ tokenPriceText(price.prompt_price) }} / {{ tokenPriceText(price.completion_price) }}
          </span>
        </div>
        <p class="pt-0.5 text-[11px] text-ink-500">
          {{ model.prices.some(isPerCall) ? '按次计费' : '输入 / 输出（每 100 万 token）' }}
        </p>
      </template>
      <p v-else class="text-xs text-ink-500">未配置价格（调用不计费）</p>
    </div>

    <!-- 底部提示：让"卡片可点"这件事变得显式 -->
    <p class="-mb-0.5 flex items-center gap-1 text-[11px] font-medium text-brand-700 opacity-0 transition-opacity group-hover:opacity-100">
      <AppIcon name="info" :size="12" />
      查看详情
    </p>
  </article>
</template>
