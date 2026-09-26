<script setup lang="ts">
/**
 * 模型广场主体（公开页与控制台内嵌页共用）。
 *
 * 意图（Why）：
 *   模型数量会到上百个，用户来找模型时的问题只有三个：「有没有」「多贵」「怎么调」。
 *   因此把广场做成「左侧栏目 + 工具条 + 结果区」三段式：
 *     · 左侧栏目：分组 / 厂商 / 状态三个维度，全部就地切换（不跳页、不发请求）；
 *     · 工具条：搜索、排序、卡片或列表视图 —— 视图只影响呈现，不影响数据；
 *     · 结果区：卡片（好看、信息全）或列表（紧凑、一屏扫读更多）。
 *   点卡片就地弹详情，不再把人导到另一个页面。
 *
 *   为什么一次拉全量、在本地做筛选：
 *   分面计数（"这个分组下有几个"）必须基于完整数据集才准确，
 *   而且本地筛选切换是瞬时的、没有网络等待 —— 这正是"操作简单"的来源。
 *   模型清单的字段很少，一次性传输的成本远低于每次点击都往返一次。
 *
 *   文案走词条（components.plaza.*）；模型名一律 dir="ltr"。
 *
 * 流转（Flow）：
 *   挂载 → fetchModelPlaza()（不带参数，取全量）
 *   → base（关键词过滤）→ matchedExcept(维度)（分面计数）→ visible（最终结果）
 *   → 点击卡片 → selected → ModelDetailModal
 *
 * 扩展（Extend）：
 *   新增筛选维度：加一个 ref + 在 matchedExcept 里加一条分支 + 在左栏加一段；
 *   新增排序方式：在 SORT_OPTIONS 里加一项并在 sortModels 中处理（同时补词条键）。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import AppIcon from './AppIcon.vue'
import DataState from './DataState.vue'
import ModelCard from './ModelCard.vue'
import ModelDetailModal from './ModelDetailModal.vue'
import { ApiError } from '@/api/client'
import { fetchModelPlaza } from '@/api/site'
import type { PlazaModel } from '@/api/types'
import { toastSuccess } from '@/composables/useToast'
import { copyText } from '@/utils/clipboard'
import { formatNumber } from '@/utils/format'
import { vendorInitial, vendorLabel, vendorOf, vendorTone } from '@/utils/vendor'

/** 状态筛选维度 */
type StatusFilter = 'all' | 'available' | 'unavailable'
/** 排序维度 */
type SortKey = 'default' | 'name' | 'vendor'
/** 视图形态 */
type ViewMode = 'grid' | 'list'
/** 分面计数时需要跳过的维度（算 A 维度的计数时，不该被 A 的当前选择影响） */
type SkipFacet = 'none' | 'group' | 'vendor' | 'status'

const { t } = useI18n()

/** 排序选项（computed：文案随语言切换重算） */
const SORT_OPTIONS = computed<{ key: SortKey; label: string }[]>(() => [
  { key: 'default', label: t('components.plaza.sort.default') },
  { key: 'name', label: t('components.plaza.sort.name') },
  { key: 'vendor', label: t('components.plaza.sort.vendor') },
])

/** 状态筛选项 */
const STATUS_OPTIONS = computed<{ key: StatusFilter; label: string }[]>(() => [
  { key: 'all', label: t('components.plaza.status.all') },
  { key: 'available', label: t('components.plaza.status.available') },
  { key: 'unavailable', label: t('components.plaza.status.unavailable') },
])

const models = ref<PlazaModel[]>([])
const groups = ref<{ name: string; label: string; ratio: number; description: string }[]>([])
const loading = ref(true)
const error = ref('')

/* ── 筛选与呈现状态 ───────────────────────────────────── */

const activeGroup = ref('')
const activeVendor = ref('')
const activeStatus = ref<StatusFilter>('all')
const keyword = ref('')
const sortKey = ref<SortKey>('default')
const view = ref<ViewMode>('grid')
/** 窄屏下的筛选面板开关（桌面端左栏常驻，不需要开关） */
const railOpen = ref(false)

