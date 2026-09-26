/**
 * 邀请返利 / 每日签到接口（需登录）。
 *
 * 意图（Why）：
 *   把「我的邀请」页所需的两组数据收敛到一处：邀请概况（邀请码、邀请链接、
 *   邀请人数、累计返利）与签到状态（今日是否已签、连续/累计天数与额度）。
 *   后端把签到概况也内嵌在邀请概况里返回，前端首屏一次请求即可渲染整页。
 *
 * 流转（Flow）：
 *   views/console/ReferralView.vue → fetchReferral() / checkin() / fetchCheckin()
 *                                  → /api/user/referral、/api/user/checkin
 *
 * 扩展（Extend）：
 *   新增邀请相关接口时在此追加函数；本文件只做"路径 + 类型"映射，不含业务逻辑。
 */
import { api } from './client'

/** 签到状态（GET/POST /api/user/checkin 与 referral.checkin 共用） */
export interface CheckinStatus {
  /** 签到功能是否开启；false 时前端隐藏签到按钮 */
  enabled: boolean
  /** 每次签到发放额度（0 表示只记天数、不发额度） */
  daily_quota: number
  /** 北京时间今天是否已签到 */
  checked_today: boolean
  /** 当前连续签到天数 */
  streak_days: number
  /** 累计签到天数 */
  total_days: number
  /** 累计签到获得额度 */
  total_quota: number
}

/** GET /api/user/referral 的响应 */
export interface ReferralInfo {
  /** 我的邀请码（首都由后端懒生成，前端直接展示） */
  invite_code: string
  /** 邀请链接路径（如 /register?invite=CODE），前端与站点域名拼接后展示 */
  invite_path: string
  /** 已成功邀请的人数 */
  invited_count: number
  /** 当前配置的"邀请注册奖"额度（便于页面提示用户能拿多少） */
  register_bonus_quota: number
  /** 我累计获得的邀请返利额度 */
  total_reward_quota: number
  /** 签到概况 */
  checkin: CheckinStatus
}

/** GET /api/user/referral：读取我的邀请与签到概况 */
export function fetchReferral(): Promise<ReferralInfo> {
  return api.get<ReferralInfo>('/user/referral')
}

/** GET /api/user/checkin：读取签到状态 */
export function fetchCheckin(): Promise<CheckinStatus> {
  return api.get<CheckinStatus>('/user/checkin')
}

/** POST /api/user/checkin：执行今日签到（当天重复签到会返回 409） */
export function checkin(): Promise<CheckinStatus> {
  return api.post<CheckinStatus>('/user/checkin')
}
