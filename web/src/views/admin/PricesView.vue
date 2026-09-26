<script setup lang="ts">
/**
 * 管理后台 · 计价规则：模型价格表与费用试算。
 *
 * 意图（Why）：
 *   价格是「用量 → 费用」的唯一换算依据（见后端 model.ModelPrice 的口径说明）。
 *   本页要保证两件事对管理员始终可见：
 *     1) 口径本身——「每 100 万 token 的额度」与「每次调用的额度」是两种口径，
 *        混在一起最容易定价错误，因此列表中分别用提示文字标出；
 *     2) 匹配优先级——支持通配（gpt-4* / *），列表按「具体 → 笼统」排序，
 *        管理员能一眼看出"哪条会生效"。
 *
 * 流转（Flow）：
 *   进入页面 → listGroups() + listPrices() → 表格
 *   新建/编辑 → Modal 表单 → createPrice / updatePrice（后端会清计费缓存）
 *   费用试算 → quotePrice(model, prompt, completion) → 展示应扣额度
 *
 * 扩展（Extend）：
 *   新增计价维度（缓存命中价等）：在 types.ts 的 ModelPrice 与后端 model_prices 加字段，
 *   在本页表格与表单各补一处。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import { ApiError } from '@/api/client'
import { createPrice, deletePrice, listGroups, listPrices, quotePrice, updatePrice } from '@/api/admin'
import type { ModelGroup, ModelPrice } from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatNumber } from '@/utils/format'

const prices = ref<ModelPrice[]>([])
const groups = ref<ModelGroup[]>([])
const loading = ref(true)
const error = ref('')

/** 分组筛选（空串表示全部） */
const groupFilter = ref('')

const formOpen = ref(false)
const editing = ref<ModelPrice | null>(null)
const saving = ref(false)
const formError = ref('')

const form = ref({
  model: '',
  group: 'default',
  promptPrice: '0',
  completionPrice: '0',
  perCallPrice: '0',
  enabled: true,
  remark: '',
})

/** 费用试算 */
const quote = ref({ model: '', promptTokens: '1000', completionTokens: '1000' })
const quoteResult = ref<{ quota: number; priced: boolean } | null>(null)
const quoting = ref(false)
const quoteError = ref('')

const groupLabels = computed(() => {
  const map: Record<string, string> = {}
  for (const group of groups.value) map[group.name] = group.label
  return map
})

/** 是否为通配规则（界面上据此提示"这条会被更具体的规则覆盖"） */
function patternHint(price: ModelPrice): string {
  if (price.model === '*') return '全局通配（优先级最低）'
  if (price.model.endsWith('*')) return '前缀通配'
  return '精确匹配'
}

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listPrices(groupFilter.value)
    prices.value = result.items ?? []
  } catch (err) {
    prices.value = []
    error.value = err instanceof ApiError ? err.message : '计价规则加载失败'
  } finally {
    loading.value = false
  }
}

async function loadGroups(): Promise<void> {
  try {
    const result = await listGroups()
    groups.value = result.items ?? []
  } catch {
    // 分组仅用于下拉选项与展示名，失败不阻断价格管理
    groups.value = []
  }
}

onMounted(async () => {
  await Promise.all([loadGroups(), load()])
})

function openCreate(): void {
  editing.value = null
  formError.value = ''
  form.value = {
    model: '',
    group: groups.value[0]?.name || 'default',
    promptPrice: '0',
    completionPrice: '0',
    perCallPrice: '0',
    enabled: true,
    remark: '',
  }
  formOpen.value = true
}

function openEdit(price: ModelPrice): void {
  editing.value = price
  formError.value = ''
  form.value = {
    model: price.model,
    group: price.group,
    promptPrice: String(price.prompt_price),
    completionPrice: String(price.completion_price),
    perCallPrice: String(price.per_call_price),
    enabled: price.enabled,
    remark: price.remark,
  }
  formOpen.value = true
}

function toNonNegative(value: string, fallback = 0): number {
  const parsed = Number(value)
  if (!Number.isFinite(parsed) || parsed < 0) return fallback
  return Math.round(parsed)
}

