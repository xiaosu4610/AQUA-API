<script setup lang="ts">
/**
 * 用户门户 · 访问令牌：列表 + 创建（一次性明文展示）+ 改名 + 启停 + 删除。
 *
 * 意图（Why）：
 *   令牌是用户唯一的调用凭据，操作必须以「安全提示明确」为第一优先级：
 *   创建成功后立即弹出一次性密钥弹窗，并在关闭后清空内存中的明文。
 *
 * 流转（Flow）：
 *   进入页面 → listMyTokens() → 表格渲染
 *   创建：Modal(TokenFormFields) → createMyToken() → 拿到 result.key → OneTimeKeyDialog 展示
 *   启停/改名：updateMyToken(id, {...}) → 就地更新列表中的对应行（避免整表刷新闪烁）
 *   删除：confirmDialog 确认 → deleteMyToken() → 重新拉取列表
 *
 * 扩展（Extend）：
 *   新增令牌字段时，改 composables/tokenForm.ts（表单三处同步）即可，本页无需改动表单结构。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import CopyButton from '@/components/CopyButton.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import OneTimeKeyDialog from '@/components/OneTimeKeyDialog.vue'
import Pagination from '@/components/Pagination.vue'
import TokenFormFields from '@/components/TokenFormFields.vue'
import { ApiError } from '@/api/client'
import { createMyToken, deleteMyToken, listMyTokens, updateMyToken } from '@/api/portal'
import { STATUS_DISABLED, STATUS_ENABLED, type AccessToken } from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { emptyTokenForm, toTokenPayload, validateTokenForm, type TokenFormState } from '@/composables/tokenForm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { useSiteStore } from '@/stores/site'
import { statusBadgeClass } from '@/utils/display'
import { formatDateTime, formatExpiry, formatNumber, formatQuota } from '@/utils/format'

const site = useSiteStore()

const tokens = ref<AccessToken[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')

/** 行级操作中的令牌 id，用于禁用该行按钮并显示加载态 */
const rowBusyId = ref<number | null>(null)

/* ── 列表加载 ─────────────────────────────────────────── */

