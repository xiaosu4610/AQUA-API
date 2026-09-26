<script setup lang="ts">
/**
 * 管理后台 · 运维监控：运行状态概览 + 数据库备份导出 + 备份只读校验。
 *
 * 意图（Why）：
 *   站长需要一个「一眼看清 + 一键留档」的页面：
 *   顶部卡片回答「版本 / 运行时长 / 数据库体积 / 磁盘 / 近 24h 与 7 天调用健康度」，
 *   其下用表格列出各表行数；再提供「下载一致性快照」与「校验备份文件」两个动作。
 *   页面刻意不提供在线恢复（后端也不提供）：恢复需停机手工完成，这里只给步骤说明。
 *
 * 流转（Flow）：
 *   进入页面 → fetchMaintenanceOverview() → 卡片 + 行数表
 *   点击「下载备份」→ downloadMaintenanceBackup()（浏览器保存 .db）
 *   选择文件并「校验」→ inspectMaintenanceBackup(file) → 展示版本对比与恢复步骤
 *
 * 扩展（Extend）：
 *   新增指标：在 api/maintenance.ts 的 MaintenanceOverview 加字段 → 本页加卡片。
 */
import { computed, onMounted, ref } from 'vue'

import AppIcon from '@/components/AppIcon.vue'
import StatCard from '@/components/StatCard.vue'
import { ApiError } from '@/api/client'
import {
  downloadMaintenanceBackup,
  fetchMaintenanceOverview,
  inspectMaintenanceBackup,
} from '@/api/maintenance'
import type { MaintenanceInspectResult, MaintenanceOverview } from '@/api/maintenance'
import { formatCompact, formatDateTime, formatLatency, formatNumber, formatPercent } from '@/utils/format'

const overview = ref<MaintenanceOverview | null>(null)
const loading = ref(true)
const error = ref('')

const downloading = ref(false)
const downloadError = ref('')

const selectedFile = ref<File | null>(null)
const inspecting = ref(false)
const inspectError = ref('')
const inspectResult = ref<MaintenanceInspectResult | null>(null)

async function loadOverview(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    overview.value = await fetchMaintenanceOverview()
  } catch (err) {
    overview.value = null
    error.value = err instanceof ApiError ? err.message : '运维数据加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(loadOverview)

/* ── 格式化辅助 ───────────────────────────────────────── */

/** 字节 → 人类可读（B / KiB / MiB / GiB） */
function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || Number.isNaN(bytes)) return '—'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let value = bytes
  let index = 0
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024
    index += 1
  }
  return `${value.toFixed(index === 0 ? 0 : 1)} ${units[index]}`
}

