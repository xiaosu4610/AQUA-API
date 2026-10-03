/** 用户门户：任意门 → 多人推理（/console/playground/anydoor/findundercoveragent）
 *
 * 意图（Why）：
 *   一个由本站 AI 主持的「传设备」派对推理游戏：玩家围坐一台设备轮流操作，
 *   AI 在开局生成剧情与秘密身份，过程中可让 AI 推进剧情，最终投票揪出卧底特工。
 *   约束：顶部必须显示站点图标 + 站点名 + 游戏名 + 返回任意门按钮；开始前让用户
 *   自选模型；游戏内所有 AI 调用走本站 /v1/chat/completions（OpenAI 兼容网关），
 *   鉴权用玩家自己的 sk- 访问令牌（不是登录会话令牌）；不登录由 /console 布局守卫
 *   自动跳转到 /login；配色自定，但保持与站点设计语言一致（复用 brand / ink / line 等 token）。
 *
 * 流转（Flow）：
 *   setup（开局设置）→ briefing（公开剧情）→ reveal（传设备看身份）→ discuss（讨论计时 + AI 推进）
 *   → vote（传设备投票）→ result（代码计票 + AI 复盘旁白）；阶段切换由 phase 状态机驱动。
 *   全部用户可见文案走 i18n 词条（命名空间 portal.anydoorGame.*），AI 提示词也已词条化。
 *
 * 扩展（Extend）：
 *   新增阶段：在 Phase 联合类型加值 + 补对应 Panel 组件 + 在主渲染里按 phase 分支；
 *   新增文案：同时补 portal.anydoorGame.* 的六语言词条（键集合必须完全一致）；
 *   改 AI 设定：同步六语言的 prompt* 词条即可，勿动解析逻辑（role 枚举 undercover/citizen 与 JSON 结构固定）。
 */
'use client'

import { useCallback, useEffect, useState } from 'react'

import Link from 'next/link'

import { Badge, Card } from '@/components/ui/Display'
import { Button } from '@/components/ui/Button'
import { Field, Input, Select } from '@/components/ui/Form'
import { BrandLogo } from '@/components/BrandMark'
import { fetchModelPlaza } from '@/api/site'
import { useI18n } from '@/i18n'
import { useSite } from '@/lib/site/site-context'
import { useToast } from '@/lib/toast/toast-context'

type Phase = 'setup' | 'briefing' | 'reveal' | 'discuss' | 'vote' | 'result'

interface PlayerRole {
  name: string
  role: 'undercover' | 'citizen'
  secret: string
}

interface GameSetup {
  scenario: string
  roles: PlayerRole[]
}

const DISCUSS_SECONDS = 120

