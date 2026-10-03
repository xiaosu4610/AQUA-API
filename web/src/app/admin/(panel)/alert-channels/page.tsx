/** 管理后台：告警通道（/admin/alert-channels）。
 *
 * 意图（Why）：
 *   渠道熔断、成功率自动停用、账号锁定这类事件此前只落服务端日志，
 *   "站点出事了"要靠第二天翻日志才发现。本页把外发通道显式管起来，
 *   目标只有一个：让故障在几秒内出现在站长的手机上。
 *
 *   三处刻意的设计：
 *     1) 投递目标永远只显示脱敏值。钉钉/企微的 URL 自带凭据，
 *        一次截图或一次日志粘贴就等于把机器人凭据送出去；
 *     2) 外发总开关默认关闭，页面上明确提示"开启后才会真正外发"——
 *        配好通道不等于已经会收到告警，这一步必须让站长自己按下去；
 *     3) 「发送测试」同步等结果。让站长当场知道地址对不对，
 *        而不是等到真出事才发现告警根本没到。
 *
 * 流转（Flow）：
 *   加载 → listAlertChannels（列表）+ listAlertChannelKinds（类型与事件目录）+ getAlertSettings（总开关）
 *   新建/编辑 → AlertChannelFormModal → createAlertChannel / updateAlertChannel
 *   删除 → ConfirmDialog → deleteAlertChannel
 *   发送测试 / 就地启停 → testAlertChannel / updateAlertChannel
 *   总开关 → updateAlertSettings
 *
 * 扩展（Extend）：
 *   后端新增通道类型或事件类型时本页无需改动——下拉与多选内容全部来自
 *   /alert-channel-kinds；在 locales 各语言 admin.ts 的 alertChannels 补词条即可。
 */
'use client'

import { useCallback, useEffect, useMemo, useState } from 'react'

import {
  createAlertChannel,
  deleteAlertChannel,
  getAlertSettings,
  listAlertChannelKinds,
  listAlertChannels,
  testAlertChannel,
  updateAlertChannel,
  updateAlertSettings,
  type AlertChannel,
  type AlertChannelKinds,
  type AlertChannelPayload,
} from '@/api/alert'
import { Badge, Card } from '@/components/ui/Display'
import { DataTable, type Column } from '@/components/ui/Table'
import { Button } from '@/components/ui/Button'
import { Field, Input, Select, Switch } from '@/components/ui/Form'
import { Modal, ConfirmDialog } from '@/components/ui/Modal'
import { useI18n } from '@/i18n'
import { useToast } from '@/lib/toast/toast-context'
import { formatDateTime } from '@/utils/format'

/**
 * 「测试消息」事件键：由本页的「发送测试」按钮触发，不对应任何真实故障，
 * 因此不作为可订阅事件出现在多选里（后端目录仍会下发它，这里只做展示过滤）。
 */
const TEST_EVENT_KEY = 'system.test'

