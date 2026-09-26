<script setup lang="ts">
/**
 * 管理后台 · 订阅账号（OAuth）：配置令牌刷新所需的提供方参数。
 *
 * 意图（Why）：
 *   把"订阅制账号"（如某些按订阅计费的模型服务）当作上游凭据使用时，
 *   其 access_token 会过期，网关必须在过期前用 refresh_token 换新的。
 *   刷新协议是标准的 OAuth2，但每个服务商的令牌端点、client_id 与
 *   client_secret 都不同，因此需要在这里登记"提供方"。
 *   真正的凭据（refresh_token）在「渠道管理 → 密钥池 → OAuth 账号」里导入，
 *   本页只配置"怎么刷新"。
 *
 * 安全约定（界面层面必须体现）：
 *   - client_secret 只写入不读出：后端只返回掩码，因此编辑时留空表示"不修改"；
 *   - 令牌端点必须以 https 开头（明文传输令牌不可接受），后端会强校验。
 *
 * 流转（Flow）：
 *   进入页面 → listOAuthProviders() → 表格
 *   新建/编辑 → Modal → createOAuthProvider / updateOAuthProvider
 *
 * 扩展（Extend）：
 *   若某服务商需要额外的授权参数（如 audience），在 types.ts 与后端加字段，
 *   并在刷新实现（internal/relay/oauth.go）中一并使用。
 */
import { onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import DataState from '@/components/DataState.vue'
import Modal from '@/components/Modal.vue'
import { ApiError } from '@/api/client'
import { createOAuthProvider, deleteOAuthProvider, listOAuthProviders, updateOAuthProvider } from '@/api/admin'
import type { OAuthProvider } from '@/api/types'
import { confirmDialog } from '@/composables/useConfirm'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatDateTime } from '@/utils/format'

const providers = ref<OAuthProvider[]>([])
const loading = ref(true)
const error = ref('')

const formOpen = ref(false)
const editing = ref<OAuthProvider | null>(null)
const saving = ref(false)
const formError = ref('')