const detailOpen = ref(false)
const selected = ref<PlazaModel | null>(null)

/** 分组名 → 展示名 */
const groupLabels = computed(() => {
  const map: Record<string, string> = {}
  for (const group of groups.value) map[group.name] = group.label
  return map
})

/** 对外 base_url：用浏览器地址而非硬编码域名，任何部署环境复制出来都能直接用 */
const baseUrl = computed(() => `${window.location.origin}/v1`)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const plaza = await fetchModelPlaza()
    models.value = plaza.items ?? []
    groups.value = plaza.groups ?? []
  } catch (err) {
    models.value = []
    groups.value = []
    error.value = err instanceof ApiError ? err.message : t('components.plaza.loadFailed')
  } finally {
    loading.value = false
  }
}

onMounted(load)

/* ── 筛选计算 ─────────────────────────────────────────── */

/** 关键词过滤后的基础集合（关键词与所有分面都独立） */
const base = computed(() => {
  const needle = keyword.value.trim().toLowerCase()
  if (!needle) return models.value
  return models.value.filter((item) => item.model.toLowerCase().includes(needle))
})

/**
 * 计算某个维度的分面计数时使用的集合：跳过该维度自身的选择，
 * 其余条件全部生效。这样"当前选中项之外的计数"才是用户真正需要的信息
 * ——否则选中一个分组后，其它分组的计数会全部变成 0，毫无参考价值。
 */
function matchedExcept(skip: SkipFacet): PlazaModel[] {
  return base.value.filter((item) => {
    if (skip !== 'group' && activeGroup.value && !item.groups.includes(activeGroup.value)) return false
    if (skip !== 'vendor' && activeVendor.value && vendorOf(item.model) !== activeVendor.value) return false
    if (skip !== 'status' && activeStatus.value !== 'all') {
      const wantAvailable = activeStatus.value === 'available'
      if (item.available !== wantAvailable) return false
    }
    return true
  })
}

/** 分组分面：一个模型可能同属多个分组，因此按归属逐个计数 */
const groupCounts = computed(() => {
  const counts: Record<string, number> = {}
  for (const item of matchedExcept('group')) {
    for (const name of item.groups) counts[name] = (counts[name] ?? 0) + 1
  }
  return counts
})

/** 厂商分面 */
const vendorCounts = computed(() => {
  const counts: Record<string, number> = {}
  for (const item of matchedExcept('vendor')) {
    const vendor = vendorOf(item.model)
    counts[vendor] = (counts[vendor] ?? 0) + 1
  }
  return counts
})

/** 厂商列表：数量多的排前面，数量相同按名称，便于快速找到主力厂商 */
const vendors = computed(() =>
  Object.entries(vendorCounts.value)
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([name, count]) => ({ name, count })),
)

/** 状态分面 */
const statusCounts = computed(() => {
  const list = matchedExcept('status')
  const available = list.filter((item) => item.available).length
  return { all: list.length, available, unavailable: list.length - available }
})

/** 排序：保持稳定（同键时回退到名称），避免列表在刷新后乱序 */
function sortModels(list: PlazaModel[]): PlazaModel[] {
  const sorted = [...list]
  const byName = (a: PlazaModel, b: PlazaModel) => a.model.localeCompare(b.model)
  if (sortKey.value === 'name') return sorted.sort(byName)
  if (sortKey.value === 'vendor') {
    return sorted.sort((a, b) => vendorOf(a.model).localeCompare(vendorOf(b.model)) || byName(a, b))
  }
  // 推荐排序：可用的在前（用户最关心"现在能用什么"），其余按名称
  return sorted.sort((a, b) => Number(b.available) - Number(a.available) || byName(a, b))
}

/** 最终展示的模型列表 */
const visible = computed(() => sortModels(matchedExcept('none')))

/** 是否有任何筛选条件生效（决定是否显示"清除筛选"） */
const hasFilter = computed(
  () => Boolean(activeGroup.value || activeVendor.value || keyword.value) || activeStatus.value !== 'all',
)