export default function AdminAlertChannelsPage() {
  const { toast, toastError } = useToast()
  const { t } = useI18n()

  const [items, setItems] = useState<AlertChannel[]>([])
  const [catalog, setCatalog] = useState<AlertChannelKinds | null>(null)
  const [loading, setLoading] = useState(true)
  // 外发总开关：与通道列表分开加载，互不阻塞（总开关拉取失败不应挡住通道管理）
  const [alertEnabled, setAlertEnabled] = useState(false)
  const [settingsLoading, setSettingsLoading] = useState(true)
  const [settingsSaving, setSettingsSaving] = useState(false)
  const [editing, setEditing] = useState<AlertChannel | null | 'new'>(null)
  const [deleteTarget, setDeleteTarget] = useState<AlertChannel | null>(null)
  // 正在执行测试/启停的通道 id（0 = 无），用于按钮与开关置灰防重复点击
  const [busyId, setBusyId] = useState(0)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [list, kinds] = await Promise.all([listAlertChannels(), listAlertChannelKinds()])
      setItems(list.items ?? [])
      setCatalog(kinds)
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.alertChannels.toast.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [toastError, t])

  const loadSettings = useCallback(async () => {
    setSettingsLoading(true)
    try {
      const data = await getAlertSettings()
      setAlertEnabled(data.enabled)
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.alertChannels.settings.loadFailed'))
    } finally {
      setSettingsLoading(false)
    }
  }, [toastError, t])

  useEffect(() => {
    void load()
    void loadSettings()
  }, [load, loadSettings])

  const enabledCount = useMemo(() => items.filter((item) => item.enabled).length, [items])

  /** 切换外发总开关：成功后以后端返回值为准（避免本地状态与服务端不一致）。 */
  async function handleToggleAlertEnabled(next: boolean) {
    setSettingsSaving(true)
    try {
      const data = await updateAlertSettings(next)
      setAlertEnabled(data.enabled)
      toast(t('admin.alertChannels.settings.saved'))
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.alertChannels.settings.saveFailed'))
    } finally {
      setSettingsSaving(false)
    }
  }

  /** 发送测试：同步等结果，成功/失败都给明确反馈。 */
  async function handleTest(row: AlertChannel) {
    setBusyId(row.id)
    try {
      await testAlertChannel(row.id)
      toast(t('admin.alertChannels.toast.testSent'))
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.alertChannels.toast.testFailed'))
    } finally {
      setBusyId(0)
    }
  }

  /**
   * 就地启停：停用是"暂时别发"，不是删配置，因此不加确认弹层。
   * target 传空串 = 沿用原值（目标在界面上是脱敏的，抄不回原样）。
   */
  async function handleToggleEnabled(row: AlertChannel) {
    setBusyId(row.id)
    try {
      await updateAlertChannel(row.id, {
        name: row.name,
        kind: row.kind,
        target: '',
        events: row.events.join(','),
        enabled: !row.enabled,
      })
      toast(row.enabled ? t('admin.alertChannels.toast.toggleDisabled') : t('admin.alertChannels.toast.toggleEnabled'))
      void load()
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.alertChannels.toast.toggleFailed'))
    } finally {
      setBusyId(0)
    }
  }

  async function handleDelete() {
    if (!deleteTarget) return
    try {
      await deleteAlertChannel(deleteTarget.id)
      toast(t('admin.alertChannels.toast.deleted'))
      setDeleteTarget(null)
      void load()
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.alertChannels.toast.deleteFailed'))
    }
  }

  /** 事件键 → 展示文案（优先用接口下发的 text，未知键原样显示便于排查）。 */
  const eventTextMap = useMemo(
    () => new Map((catalog?.events ?? []).map((event) => [event.value, event.text])),
    [catalog],
  )

  /** 订阅事件展示：空 = 订阅全部，必须显式说明而不是显示"—"。 */
  function renderEvents(row: AlertChannel) {
    if (row.events.length === 0) {
      return <Badge tone="brand">{t('admin.alertChannels.events.all')}</Badge>
    }
    return (
      <span className="flex flex-wrap gap-1">
        {row.events.map((key) => (
          <Badge key={key} tone="info">
            {eventTextMap.get(key) ?? key}
          </Badge>
        ))}
      </span>
    )
  }

  const columns: Column<AlertChannel>[] = [
    {
      title: t('admin.alertChannels.col.name'),
      render: (row) => (
        <div className="min-w-0">
          <div className="truncate text-[13px] font-medium text-ink" title={row.name}>
            {row.name}
          </div>
          <div className="mt-0.5 text-[12px] text-ink-3">{row.kind_text}</div>
        </div>
      ),
    },
    {
      title: t('admin.alertChannels.col.kind'),
      render: (row) => <span className="text-ink-2">{row.kind_text}</span>,
    },
    {
      title: t('admin.alertChannels.col.target'),
      // 脱敏值在这里是唯一形态：不提供「显示明文」，因为根本没有明文可回传。
      render: (row) => (
        <code className="block max-w-72 truncate text-[12px] text-ink-2" title={row.target_masked}>
          {row.target_masked}
        </code>
      ),
    },
    { title: t('admin.alertChannels.col.events'), render: renderEvents },
    {
      title: t('admin.alertChannels.col.enabled'),
      align: 'center',
      render: (row) => (
        <div className="flex justify-center">
          <Switch
            checked={row.enabled}
            disabled={busyId === row.id}
            onChange={() => handleToggleEnabled(row)}
            label={t('admin.alertChannels.switchLabel', { name: row.name })}
          />
        </div>
      ),
    },
    {
      title: t('admin.alertChannels.col.createdAt'),
      className: 'hidden lg:table-cell',
      render: (row) => <span className="whitespace-nowrap text-[13px] text-ink-3">{formatDateTime(row.created_at)}</span>,
    },
    {
      title: t('admin.alertChannels.col.actions'),
      align: 'right',
      render: (row) => (
        <span className="flex items-center justify-end gap-3 text-[13px]">
          <button
            type="button"
            disabled={busyId === row.id}
            onClick={() => handleTest(row)}
            className="text-brand hover:underline disabled:opacity-50"
          >
            {t('admin.alertChannels.action.test')}
          </button>
          <button
            type="button"
            disabled={busyId === row.id}
            onClick={() => setEditing(row)}
            className="text-ink-3 hover:text-ink disabled:opacity-50"
          >
            {t('admin.alertChannels.action.edit')}
          </button>
          <button
            type="button"
            disabled={busyId === row.id}
            onClick={() => setDeleteTarget(row)}
            className="text-ink-3 hover:text-err disabled:opacity-50"
          >
            {t('admin.alertChannels.action.delete')}
          </button>
        </span>
      ),
    },
  ]

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-bold text-ink">{t('admin.alertChannels.title')}</h1>
          <p className="mt-0.5 text-[13px] text-ink-3">
            {t('admin.alertChannels.subtitle', { total: items.length, enabled: enabledCount })}
          </p>
        </div>
        <div className="flex items-center gap-3">
          <Button variant="secondary" onClick={() => void load()} loading={loading}>
            {t('admin.alertChannels.refresh')}
          </Button>
          <Button variant="primary" onClick={() => setEditing('new')} disabled={!catalog}>
            {t('admin.alertChannels.create')}
          </Button>
        </div>
      </div>

      {/* 外发总开关：默认关闭。配好通道不等于会收到告警，必须显式打开，故放在列表最上方。 */}
      <Card>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div className="text-[13px] font-semibold text-ink">{t('admin.alertChannels.settings.title')}</div>
            <p className="mt-0.5 text-[13px] text-ink-3">{t('admin.alertChannels.settings.notice')}</p>
          </div>
          <div className="flex items-center gap-3">
            <Badge tone={alertEnabled ? 'ok' : 'off'}>
              {alertEnabled ? t('admin.alertChannels.settings.enabled') : t('admin.alertChannels.settings.disabled')}
            </Badge>
            <Switch
              checked={alertEnabled}
              disabled={settingsLoading || settingsSaving}
              onChange={handleToggleAlertEnabled}
              label={t('admin.alertChannels.settings.switchLabel')}
            />
          </div>
        </div>
      </Card>

      <AlertEventExplainer catalog={catalog} />

      <Card padding="none">
        <DataTable
          columns={columns}
          rows={loading ? null : items}
          loading={loading}
          rowKey={(row) => row.id}
          emptyTitle={t('admin.alertChannels.emptyTitle')}
          emptyDescription={t('admin.alertChannels.emptyDescription')}
        />
      </Card>

      <AlertChannelFormModal
        open={editing !== null}
        channel={editing === 'new' ? null : editing}
        catalog={catalog}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          void load()
        }}
      />

      <ConfirmDialog
        open={Boolean(deleteTarget)}
        title={t('admin.alertChannels.delete.title')}
        message={t('admin.alertChannels.delete.message', { name: deleteTarget?.name ?? '' })}
        danger
        confirmText={t('admin.alertChannels.delete.confirm')}
        onConfirm={handleDelete}
        onCancel={() => setDeleteTarget(null)}
      />
    </div>
  )
}

