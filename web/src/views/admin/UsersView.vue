<script setup lang="ts">
/**
 * 管理后台 · 用户管理：列表 + 新建 + 编辑 + 调整额度 + 启停 + 删除。
 *
 * 意图（Why）：
 *   「额度」是运营最常调整的字段，如果混在通用编辑里容易误改其它字段；
 *   因此把「调整额度」做成独立入口（独立弹窗），降低误操作风险。
 *
 * 流转（Flow）：
 *   列表：listUsers({page,size}) → 表格
 *   新建：createUser({username,password,email,role,quota,status})
 *   编辑：updateUser(id, {email,role,status})
 *   调额：updateUser(id, {quota})
 *   启停：updateUser(id, {status})
 *   删除：confirmDialog 确认 → deleteUser(id)
 *
 * 扩展（Extend）：
 *   若后端支持管理员重置密码，在编辑弹窗中追加「重置密码」字段并一并提交（当前契约未定义）。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import Pagination from '@/components/Pagination.vue'
import { ApiError } from '@/api/client'
import { createUser, deleteUser, listUsers, updateUser } from '@/api/admin'
import { ROLE_ADMIN, ROLE_USER, STATUS_DISABLED, STATUS_ENABLED, type AdminUser } from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { roleBadgeClass, roleLabel, statusBadgeClass } from '@/utils/display'
import { formatDateTime, formatNumber } from '@/utils/format'

const users = ref<AdminUser[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const loading = ref(true)
const error = ref('')
const busyId = ref<number | null>(null)

async function loadUsers(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listUsers({ page: page.value, size: size.value })
    users.value = result.items ?? []
    total.value = result.total ?? 0
  } catch (err) {
    users.value = []
    total.value = 0
    error.value = err instanceof ApiError ? err.message : '用户列表加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(loadUsers)

function changePage(next: number): void {
  page.value = next
  void loadUsers()
}

function changeSize(next: number): void {
  size.value = next
  page.value = 1
  void loadUsers()
}

/* ── 新建 ─────────────────────────────────────────────── */

interface CreateForm {
  username: string
  password: string
  email: string
  role: number
  quota: number
  status: number
}

const createOpen = ref(false)
const creating = ref(false)
const createError = ref('')
const createForm = ref<CreateForm>({
  username: '',
  password: '',
  email: '',
  role: ROLE_USER,
  quota: 0,
  status: STATUS_ENABLED,
})

function openCreate(): void {
  createForm.value = { username: '', password: '', email: '', role: ROLE_USER, quota: 0, status: STATUS_ENABLED }
  createError.value = ''
  createOpen.value = true
}

async function submitCreate(): Promise<void> {
  const username = createForm.value.username.trim()
  if (!username) {
    createError.value = '请填写用户名'
    return
  }
  if (!createForm.value.password) {
    createError.value = '请填写初始密码'
    return
  }

  creating.value = true
  createError.value = ''
  try {
    await createUser({
      username,
      password: createForm.value.password,
      email: createForm.value.email.trim() || undefined,
      role: createForm.value.role,
      quota: Number(createForm.value.quota) || 0,
      status: createForm.value.status,
    })
    createOpen.value = false
    toastSuccess('用户已创建')
    page.value = 1
    await loadUsers()
  } catch (err) {
    createError.value = err instanceof ApiError ? err.message : '创建失败，请稍后重试'
  } finally {
    creating.value = false
  }
}

/* ── 编辑 ─────────────────────────────────────────────── */

interface EditForm {
  email: string
  role: number
  status: number
}

const editOpen = ref(false)
const editTarget = ref<AdminUser | null>(null)
const editForm = ref<EditForm>({ email: '', role: ROLE_USER, status: STATUS_ENABLED })
const editing = ref(false)
const editError = ref('')

function openEdit(user: AdminUser): void {
  editTarget.value = user
  editForm.value = { email: user.email || '', role: user.role, status: user.status }
  editError.value = ''
  editOpen.value = true
}

async function submitEdit(): Promise<void> {
  if (!editTarget.value) return
  editing.value = true
  editError.value = ''
  try {
    await updateUser(editTarget.value.id, {
      email: editForm.value.email.trim(),
      role: editForm.value.role,
      status: editForm.value.status,
    })
    editOpen.value = false
    toastSuccess('用户信息已更新')
    await loadUsers()
  } catch (err) {
    editError.value = err instanceof ApiError ? err.message : '更新失败，请稍后重试'
  } finally {
    editing.value = false
  }
}

/* ── 调整额度 ─────────────────────────────────────────── */

const quotaOpen = ref(false)
const quotaTarget = ref<AdminUser | null>(null)
const quotaValue = ref(0)
const quotaSaving = ref(false)
const quotaError = ref('')

function openQuota(user: AdminUser): void {
  quotaTarget.value = user
  quotaValue.value = user.quota
  quotaError.value = ''
  quotaOpen.value = true
}

