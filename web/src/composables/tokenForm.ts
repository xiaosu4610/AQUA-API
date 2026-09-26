/**
 * 令牌表单的状态与校验（用户门户与管理端共用）。
 *
 * 意图（Why）：
 *   「创建访问令牌」在门户页与管理页都要用，字段与校验规则完全一致；
 *   若不抽出来，两处表单很快会出现规则漂移（例如一处允许空名称）。
 *
 * 流转（Flow）：
 *   TokensView（门户/管理） → tokenForm 状态 → TokenFormFields.vue 渲染
 *   → validateTokenForm() 通过 → toTokenPayload() → api 层提交
 *
 * 扩展（Extend）：
 *   新增令牌字段（如「IP 白名单」）：在 TokenFormState 加字段、
 *   在 emptyTokenForm/validateTokenForm/toTokenPayload 三处同步，
 *   并在 TokenFormFields.vue 补表单项。
 */
import type { CreateTokenPayload } from '@/api/types'
import { parseModelList } from '@/utils/format'

/** 表单状态（modelText 是模型的文本输入形态，提交前转成数组） */
export interface TokenFormState {
  name: string
  /** 0 = 永不过期 */
  expires_in_days: number
  unlimited_quota: boolean
  remain_quota: number
  /** 逗号分隔的模型白名单，留空表示不限制 */
  modelText: string
}

/** 默认表单值：额度不限、永不过期、模型不限，降低首次创建的决策成本 */
export function emptyTokenForm(): TokenFormState {
  return {
    name: '',
    expires_in_days: 0,
    unlimited_quota: true,
    remain_quota: 100000,
    modelText: '',
  }
}

/**
 * 校验表单，返回第一条错误信息；通过则返回 null。
 * 只做前端可判定的校验（必填、范围），业务冲突（如重名）由后端返回 409。
 */
export function validateTokenForm(form: TokenFormState): string | null {
  if (!form.name.trim()) return '请填写令牌名称'
  if (form.name.trim().length > 64) return '令牌名称不能超过 64 个字符'
  if (!form.unlimited_quota) {
    if (!Number.isFinite(form.remain_quota) || form.remain_quota <= 0) {
      return '限定额度时必须填写大于 0 的额度值'
    }
  }
  if (form.expires_in_days < 0 || !Number.isFinite(form.expires_in_days)) {
    return '有效期天数不能为负数'
  }
  return null
}

/** 表单 → 契约请求体（POST /api/user/tokens 与 /api/admin/tokens 通用） */
export function toTokenPayload(form: TokenFormState, userId?: number): CreateTokenPayload {
  const payload: CreateTokenPayload = {
    name: form.name.trim(),
    expires_in_days: Number(form.expires_in_days) || 0,
    models: parseModelList(form.modelText),
    unlimited_quota: form.unlimited_quota,
    // 不限额度时 remain_quota 无意义（契约明确），统一传 -1 以免后端误判为「已耗尽」
    remain_quota: form.unlimited_quota ? -1 : Number(form.remain_quota),
  }
  if (userId !== undefined) payload.user_id = userId
  return payload
}