/* ── 事件说明 ──────────────────────────────────────────────────────────── */

/**
 * 事件说明条：把"哪些事情会触发告警"直接摆在页面上。
 *
 * 为什么不做成折叠：这是站长唯一需要记住的配置知识，
 * 藏起来等于让人去翻文档，而文档往往不存在于自建站里。
 * 「测试消息」不作为告警事件列出，由本页的「发送测试」按钮单独触发。
 */
function AlertEventExplainer({ catalog }: { catalog: AlertChannelKinds | null }) {
  const { t } = useI18n()
  if (!catalog) return null
  const events = catalog.events.filter((event) => event.value !== TEST_EVENT_KEY)
  return (
    <Card>
      <div className="mb-2 text-[13px] font-semibold text-ink">{t('admin.alertChannels.explainer.title')}</div>
      <ul className="space-y-1.5">
        {events.map((event) => (
          <li key={event.value} className="flex gap-2 text-[13px] text-ink-2">
            <span className="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-ink-3" />
            <span>{event.text}</span>
          </li>
        ))}
      </ul>
      <p className="mt-3 text-[12px] leading-relaxed text-ink-3">{t('admin.alertChannels.explainer.note')}</p>
    </Card>
  )
}

/* ── 新建 / 编辑弹层 ───────────────────────────────────────────────────── */

