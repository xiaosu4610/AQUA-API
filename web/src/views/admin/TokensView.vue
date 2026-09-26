<script setup lang="ts">
/**
 * 管理后台 · 令牌管理：全部令牌列表 + 为指定用户创建 + 编辑 + 启停 + 删除。
 *
 * 意图（Why）：
 *   管理员需要替用户签发令牌（用户忘记创建或需要代为开通），因此创建时必须先选归属用户；
 *   编辑则覆盖「改名 / 启停 / 调整额度」这些运营常见诉求。
 *
 * 流转（Flow）：
 *   列表：listAllTokens({page,size}) → 表格
 *   创建：打开弹窗时惰性加载用户列表（listUsers，最多 100 条）→ 选择用户 → createTokenForUser
 *         → 成功拿到一次性明文 key → OneTimeKeyDialog
 *   编辑：updateToken(id, {name, status, unlimited_quota, remain_quota})
 *   删除：confirmDialog 确认 → deleteToken
 *
 * 扩展（Extend）：
 *   若用户数超过 100，需要把用户选择改为「远程搜索」（后端需支持 ?keyword=），
 *   届时替换 loadUsers 的实现即可。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import OneTimeKeyDialog from '@/components/OneTimeKeyDialog.vue'
import Pagination from '@/components/Pagination.vue'
import TokenFormFields from '@/components/TokenFormFields.vue'
import { ApiError } from '@/api/client'
import { createTokenForUser, deleteToken, listAllTokens, listUsers, updateToken } from '@/api/admin'
import { STATUS_DISABLED, STATUS_ENABLED, type AccessToken, type AdminUser } from '@/api/types'
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
const busyId = ref<number | null>(null)

async function loadTokens(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listAllTokens({ page: page.value, size: size.value })
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

/* ── 用户列表（供「归属用户」选择）─────────────────────── */

const users = ref<AdminUser[]>([])
const usersLoading = ref(false)
let usersLoaded = false

async function loadUsers(): Promise<void> {
  if (usersLoaded || usersLoading.value) return
  usersLoading.value = true
  try {
    const result = await listUsers({ page: 1, size: 100 })
    users.value = result.items ?? []
    usersLoaded = true
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '用户列表加载失败，无法选择归属用户')
  } finally {
    usersLoading.value = false
  }
}

/* ── 创建 ─────────────────────────────────────────────── */

const createOpen = ref(false)
const creating = ref(false)
const createError = ref('')
const form = ref<TokenFormState>(emptyTokenForm())
const ownerUserId = ref<number | ''>('')

const createdKey = ref('')
const createdName = ref('')
const keyDialogOpen = ref(false)

async function openCreate(): Promise<void> {
  form.value = emptyTokenForm()
  createError.value = ''
  ownerUserId.value = ''
  createOpen.value = true
  void loadUsers()
}