const form = ref({
  name: '',
  token_url: '',
  client_id: '',
  client_secret: '',
  scope: '',
  remark: '',
  enabled: true,
})

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const result = await listOAuthProviders()
    providers.value = result.items ?? []
  } catch (err) {
    providers.value = []
    error.value = err instanceof ApiError ? err.message : '提供方加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(load)

function openCreate(): void {
  editing.value = null
  formError.value = ''
  form.value = { name: '', token_url: '', client_id: '', client_secret: '', scope: '', remark: '', enabled: true }
  formOpen.value = true
}

function openEdit(provider: OAuthProvider): void {
  editing.value = provider
  formError.value = ''
  form.value = {
    name: provider.name,
    token_url: provider.token_url,
    client_id: provider.client_id,
    // 留空表示不修改：后端只返回掩码，我们无法（也不应该）回填明文
    client_secret: '',
    scope: provider.scope,
    remark: provider.remark,
    enabled: provider.enabled,
  }
  formOpen.value = true
}

async function submit(): Promise<void> {
  const name = form.value.name.trim().toLowerCase()
  const tokenUrl = form.value.token_url.trim()

  if (!name) {
    formError.value = '提供方名称不能为空'
    return
  }
  if (!tokenUrl) {
    formError.value = '令牌端点不能为空'
    return
  }
  if (!tokenUrl.startsWith('https://')) {
    formError.value = '令牌端点必须以 https:// 开头（明文传输令牌不可接受）'
    return
  }
  if (!editing.value && !form.value.client_secret.trim()) {
    formError.value = '新建提供方时必须填写客户端密钥'
    return
  }

  saving.value = true
  formError.value = ''
  try {
    if (editing.value) {
      await updateOAuthProvider(editing.value.id, {
        token_url: tokenUrl,
        client_id: form.value.client_id.trim(),
        client_secret: form.value.client_secret.trim() || undefined,
        scope: form.value.scope.trim(),
        remark: form.value.remark.trim(),
        enabled: form.value.enabled,
      })
      toastSuccess(`提供方「${editing.value.name}」已更新`)
    } else {
      await createOAuthProvider({
        name,
        token_url: tokenUrl,
        client_id: form.value.client_id.trim(),
        client_secret: form.value.client_secret.trim(),
        scope: form.value.scope.trim(),
        remark: form.value.remark.trim(),
        enabled: form.value.enabled,
      })
      toastSuccess(`提供方「${name}」已创建`)
    }
    formOpen.value = false
    await load()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

async function remove(provider: OAuthProvider): Promise<void> {
  const ok = await confirmDialog({
    title: `删除提供方「${provider.name}」`,
    message: '删除后，使用该提供方刷新令牌的订阅账号将无法自动续期（表现为调用时偶发 401）。渠道密钥池里的账号不会被删除。',
    confirmText: '删除',
    danger: true,
  })
  if (!ok) return

  try {
    await deleteOAuthProvider(provider.id)
    toastSuccess('提供方已删除')
    await load()
  } catch (err) {
    toastError(err instanceof ApiError ? err.message : '删除失败')
  }
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">订阅账号（OAuth）</h2>
        <p class="page-desc">
          配置"订阅制上游账号"的令牌刷新参数。真正的账号凭据在
          <RouterLink to="/admin/channels" class="text-brand-700 hover:underline">渠道管理</RouterLink>
          → 密钥池 → OAuth 账号中导入；本页只定义"怎么刷新令牌"。
        </p>
      </div>
      <div class="toolbar">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load">
          <AppIcon name="refresh" :size="14" />
          刷新
        </button>
        <button type="button" class="btn btn-primary btn-sm" @click="openCreate">
          <AppIcon name="plus" :size="14" />
          新建提供方
        </button>
      </div>
    </div>

    <div class="table-wrap table-cards">
      <table class="data-table">
        <thead>
          <tr>
            <th>名称</th>
            <th>令牌端点</th>
            <th>客户端 ID</th>
            <th>客户端密钥</th>
            <th>授权范围</th>
            <th>状态</th>
            <th>更新时间</th>
            <th class="cell-actions">操作</th>
          </tr>
        </thead>
        <tbody>
          <DataState
            :loading="loading"
            :error="error"
            :empty="!loading && !error && providers.length === 0"
            :colspan="8"
            loading-text="正在读取提供方配置…"
            empty-text="还没有配置任何 OAuth 提供方"
            empty-hint="若你不使用订阅制账号作为上游，可以忽略本页；否则请先创建提供方，再到渠道密钥池导入账号。"
            @retry="load"
          />

          <tr v-for="provider in providers" :key="provider.id">
            <td data-label="名称">
              <div class="flex items-center gap-2">
                <span class="font-medium text-ink-100">{{ provider.name }}</span>
                <span v-if="provider.remark" class="cell-muted" :title="provider.remark">…</span>
              </div>
            </td>
            <td class="cell-muted max-w-[18rem]" data-label="令牌端点">
              <span class="line-clamp-1 font-mono text-[12px]">{{ provider.token_url }}</span>
            </td>
            <td class="cell-muted" data-label="客户端 ID"><code class="chip">{{ provider.client_id || '—' }}</code></td>
            <td class="cell-muted" data-label="客户端密钥"><code class="chip">{{ provider.masked_client_secret || '未设置' }}</code></td>
            <td class="cell-muted max-w-[10rem]" data-label="授权范围">
              <span class="line-clamp-1">{{ provider.scope || '—' }}</span>
            </td>
            <td data-label="状态">
              <span class="badge" :class="provider.enabled ? 'badge-ok' : 'badge-off'">
                {{ provider.enabled ? '启用' : '停用' }}
              </span>
            </td>
            <td class="cell-muted" data-label="更新时间">{{ formatDateTime(provider.updated_at) }}</td>
            <td class="cell-actions" data-label="操作">
              <div class="flex items-center justify-end gap-1">
                <button type="button" class="btn-row" title="编辑" @click="openEdit(provider)">
                  <AppIcon name="edit" :size="14" />
                </button>
                <button type="button" class="btn-row" title="删除" @click="remove(provider)">
                  <AppIcon name="trash" :size="14" />
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <section class="mt-5 card card-pad">
      <h3 class="section-title flex items-center gap-2">
        <AppIcon name="info" :size="16" class="text-brand-700" />
        它是怎么工作的
      </h3>
      <ol class="mt-3 space-y-1.5 text-xs leading-relaxed text-ink-400">
        <li>1. 你在渠道密钥池导入账号的 refresh_token（可附邮箱备注）。</li>
        <li>2. 转发前若 access_token 即将过期（剩不到 60 秒），网关会用本页的配置去刷新。</li>
        <li>3. 刷新失败会累计到该账号的失败次数，达到阈值后自动摘除，避免持续撞一个坏账号。</li>
        <li>4. 刷新成功后令牌被加密写回，界面上只显示账号标识，永远看不到明文密钥。</li>
      </ol>
    </section>

    <Modal
      :open="formOpen"
      :title="editing ? `编辑提供方「${editing.name}」` : '新建 OAuth 提供方'"
      subtitle="提供方名称会作为配置键被账号引用，创建后不可修改。"
      width="max-w-xl"
      :close-on-backdrop="false"
      @close="formOpen = false"
    >
      <div class="space-y-4">
        <div>
          <label class="label" for="oauth-name">提供方名称<span class="text-red-600">*</span></label>
          <input
            id="oauth-name"
            v-model="form.name"
            class="input input-mono"
            type="text"
            :disabled="Boolean(editing)"
            placeholder="如 anthropic、openai（小写字母）"
          />
        </div>

        <div>
          <label class="label" for="oauth-token-url">令牌端点（token_url）<span class="text-red-600">*</span></label>
          <input
            id="oauth-token-url"
            v-model="form.token_url"
            class="input input-mono"
            type="url"
            placeholder="https://api.example.com/oauth/token"
          />
          <p class="hint">必须使用 https。这是服务商文档中「换取 access_token」的地址。</p>
        </div>

        <div>
          <label class="label" for="oauth-client-id">客户端 ID</label>
          <input id="oauth-client-id" v-model="form.client_id" class="input input-mono" type="text" />
        </div>

        <div>
          <label class="label" for="oauth-client-secret">
            客户端密钥{{ editing ? '（留空表示不修改）' : '' }}<span v-if="!editing" class="text-red-600">*</span>
          </label>
          <input
            id="oauth-client-secret"
            v-model="form.client_secret"
            class="input input-mono"
            type="password"
            autocomplete="new-password"
            :placeholder="editing ? '留空则保持原密钥不变' : '仅写入不读出，界面只显示掩码'"
          />
        </div>

        <div>
          <label class="label" for="oauth-scope">授权范围（scope）</label>
          <input id="oauth-scope" v-model="form.scope" class="input input-mono" type="text" placeholder="多个用空格分隔" />
        </div>

        <div>
          <label class="label" for="oauth-remark">备注</label>
          <input id="oauth-remark" v-model="form.remark" class="input" type="text" placeholder="便于日后辨认，如「团队订阅账号」" />
        </div>

        <label class="flex items-center gap-2 text-sm text-ink-200">
          <input v-model="form.enabled" class="checkbox" type="checkbox" />
          启用该提供方
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