async function submit(): Promise<void> {
  if (!form.value.model.trim()) {
    formError.value = '模型名不能为空（可用 * 表示全部模型）'
    return
  }

  saving.value = true
  formError.value = ''
  const payload = {
    model: form.value.model.trim(),
    group: form.value.group.trim() || 'default',
    prompt_price: toNonNegative(form.value.promptPrice),
    completion_price: toNonNegative(form.value.completionPrice),
    per_call_price: toNonNegative(form.value.perCallPrice),
    enabled: form.value.enabled,
    remark: form.value.remark.trim(),
  }

  try {
    if (editing.value) {
      await updatePrice(editing.value.id, payload)
      toastSuccess('计价规则已更新，新价格立即生效')
    } else {
      await createPrice(payload)
      toastSuccess('计价规则已创建，新价格立即生效')
    }
    formOpen.value = false
    await load()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

async function remove(price: ModelPrice): Promise<void> {
  const ok = await confirmDialog({
    title: `删除规则「${price.model}」`,
    message: '删除后该模型变为「未定价」：调用仍然可用，但不再扣费（日志中额度记 0）。',
    confirmText: '删除',
    danger: true,
  })
  if (!ok) return

  try {
    await deletePrice(price.id)
    toastSuccess('计价规则已删除')
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  }
}

async function runQuote(): Promise<void> {
  if (!quote.value.model.trim()) {
    quoteError.value = '请先填写模型名'
    return
  }
  quoting.value = true
  quoteError.value = ''
  quoteResult.value = null
  try {
    const result = await quotePrice(
      quote.value.model.trim(),
      toNonNegative(quote.value.promptTokens),
      toNonNegative(quote.value.completionTokens),
    )
    quoteResult.value = { quota: result.quota, priced: result.priced }
  } catch (err) {
    quoteError.value = err instanceof ApiError ? err.message : '试算失败'
  } finally {
    quoting.value = false
  }
}

/** 通配规则会与具体规则并存，按"具体 → 笼统"提示优先级 */
function priceRowClass(price: ModelPrice): string {
  if (price.model === '*') return 'text-ink-400'
  return ''
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">计价规则</h2>
        <p class="page-desc">
          价格口径：<strong>输入/输出</strong>为「每 100 万 token 的额度」，
          <strong>每次</strong>为「每调用一次的额度」（图像/视频等生成类能力）。
          支持通配：<code class="chip">gpt-4*</code> 前缀匹配、<code class="chip">*</code> 全局兜底。
          列表按「精确 → 长前缀 → 全局」排序，靠上的规则优先生效。
        </p>
      </div>
      <div class="toolbar">
        <select v-model="groupFilter" class="input min-w-[9rem]" @change="load">
          <option value="">全部分组</option>
          <option v-for="group in groups" :key="group.name" :value="group.name">{{ group.label }}</option>
        </select>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="14" />
          新建规则
        </button>
      </div>
    </div>

    <!-- 费用试算：定价是否合理，直接算一笔比看数字更直观 -->
    <section class="card card-pad mb-5">
      <h3 class="section-title flex items-center gap-2">
        <AppIcon name="quota" :size="16" class="text-brand-700" />
        费用试算
      </h3>
      <div class="mt-3 flex flex-wrap items-end gap-3">
        <div class="min-w-[12rem] flex-1">
          <label class="label" for="quote-model">模型名</label>
          <input id="quote-model" v-model="quote.model" class="input input-mono" placeholder="如 gpt-4o" />
        </div>
        <div class="w-32">
          <label class="label" for="quote-prompt">输入 token</label>
          <input id="quote-prompt" v-model="quote.promptTokens" class="input" type="number" min="0" />
        </div>
        <div class="w-32">
          <label class="label" for="quote-completion">输出 token</label>
          <input id="quote-completion" v-model="quote.completionTokens" class="input" type="number" min="0" />
        </div>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="quoting" @click="runQuote">
          <AppIcon name="play" :size="14" />
          试算
        </button>
      </div>

      <p v-if="quoteError" class="field-error mt-3">{{ quoteError }}</p>
      <p v-else-if="quoteResult" class="mt-3 text-sm text-ink-200">
        应扣额度：
        <strong class="font-mono text-ink-50">{{ formatNumber(quoteResult.quota) }}</strong>
        <span v-if="!quoteResult.priced" class="ml-2 text-xs text-amber-700">
          该模型未定价（或价格为 0），调用不会扣费
        </span>
      </p>
    </section>

    <div class="table-wrap">
      <table class="data-table">
        <thead>
          <tr>
            <th>模型 / 模式</th>
            <th>匹配方式</th>
            <th>分组</th>
            <th class="text-right">输入 / 1M</th>
            <th class="text-right">输出 / 1M</th>
            <th class="text-right">每次</th>
            <th>状态</th>
            <th>备注</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>
        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="!loading && !error && prices.length === 0"
            :colspan="9"
            loading-text="正在读取计价规则…"
            empty-text="还没有配置任何价格"
            empty-hint="未配置价格的模型仍然可以调用，只是不会扣费。点击「新建规则」开始定价。"
            @retry="load"
          />

          <tr v-for="price in prices" :key="price.id">
            <td>
              <code class="font-mono text-[13px] text-ink-100" :class="priceRowClass(price)">{{ price.model }}</code>
            </td>
            <td class="cell-muted">{{ patternHint(price) }}</td>
            <td class="cell-muted">{{ groupLabels[price.group] || price.group }}</td>
            <td class="cell-num">{{ price.prompt_price > 0 ? formatNumber(price.prompt_price) : '—' }}</td>
            <td class="cell-num">{{ price.completion_price > 0 ? formatNumber(price.completion_price) : '—' }}</td>
            <td class="cell-num">{{ price.per_call_price > 0 ? formatNumber(price.per_call_price) : '—' }}</td>
            <td>
              <span class="badge" :class="price.enabled ? 'badge-ok' : 'badge-off'">
                {{ price.enabled ? '启用' : '停用' }}
              </span>
            </td>
            <td class="cell-muted max-w-[14rem]">
              <span class="line-clamp-1">{{ price.remark || '—' }}</span>
            </td>
            <td class="cell-actions">
              <div class="flex items-center justify-end gap-1">
                <button type="button" class="btn-row" title="编辑" @click="openEdit(price)">
                  <AppIcon name="edit" :size="14" />
                </button>
                <button type="button" class="btn-row" title="删除" @click="remove(price)">
                  <AppIcon name="trash" :size="14" />
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <Modal
      :open="formOpen"
      :title="editing ? `编辑规则「${editing.model}」` : '新建计价规则'"
      subtitle="同分组下同一模型只能有一条规则；保存后立即影响后续所有计费。"
      width="max-w-xl"
      :close-on-backdrop="false"
      @close="formOpen = false"
    >
      <div class="space-y-4">
        <div>
          <label class="label" for="price-model">模型名 / 通配模式<span class="text-red-600">*</span></label>
          <input
            id="price-model"
            v-model="form.model"
            class="input input-mono"
            type="text"
            placeholder="gpt-4o 或 gpt-4* 或 *"
          />
          <p class="hint">精确名优先于前缀通配，前缀通配优先于 *（全局兜底）。</p>
        </div>

        <div>
          <label class="label" for="price-group">适用分组</label>
          <select id="price-group" v-model="form.group" class="input">
            <option v-for="group in groups" :key="group.name" :value="group.name">{{ group.label }}</option>
            <option v-if="!groups.length" value="default">默认分组</option>
          </select>
          <p class="hint">不同分组可以有不同价格（这就是「分组」的价值）。</p>
        </div>

        <div class="grid gap-3 sm:grid-cols-3">
          <div>
            <label class="label" for="price-prompt">输入 / 1M token</label>
            <input id="price-prompt" v-model="form.promptPrice" class="input" type="number" min="0" />
          </div>
          <div>
            <label class="label" for="price-completion">输出 / 1M token</label>
            <input id="price-completion" v-model="form.completionPrice" class="input" type="number" min="0" />
          </div>
          <div>
            <label class="label" for="price-percall">每次调用</label>
            <input id="price-percall" v-model="form.perCallPrice" class="input" type="number" min="0" />
          </div>
        </div>
        <p class="hint -mt-2">
          值为 0 表示该口径不计费。对话类只看前两项；异步任务（图像/视频）只看「每次调用」。
        </p>

        <div>
          <label class="label" for="price-remark">备注</label>
          <input id="price-remark" v-model="form.remark" class="input" type="text" placeholder="定价依据，如「按上游官方价 ×1.2」" />
        </div>

        <label class="flex items-center gap-2 text-sm text-ink-200">
          <input v-model="form.enabled" class="checkbox" type="checkbox" />
          启用该规则（停用即视为未定价）
        </label>

        <p v-if="formError" class="field-error">{{ formError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="formOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="saving" @click="submit">
          {{ saving ? '保存中…' : '保存' }}
        </button>
      </template>
    </Modal>
  </div>
</template>