function AlertChannelFormModal({
  open,
  channel,
  catalog,
  onClose,
  onSaved,
}: {
  open: boolean
  /** 非 null 表示编辑模式（此时 target 留空 = 沿用原值） */
  channel: AlertChannel | null
  catalog: AlertChannelKinds | null
  onClose: () => void
  onSaved: () => void
}) {
  const { toast, toastError } = useToast()
  const { t } = useI18n()
  const [name, setName] = useState('')
  const [kind, setKind] = useState('')
  const [target, setTarget] = useState('')
  const [events, setEvents] = useState<string[]>([])
  const [enabled, setEnabled] = useState(true)
  const [loading, setLoading] = useState(false)

  const isEdit = channel !== null

  useEffect(() => {
    if (!open) return
    setName(channel?.name ?? '')
    // 类型下拉的初始值：编辑时回显当前值，新建时取目录第一项（与后端 kind 语义一致）
    setKind(channel?.kind ?? catalog?.kinds[0]?.value ?? '')
    // 目标一律清空：编辑时"留空 = 沿用原值"，不回显脱敏串（它也不能当原值提交）
    setTarget('')
    setEvents(channel?.events ?? [])
    setEnabled(channel?.enabled ?? true)
  }, [open, channel, catalog])

  /** 可订阅事件：过滤掉仅由「发送测试」按钮触发的测试消息 */
  const subscribableEvents = useMemo(
    () => (catalog?.events ?? []).filter((event) => event.value !== TEST_EVENT_KEY),
    [catalog],
  )

  function toggleEvent(key: string) {
    setEvents((prev) => (prev.includes(key) ? prev.filter((item) => item !== key) : [...prev, key]))
  }

  async function handleSubmit() {
    if (!name.trim()) {
      toastError(t('admin.alertChannels.error.nameRequired'))
      return
    }
    // 编辑时目标留空是合法的（沿用原值），新建时必须填
    if (!isEdit && !target.trim()) {
      toastError(t('admin.alertChannels.error.targetRequired'))
      return
    }
    setLoading(true)
    try {
      const payload: AlertChannelPayload = {
        name: name.trim(),
        kind,
        // 空串 → 编辑沿用原值 / 新建由后端校验拒绝（页面已先拦一道）
        target: target.trim(),
        // 空数组 → 空串 → 后端解释为"订阅全部"
        events: events.join(','),
        enabled,
      }
      if (channel) {
        await updateAlertChannel(channel.id, payload)
        toast(t('admin.alertChannels.toast.updated'))
      } else {
        await createAlertChannel(payload)
        toast(t('admin.alertChannels.toast.created'))
      }
      onSaved()
    } catch (err) {
      toastError(err instanceof Error ? err.message : t('admin.alertChannels.toast.saveFailed'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={isEdit ? t('admin.alertChannels.form.editTitle') : t('admin.alertChannels.form.newTitle')}
      width={600}
    >
      <div className="space-y-4">
        <Field label={t('admin.alertChannels.form.name')} required help={t('admin.alertChannels.form.nameHelp')}>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t('admin.alertChannels.form.namePlaceholder')}
          />
        </Field>

        <Field label={t('admin.alertChannels.form.kind')} required>
          <Select value={kind} onChange={(e) => setKind(e.target.value)}>
            {(catalog?.kinds ?? []).map((option) => (
              <option key={option.value} value={option.value}>
                {option.text}
              </option>
            ))}
          </Select>
        </Field>

        <Field
          label={t('admin.alertChannels.form.target')}
          required={!isEdit}
          help={isEdit ? t('admin.alertChannels.form.targetHelpEdit') : t('admin.alertChannels.form.targetHelpNew')}
        >
          <Input
            value={target}
            onChange={(e) => setTarget(e.target.value)}
            placeholder={isEdit ? t('admin.alertChannels.form.targetPlaceholderEdit') : t('admin.alertChannels.form.targetPlaceholderNew')}
            autoComplete="off"
          />
        </Field>

        <Field label={t('admin.alertChannels.form.events')} help={t('admin.alertChannels.form.eventsHelp')}>
          <div className="grid gap-2 rounded-md border border-line-2 bg-surface p-3 sm:grid-cols-2">
            {subscribableEvents.map((event) => (
              <label key={event.value} className="flex items-center gap-2 text-[13px] text-ink-2">
                <input
                  type="checkbox"
                  checked={events.includes(event.value)}
                  onChange={() => toggleEvent(event.value)}
                  className="h-3.5 w-3.5 accent-[var(--brand)]"
                />
                {event.text}
              </label>
            ))}
            {events.length === 0 && (
              <p className="text-[12px] text-ink-3 sm:col-span-2">{t('admin.alertChannels.form.eventsAllHint')}</p>
            )}
          </div>
        </Field>

        <label className="flex items-center justify-between text-[13px] text-ink-2">
          <span>{t('admin.alertChannels.form.enabled')}</span>
          <Switch checked={enabled} onChange={setEnabled} label={t('admin.alertChannels.form.enabled')} />
        </label>
      </div>

      <div className="mt-5 flex justify-end gap-2">
        <Button variant="secondary" onClick={onClose}>
          {t('common.action.cancel')}
        </Button>
        <Button variant="primary" loading={loading} onClick={handleSubmit}>
          {isEdit ? t('admin.alertChannels.form.save') : t('admin.alertChannels.form.create')}
        </Button>
      </div>
    </Modal>
  )
}