async function submitQuota(): Promise<void> {
  if (!quotaTarget.value) return
  if (!Number.isFinite(quotaValue.value) || quotaValue.value < 0) {
    quotaError.value = '额度必须是不小于 0 的数值'
    return
  }
  quotaSaving.value = true
  quotaError.value = ''
  try {
    await updateUser(quotaTarget.value.id, { quota: Number(quotaValue.value) })
    quotaOpen.value = false
    toastSuccess('额度已更新')
    await loadUsers()
  } catch (err) {
    quotaError.value = err instanceof ApiError ? err.message : '额度更新失败'
  } finally {
    quotaSaving.value = false
  }
}

/* ── 启停 / 删除 ──────────────────────────────────────── */

async function toggleStatus(user: AdminUser): Promise<void> {
  const nextStatus = user.status === STATUS_ENABLED ? STATUS_DISABLED : STATUS_ENABLED
  busyId.value = user.id
  try {
    await updateUser(user.id, { status: nextStatus })
    toastSuccess(nextStatus === STATUS_ENABLED ? '用户已启用' : '用户已禁用')
    await loadUsers()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    busyId.value = null
  }
}

async function removeUser(user: AdminUser): Promise<void> {
  const ok = await confirmDialog({
    title: '删除用户',
    message: `删除「${user.username}」后，该用户将无法登录，其名下的访问令牌也会失效，操作不可恢复。`,
    confirmText: '删除用户',
    danger: true,
  })
  if (!ok) return

  busyId.value = user.id
  try {
    await deleteUser(user.id)
    toastSuccess('用户已删除')
    if (users.value.length === 1 && page.value > 1) page.value -= 1
    await loadUsers()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  } finally {
    busyId.value = null
  }
}

