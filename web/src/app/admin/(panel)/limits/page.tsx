/** 管理后台：运行上限（/admin/limits）。
 *
 * 意图（Why）：
 *   网关里有一批"保护性上限"（请求体大小、批量导入条数、统计窗口、试用时长、
 *   公告条数、SSE 单行/尾部缓冲字节数），此前写死在代码里。本页把它们集中呈现、
 *   允许超管调整：
 *   每项都显示当前值、默认值与合法区间，数值全部由后端 /api/admin/limits 下发，
 *   前端不硬编码任何阈值——改默认值或区间只改后端一处。
 *
 * 流转（Flow）：
 *   加载 → fetchLimits() 拿到 items(field/value/default/min/max) → 铺进本地编辑态
 *   保存 → 只提交发生变化的字段 → updateLimits() → 重新拉取回显（纠正非法输入）
 *
 * 扩展（Extend）：
 *   新增一项上限：后端 LimitSettings 加字段并在 buildLimitItems 输出一行，
 *   本页在 FIELD_META 里补 field → 文案键/单位的映射，再到 locales 各语言 admin.ts 补词条。
 */
'use client'

import { useEffect, useState } from 'react'

import { fetchLimits, updateLimits } from '@/api/admin'
import type { LimitItem, UpdateLimitsPayload } from '@/api/types'
import { Button } from '@/components/ui/Button'
import { Card, SkeletonRows } from '@/components/ui/Display'
import { Field, Input } from '@/components/ui/Form'
import { useI18n } from '@/i18n'
import { useToast } from '@/lib/toast/toast-context'

/** 字段 → 文案键与单位（与后端 buildLimitItems 的 field 一一对应） */
const FIELD_META: Record<string, { labelKey: string; helpKey: string; unitKey: string }> = {
  body_max_bytes: {
    labelKey: 'admin.limits.field.bodyMaxBytes.label',
    helpKey: 'admin.limits.field.bodyMaxBytes.help',
    unitKey: 'admin.limits.unit.bytes',
  },
  sensitive_import_max_words: {
    labelKey: 'admin.limits.field.sensitiveImportMaxWords.label',
    helpKey: 'admin.limits.field.sensitiveImportMaxWords.help',
    unitKey: 'admin.limits.unit.words',
  },
  leaderboard_max_days: {
    labelKey: 'admin.limits.field.leaderboardMaxDays.label',
    helpKey: 'admin.limits.field.leaderboardMaxDays.help',
    unitKey: 'admin.limits.unit.days',
  },
  model_stats_max_minutes: {
    labelKey: 'admin.limits.field.modelStatsMaxMinutes.label',
    helpKey: 'admin.limits.field.modelStatsMaxMinutes.help',
    unitKey: 'admin.limits.unit.minutes',
  },
  trial_grant_max_hours: {
    labelKey: 'admin.limits.field.trialGrantMaxHours.label',
    helpKey: 'admin.limits.field.trialGrantMaxHours.help',
    unitKey: 'admin.limits.unit.hours',
  },
  announcement_active_max: {
    labelKey: 'admin.limits.field.announcementActiveMax.label',
    helpKey: 'admin.limits.field.announcementActiveMax.help',
    unitKey: 'admin.limits.unit.items',
  },
  sse_anthropic_line_bytes: {
    labelKey: 'admin.limits.field.sseAnthropicLineBytes.label',
    helpKey: 'admin.limits.field.sseAnthropicLineBytes.help',
    unitKey: 'admin.limits.unit.bytes',
  },
  sse_gemini_line_bytes: {
    labelKey: 'admin.limits.field.sseGeminiLineBytes.label',
    helpKey: 'admin.limits.field.sseGeminiLineBytes.help',
    unitKey: 'admin.limits.unit.bytes',
  },
  sse_codex_line_bytes: {
    labelKey: 'admin.limits.field.sseCodexLineBytes.label',
    helpKey: 'admin.limits.field.sseCodexLineBytes.help',
    unitKey: 'admin.limits.unit.bytes',
  },
  sse_usage_tail_bytes: {
    labelKey: 'admin.limits.field.sseUsageTailBytes.label',
    helpKey: 'admin.limits.field.sseUsageTailBytes.help',
    unitKey: 'admin.limits.unit.bytes',
  },
}

