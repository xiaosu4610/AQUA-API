/** 用户门户：任意门（/console/playground/anydoor）—— 游戏/功能入口列表页。
 *
 * 意图（Why）：
 *   游乐场下的功能入口页：用长条卡片列出站点内的休闲小游戏与互动功能，
 *   让用户一眼看清每个入口的标题、简介与可用状态（开放中 / 维护中）。
 *
 * 流转（Flow）：
 *   本页 → GAMES 数组（页面唯一数据源）→ 逐项渲染 Card + Badge + Button；
 *   状态为 open 的入口经 next/link 跳到对应子页（目前仅「多人推理」已落地）。
 *   全部文案（标题/副标题、说明区、各游戏卡片、状态徽章）走 i18n 词条。
 *
 * 扩展（Extend）：
 *   增删或调整入口只改下方 GAMES 数组：给新项一个英文短 id，并同时补
 *   portal.anydoor.games.<id>.title / .desc 的六语言词条；新游戏上线时把该项
 *   status 由 maintenance 改为 open 并填上真实 href 即可，无需改动渲染逻辑。
 */
'use client'

import Link from 'next/link'

import { Badge, Card } from '@/components/ui/Display'
import { Button } from '@/components/ui/Button'
import { useI18n } from '@/i18n'

/** 游戏/功能入口数据源：后续新增或调整入口，仅需修改此数组 */
interface GameEntry {
  /** 英文短标识：用于拼 i18n 键（portal.anydoor.games.<id>.title / .desc）与 React key */
  id: string
  /** 入口状态：open=可进入；maintenance=路线图项（渲染为禁用的「暂不可用」按钮） */
  status: 'open' | 'maintenance'
  /** 目标路由：仅 status=open 时被跳转使用 */
  href: string
}

// 已实现的入口为 open；其余保留为路线图（maintenance），避免点击落到不存在的路由。
const GAMES: GameEntry[] = [
  {
    id: 'undercover',
    status: 'open',
    href: '/console/playground/anydoor/findundercoveragent',
  },
  {
    id: 'tictactoe',
    status: 'maintenance',
    href: '/console/playground/tictactoe',
  },
  {
    id: 'wheel',
    status: 'maintenance',
    href: '/console/playground/wheel',
  },
  {
    id: 'pinch',
    status: 'maintenance',
    href: '/console/playground/pinch',
  },
  {
    id: 'looptap',
    status: 'maintenance',
    href: '/console/playground/looptap',
  },
  {
    id: 'fruit',
    status: 'maintenance',
    href: '/console/playground/fruit',
  },
]

// 状态 → 徽章语义（复用项目既有 Badge tone，不引入新配色）；labelKey 为 i18n 词条键
const STATUS_META: Record<GameEntry['status'], { tone: 'ok' | 'warn'; labelKey: string }> = {
  open: { tone: 'ok', labelKey: 'portal.anydoor.statusOpen' },
  maintenance: { tone: 'warn', labelKey: 'portal.anydoor.statusMaintenance' },
}

export default function ConsoleAnyDoorPage() {
  const { t } = useI18n()
  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-bold text-ink">{t('portal.anydoor.title')}</h1>
        <p className="mt-0.5 text-[13px] text-ink-3">{t('portal.anydoor.subtitle')}</p>
      </div>

      <div className="grid gap-5 lg:grid-cols-[1fr_300px]">
        {/* 左：任意门说明 */}
        <Card className="flex flex-col">
          <h2 className="text-[15px] font-semibold text-ink">{t('portal.anydoor.aboutTitle')}</h2>
          <p className="mt-2 text-[13px] leading-relaxed text-ink-3">{t('portal.anydoor.aboutBody')}</p>
        </Card>

        {/* 右：游戏入口长条卡片列表（数据驱动，打开即渲染） */}
        <div className="space-y-3">
          {GAMES.map((game) => {
            const meta = STATUS_META[game.status]
            const playable = game.status === 'open'
            return (
              <Card key={game.id} className="rounded-lg">
                <div className="flex flex-col gap-2.5">
                  <div className="flex items-start justify-between gap-2">
                    <h3 className="text-[15px] font-semibold text-ink">
                      {t(`portal.anydoor.games.${game.id}.title`)}
                    </h3>
                    <Badge tone={meta.tone}>{t(meta.labelKey)}</Badge>
                  </div>
                  <p className="line-clamp-2 text-[13px] leading-relaxed text-ink-3">
                    {t(`portal.anydoor.games.${game.id}.desc`)}
                  </p>
                  <div className="pt-0.5">
                    {playable ? (
                      <Link href={game.href}>
                        <Button variant="primary" size="sm">
                          {t('portal.anydoor.enter')}
                        </Button>
                      </Link>
                    ) : (
                      <Button variant="secondary" size="sm" disabled>
                        {t('portal.anydoor.unavailable')}
                      </Button>
                    )}
                  </div>
                </div>
              </Card>
            )
          })}
        </div>
      </div>
    </div>
  )
}