/** 秒 → 「N 天 M 小时 / N 小时 M 分」 */
function formatUptime(seconds: number): string {
  if (!seconds || seconds < 0) return '—'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${minutes} 分`
  return `${minutes} 分 ${seconds % 60} 秒`
}

/* ── 卡片数值 ─────────────────────────────────────────── */

const versionValue = computed(() => overview.value?.version ?? '—')
const versionHint = computed(() => {
  const data = overview.value
  if (!data) return ''
  const parts: string[] = []
  if (data.git_commit && data.git_commit !== 'unknown') parts.push(data.git_commit.slice(0, 8))
  if (data.build_time && data.build_time !== 'unknown') parts.push(data.build_time)
  return parts.join(' · ')
})

const uptimeValue = computed(() => formatUptime(overview.value?.uptime_seconds ?? 0))
const uptimeHint = computed(() =>
  overview.value ? `启动于 ${formatDateTime(overview.value.started_at)}` : '',
)

const dbValue = computed(() => {
  const database = overview.value?.database
  if (!database || !database.size_available) return '—'
  return formatBytes(database.size_bytes)
})
const dbHint = computed(() => (overview.value ? `驱动 ${overview.value.database.driver}` : ''))

const diskValue = computed(() => {
  const disk = overview.value?.disk
  if (!disk?.available) return '—'
  return formatBytes(disk.free_bytes)
})
const diskHint = computed(() => {
  const disk = overview.value?.disk
  if (!disk?.available) return '当前平台暂不提供磁盘数据'
  return `共 ${formatBytes(disk.total_bytes)} · 已用 ${formatPercent(disk.used_ratio)}`
})

const usage24 = computed(() => overview.value?.usage.last_24h)
const usage7d = computed(() => overview.value?.usage.last_7d)

const usage24Value = computed(() => formatCompact(usage24.value?.requests ?? 0))
const usage24Hint = computed(() => {
  const item = usage24.value
  if (!item) return ''
  return `失败 ${formatNumber(item.failures)} · 失败率 ${formatPercent(item.failure_rate)} · 平均 ${formatLatency(Math.round(item.avg_latency_ms))}`
})
const usage24Tone = computed<'ok' | 'warn' | 'err'>(() => {
  const rate = usage24.value?.failure_rate ?? 0
  if (rate >= 0.05) return 'err'
  if (rate > 0) return 'warn'
  return 'ok'
})

const usage7dValue = computed(() => formatCompact(usage7d.value?.requests ?? 0))
const usage7dHint = computed(() => {
  const item = usage7d.value
  if (!item) return ''
  return `失败 ${formatNumber(item.failures)} · 失败率 ${formatPercent(item.failure_rate)} · 平均 ${formatLatency(Math.round(item.avg_latency_ms))}`
})
const usage7dTone = computed<'ok' | 'warn' | 'err'>(() => {
  const rate = usage7d.value?.failure_rate ?? 0
  if (rate >= 0.05) return 'err'
  if (rate > 0) return 'warn'
  return 'ok'
})

const hasTables = computed(() => (overview.value?.tables.length ?? 0) > 0)

/* ── 备份下载 ─────────────────────────────────────────── */

async function onDownloadBackup(): Promise<void> {
  downloading.value = true
  downloadError.value = ''
  try {
    await downloadMaintenanceBackup()
  } catch (err) {
    downloadError.value = err instanceof ApiError ? err.message : err instanceof Error ? err.message : '备份下载失败'
  } finally {
    downloading.value = false
  }
}

/* ── 备份校验 ─────────────────────────────────────────── */

function onFileChange(event: Event): void {
  const target = event.target as HTMLInputElement
  selectedFile.value = target.files?.[0] ?? null
  inspectResult.value = null
  inspectError.value = ''
}

async function onInspect(): Promise<void> {
  if (!selectedFile.value) return
  inspecting.value = true
  inspectError.value = ''
  inspectResult.value = null
  try {
    inspectResult.value = await inspectMaintenanceBackup(selectedFile.value)
  } catch (err) {
    inspectError.value = err instanceof ApiError ? err.message : '备份文件校验失败'
  } finally {
    inspecting.value = false
  }
}

/** 校验结果的表名对比中，行数是否一致 */
function rowsMatch(backupRows: number, currentRows: number): boolean {
  return backupRows === currentRows
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">运维监控</h2>
        <p class="page-desc">运行状态、数据库体积与磁盘水位，以及备份导出与校验。</p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadOverview">
        <AppIcon name="refresh" :size="14" />
        刷新数据
      </button>
    </div>

    <!-- 整页错误态：数据同源，重试一次即可恢复 -->
    <div v-if="error && !loading" class="card card-pad">
      <div class="flex flex-col items-center justify-center gap-3 py-10 text-center">
        <span class="flex h-11 w-11 items-center justify-center rounded-xl bg-ink-850 text-red-700 ring-1 ring-inset ring-red-500/25">
          <AppIcon name="alert" :size="20" />
        </span>
        <p class="text-sm text-ink-200">{{ error }}</p>
        <button type="button" class="btn btn-secondary btn-sm" @click="loadOverview">
          <AppIcon name="refresh" :size="14" />
          重新加载
        </button>
      </div>
    </div>

    <template v-else>
      <!-- 概览卡片 -->
      <section class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
        <template v-if="loading">
          <div v-for="index in 6" :key="index" class="card card-pad">
            <div class="h-4 w-16 skeleton" />
            <div class="mt-3 h-7 w-24 skeleton" />
            <div class="mt-3 h-3 w-32 skeleton" />
          </div>
        </template>

        <template v-else>
          <StatCard label="当前版本" :value="versionValue" :hint="versionHint" icon="chart" tone="brand" />
          <StatCard label="运行时长" :value="uptimeValue" :hint="uptimeHint" icon="clock" tone="mute" />
          <StatCard label="数据库体积" :value="dbValue" :hint="dbHint" icon="quota" tone="brand" />
          <StatCard label="磁盘可用" :value="diskValue" :hint="diskHint" icon="server" tone="mute" />
          <StatCard label="近 24 小时调用" :value="usage24Value" :hint="usage24Hint" icon="bolt" :tone="usage24Tone" />
          <StatCard label="近 7 天调用" :value="usage7dValue" :hint="usage7dHint" icon="trend" :tone="usage7dTone" />
        </template>
      </section>

      <!-- 各表行数 -->
      <section class="mt-6 card">
        <div class="card-head">
          <div>
            <h3 class="section-title">数据表明细</h3>
            <p class="mt-1 text-xs text-ink-400">各核心表的行数，用于快速判断数据规模。</p>
          </div>
        </div>
        <div class="table-wrap table-cards">
          <table class="data-table">
            <thead>
              <tr>
                <th>表名</th>
                <th>行数</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="loading">
                <td colspan="2" class="cell-muted">正在统计…</td>
              </tr>
              <tr v-else-if="!hasTables">
                <td colspan="2" class="cell-muted">暂无可统计的数据表。</td>
              </tr>
              <template v-else>
                <tr v-for="table in overview?.tables ?? []" :key="table.name">
                  <td data-label="表名"><code class="chip">{{ table.name }}</code></td>
                  <td data-label="行数" class="tabular-nums">{{ formatNumber(table.rows) }}</td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 备份导出 -->
      <section class="mt-6 card">
        <div class="card-head">
          <div>
            <h3 class="section-title">数据库备份</h3>
            <p class="mt-1 text-xs text-ink-400">
              导出 SQLite 一致性快照（VACUUM INTO），可直接用于离线保存。仅支持 SQLite 驱动。
            </p>
          </div>
        </div>
        <div class="card-pad">
          <button type="button" class="btn btn-primary btn-sm" :disabled="downloading" @click="onDownloadBackup">
            <AppIcon name="quota" :size="14" />
            {{ downloading ? '正在生成备份…' : '下载备份' }}
          </button>
          <p v-if="downloadError" class="mt-3 text-xs text-red-600">{{ downloadError }}</p>
        </div>
      </section>

      <!-- 备份校验 -->
      <section class="mt-6 card">
        <div class="card-head">
          <div>
            <h3 class="section-title">校验备份文件</h3>
            <p class="mt-1 text-xs text-ink-400">
              上传一个 .db 备份做只读校验，并与当前数据库的表行数对比（不会改动任何数据）。
            </p>
          </div>
        </div>
        <div class="card-pad">
          <div class="flex flex-wrap items-end gap-3">
            <div class="min-w-[16rem] flex-1">
              <label class="label" for="maintenance-backup-file">备份文件（.db）</label>
              <input
                id="maintenance-backup-file"
                class="input"
                type="file"
                accept=".db,application/octet-stream"
                @change="onFileChange"
              />
            </div>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              :disabled="!selectedFile || inspecting"
              @click="onInspect"
            >
              <AppIcon name="search" :size="14" />
              {{ inspecting ? '校验中…' : '开始校验' }}
            </button>
          </div>

          <p v-if="inspectError" class="mt-3 text-xs text-red-600">{{ inspectError }}</p>

          <template v-if="inspectResult">
            <div class="mt-4 flex flex-wrap items-center gap-2 text-xs text-ink-300">
              <span class="badge badge-ok">校验通过</span>
              <span>
                备份 schema 版本
                <strong class="text-ink-100">{{ inspectResult.schema_version }}</strong>
                <template v-if="inspectResult.schema_table_present">（含迁移表）</template>
                <template v-else>（无迁移表）</template>
              </span>
              <span class="text-ink-500">·</span>
              <span>
                当前库版本
                <strong class="text-ink-100">{{ inspectResult.current_schema_version }}</strong>
              </span>
            </div>

            <div class="table-wrap table-cards mt-4">
              <table class="data-table">
                <thead>
                  <tr>
                    <th>表名</th>
                    <th>备份行数</th>
                    <th>当前行数</th>
                    <th>对比</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="row in inspectResult.tables" :key="row.name">
                    <td data-label="表名"><code class="chip">{{ row.name }}</code></td>
                    <td data-label="备份行数" class="tabular-nums">
                      <template v-if="row.in_backup">{{ formatNumber(row.backup_rows) }}</template>
                      <span v-else class="text-ink-500">缺失</span>
                    </td>
                    <td data-label="当前行数" class="tabular-nums">{{ formatNumber(row.current_rows) }}</td>
                    <td data-label="对比">
                      <span v-if="!row.in_backup" class="badge badge-warn">备份缺少</span>
                      <span v-else-if="rowsMatch(row.backup_rows, row.current_rows)" class="badge badge-ok">一致</span>
                      <span v-else class="badge badge-info">不同</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <div class="mt-4 rounded-xl border border-amber-500/25 bg-amber-500/5 p-3">
              <p class="flex items-center gap-2 text-xs font-medium text-amber-700">
                <AppIcon name="alert" :size="14" />
                恢复需人工操作（不提供在线恢复）
              </p>
              <p class="mt-2 text-xs leading-relaxed text-ink-400">{{ inspectResult.note }}</p>
              <ol class="mt-2 space-y-1.5 text-xs leading-relaxed text-ink-400">
                <li v-for="(step, index) in inspectResult.restore_steps" :key="index">{{ step }}</li>
              </ol>
            </div>
          </template>
        </div>
      </section>
    </template>
  </div>
</template>