const isEmpty = computed(() => !loading.value && !error.value && users.value.length === 0)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">用户管理</h2>
        <p class="page-desc">账号、角色与额度。角色为管理员时可访问本后台。</p>
      </div>
      <div class="toolbar">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadUsers">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="15" />
          新建用户
        </button>
      </div>
    </div>

    <div class="table-wrap">
      <table class="data-table min-w-[1080px]">
        <thead>
          <tr>
            <th class="w-16">ID</th>
            <th>用户名</th>
            <th>邮箱</th>
            <th>角色</th>
            <th>状态</th>
            <th class="text-right">额度</th>
            <th class="text-right">已用额度</th>
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
            loading-text="正在加载用户列表…"
            empty-text="还没有用户"
            empty-hint="创建第一个用户后，就可以为其签发访问令牌了。"
            @retry="loadUsers"
          >
            <template #action>
              <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
                <AppIcon name="plus" :size="14" />
                新建用户
              </button>
            </template>
          </DataState>

          <template v-if="!loading && !error && users.length">
            <tr v-for="user in users" :key="user.id">
              <td class="font-mono text-xs text-ink-400">{{ user.id }}</td>
              <td class="font-medium text-ink-100">{{ user.username }}</td>
              <td class="text-ink-300">{{ user.email || '—' }}</td>

              <td>
                <span :class="roleBadgeClass(user.role)">{{ roleLabel(user.role) }}</span>
              </td>

              <td>
                <span :class="statusBadgeClass(user.status)">
                  <span class="dot" />
                  {{ user.status === STATUS_ENABLED ? '启用' : '禁用' }}
                </span>
              </td>

              <td class="cell-num">{{ formatNumber(user.quota) }}</td>
              <td class="cell-num text-ink-300">{{ formatNumber(user.used_quota) }}</td>
              <td class="cell-muted whitespace-nowrap">{{ formatDateTime(user.created_at) }}</td>

              <td class="cell-actions">
                <div class="flex items-center justify-end gap-1">
                  <button type="button" class="btn btn-row" title="调整额度" @click="openQuota(user)">
                    <AppIcon name="quota" :size="14" />
                  </button>

                  <button type="button" class="btn btn-row" title="编辑" @click="openEdit(user)">
                    <AppIcon name="edit" :size="14" />
                  </button>

                  <button
                    type="button"
                    class="btn btn-row"
                    :title="user.status === STATUS_ENABLED ? '禁用' : '启用'"
                    :disabled="busyId === user.id"
                    @click="toggleStatus(user)"
                  >
                    <AppIcon :name="user.status === STATUS_ENABLED ? 'lock' : 'play'" :size="14" />
                  </button>

                  <button
                    type="button"
                    class="btn btn-row text-ink-400 hover:text-red-700"
                    title="删除"
                    :disabled="busyId === user.id"
                    @click="removeUser(user)"
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

    <!-- 新建用户 -->
    <Modal
      :open="createOpen"
      title="新建用户"
      subtitle="创建后用户即可登录门户，并按角色获得对应权限。"
      width="max-w-xl"
      :close-on-backdrop="false"
      @close="createOpen = false"
    >
      <div class="space-y-5">
        <div class="grid gap-5 sm:grid-cols-2">
          <div>
            <label class="label" for="user-username">用户名 <span class="text-red-600">*</span></label>
            <input id="user-username" v-model="createForm.username" class="input" type="text" placeholder="唯一用户名" />
          </div>
          <div>
            <label class="label" for="user-password">初始密码 <span class="text-red-600">*</span></label>
            <input
              id="user-password"
              v-model="createForm.password"
              class="input"
              type="password"
              autocomplete="new-password"
              placeholder="任意长度与字符"
            />
          </div>
        </div>

        <div>
          <label class="label" for="user-email">邮箱（可选）</label>
          <input id="user-email" v-model="createForm.email" class="input" type="email" placeholder="user@example.com" />
        </div>

        <div class="grid gap-5 sm:grid-cols-3">
          <div>
            <label class="label" for="user-role">角色</label>
            <select id="user-role" v-model.number="createForm.role" class="input">
              <option :value="ROLE_USER">普通用户</option>
              <option :value="ROLE_ADMIN">管理员</option>
            </select>
          </div>
          <div>
            <label class="label" for="user-quota">初始额度</label>
            <input id="user-quota" v-model.number="createForm.quota" class="input text-right tabular-nums" type="number" min="0" />
          </div>
          <div>
            <label class="label" for="user-status">状态</label>
            <select id="user-status" v-model.number="createForm.status" class="input">
              <option :value="STATUS_ENABLED">启用</option>
              <option :value="STATUS_DISABLED">禁用</option>
            </select>
          </div>
        </div>

        <p class="hint">额度为平台内部计量单位，用于限制该账号的模型调用消耗。</p>
        <p v-if="createError" class="field-error">{{ createError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="creating" @click="createOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="creating" @click="submitCreate">
          <span
            v-if="creating"
            class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
            aria-hidden="true"
          />
          <AppIcon v-else name="plus" :size="16" />
          {{ creating ? '创建中…' : '创建用户' }}
        </button>
      </template>
    </Modal>

    <!-- 编辑用户 -->
    <Modal
      :open="editOpen"
      title="编辑用户"
      :subtitle="editTarget ? `用户：${editTarget.username}（#${editTarget.id}）` : ''"
      width="max-w-lg"
      :close-on-backdrop="false"
      @close="editOpen = false"
    >
      <div class="space-y-5">
        <div>
          <label class="label" for="edit-user-email">邮箱</label>
          <input id="edit-user-email" v-model="editForm.email" class="input" type="email" placeholder="留空表示不设置" />
        </div>

        <div class="grid gap-5 sm:grid-cols-2">
          <div>
            <label class="label" for="edit-user-role">角色</label>
            <select id="edit-user-role" v-model.number="editForm.role" class="input">
              <option :value="ROLE_USER">普通用户</option>
              <option :value="ROLE_ADMIN">管理员</option>
            </select>
          </div>
          <div>
            <label class="label" for="edit-user-status">状态</label>
            <select id="edit-user-status" v-model.number="editForm.status" class="input">
              <option :value="STATUS_ENABLED">启用</option>
              <option :value="STATUS_DISABLED">禁用</option>
            </select>
          </div>
        </div>

        <p class="hint">密码重置不在本版本范围内（接口契约未定义），如需重置请联系后端补充接口。</p>
        <p v-if="editError" class="field-error">{{ editError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="editing" @click="editOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="editing" @click="submitEdit">
          {{ editing ? '保存中…' : '保存修改' }}
        </button>
      </template>
    </Modal>

    <!-- 调整额度 -->
    <Modal
      :open="quotaOpen"
      title="调整额度"
      subtitle="仅修改该账号的可用额度，不影响其令牌与历史记录。"
      width="max-w-md"
      :close-on-backdrop="false"
      @close="quotaOpen = false"
    >
      <div v-if="quotaTarget" class="space-y-4">
        <div class="flex items-center justify-between rounded-lg border border-ink-800 bg-ink-850/50 px-3.5 py-2.5">
          <span class="text-sm text-ink-200">{{ quotaTarget.username }}</span>
          <span class="text-xs text-ink-400">已用额度 {{ formatNumber(quotaTarget.used_quota) }}</span>
        </div>

        <div>
          <label class="label" for="quota-value">账户额度</label>
          <input
            id="quota-value"
            v-model.number="quotaValue"
            class="input text-right tabular-nums"
            type="number"
            min="0"
            @keydown.enter="submitQuota"
          />
          <p class="hint">填 0 表示该账号不可再消耗额度（是否允许归档查看取决于后端策略）。</p>
        </div>

        <p v-if="quotaError" class="field-error">{{ quotaError }}</p>
      </div>

      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="quotaSaving" @click="quotaOpen = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="quotaSaving" @click="submitQuota">
          {{ quotaSaving ? '保存中…' : '保存额度' }}
        </button>
      </template>
    </Modal>
  </div>
</template>