export default function FindUndercoverAgentPage() {
  const { t } = useI18n()
  const { siteName } = useSite()
  const { toastError } = useToast()

  // 模型与令牌（开始前自选）
  const [models, setModels] = useState<string[]>([])
  const [model, setModel] = useState('')
  const [token, setToken] = useState('')

  // 开局设置
  const [playerCount, setPlayerCount] = useState(5)
  const [nameInput, setNameInput] = useState('')

  // 运行时
  const [phase, setPhase] = useState<Phase>('setup')
  const [setup, setSetup] = useState<GameSetup | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  // 身份分发（传设备）
  const [revealIdx, setRevealIdx] = useState(0)
  const [revealShown, setRevealShown] = useState(false)

  // 讨论计时
  const [secsLeft, setSecsLeft] = useState(DISCUSS_SECONDS)
  const [timerOn, setTimerOn] = useState(false)
  const [twist, setTwist] = useState('')
  const [twistStreaming, setTwistStreaming] = useState(false)

  // 投票（传设备）
  const [votes, setVotes] = useState<Record<number, number>>({})
  const [voteIdx, setVoteIdx] = useState(0)
  const [voteTarget, setVoteTarget] = useState<number | null>(null)

  // 结算
  const [resultText, setResultText] = useState('')
  const [resultStreaming, setResultStreaming] = useState(false)

  // 模型列表：打开即加载（取自公开广场，与游乐场一致）
  useEffect(() => {
    let alive = true
    fetchModelPlaza()
      .then((p) => {
        if (!alive) return
        const names = p.items.map((i) => i.model)
        setModels(names)
        setModel((prev) => prev || names[0] || '')
      })
      .catch(() => {
        /* 加载失败静默：用户仍可看到空列表提示 */
      })
    return () => {
      alive = false
    }
  }, [])

  // 讨论倒计时
  useEffect(() => {
    if (!timerOn) return
    if (secsLeft <= 0) {
      setTimerOn(false)
      return
    }
    const timer = setTimeout(() => setSecsLeft((s) => s - 1), 1000)
    return () => clearTimeout(timer)
  }, [timerOn, secsLeft])

  // 调用本站 AI（/v1/chat/completions，自选模型 + sk- 令牌）
  const callAI = useCallback(
    async (system: string, user: string, onDelta?: (text: string) => void): Promise<string> => {
      const res = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token.trim()}` },
        body: JSON.stringify({
          model: model.trim(),
          messages: [
            { role: 'system', content: system },
            { role: 'user', content: user },
          ],
          stream: Boolean(onDelta),
        }),
      })
      if (!res.ok || !res.body) {
        let message = t('portal.anydoorGame.requestFailed', { status: res.status })
        try {
          const data = (await res.json()) as { error?: { message?: string } }
          message = data?.error?.message || message
        } catch {
          /* 保留默认提示 */
        }
        throw new Error(message)
      }
      if (onDelta) {
        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        let buf = ''
        let full = ''
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          buf += decoder.decode(value, { stream: true })
          const lines = buf.split('\n')
          buf = lines.pop() ?? ''
          for (const line of lines) {
            const trimmed = line.trim()
            if (!trimmed.startsWith('data:')) continue
            const payload = trimmed.slice(5).trim()
            if (payload === '[DONE]') continue
            try {
              const json = JSON.parse(payload) as { choices?: { delta?: { content?: string } }[] }
              const delta = json.choices?.[0]?.delta?.content
              if (delta) {
                full += delta
                onDelta(full)
              }
            } catch {
              /* 忽略不可解析块 */
            }
          }
        }
        return full
      }
      const data = (await res.json()) as { choices?: { message?: { content?: string } }[] }
      return data.choices?.[0]?.message?.content ?? ''
    },
    [token, model, t],
  )

  // 解析 AI 返回的开局设定（容错：缺卧底则随机指定，多卧底则只留一个）
  const parseSetup = useCallback(
    (raw: string, names: string[]): GameSetup => {
      let text = raw.trim()
      const fence = text.match(/```(?:json)?\s*([\s\S]*?)```/i)
      if (fence) text = fence[1].trim()
      const obj = JSON.parse(text) as {
        scenario?: string
        roles?: { role?: string; secret?: string }[]
      }
      const scenario = String(obj.scenario ?? t('portal.anydoorGame.defaultScenario'))
      const src = Array.isArray(obj.roles) ? obj.roles : []
      let roles: PlayerRole[] = names.map((name, i) => {
        const r = src[i] ?? {}
        const role: PlayerRole['role'] = r.role === 'undercover' ? 'undercover' : 'citizen'
        return { name, role, secret: String(r.secret ?? '') }
      })
      if (!roles.some((r) => r.role === 'undercover')) {
        const pick = Math.floor(Math.random() * roles.length)
        roles[pick] = { ...roles[pick], role: 'undercover' }
      }
      let found = false
      roles = roles.map((r) => {
        if (r.role === 'undercover') {
          if (found) return { ...r, role: 'citizen' }
          found = true
        }
        return r
      })
      return { scenario, roles }
    },
    [t],
  )

  // 把「人数 + 昵称输入」解析成玩家名列表
  function resolveNames(): string[] {
    const custom = nameInput
      .split(/[\n,，、]/)
      .map((s) => s.trim())
      .filter(Boolean)
    if (custom.length >= 3) return custom.slice(0, 12)
    const n = Math.max(3, Math.min(12, playerCount || 3))
    return Array.from({ length: n }, (_, i) => t('portal.anydoorGame.playerName', { n: i + 1 }))
  }

  // 开局：生成剧情 + 身份
  const startGame = useCallback(async () => {
    const names = resolveNames()
    if (names.length < 3) {
      toastError(t('portal.anydoorGame.errMinPlayers'))
      return
    }
    if (!model) {
      toastError(t('portal.anydoorGame.errSelectModel'))
      return
    }
    if (!token.trim().startsWith('sk-')) {
      toastError(t('portal.anydoorGame.errToken'))
      return
    }
    setLoading(true)
    setError('')
    try {
      const system = t('portal.anydoorGame.promptStartSystem', { count: names.length })
      const user = t('portal.anydoorGame.promptStartUser', { names: names.join('、') })
      const raw = await callAI(system, user)
      const next = parseSetup(raw, names)
      setSetup(next)
      setRevealIdx(0)
      setRevealShown(false)
      setVotes({})
      setVoteIdx(0)
      setVoteTarget(null)
      setTwist('')
      setResultText('')
      setSecsLeft(DISCUSS_SECONDS)
      setTimerOn(false)
      setPhase('briefing')
    } catch (e) {
      setError((e as Error).message || t('portal.anydoorGame.errGenerate'))
    } finally {
      setLoading(false)
    }
  }, [model, token, callAI, parseSetup, toastError, nameInput, playerCount, t])

  // 讨论阶段：让 AI 推进一段剧情（可选功能，同一模型）
  const generateTwist = useCallback(async () => {
    if (!setup) return
    setTwistStreaming(true)
    setTwist('')
    try {
      const system = t('portal.anydoorGame.promptTwistSystem')
      const user = t('portal.anydoorGame.promptTwistUser', {
        scenario: setup.scenario,
        players: setup.roles.map((r) => r.name).join('、'),
      })
      await callAI(system, user, (text) => setTwist(text))
    } catch (e) {
      setTwist((e as Error).message || t('portal.anydoorGame.twistFailed'))
    } finally {
      setTwistStreaming(false)
    }
  }, [setup, callAI, t])

  // 投票：提交当前投票者选择
  const commitVote = useCallback(() => {
    if (voteTarget === null || !setup) return
    const nextVotes = { ...votes, [voteIdx]: voteTarget }
    if (voteIdx >= setup.roles.length - 1) {
      setVotes(nextVotes)
      setVoteIdx(0)
      setVoteTarget(null)
      void revealResult(nextVotes)
    } else {
      setVotes(nextVotes)
      setVoteIdx((i) => i + 1)
      setVoteTarget(null)
    }
  }, [voteTarget, votes, voteIdx, setup])

  // 结算：代码计票 + AI 复盘旁白
  const revealResult = useCallback(
    async (finalVotes: Record<number, number>) => {
      if (!setup) return
      setPhase('result')
      const n = setup.roles.length
      const tally = Array.from({ length: n }, (_, i) =>
        Object.values(finalVotes).filter((v) => v === i).length,
      )
      const undercoverIdx = setup.roles.findIndex((r) => r.role === 'undercover')
      const maxVotes = Math.max(...tally)
      const expelled = tally
        .map((c, i) => (c === maxVotes && maxVotes > 0 ? i : -1))
        .filter((i) => i >= 0)
      const undercoverCaught = expelled.includes(undercoverIdx)
      const winner = undercoverCaught
        ? t('portal.anydoorGame.winnerCitizens')
        : t('portal.anydoorGame.roleUndercover')

      setResultStreaming(true)
      setResultText('')
      try {
        const system = t('portal.anydoorGame.promptResultSystem')
        const rolesText = setup.roles
          .map(
            (r) =>
              `${r.name}(${
                r.role === 'undercover'
                  ? t('portal.anydoorGame.roleUndercover')
                  : t('portal.anydoorGame.roleCitizen')
              })`,
          )
          .join('、')
        const votesText = setup.roles
          .map((r, i) =>
            t('portal.anydoorGame.voteCastedTo', {
              voter: r.name,
              target: setup.roles[finalVotes[i]]?.name ?? t('portal.anydoorGame.voteAbstain'),
            }),
          )
          .join('；')
        const lines = [
          t('portal.anydoorGame.resultLineScenario', { scenario: setup.scenario }),
          t('portal.anydoorGame.resultLineRoles', { roles: rolesText }),
          t('portal.anydoorGame.resultLineVotes', { votes: votesText }),
          t('portal.anydoorGame.resultLineOutcome', {
            expelled: expelled.map((i) => setup.roles[i].name).join('、'),
            maxVotes,
            undercover: setup.roles[undercoverIdx].name,
            winner,
          }),
        ].join('\n')
        await callAI(system, lines, (text) => setResultText(text))
      } catch {
        setResultText(
          t('portal.anydoorGame.resultFallback', {
            winner,
            name: setup.roles[undercoverIdx].name,
          }),
        )
      } finally {
        setResultStreaming(false)
      }
    },
    [setup, callAI, t],
  )

  // 重开
  const reset = useCallback(() => {
    setSetup(null)
    setPhase('setup')
    setError('')
    setTwist('')
    setResultText('')
    setVotes({})
    setRevealIdx(0)
    setVoteIdx(0)
  }, [])

  return (
    <div className="mx-auto max-w-3xl space-y-4">
      {/* 游戏自有顶栏：站点图标 + 站名 | 游戏名 | 返回任意门 */}
      <header className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-line bg-linear-to-r from-brand/10 to-transparent px-4 py-3">
        <div className="flex items-center gap-2">
          <BrandLogo name={siteName} />
        </div>
        <div className="order-3 w-full text-center text-[15px] font-semibold text-ink sm:order-2 sm:w-auto">
          {t('portal.anydoorGame.title')}
        </div>
        <Link href="/console/playground/anydoor" className="order-2 sm:order-3">
          <Button variant="secondary" size="sm">
            {t('portal.anydoorGame.back')}
          </Button>
        </Link>
      </header>

      {error && (
        <div className="rounded-md border border-err/25 bg-err/8 px-3 py-2 text-[13px] text-err">{error}</div>
      )}

      {phase === 'setup' && (
        <SetupPanel
          models={models}
          model={model}
          onModel={setModel}
          token={token}
          onToken={setToken}
          playerCount={playerCount}
          onPlayerCount={setPlayerCount}
          nameInput={nameInput}
          onNameInput={setNameInput}
          loading={loading}
          onStart={startGame}
        />
      )}

      {phase === 'briefing' && setup && (
        <Card className="space-y-4">
          <div>
            <h2 className="text-[15px] font-semibold text-ink">{t('portal.anydoorGame.scenarioTitle')}</h2>
            <p className="mt-2 whitespace-pre-wrap text-[14px] leading-relaxed text-ink-2">{setup.scenario}</p>
          </div>
          <div className="rounded-md border border-line bg-surface px-3 py-2 text-[13px] text-ink-3">
            {t('portal.anydoorGame.briefingHintBefore')}
            <b className="text-ink">{t('portal.anydoorGame.secretIdentity')}</b>
            {t('portal.anydoorGame.briefingHintAfter')}
          </div>
          <div className="flex justify-end">
            <Button
              variant="primary"
              onClick={() => {
                setRevealIdx(0)
                setRevealShown(false)
                setPhase('reveal')
              }}
            >
              {t('portal.anydoorGame.startReveal')}
            </Button>
          </div>
        </Card>
      )}

      {phase === 'reveal' && setup && (
        <RevealPanel
          setup={setup}
          idx={revealIdx}
          shown={revealShown}
          onShow={() => setRevealShown(true)}
          onNext={() => {
            if (revealIdx >= setup.roles.length - 1) {
              setRevealShown(false)
              setPhase('discuss')
            } else {
              setRevealIdx((i) => i + 1)
              setRevealShown(false)
            }
          }}
        />
      )}

      {phase === 'discuss' && setup && (
        <DiscussPanel
          scenario={setup.scenario}
          twist={twist}
          twistStreaming={twistStreaming}
          secsLeft={secsLeft}
          timerOn={timerOn}
          onToggleTimer={() => setTimerOn((v) => !v)}
          onResetTimer={() => {
            setSecsLeft(DISCUSS_SECONDS)
            setTimerOn(false)
          }}
          onTwist={generateTwist}
          onVote={() => {
            setVoteIdx(0)
            setVoteTarget(null)
            setPhase('vote')
          }}
        />
      )}

      {phase === 'vote' && setup && (
        <VotePanel
          setup={setup}
          idx={voteIdx}
          target={voteTarget}
          onTarget={setVoteTarget}
          onCommit={commitVote}
        />
      )}

      {phase === 'result' && setup && (
        <ResultPanel
          setup={setup}
          votes={votes}
          text={resultText}
          streaming={resultStreaming}
          onRestart={reset}
        />
      )}
    </div>
  )
}

/* ── 子组件 ───────────────────────────────────────────── */

function SetupPanel(props: {
  models: string[]
  model: string
  onModel: (v: string) => void
  token: string
  onToken: (v: string) => void
  playerCount: number
  onPlayerCount: (v: number) => void
  nameInput: string
  onNameInput: (v: string) => void
  loading: boolean
  onStart: () => void
}) {
  const { models, model, onModel, token, onToken, playerCount, onPlayerCount, nameInput, onNameInput, loading, onStart } = props
  const { t } = useI18n()
  return (
    <Card className="space-y-4">
      <div>
        <h2 className="text-[15px] font-semibold text-ink">{t('portal.anydoorGame.setupTitle')}</h2>
        <p className="mt-1 text-[13px] text-ink-3">{t('portal.anydoorGame.setupDesc')}</p>
      </div>

      <Field
        label={t('portal.anydoorGame.modelLabel')}
        required
        help={t('portal.anydoorGame.modelHelp')}
      >
        <Select value={model} onChange={(e) => onModel(e.target.value)}>
          {models.length === 0 && <option value="">{t('portal.anydoorGame.loadingModels')}</option>}
          {models.map((m) => (
            <option key={m} value={m}>{m}</option>
          ))}
        </Select>
      </Field>

      <Field
        label={t('portal.anydoorGame.tokenLabel')}
        required
        help={t('portal.anydoorGame.tokenHelp')}
      >
        <Input
          type="password"
          value={token}
          onChange={(e) => onToken(e.target.value)}
          placeholder={t('portal.anydoorGame.tokenPlaceholder')}
        />
      </Field>

      <div className="grid gap-4 sm:grid-cols-2">
        <Field label={t('portal.anydoorGame.playerCountLabel')} help={t('portal.anydoorGame.playerCountHelp')}>
          <Input
            type="number"
            min={3}
            max={12}
            value={playerCount}
            onChange={(e) => onPlayerCount(Number(e.target.value))}
          />
        </Field>
        <Field label={t('portal.anydoorGame.namesLabel')} help={t('portal.anydoorGame.namesHelp')}>
          <Input
            value={nameInput}
            onChange={(e) => onNameInput(e.target.value)}
            placeholder={t('portal.anydoorGame.namesPlaceholder')}
          />
        </Field>
      </div>

      <div className="flex items-center gap-3 pt-1">
        <Button variant="primary" onClick={onStart} loading={loading}>
          {loading ? t('portal.anydoorGame.starting') : t('portal.anydoorGame.start')}
        </Button>
        <span className="text-xs text-ink-3">{t('portal.anydoorGame.setupHint')}</span>
      </div>
    </Card>
  )
}

function RevealPanel(props: {
  setup: GameSetup
  idx: number
  shown: boolean
  onShow: () => void
  onNext: () => void
}) {
  const { setup, idx, shown, onShow, onNext } = props
  const { t } = useI18n()
  const player = setup.roles[idx]
  const isLast = idx >= setup.roles.length - 1
  return (
    <Card className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-[15px] font-semibold text-ink">{t('portal.anydoorGame.revealTitle')}</h2>
        <Badge tone="info">{idx + 1} / {setup.roles.length}</Badge>
      </div>

      <p className="text-[13px] text-ink-3">
        {t('portal.anydoorGame.revealHintBefore')}
        <b className="text-ink">{player.name}</b>
        {t('portal.anydoorGame.revealHintAfter')}
      </p>

      {!shown ? (
        <div className="flex justify-center py-6">
          <Button variant="primary" onClick={onShow}>
            {t('portal.anydoorGame.revealButton', { name: player.name })}
          </Button>
        </div>
      ) : (
        <div className="rounded-lg border border-line bg-surface px-4 py-5 text-center">
          <div className="text-[13px] text-ink-3">{t('portal.anydoorGame.yourRole')}</div>
          <div className={`mt-1 text-2xl font-bold ${player.role === 'undercover' ? 'text-err' : 'text-ok'}`}>
            {player.role === 'undercover'
              ? t('portal.anydoorGame.roleUndercover')
              : t('portal.anydoorGame.roleCitizen')}
          </div>
          {player.secret && (
            <p className="mt-3 text-[14px] leading-relaxed text-ink-2">{player.secret}</p>
          )}
          <p className="mt-3 text-xs text-ink-3">{t('portal.anydoorGame.revealAfter')}</p>
        </div>
      )}

      <div className="flex justify-end">
        <Button variant="secondary" onClick={onNext} disabled={!shown}>
          {isLast ? t('portal.anydoorGame.revealNextLast') : t('portal.anydoorGame.revealNext')}
        </Button>
      </div>
    </Card>
  )
}

function DiscussPanel(props: {
  scenario: string
  twist: string
  twistStreaming: boolean
  secsLeft: number
  timerOn: boolean
  onToggleTimer: () => void
  onResetTimer: () => void
  onTwist: () => void
  onVote: () => void
}) {
  const { scenario, twist, twistStreaming, secsLeft, timerOn, onToggleTimer, onResetTimer, onTwist, onVote } = props
  const { t } = useI18n()
  const mm = String(Math.floor(secsLeft / 60)).padStart(2, '0')
  const ss = String(secsLeft % 60).padStart(2, '0')
  return (
    <Card className="space-y-4">
      <h2 className="text-[15px] font-semibold text-ink">{t('portal.anydoorGame.discussTitle')}</h2>
      <p className="whitespace-pre-wrap text-[14px] leading-relaxed text-ink-2">{scenario}</p>

      <div className="flex flex-wrap items-center gap-3 rounded-md border border-line bg-surface px-3 py-2">
        <span className="font-mono text-lg text-ink">{mm}:{ss}</span>
        <Button variant="secondary" size="sm" onClick={onToggleTimer}>
          {timerOn ? t('portal.anydoorGame.timerPause') : t('portal.anydoorGame.timerStart')}
        </Button>
        <Button variant="ghost" size="sm" onClick={onResetTimer}>
          {t('portal.anydoorGame.timerReset')}
        </Button>
        <span className="text-xs text-ink-3">{t('portal.anydoorGame.discussHint')}</span>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <Button variant="secondary" size="sm" onClick={onTwist} loading={twistStreaming}>
          {twistStreaming ? t('portal.anydoorGame.twistLoading') : t('portal.anydoorGame.twist')}
        </Button>
      </div>
      {twist && (
        <div className="whitespace-pre-wrap rounded-md border border-line bg-card px-3 py-2 text-[14px] leading-relaxed text-ink-2">
          {twist}
        </div>
      )}

      <div className="flex justify-end">
        <Button variant="primary" onClick={onVote}>
          {t('portal.anydoorGame.startVote')}
        </Button>
      </div>
    </Card>
  )
}

function VotePanel(props: {
  setup: GameSetup
  idx: number
  target: number | null
  onTarget: (v: number) => void
  onCommit: () => void
}) {
  const { setup, idx, target, onTarget, onCommit } = props
  const { t } = useI18n()
  const voter = setup.roles[idx]
  const isLast = idx >= setup.roles.length - 1
  return (
    <Card className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-[15px] font-semibold text-ink">{t('portal.anydoorGame.voteTitle')}</h2>
        <Badge tone="info">{idx + 1} / {setup.roles.length}</Badge>
      </div>
      <p className="text-[13px] text-ink-3">
        {t('portal.anydoorGame.voteHintBefore')}
        <b className="text-ink">{voter.name}</b>
        {t('portal.anydoorGame.voteHintAfter')}
      </p>

      <div className="grid gap-2 sm:grid-cols-2">
        {setup.roles.map((r, i) => {
          if (i === idx) return null
          const active = target === i
          return (
            <button
              key={r.name}
              type="button"
              onClick={() => onTarget(i)}
              className={`rounded-lg border px-3 py-2.5 text-left text-[14px] transition ${
                active
                  ? 'border-brand bg-brand/8 font-medium text-brand'
                  : 'border-line bg-card text-ink-2 hover:border-ink-3'
              }`}
            >
              {r.name}
            </button>
          )
        })}
      </div>

      <div className="flex justify-end">
        <Button variant="primary" onClick={onCommit} disabled={target === null}>
          {isLast ? t('portal.anydoorGame.voteNextLast') : t('portal.anydoorGame.voteNext')}
        </Button>
      </div>
    </Card>
  )
}

function ResultPanel(props: {
  setup: GameSetup
  votes: Record<number, number>
  text: string
  streaming: boolean
  onRestart: () => void
}) {
  const { setup, votes, text, streaming, onRestart } = props
  const { t } = useI18n()
  const n = setup.roles.length
  const tally = Array.from({ length: n }, (_, i) =>
    Object.values(votes).filter((v) => v === i).length,
  )
  const undercoverIdx = setup.roles.findIndex((r) => r.role === 'undercover')
  const maxVotes = Math.max(...tally)
  const expelled = tally
    .map((c, i) => (c === maxVotes && maxVotes > 0 ? i : -1))
    .filter((i) => i >= 0)
  const undercoverCaught = expelled.includes(undercoverIdx)
  const winner = undercoverCaught
    ? t('portal.anydoorGame.winnerCitizens')
    : t('portal.anydoorGame.roleUndercover')

  return (
    <Card className="space-y-4">
      <h2 className="text-[15px] font-semibold text-ink">{t('portal.anydoorGame.resultTitle')}</h2>

      <div className="grid gap-3 sm:grid-cols-2">
        <div className="rounded-lg border border-line bg-surface px-3 py-2">
          <div className="text-xs text-ink-3">{t('portal.anydoorGame.winnerLabel')}</div>
          <div className="mt-0.5 text-lg font-bold text-brand">{winner}</div>
        </div>
        <div className="rounded-lg border border-line bg-surface px-3 py-2">
          <div className="text-xs text-ink-3">{t('portal.anydoorGame.roleUndercover')}</div>
          <div className="mt-0.5 text-lg font-bold text-err">{setup.roles[undercoverIdx].name}</div>
        </div>
      </div>

      <div>
        <div className="mb-1.5 text-[13px] text-ink-3">{t('portal.anydoorGame.tallyTitle')}</div>
        <div className="space-y-1.5">
          {setup.roles.map((r, i) => (
            <div key={r.name} className="flex items-center gap-2 text-[13px]">
              <span className="w-20 shrink-0 text-ink-2">{r.name}</span>
              <span className="h-2.5 flex-1 overflow-hidden rounded bg-ink/10">
                <span
                  className={`block h-full ${i === undercoverIdx ? 'bg-err' : 'bg-brand'}`}
                  style={{ width: `${maxVotes ? (tally[i] / maxVotes) * 100 : 0}%` }}
                />
              </span>
              <span className="w-8 shrink-0 text-right font-mono text-ink-2">{tally[i]}</span>
            </div>
          ))}
        </div>
      </div>

      {text && (
        <div className="whitespace-pre-wrap rounded-md border border-line bg-card px-3 py-3 text-[14px] leading-relaxed text-ink-2">
          {text}
          {streaming && <span className="ml-0.5 animate-pulse text-ink-3">▍</span>}
        </div>
      )}

      <div className="flex justify-end">
        <Button variant="primary" onClick={onRestart}>
          {t('portal.anydoorGame.restart')}
        </Button>
      </div>
    </Card>
  )
}
