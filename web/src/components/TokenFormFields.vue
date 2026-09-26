<script setup lang="ts">
/**
 * 令牌表单字段（用户门户与管理端共用）。
 *
 * 意图（Why）：
 *   「创建访问令牌」的字段与校验在门户页、管理页完全一致；
 *   抽成组件后，规则只需维护一处，也保证两处 UI 密度一致。
 *
 * 流转（Flow）：
 *   父页面持有 TokenFormState → v-model 传入本组件 → 各字段 patch 后回传新对象
 *   → 父页面调用 validateTokenForm() / toTokenPayload() → 提交
 *
 * 扩展（Extend）：
 *   新增字段时：改 composables/tokenForm.ts 的 TokenFormState，
 *   再在此处补表单项并调用 patch()。
 */
import { computed } from 'vue'

import AppIcon from './AppIcon.vue'
import type { TokenFormState } from '@/composables/tokenForm'

const props = withDefaults(
  defineProps<{
    modelValue: TokenFormState
    /** 站点可用模型，用于「限定模型」的快捷选择（来自 /api/status） */
    availableModels?: string[]
    /** 是否展示「所属用户」选择位（由管理端自行渲染，故此处仅用于调整说明文案） */
    adminMode?: boolean
  }>(),
  { availableModels: () => [], adminMode: false },
)

const emit = defineEmits<{ (e: 'update:modelValue', value: TokenFormState): void }>()

/** 有效期预设：契约里 0 表示永不过期，其余为天数 */
const EXPIRY_PRESETS = [0, 7, 30, 90, 365]

/** 统一的状态更新入口：始终保持「不可变」写法，便于父组件追踪变化 */
function patch<K extends keyof TokenFormState>(key: K, value: TokenFormState[K]): void {
  emit('update:modelValue', { ...props.modelValue, [key]: value })
}

/**
 * 有效期下拉的取值：
 *   '0' / '7' / ... 命中预设；其它数值视为「自定义」，切到自定义时给一个合理的初值（30 天）。
 */
const expiryChoice = computed<string>({
  get() {
    const days = props.modelValue.expires_in_days
    return EXPIRY_PRESETS.includes(days) ? String(days) : 'custom'
  },
  set(choice) {
    patch('expires_in_days', choice === 'custom' ? 30 : Number(choice))
  },
})

/** 可用模型快捷选项：最多展示 8 个，避免表单被长列表撑开 */
const modelSuggestions = computed(() => props.availableModels.slice(0, 8))

/** 已选模型（用于判断快捷项是否已添加） */
const selectedModels = computed(() => {
  const text = props.modelValue.modelText
  return text
    .split(/[,，\s]+/)
    .map((item) => item.trim())
    .filter(Boolean)
})

/** 把某个模型追加到「限定模型」输入框 */
function addModel(model: string): void {
  if (selectedModels.value.includes(model)) return
  const next = [...selectedModels.value, model]
  patch('modelText', next.join(', '))
}

function onNameInput(event: Event): void {
  patch('name', (event.target as HTMLInputElement).value)
}

function onExpiryInput(event: Event): void {
  patch('expires_in_days', Number((event.target as HTMLInputElement).value))
}

function onQuotaInput(event: Event): void {
  patch('remain_quota', Number((event.target as HTMLInputElement).value))
}

function onModelsInput(event: Event): void {
  patch('modelText', (event.target as HTMLInputElement).value)
}

function onUnlimitedChange(event: Event): void {
  patch('unlimited_quota', (event.target as HTMLInputElement).checked)
}
</script>

<template>
  <div class="space-y-5">
    <!-- 名称：用于在日志里区分调用来源，故为必填 -->
    <div>
      <label class="label" for="token-name">令牌名称 <span class="text-red-600">*</span></label>
      <input
        id="token-name"
        class="input"
        type="text"
        placeholder="例如：CI 构建、本地开发"
        maxlength="64"
        :value="modelValue.name"
        @input="onNameInput"
      />
      <p class="hint">名称会出现在调用日志中，建议写明用途，便于后续排查与停用。</p>
    </div>

    <!-- 有效期 -->
    <div>
      <label class="label" for="token-expiry">有效期</label>
      <div class="flex flex-wrap gap-2">
        <select id="token-expiry" v-model="expiryChoice" class="input max-w-[12rem]">
          <option value="0">永不过期</option>
          <option value="7">7 天</option>
          <option value="30">30 天</option>
          <option value="90">90 天</option>
          <option value="365">365 天</option>
          <option value="custom">自定义…</option>
        </select>
        <div v-if="expiryChoice === 'custom'" class="flex items-center gap-2">
          <input
            class="input max-w-[8rem] text-right tabular-nums"
            type="number"
            min="1"
            :value="modelValue.expires_in_days"
            @input="onExpiryInput"
          />
          <span class="text-sm text-ink-300">天</span>
        </div>
      </div>
      <p class="hint">到期后令牌自动失效（0 表示永不过期）。</p>
    </div>

    <!-- 额度 -->
    <div>
      <label class="label">额度限制</label>
      <label class="flex cursor-pointer items-start gap-2.5 rounded-lg border border-ink-700 bg-ink-900 px-3 py-2.5">
        <input
          class="checkbox mt-0.5"
          type="checkbox"
          :checked="modelValue.unlimited_quota"
          @change="onUnlimitedChange"
        />
        <span class="min-w-0">
          <span class="block text-sm text-ink-100">不限制额度</span>
          <span class="block text-xs text-ink-400">该令牌额度随所属用户额度计算，适用于个人使用场景。</span>
        </span>
      </label>

      <div v-if="!modelValue.unlimited_quota" class="mt-3">
        <label class="label" for="token-quota">令牌可用额度</label>
        <input
          id="token-quota"
          class="input max-w-[14rem] text-right tabular-nums"
          type="number"
          min="1"
          :value="modelValue.remain_quota"
          @input="onQuotaInput"
        />
        <p class="hint">额度为平台内部计量单位，用尽后该令牌的调用会被拒绝（HTTP 429）。</p>
      </div>
    </div>

    <!-- 模型限制 -->
    <div>
      <label class="label" for="token-models">限定可用模型（可选）</label>
      <input
        id="token-models"
        class="input input-mono"
        type="text"
        placeholder="留空表示不限制，多个模型用英文逗号分隔"
        :value="modelValue.modelText"
        @input="onModelsInput"
      />
      <div v-if="modelSuggestions.length" class="mt-2 flex flex-wrap gap-1.5">
        <button
          v-for="model in modelSuggestions"
          :key="model"
          type="button"
          class="chip transition hover:border-brand-500/40 hover:text-brand-700"
          :class="selectedModels.includes(model) ? 'border-brand-500/50 text-brand-700' : ''"
          @click="addModel(model)"
        >
          <AppIcon v-if="selectedModels.includes(model)" name="check" :size="12" class="mr-1" />
          {{ model }}
        </button>
      </div>
      <p class="hint">留空即不限制；限定后仅允许调用列出的模型。</p>
    </div>
  </div>
</template>