/** 全部模型的可用数（不受筛选影响，用于页头统计） */
const totalAvailable = computed(() => models.value.filter((item) => item.available).length)

function selectGroup(name: string): void {
  activeGroup.value = activeGroup.value === name ? '' : name
}

function selectVendor(name: string): void {
  activeVendor.value = activeVendor.value === name ? '' : name
}

function selectStatus(key: StatusFilter): void {
  activeStatus.value = key
}

function clearFilters(): void {
  activeGroup.value = ''
  activeVendor.value = ''
  activeStatus.value = 'all'
  keyword.value = ''
}

/* ── 行内展示辅助 ─────────────────────────────────────── */

/** 列表视图的价格摘要：按次计费优先展示，否则取第一个分组的价格 */
function priceSummary(model: PlazaModel): string {
  if (!model.prices.length) return t('components.plaza.unpriced')
  const perCall = model.prices.find((price) => price.per_call_price > 0)
  if (perCall) return `${formatNumber(perCall.per_call_price)} ${t('components.modelCard.perCall')}`
  const first = model.prices[0]
  return `${formatNumber(first.prompt_price)} / ${formatNumber(first.completion_price)}`
}

/** 列表视图的价格口径说明（列头用，避免用户误读单位） */
const priceCaption = computed(() =>
  visible.value.some((item) => item.prices.some((price) => price.per_call_price > 0))
    ? t('components.plaza.priceCaptionWithCall')
    : t('components.plaza.priceCaption'),
)

function openDetail(model: PlazaModel): void {
  selected.value = model
  detailOpen.value = true
}

async function handleCopy(value: string): Promise<void> {
  const ok = await copyText(value)
  if (ok) toastSuccess(t('components.plaza.copied', { model: value }))
}

defineExpose({ reload: load })
</script>