export default function AdminLimitsPage() {
  const { toast, toastError } = useToast()
  const { t } = useI18n()

  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [items, setItems] = useState<LimitItem[]>([])
  // values 用字符串保存编辑态：允许"正在输入"的中间态（如空串），提交时再解析。
  const [values, setValues] = useState<Record<string, string>>({})

  useEffect(() => {
    void fetchLimits()
      .then((data) => {
        const next = data.items ?? []
        setItems(next)
        const nextValues: Record<string, string> = {}
        for (const item of next) {
          nextValues[item.field] = String(item.value)
        }
        setValues(nextValues)
      })
      .catch((err) => toastError(err instanceof Error ? err.message : t('admin.limits.toast.loadFailed')))
      .finally(() => setLoading(false))
  }, [toastError, t])

  /** 用接口返回的条目重置列表与编辑态（保存后用它回显后端最终值）。 */
  function applyLoaded(next: LimitItem[]) {
    setItems(next)
    const nextValues: Record<string, string> = {}
    for (const item of next) {
      nextValues[item.field] = String(item.value)
    }
    setValues(nextValues)
  }

  /** 只提交"发生了变化的字段"，避免整对象覆盖带来的意外。 */
  async function handleSave() {
    setSaving(true)
    try {
      const payload: Record<string, number> = {}
      for (const item of items) {
        const raw = values[item.field]
        if (raw === undefined || raw.trim() === '') continue
        const parsed = Number(raw)
        if (!Number.isFinite(parsed)) continue
        if (parsed !== item.value) {
          payload[item.field] = parsed
        }
      }
      await updateLimits(payload as UpdateLimitsPayload)
      toast(t('admin.limits.toast.saved'))
      // 重新拉取：以后端最终值回显（越界会被拒绝且此处会把输入纠正回当前值）。
      applyLoaded((await fetchLimits()).items ?? [])
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.limits.toast.saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  if (loading) {
    return (
      <div className="space-y-5">
        <div>
          <h1 className="text-xl font-bold text-ink">{t('admin.limits.title')}</h1>
          <p className="mt-0.5 text-[13px] text-ink-3">{t('admin.limits.subtitle')}</p>
        </div>
        <Card>
          <SkeletonRows rows={6} />
        </Card>
      </div>
    )
  }

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-bold text-ink">{t('admin.limits.title')}</h1>
          <p className="mt-0.5 text-[13px] text-ink-3">{t('admin.limits.subtitle')}</p>
        </div>
        <Button variant="primary" loading={saving} onClick={handleSave}>{t('admin.limits.save')}</Button>
      </div>

      <Card className="space-y-4">
        {items.map((item) => {
          const meta = FIELD_META[item.field]
          if (!meta) return null
          return (
            <Field
              key={item.field}
              label={`${t(meta.labelKey)}（${t(meta.unitKey)}）`}
              help={`${t(meta.helpKey)} · ${t('admin.limits.rangeHint', { min: item.min, max: item.max })} · ${t('admin.limits.defaultHint', { value: item.default })}`}
            >
              <Input
                type="number"
                min={item.min}
                max={item.max}
                value={values[item.field] ?? ''}
                onChange={(e) => setValues((prev) => ({ ...prev, [item.field]: e.target.value }))}
              />
            </Field>
          )
        })}
      </Card>

      <div className="rounded-md border border-line bg-surface p-3 text-xs text-ink-3">
        {t('admin.limits.notice')}
      </div>
    </div>
  )
}