async function loadTokens(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listMyTokens({ page: page.value, size: size.value })
    tokens.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    tokens.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '令牌列表加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(loadTokens)

function changePage(next: number): void {
  page.value = next
  void loadTokens()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void loadTokens()
}

/* ── 创建 ─────────────────────────────────────────────── */

const createOpen = ref(false)
const creating = ref(false)
const createError = ref('')
const form = ref<TokenFormState>(emptyTokenForm())

/** 创建成功后的明文密钥（仅驻留内存，关闭弹窗即清空） */
const createdKey = ref('')
const createdName = ref('')
const keyDialogOpen = ref(false)

function openCreate(): void {
  form.value = emptyTokenForm()
  createError.value = ''
  createOpen.value = true
}

async function submitCreate(): Promise<void> {
  const invalid = validateTokenForm(form.value)
  if (invalid) {
    createError.value = invalid
    return
  }
  creating.value = true
  createError.value = ''
  try {
    const result = await createMyToken(toTokenPayload(form.value))
    createdKey.value = result.key
    createdName.value = result.name
    createOpen.value = false
    keyDialogOpen.value = true
    page.value = 1
    await loadTokens()
  } catch (err) {
    createError.value = err instanceof ApiError ? err.message : '创建失败，请稍后重试'
  } finally {
    creating.value = false
  }
}

/** 关闭一次性密钥弹窗：同时清空明文，降低内存中泄漏风险 */
function closeKeyDialog(): void {
  keyDialogOpen.value = false
  createdKey.value = ''
  createdName.value = ''
  toastSuccess('令牌已创建，请使用已保存的密钥')
}

/* ── 启停 / 改名 / 删除 ───────────────────────────────── */

/** 就地更新列表中的某一行，避免整表重新请求造成闪烁 */
function replaceToken(updated: AccessToken): void {
  const index = tokens.value.findIndex((item) => item.id === updated.id)
  if (index >= 0) tokens.value[index] = { ...tokens.value[index], ...updated }
}

async function toggleStatus(token: AccessToken): Promise<void> {
  const nextStatus = token.status === STATUS_ENABLED ? STATUS_DISABLED : STATUS_ENABLED
  rowBusyId.value = token.id
  try {
    const updated = await updateMyToken(token.id, { status: nextStatus })
    replaceToken(updated)
    toastSuccess(nextStatus === STATUS_ENABLED ? '令牌已启用' : '令牌已停用')
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    rowBusyId.value = null
  }
}

const renameOpen = ref(false)
const renameTarget = ref<AccessToken | null>(null)
const renameValue = ref('')
const renaming = ref(false)
const renameError = ref('')

function openRename(token: AccessToken): void {
  renameTarget.value = token
  renameValue.value = token.name
  renameError.value = ''
  renameOpen.value = true
}

async function submitRename(): Promise<void> {
  if (!renameTarget.value) return
  const name = renameValue.value.trim()
  if (!name) {
    renameError.value = '名称不能为空'
    return
  }
  renaming.value = true
  renameError.value = ''
  try {
    const updated = await updateMyToken(renameTarget.value.id, { name })
    replaceToken(updated)
    renameOpen.value = false
    toastSuccess('名称已更新')
  } catch (err) {
    renameError.value = err instanceof ApiError ? err.message : '更新失败'
  } finally {
    renaming.value = false
  }
}

async function removeToken(token: AccessToken): Promise<void> {
  const ok = await confirmDialog({
    title: '删除访问令牌',
    message: `删除后「${token.name}」将立即失效，使用该令牌的客户端会收到鉴权错误，且无法恢复。`,
    confirmText: '删除令牌',
    danger: true,
  })
  if (!ok) return

  rowBusyId.value = token.id
  try {
    await deleteMyToken(token.id)
    toastSuccess('令牌已删除')
    // 删除后若当前页已空，回退一页，避免停留在空白页
    if (tokens.value.length === 1 && page.value > 1) page.value -= 1
    await loadTokens()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    rowBusyId.value = null
  }
}

/** 空态判断：区分「首次无数据」与「筛选/翻页后无数据」 */
const isEmpty = computed(() => !loading.value && !error.value && tokens.value.length === 0)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">访问令牌</h2>
        <p class="page-desc">
          令牌用于调用 <code class="chip">/v1</code> 接口。明文仅在创建时展示一次，请及时保存。
        </p>
      </div>
      <div class="toolbar">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadTokens">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="15" />
          创建令牌
        </button>
      </div>
    </div>

    <!-- 滑动提示只在桌面端出现：窄屏已切换为卡片视图（.table-cards），不需要左右滑动 -->
    <p class="mb-2 hidden text-xs text-ink-400 lg:block">表格列较多，可左右滑动查看完整内容。</p>

    <div class="table-wrap table-cards">
      <table class="data-table min-w-[1020px]">
        <thead>
          <tr>
            <th>名称</th>
            <th>密钥</th>
            <th>状态</th>
            <th class="text-right">剩余额度</th>
            <th class="text-right">已用额度</th>
            <th>有效期</th>
            <th>最近使用</th>
            <th>创建时间</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>

        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="isEmpty"
            :colspan="9"
            loading-text="正在加载令牌列表…"
            empty-text="还没有访问令牌"
            empty-hint="创建第一个令牌后，就可以用它调用 /v1 接口了。"
            @retry="loadTokens"
          >
            <template #action>
              <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
                <AppIcon name="plus" :size="14" />
                创建令牌
              </button>
            </template>
          </DataState>

          <template v-if="!loading && !error && tokens.length">
            <tr v-for="token in tokens" :key="token.id">
              <td class="font-medium text-ink-100" data-label="名称">{{ token.name }}</td>

              <td data-label="密钥">
                <span class="flex items-center gap-1.5">
                  <code class="chip">{{ token.masked_key }}</code>
                </span>
              </td>

              <td data-label="状态">
                <span :class="statusBadgeClass(token.status)">
                  <span class="dot" />
                  {{ token.status_text || (token.status === STATUS_ENABLED ? '启用' : '停用') }}
                </span>
              </td>

              <td class="cell-num" data-label="剩余额度">{{ formatQuota(token.remain_quota, token.unlimited_quota) }}</td>
              <td class="cell-num text-ink-300" data-label="已用额度">{{ formatNumber(token.used_quota) }}</td>
              <td class="cell-muted whitespace-nowrap" data-label="有效期">{{ formatExpiry(token.expires_at) }}</td>
              <td class="cell-muted whitespace-nowrap" data-label="最近使用">{{ token.last_used_at ? formatDateTime(token.last_used_at) : '从未使用' }}</td>
              <td class="cell-muted whitespace-nowrap" data-label="创建时间">{{ formatDateTime(token.created_at) }}</td>

              <td class="cell-actions" data-label="操作">
                <div class="flex items-center justify-end gap-1">
                  <button
                    type="button"
                    class="btn btn-row"
                    :title="token.status === STATUS_ENABLED ? '停用' : '启用'"
                    :disabled="rowBusyId === token.id"
                    @click="toggleStatus(token)"
                  >
                    <span
                      v-if="rowBusyId === token.id"
                      class="mx-auto block h-3.5 w-3.5 animate-spin rounded-full border-2 border-ink-600 border-t-brand-400"
                    />
                    <AppIcon v-else :name="token.status === STATUS_ENABLED ? 'lock' : 'play'" :size="14" />
                  </button>

                  <button type="button" class="btn btn-row" title="改名" @click="openRename(token)">
                    <AppIcon name="edit" :size="14" />
                  </button>

                  <CopyButton :value="token.masked_key" small class="btn-row" success-text="掩码已复制" />

                  <button
                    type="button"
                    class="btn btn-row text-ink-400 hover:text-red-700"
                    title="删除"
                    :disabled="rowBusyId === token.id"
                    @click="removeToken(token)"
                  >
                    <AppIcon name="trash" :size="14" />
                  </button>
                </div>
              </td>
            </tr>
          </template>
        </tbody>
      </table>

      <Pagination
        v-if="total > 0"
        :page="page"
        :size="size"
        :total="total"
        :disabled="loading"
        @update:page="changePage"
        @update:size="changeSize"
      />
    </div>

    <!-- 创建令牌 -->
    <Modal
      :open="createOpen"
      title="创建访问令牌"
      subtitle="令牌将继承你的账号权限，请按用途分别创建，便于单独停用。"
      width="max-w-xl"
      :close-on-backdrop="false"
      @close="createOpen = false"
    >
      <TokenFormFields v-model="form" :available-models="site.models" />

      <p
        v-if="createError"
        class="mt-4 flex items-start gap-2 rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs leading-relaxed text-red-800"
      >
        <AppIcon name="alert" :size="14" class="mt-0.5 shrink-0" />
        {{ createError }}
      </p>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="creating" @click="createOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="creating" @click="submitCreate">
          <span
            v-if="creating"
            class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
            aria-hidden="true"
          />
          <AppIcon v-else name="plus" :size="16" />
          {{ creating ? '创建中…' : '创建令牌' }}
        </button>
      </template>
    </Modal>

    <!-- 改名 -->
    <Modal
      :open="renameOpen"
      title="重命名令牌"
      subtitle="仅修改展示名称，不影响密钥与调用。"
      width="max-w-md"
      :close-on-backdrop="false"
      @close="renameOpen = false"
    >
      <label class="label" for="rename-token">令牌名称</label>
      <input
        id="rename-token"
        v-model="renameValue"
        class="input"
        type="text"
        maxlength="64"
        @keydown.enter="submitRename"
      />
      <p v-if="renameError" class="field-error">{{ renameError }}</p>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="renaming" @click="renameOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="renaming" @click="submitRename">
          {{ renaming ? '保存中…' : '保存' }}
        </button>
      </template>
    </Modal>

    <!-- 一次性明文密钥 -->
    <OneTimeKeyDialog
      :open="keyDialogOpen"
      :api-key="createdKey"
      :token-name="createdName"
      @close="closeKeyDialog"
    />
  </div>
</template>