<template>
  <div>
    <!-- 窄屏筛选开关：桌面端左栏常驻，无需开关 -->
    <div class="mb-3 flex items-center gap-2 lg:hidden">
      <button type="button" class="btn btn-secondary btn-sm" @click="railOpen = !railOpen">
        <AppIcon name="filter" :size="14" />
        {{ railOpen ? t('components.plaza.collapseFilter') : t('components.plaza.filter') }}
      </button>
      <span v-if="hasFilter" class="text-xs text-ink-400">
        {{ t('components.plaza.filteredCount', { count: visible.length }) }}
      </span>
    </div>

    <div class="grid gap-5 lg:grid-cols-[250px_minmax(0,1fr)]">
      <!-- ── 左侧栏目 ─────────────────────────────────── -->
      <aside
        class="card h-fit lg:sticky lg:top-20 lg:max-h-[calc(100vh-6rem)] lg:overflow-y-auto"
        :class="railOpen ? 'block' : 'hidden lg:block'"
      >
        <div class="border-b border-ink-800/60 px-3 py-3">
          <p class="px-1 text-xs text-ink-400">{{ t('components.plaza.totalModels', { total: models.length }) }}</p>
          <p class="mt-1 flex items-center gap-1.5 px-1 text-sm font-semibold text-ink-50">
            <span class="dot bg-emerald-500" />
            {{ t('components.plaza.availableCount', { count: totalAvailable }) }}
          </p>
        </div>

        <nav class="px-2 pb-3">
          <!-- 分组 -->
          <p class="rail-title">
            <AppIcon name="tag" :size="12" />
            {{ t('components.plaza.modelGroups') }}
          </p>
          <button
            type="button"
            class="rail-item"
            :class="activeGroup === '' ? 'rail-item-active' : ''"
            @click="selectGroup('')"
          >
            <AppIcon name="layers" :size="15" />
            {{ t('components.plaza.allGroups') }}
            <span class="rail-count">{{ statusCounts.all }}</span>
          </button>
          <button
            v-for="group in groups"
            :key="group.name"
            type="button"
            class="rail-item"
            :class="activeGroup === group.name ? 'rail-item-active' : ''"
            :title="group.description || group.name"
            @click="selectGroup(group.name)"
          >
            <AppIcon name="tag" :size="15" />
            <span class="min-w-0 flex-1 truncate">{{ group.label }}</span>
            <span class="rail-count">{{ groupCounts[group.name] ?? 0 }}</span>
          </button>

          <!-- 厂商 -->
          <p class="rail-title">
            <AppIcon name="server" :size="12" />
            {{ t('components.plaza.upstreamVendors') }}
          </p>
          <button
            type="button"
            class="rail-item"
            :class="activeVendor === '' ? 'rail-item-active' : ''"
            @click="selectVendor('')"
          >
            <AppIcon name="layers" :size="15" />
            {{ t('components.plaza.allVendors') }}
            <span class="rail-count">{{ vendors.reduce((sum, item) => sum + item.count, 0) }}</span>
          </button>
          <button
            v-for="vendor in vendors"
            :key="vendor.name"
            type="button"
            class="rail-item"
            :class="activeVendor === vendor.name ? 'rail-item-active' : ''"
            @click="selectVendor(vendor.name)"
          >
            <span
              class="vendor-avatar h-6 w-6 rounded-lg text-[11px]"
              :class="vendorTone(vendor.name)"
              aria-hidden="true"
            >
              {{ vendorInitial(vendor.name) }}
            </span>
            <span class="min-w-0 flex-1 truncate">{{ vendorLabel(vendor.name) }}</span>
            <span class="rail-count">{{ vendor.count }}</span>
          </button>

          <!-- 状态 -->
          <p class="rail-title">
            <AppIcon name="bolt" :size="12" />
            {{ t('components.plaza.availability') }}
          </p>
          <button
            v-for="option in STATUS_OPTIONS"
            :key="option.key"
            type="button"
            class="rail-item"
            :class="activeStatus === option.key ? 'rail-item-active' : ''"
            @click="selectStatus(option.key)"
          >
            <AppIcon :name="option.key === 'unavailable' ? 'alert' : 'check'" :size="15" />
            {{ option.label }}
            <span class="rail-count">
              {{ option.key === 'all' ? statusCounts.all : option.key === 'available' ? statusCounts.available : statusCounts.unavailable }}
            </span>
          </button>
        </nav>
      </aside>

      <!-- ── 工具条 + 结果区 ─────────────────────────── -->
      <section class="min-w-0">
        <div class="mb-4 flex flex-wrap items-center gap-2">
          <div class="relative min-w-[200px] flex-1">
            <AppIcon
              name="search"
              :size="15"
              class="pointer-events-none absolute start-3 top-1/2 -translate-y-1/2 text-ink-500"
            />
            <input
              v-model="keyword"
              class="input ps-9"
              type="search"
              :placeholder="t('components.plaza.searchPlaceholder')"
              :aria-label="t('components.plaza.searchAria')"
            />
          </div>

          <select v-model="sortKey" class="input w-auto" :aria-label="t('components.plaza.sortLabel')">
            <option v-for="option in SORT_OPTIONS" :key="option.key" :value="option.key">
              {{ option.label }}
            </option>
          </select>

          <div class="seg" role="group" :aria-label="t('components.plaza.viewSwitch')">
            <button
              type="button"
              class="seg-item"
              :class="view === 'grid' ? 'seg-item-active' : ''"
              :title="t('components.plaza.viewGridTitle')"
              @click="view = 'grid'"
            >
              <AppIcon name="grid" :size="15" />
              {{ t('components.plaza.viewGrid') }}
            </button>
            <button
              type="button"
              class="seg-item"
              :class="view === 'list' ? 'seg-item-active' : ''"
              :title="t('components.plaza.viewListTitle')"
              @click="view = 'list'"
            >
              <AppIcon name="list" :size="15" />
              {{ t('components.plaza.viewList') }}
            </button>
          </div>

          <button
            type="button"
            class="btn btn-secondary btn-sm"
            :disabled="loading"
            :title="t('components.plaza.refreshTitle')"
            @click="load"
          >
            <AppIcon name="refresh" :size="14" />
          </button>
        </div>

        <!-- 结果计数 + 清除筛选 -->
        <div v-if="!loading && !error" class="mb-3 flex flex-wrap items-center gap-2 text-xs text-ink-400">
          <span>
            {{ t('components.plaza.showing', { visible: visible.length, total: models.length }) }}
          </span>
          <template v-if="hasFilter">
            <span class="text-ink-600">·</span>
            <button type="button" class="btn btn-ghost btn-sm" @click="clearFilters">
              <AppIcon name="close" :size="13" />
              {{ t('components.plaza.clearFilter') }}
            </button>
          </template>
        </div>

        <DataState
          :loading="loading"
          :error="error"
          :empty="!loading && !error && visible.length === 0"
          :loading-text="t('components.plaza.loading')"
          :empty-text="t('components.plaza.empty')"
          :empty-hint="t('components.plaza.emptyHint')"
          @retry="load"
        >
          <template #action>
            <button v-if="hasFilter" type="button" class="btn btn-secondary btn-sm" @click="clearFilters">
              {{ t('components.plaza.clearAllFilter') }}
            </button>
          </template>
        </DataState>

        <!-- 卡片视图 -->
        <div v-if="!loading && !error && visible.length && view === 'grid'" class="grid gap-4 sm:grid-cols-2 2xl:grid-cols-3">
          <ModelCard
            v-for="item in visible"
            :key="item.model"
            :model="item"
            :group-labels="groupLabels"
            @copy="handleCopy"
            @select="openDetail"
          />
        </div>

        <!-- 列表视图：模型很多时一屏能扫读更多 -->
        <div v-if="!loading && !error && visible.length && view === 'list'" class="table-wrap">
          <div
            class="grid grid-cols-[minmax(0,1fr)_120px_140px_92px] gap-3 border-b border-ink-800/70 bg-ink-850/70 px-4 py-2.5 text-xs font-medium text-ink-300"
          >
            <span>{{ t('components.plaza.col.model') }}</span>
            <span>{{ t('components.plaza.col.status') }}</span>
            <span class="text-right">{{ priceCaption }}</span>
            <span class="text-right">{{ t('components.plaza.col.action') }}</span>
          </div>

          <button
            v-for="item in visible"
            :key="item.model"
            type="button"
            class="plaza-row grid grid-cols-[minmax(0,1fr)_120px_140px_92px] border-b border-ink-800/50 last:border-b-0"
            @click="openDetail(item)"
          >
            <span class="flex min-w-0 items-center gap-2.5">
              <span
                class="vendor-avatar h-7 w-7 rounded-lg text-[11px]"
                :class="vendorTone(vendorOf(item.model))"
                aria-hidden="true"
              >
                {{ vendorInitial(vendorOf(item.model)) }}
              </span>
              <span class="min-w-0">
                <span class="block truncate font-mono text-[13px] font-medium text-ink-50" dir="ltr">{{ item.model }}</span>
                <span class="mt-0.5 block truncate text-[11px] text-ink-500">
                  {{ item.groups.map((name) => groupLabels[name] || name).join(' · ') || t('components.plaza.ungrouped') }}
                </span>
              </span>
            </span>

            <span>
              <span class="badge" :class="item.available ? 'badge-ok' : 'badge-off'">
                <span class="dot" />
                {{ item.available ? t('components.modelCard.available') : t('components.modelCard.unavailable') }}
              </span>
            </span>

            <span class="text-right font-mono text-[13px] text-ink-200">{{ priceSummary(item) }}</span>

            <span class="flex items-center justify-end gap-1 text-brand-700">
              <span class="text-xs font-medium">{{ t('components.plaza.detail') }}</span>
              <AppIcon name="chevron-right" :size="13" class="rtl-flip" />
            </span>
          </button>
        </div>
      </section>
    </div>

    <ModelDetailModal
      :open="detailOpen"
      :model="selected"
      :group-labels="groupLabels"
      :base-url="baseUrl"
      @close="detailOpen = false"
    />
  </div>
</template>
