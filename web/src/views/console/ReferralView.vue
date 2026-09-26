<script setup lang="ts">
/**
 * 用户门户 · 邀请返利 & 每日签到。
 *
 * 意图（Why）：
 *   把"拉新"与"留存"两件事放在同一页：
 *     1) 邀请卡片让用户一眼拿到自己的邀请码与邀请链接，并看到邀请人数与累计返利；
 *     2) 签到卡片把"今天签没签、连续几天、累计多少"讲清楚，并给一个明确的签到按钮。
 *   额度奖励的发放规则（注册奖、返利比例）完全由后端设置决定，前端只做展示，
 *   避免把规则复制到前端后与后端失配。
 *
 * 流转（Flow）：
 *   onMounted → fetchReferral() → 渲染邀请码/链接/人数/累计返利 + 签到概况
 *   点击签到 → checkin() → 用返回的最新状态刷新签到卡片（无需再发一次查询）
 *
 * 扩展（Extend）：
 *   新增邀请玩法（邀请排行榜、阶梯奖励）时，在本页追加区块并复用 fetchReferral 的数据；
 *   不要在页面里硬编码奖励数值（一律来自后端）。
 */
import { computed, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import CopyButton from '@/components/CopyButton.vue'
import DataState from '@/components/DataState.vue'
import { ApiError } from '@/api/client'
import { checkin, fetchReferral, type ReferralInfo } from '@/api/referral'
import { toastError, toastSuccess } from '@/composables/useToast'
import { formatNumber } from '@/utils/format'

const info = ref<ReferralInfo | null>(null)
const loading = ref(true)
const errorMessage = ref('')
const checkingIn = ref(false)

/** 完整邀请链接：邀请码只带路径，域名用当前站点（部署在哪个域名就分享哪个） */
const inviteLink = computed(() => {
  if (!info.value) return ''
  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  return origin + info.value.invite_path
})

async function load(): Promise<void> {
  loading.value = true
  errorMessage.value = ''
  try {
    info.value = await fetchReferral()
  } catch (error) {
    info.value = null
    errorMessage.value = error instanceof ApiError ? error.message : '邀请信息加载失败'
  } finally {
    loading.value = false
  }
}

async function handleCheckin(): Promise<void> {
  if (checkingIn.value || !info.value) return
  checkingIn.value = true
  try {
    const status = await checkin()
    info.value = { ...info.value, checkin: status }
    if (status.daily_quota > 0) {
      toastSuccess(`签到成功，获得 ${formatNumber(status.daily_quota)} 额度`)
    } else {
      toastSuccess('签到成功')
    }
  } catch (error) {
    toastError(error instanceof ApiError ? error.message : '签到失败，请稍后重试')
    // 失败可能是"今天已签过"：刷新一次状态让按钮进入已签态
    void load()
  } finally {
    checkingIn.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">邀请返利</h2>
        <p class="page-desc">把邀请链接分享给朋友：对方注册即得奖励，对方充值你还可持续获得返利。</p>
      </div>
      <RouterLink to="/console" class="btn btn-secondary btn-sm">
        <AppIcon name="home" :size="14" />
        返回概览
      </RouterLink>
    </div>

    <DataState
      :loading="loading"
      :error="errorMessage"
      :empty="!loading && !errorMessage && !info"
      loading-text="正在读取邀请信息…"
      empty-text="暂无邀请信息"
      @retry="load"
    />

    <div v-if="info" class="grid gap-5 lg:grid-cols-[1.15fr_1fr]">
      <!-- ── 邀请卡片 ───────────────────────────────────── -->
      <section class="card card-pad">
        <h3 class="section-title flex items-center gap-2">
          <AppIcon name="users" :size="16" class="text-brand-700" />
          我的邀请码
        </h3>

        <div class="mt-4 flex items-center gap-2">
          <code class="chip flex-1 text-center text-lg font-semibold tracking-[0.3em]">{{ info.invite_code }}</code>
          <CopyButton :value="info.invite_code" small outline />
        </div>

        <div class="mt-4">
          <label class="label" for="referral-link">邀请链接</label>
          <div class="flex items-center gap-2">
            <input id="referral-link" class="input input-mono flex-1" type="text" :value="inviteLink" readonly />
            <CopyButton :value="inviteLink" label="复制" small outline />
          </div>
          <p class="hint">
            好友通过该链接注册即视为你的邀请；注册奖励与充值返利由管理员在后台配置。
          </p>
        </div>

        <div class="mt-5 grid grid-cols-2 gap-3">
          <div class="rounded-lg border border-ink-800 px-3 py-3">
            <p class="text-xs text-ink-500">已邀请</p>
            <p class="mt-1 font-mono text-xl font-semibold text-ink-100">{{ formatNumber(info.invited_count) }}</p>
          </div>
          <div class="rounded-lg border border-ink-800 px-3 py-3">
            <p class="text-xs text-ink-500">累计返利</p>
            <p class="mt-1 font-mono text-xl font-semibold text-brand-700">{{ formatNumber(info.total_reward_quota) }}</p>
          </div>
        </div>

        <ul class="mt-4 space-y-1.5 text-xs leading-relaxed text-ink-400">
          <li>
            · 好友注册成功，你可获得
            <strong class="text-ink-200">{{ formatNumber(info.register_bonus_quota) }}</strong> 额度。
          </li>
          <li>· 好友每笔充值入账后，你会按后台设置的比例获得返利（同一笔只返一次）。</li>
          <li>· 邀请关系在好友注册时确定，之后不可更改，请分享给真实用户。</li>
        </ul>
      </section>

      <!-- ── 签到卡片 ───────────────────────────────────── -->
      <section class="card card-pad">
        <h3 class="section-title flex items-center gap-2">
          <AppIcon name="check" :size="16" class="text-brand-700" />
          每日签到
        </h3>

        <template v-if="info.checkin.enabled">
          <p class="mt-3 text-sm text-ink-300">
            每天签到
            <template v-if="info.checkin.daily_quota > 0">
              可获得 <strong class="text-brand-700">{{ formatNumber(info.checkin.daily_quota) }}</strong> 额度
            </template>
            <template v-else>可累计连续天数</template>
            ，按北京时间计算，每天仅一次。
          </p>

          <div class="mt-4 grid grid-cols-3 gap-3 text-center">
            <div class="rounded-lg border border-ink-800 px-2 py-3">
              <p class="text-xs text-ink-500">连续天数</p>
              <p class="mt-1 font-mono text-lg font-semibold text-ink-100">{{ formatNumber(info.checkin.streak_days) }}</p>
            </div>
            <div class="rounded-lg border border-ink-800 px-2 py-3">
              <p class="text-xs text-ink-500">累计天数</p>
              <p class="mt-1 font-mono text-lg font-semibold text-ink-100">{{ formatNumber(info.checkin.total_days) }}</p>
            </div>
            <div class="rounded-lg border border-ink-800 px-2 py-3">
              <p class="text-xs text-ink-500">累计获得</p>
              <p class="mt-1 font-mono text-lg font-semibold text-brand-700">{{ formatNumber(info.checkin.total_quota) }}</p>
            </div>
          </div>

          <button
            type="button"
            class="btn mt-4 w-full"
            :class="info.checkin.checked_today ? 'btn-secondary' : 'btn-primary'"
            :disabled="info.checkin.checked_today || checkingIn"
            @click="handleCheckin"
          >
            <AppIcon :name="info.checkin.checked_today ? 'check' : 'plus'" :size="16" />
            {{
              info.checkin.checked_today
                ? '今日已签到'
                : checkingIn
                  ? '签到中…'
                  : '立即签到'
            }}
          </button>
        </template>

        <p v-else class="mt-3 text-sm leading-relaxed text-ink-400">
          本站暂未开放每日签到。签到功能开启后可在此领取额度并累计连续天数。
        </p>
      </section>
    </div>
  </div>
</template>
