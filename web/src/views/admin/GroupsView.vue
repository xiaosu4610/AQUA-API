<script setup lang="ts">
/**
 * 管理后台 · 模型分组：维护分组与其计费倍率。
 *
 * 意图（Why）：
 *   分组是运营抓手——渠道归属于分组、计价规则按分组区分，
 *   因此「给不同人群不同的价格与上游」只需改分组配置。
 *   本页要回答管理员三个问题：
 *     1) 现在有哪些分组？各自倍率多少？
 *     2) 这个分组被多少渠道/价格使用？（决定能不能删）
 *     3) 改倍率会立刻生效吗？（会——后端改完即清计费缓存）
 *
 * 流转（Flow）：
 *   进入页面 → listGroups() → 表格（含引用统计）
 *   新建/编辑 → Modal 表单 → createGroup / updateGroup → 重新加载
 *   删除 → 二次确认 → deleteGroup（被引用或默认分组会被后端拒绝）
 *
 * 扩展（Extend）：
 *   新增分组属性（如"仅管理员可见"）：在 types.ts 的 ModelGroup 加字段，
 *   在本页表格与表单各补一处。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import { ApiError } from '@/api/client'
import { createGroup, deleteGroup, listGroups, updateGroup } from '@/api/admin'
import type { ModelGroup } from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatDateTime } from '@/utils/format'

const groups = ref<ModelGroup[]>([])
const loading = ref(true)
const error = ref('')

/** 表单弹窗状态：editing 为 null 表示新建 */
const formOpen = ref(false)
const editing = ref<ModelGroup | null>(null)
const saving = ref(false)
const formError = ref('')

const form = ref({
  name: '',
  display_name: '',
  /** 用百分比整数：100 = 1.0 倍；界面用字符串承载，提交前转数字 */
  ratioText: '100',
  description: '',
  enabled: true,
})