async function submitCreate(): Promise<void> {
  if (ownerUserId.value === '') {
    createError.value = '请选择令牌归属的用户'
    return
  }
  const invalid = validateTokenForm(form.value)
  if (invalid) {
    createError.value = invalid
    return
  }

  creating.value = true
  createError.value = ''
  try {
    const result = await createTokenForUser(toTokenPayload(form.value, Number(ownerUserId.value)))
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

function closeKeyDialog(): void {
  keyDialogOpen.value = false
  createdKey.value = ''
  createdName.value = ''
  toastSuccess('令牌已创建，请使用已保存的密钥')
}

/* ── 编辑 / 启停 / 删除 ───────────────────────────────── */

interface EditForm {
  name: string
  status: number
  unlimited_quota: boolean
  remain_quota: number
}

const editOpen = ref(false)
const editTarget = ref<AccessToken | null>(null)
const editForm = ref<EditForm>({ name: '', status: STATUS_ENABLED, unlimited_quota: true, remain_quota: 0 })
const editing = ref(false)
const editError = ref('')

function openEdit(token: AccessToken): void {
  editTarget.value = token
  editForm.value = {
    name: token.name,
    status: token.status,
    unlimited_quota: token.unlimited_quota,
    remain_quota: token.unlimited_quota ? 0 : token.remain_quota,
  }
  editError.value = ''
  editOpen.value = true
}

async function submitEdit(): Promise<void> {
  if (!editTarget.value) return
  const name = editForm.value.name.trim()
  if (!name) {
    editError.value = '名称不能为空'
    return
  }
  if (!editForm.value.unlimited_quota && editForm.value.remain_quota <= 0) {
    editError.value = '限定额度时必须填写大于 0 的额度'
    return
  }

  editing.value = true
  editError.value = ''
  try {
    await updateToken(editTarget.value.id, {
      name,
      status: editForm.value.status,
      unlimited_quota: editForm.value.unlimited_quota,
      remain_quota: editForm.value.unlimited_quota ? -1 : Number(editForm.value.remain_quota),
    })
    editOpen.value = false
    toastSuccess('令牌已更新')
    await loadTokens()
  } catch (err) {
    editError.value = err instanceof ApiError ? err.message : '更新失败，请稍后重试'
  } finally {
    editing.value = false
  }
}

async function toggleStatus(token: AccessToken): Promise<void> {
  const nextStatus = token.status === STATUS_ENABLED ? STATUS_DISABLED : STATUS_ENABLED
  busyId.value = token.id
  try {
    await updateToken(token.id, { status: nextStatus })
    toastSuccess(nextStatus === STATUS_ENABLED ? '令牌已启用' : '令牌已停用')
    await loadTokens()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    busyId.value = null
  }
}

async function removeToken(token: AccessToken): Promise<void> {
  const ok = await confirmDialog({
    title: '删除访问令牌',
    message: `删除后「${token.name}」将立即失效，使用它的客户端会收到鉴权错误。`,
    confirmText: '删除令牌',
    danger: true,
  })
  if (!ok) return

  busyId.value = token.id
  try {
    await deleteToken(token.id)
    toastSuccess('令牌已删除')
    if (tokens.value.length === 1 && page.value > 1) page.value -= 1
    await loadTokens()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    busyId.value = null
  }
}

/** 归属用户展示：契约未在令牌对象中定义归属字段，这里兼容 user_id/username 两种可能 */
function ownerText(token: AccessToken): string {
  if (token.username) return token.username
  if (token.user_id) return `#${token.user_id}`
  return '—'
}

const isEmpty = computed(() => !loading.value && !error.value && tokens.value.length === 0)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">令牌管理</h2>
        <p class="page-desc">全站访问令牌。可为指定用户签发令牌，或调整额度与状态。</p>
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
      <table class="data-table min-w-[1180px]">
        <thead>
          <tr>
            <th>名称</th>
            <th>密钥</th>
            <th>归属用户</th>
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
            :colspan="10"
            loading-text="正在加载令牌列表…"
            empty-text="还没有任何访问令牌"
            empty-hint="可以为用户签发令牌，或让用户在门户自行创建。"
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
              <td data-label="密钥"><code class="chip">{{ token.masked_key }}</code></td>
              <td class="text-ink-200" data-label="归属用户">{{ ownerText(token) }}</td>

              <td data-label="状态">
                <span :class="statusBadgeClass(token.status)">
                  <span class="dot" />
                  {{ token.status_text || (token.status === STATUS_ENABLED ? '启用' : '停用') }}
                </span>
              </td>

              <td class="cell-num" data-label="剩余额度">{{ formatQuota(token.remain_quota, token.unlimited_quota) }}</td>
              <td class="cell-num text-ink-300" data-label="已用额度">{{ formatNumber(token.used_quota) }}</td>
              <td class="cell-muted whitespace-nowrap" data-label="有效期">{{ formatExpiry(token.expires_at) }}</td>
              <td class="cell-muted whitespace-nowrap" data-label="最近使用">
                {{ token.last_used_at ? formatDateTime(token.last_used_at) : '从未使用' }}
              </td>
              <td class="cell-muted whitespace-nowrap" data-label="创建时间">{{ formatDateTime(token.created_at) }}</td>

              <td class="cell-actions" data-label="操作">
                <div class="flex items-center justify-end gap-1">
                  <button
                    type="button"
                    class="btn btn-row"
                    :title="token.status === STATUS_ENABLED ? '停用' : '启用'"
                    :disabled="busyId === token.id"
                    @click="toggleStatus(token)"
                  >
                    <AppIcon :name="token.status === STATUS_ENABLED ? 'lock' : 'play'" :size="14" />
                  </button>

                  <button type="button" class="btn btn-row" title="编辑" @click="openEdit(token)">
                    <AppIcon name="edit" :size="14" />
                  </button>

                  <button
                    type="button"
                    class="btn btn-row text-ink-400 hover:text-red-700"
                    title="删除"
                    :disabled="busyId === token.id"
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

    <!-- 创建令牌（含归属用户选择） -->
    <Modal
      :open="createOpen"
      title="创建访问令牌"
      subtitle="令牌将归属于所选用户，并占用该用户的账号额度。"
      width="max-w-xl"
      :close-on-backdrop="false"
      @close="createOpen = false"
    >
      <div class="space-y-5">
        <div>
          <label class="label" for="token-owner">归属用户 <span class="text-red-600">*</span></label>
          <select
            id="token-owner"
            v-model="ownerUserId"
            class="input max-w-[20rem]"
            :disabled="usersLoading"
          >
            <option value="">{{ usersLoading ? '正在加载用户…' : '请选择用户' }}</option>
            <option v-for="user in users" :key="user.id" :value="user.id">
              {{ user.username }}（#{{ user.id }}）
            </option>
          </select>
          <p v-if="!usersLoading && !users.length" class="field-error">
            没有可选用户，请先在「用户管理」中创建用户。
          </p>
          <p v-else class="hint">列表最多展示 100 个用户。</p>
        </div>

        <TokenFormFields v-model="form" :available-models="site.models" admin-mode />
      </div>

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

    <!-- 编辑令牌 -->
    <Modal
      :open="editOpen"
      title="编辑令牌"
      subtitle="可修改名称、状态与额度限制；密钥不可更改。"
      width="max-w-xl"
      :close-on-backdrop="false"
      @close="editOpen = false"
    >
      <div class="space-y-5">
        <div v-if="editTarget" class="rounded-lg border border-ink-800 bg-ink-850/50 px-3.5 py-2.5">
          <p class="font-mono text-xs text-ink-300">{{ editTarget.masked_key }}</p>
          <p class="mt-1 text-[11px] text-ink-500">
            归属：{{ ownerText(editTarget) }} · 创建于 {{ formatDateTime(editTarget.created_at) }}
          </p>
        </div>

        <div class="grid gap-5 sm:grid-cols-2">
          <div>
            <label class="label" for="edit-token-name">名称 <span class="text-red-600">*</span></label>
            <input id="edit-token-name" v-model="editForm.name" class="input" type="text" maxlength="64" />
          </div>
          <div>
            <label class="label" for="edit-token-status">状态</label>
            <select id="edit-token-status" v-model.number="editForm.status" class="input">
              <option :value="STATUS_ENABLED">启用</option>
              <option :value="STATUS_DISABLED">停用</option>
            </select>
          </div>
        </div>

        <div>
          <label class="label">额度限制</label>
          <label class="flex cursor-pointer items-center gap-2.5 rounded-lg border border-ink-700 bg-ink-900 px-3 py-2.5">
            <input v-model="editForm.unlimited_quota" class="checkbox" type="checkbox" />
            <span class="text-sm text-ink-100">不限制额度（跟随用户账号额度）</span>
          </label>
          <div v-if="!editForm.unlimited_quota" class="mt-3">
            <label class="label" for="edit-token-quota">令牌可用额度</label>
            <input
              id="edit-token-quota"
              v-model.number="editForm.remain_quota"
              class="input max-w-[14rem] text-right tabular-nums"
              type="number"
              min="1"
            />
          </div>
        </div>

        <p v-if="editError" class="field-error">{{ editError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="editing" @click="editOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="editing" @click="submitEdit">
          {{ editing ? '保存中…' : '保存修改' }}
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