/** 倍率预览：把百分比换算成人话，避免管理员填错单位 */
const ratioPreview = computed(() => {
  const ratio = Number(form.value.ratioText)
  if (!Number.isFinite(ratio) || ratio <= 0) return '请输入大于 0 的数字'
  return `实际扣费 = 基础额度 × ${(ratio / 100).toFixed(2)}`
})

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listGroups()
    groups.value = result.items ?? []
  } catch (err) {
    groups.value = []
    error.value = err instanceof ApiError ? err.message : '分组加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(load)

/** 打开新建表单 */
function openCreate(): void {
  editing.value = null
  formError.value = ''
  form.value = { name: '', display_name: '', ratioText: '100', description: '', enabled: true }
  formOpen.value = true
}

/** 打开编辑表单 */
function openEdit(group: ModelGroup): void {
  editing.value = group
  formError.value = ''
  form.value = {
    name: group.name,
    display_name: group.display_name,
    ratioText: String(group.ratio),
    description: group.description,
    enabled: group.enabled,
  }
  formOpen.value = true
}

async function submit(): Promise<void> {
  const ratio = Number(form.value.ratioText)
  if (!Number.isFinite(ratio) || ratio <= 0) {
    formError.value = '计费倍率必须是大于 0 的数字（100 表示 1.0 倍）'
    return
  }
  if (!editing.value && !form.value.name.trim()) {
    formError.value = '分组标识不能为空'
    return
  }

  saving.value = true
  formError.value = ''
  try {
    if (editing.value) {
      await updateGroup(editing.value.id, {
        display_name: form.value.display_name.trim(),
        ratio: Math.round(ratio),
        description: form.value.description.trim(),
        enabled: form.value.enabled,
      })
      toastSuccess(`分组「${editing.value.label}」已更新，新倍率立即生效`)
    } else {
      const created = await createGroup({
        name: form.value.name.trim().toLowerCase(),
        display_name: form.value.display_name.trim(),
        ratio: Math.round(ratio),
        description: form.value.description.trim(),
        enabled: form.value.enabled,
      })
      toastSuccess(`分组「${created.label}」已创建`)
    }
    formOpen.value = false
    await load()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

async function remove(group: ModelGroup): Promise<void> {
  const used = group.channel_count + group.price_count > 0
  const ok = await confirmDialog({
    title: `删除分组「${group.label}」`,
    message: used
      ? `该分组正被 ${group.channel_count} 个渠道与 ${group.price_count} 条计价规则使用，后端会拒绝删除。请先调整这些配置的分组。`
      : '删除后无法恢复。若仍有渠道或计价规则引用该分组名，它们将失去分组归属。',
    confirmText: '删除',
    danger: true,
  })
  if (!ok) return

  try {
    await deleteGroup(group.id)
    toastSuccess('分组已删除')
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  }
}

/** 倍率徽标：1.0 倍用中性色，非 1.0 倍用强调色（异常定价需要被一眼看到） */
function ratioBadgeClass(ratio: number): string {
  return ratio === 100 ? 'badge badge-off' : 'badge badge-warn'
}

function ratioText(ratio: number): string {
  return `${(ratio / 100).toFixed(2)}x`
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">模型分组</h2>
        <p class="page-desc">
          渠道与计价规则都归属于分组。为分组设置倍率即可实现差异化定价：
          实际扣费 = 基础额度 × 倍率（例如 150 表示 1.5 倍）。倍率修改后立即生效。
        </p>
      </div>
      <div class="toolbar">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="14" />
          新建分组
        </button>
      </div>
    </div>

    <div class="table-wrap">
      <table class="data-table">
        <thead>
          <tr>
            <th>分组</th>
            <th>倍率</th>
            <th>说明</th>
            <th>引用情况</th>
            <th>状态</th>
            <th>更新时间</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>
        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="!loading && !error && groups.length === 0"
            :colspan="7"
            loading-text="正在读取分组…"
            empty-text="还没有任何分组"
            empty-hint="默认分组由系统初始化。点击「新建分组」可以创建面向不同人群的分组。"
            @retry="load"
          />

          <tr v-for="group in groups" :key="group.id">
            <td>
              <div class="flex items-center gap-2">
                <span class="font-medium text-ink-100">{{ group.label }}</span>
                <code class="chip">{{ group.name }}</code>
              </div>
            </td>
            <td>
              <span :class="ratioBadgeClass(group.ratio)">{{ ratioText(group.ratio) }}</span>
            </td>
            <td class="cell-muted max-w-[16rem]">
              <span class="line-clamp-2">{{ group.description || '—' }}</span>
            </td>
            <td class="cell-muted">
              {{ group.channel_count }} 渠道 / {{ group.price_count }} 规则
            </td>
            <td>
              <span class="badge" :class="group.enabled ? 'badge-ok' : 'badge-off'">
                {{ group.enabled ? '启用' : '停用' }}
              </span>
            </td>
            <td class="cell-muted">{{ formatDateTime(group.updated_at) }}</td>
            <td class="cell-actions">
              <div class="flex items-center justify-end gap-1">
                <button type="button" class="btn-row" title="编辑" @click="openEdit(group)">
                  <AppIcon name="edit" :size="14" />
                </button>
                <button
                  type="button"
                  class="btn-row"
                  :disabled="group.name === 'default'"
                  :title="group.name === 'default' ? '默认分组不可删除' : '删除'"
                  @click="remove(group)"
                >
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
      :title="editing ? `编辑分组「${editing.label}」` : '新建分组'"
      :subtitle="
        editing
          ? '分组标识不可修改（渠道与计价规则通过它关联）。倍率修改后立即生效。'
          : '标识用于关联渠道与计价规则，创建后不可修改，请使用小写字母。'
      "
      width="max-w-xl"
      :close-on-backdrop="false"
      @close="formOpen = false"
    >
      <div class="space-y-4">
        <div>
          <label class="label" for="group-name">分组标识<span class="text-red-600">*</span></label>
          <input
            id="group-name"
            v-model="form.name"
            class="input input-mono"
            type="text"
            :disabled="Boolean(editing)"
            placeholder="如 vip、internal、trial"
          />
          <p class="hint">
            只允许小写字母，不能包含空格、逗号或斜杠。渠道与计价规则填的就是这个值。
          </p>
        </div>

        <div>
          <label class="label" for="group-display">展示名</label>
          <input
            id="group-display"
            v-model="form.display_name"
            class="input"
            type="text"
            placeholder="如 VIP 用户（留空则显示标识）"
          />
        </div>

        <div>
          <label class="label" for="group-ratio">计费倍率（百分比）</label>
          <input id="group-ratio" v-model="form.ratioText" class="input" type="number" min="1" step="1" />
          <p class="hint">{{ ratioPreview }}（倍率只影响扣费，不改变上游实际用量）</p>
        </div>

        <div>
          <label class="label" for="group-desc">说明</label>
          <textarea
            id="group-desc"
            v-model="form.description"
            class="input"
            rows="2"
            placeholder="这个分组面向谁？为什么这么定价？"
          />
        </div>

        <label class="flex items-center gap-2 text-sm text-ink-200">
          <input v-model="form.enabled" class="checkbox" type="checkbox" />
          启用该分组
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
