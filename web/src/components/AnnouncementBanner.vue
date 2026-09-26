<script setup lang="ts">
/**
 * 站点公告横幅（公开端，自包含）。
 *
 * 意图（Why）：
 *   1) 公告的"可见性"完全由后端决定（时间窗口 + 启用状态），组件只负责展示，
 *      不复刻过滤逻辑，避免前后端判断不一致导致"该看到的没看到/不该看到的看到"；
 *   2) 组件必须自包含：自行拉取公开接口、自行记住用户关闭过哪些公告，
 *      不依赖父组件传参——这样它可被放进任意前台布局而无需改动调用方；
 *   3) 绝不影响宿主页面：拉取失败、localStorage 不可用时静默降级，
 *      公告是锦上添花的信息，不能因为它而让页面报错或卡住。
 *
 * 流转（Flow）：
 *   挂载 → fetchPublicAnnouncements() → 过滤掉已关闭的 ID → v-if 渲染；
 *   点击「我知道了」→ 该 ID 写入 localStorage（键 aqua.dismissed_announcements）→ 立即隐藏。
 *   无可见公告时不渲染任何 DOM。
 *
 * 扩展（Extend）：
 *   新增语气：在 LEVEL_STYLES 补一组配色（与后端 model.AnnouncementLevel 白名单同步）。
 *   新增交互（如"不再显示全部"）：在模板底部追加按钮并扩展 dismissed 的持久化策略。
 */
import { computed, onMounted, ref } from 'vue'

import { fetchPublicAnnouncements, type AnnouncementLevel, type PublicAnnouncement } from '@/api/announcement'

/** 已关闭公告 ID 的本地存储键；跨页面/刷新共享，让"我知道了"真正长期生效 */
const DISMISS_KEY = 'aqua.dismissed_announcements'

/** 读取已关闭的公告 ID 列表（localStorage 不可用或数据损坏时返回空数组） */
function readDismissed(): number[] {
  try {
    const raw = localStorage.getItem(DISMISS_KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter((value): value is number => typeof value === 'number')
  } catch {
    return []
  }
}

/** 持久化已关闭的公告 ID 列表（写入失败不影响本次会话内的内存状态） */
function writeDismissed(ids: number[]): void {
  try {
    localStorage.setItem(DISMISS_KEY, JSON.stringify(ids))
  } catch {
    /* 隐私模式等场景不可写：本次会话内仍由内存状态保证已关闭项不显示 */
  }
}

const items = ref<PublicAnnouncement[]>([])
const dismissed = ref<number[]>(readDismissed())

/** 实际展示的公告：剔除用户已点过"我知道了"的那些 */
const visible = computed(() => items.value.filter((item) => !dismissed.value.includes(item.id)))

function dismiss(item: PublicAnnouncement): void {
  if (dismissed.value.includes(item.id)) return
  dismissed.value = [...dismissed.value, item.id]
  writeDismissed(dismissed.value)
}

/**
 * 各语气的配色。
 *
 * 用 Tailwind 工具类而非自定义语义类：本组件可被放进任意布局，
 * 不假定宿主页面已经引入了特定的组件样式层，减少耦合。
 */
const LEVEL_STYLES: Record<AnnouncementLevel, { wrap: string; dot: string; title: string }> = {
  info: { wrap: 'border-brand-200 bg-brand-50', dot: 'bg-brand-500', title: 'text-brand-800' },
  success: { wrap: 'border-emerald-200 bg-emerald-50', dot: 'bg-emerald-500', title: 'text-emerald-800' },
  warning: { wrap: 'border-amber-200 bg-amber-50', dot: 'bg-amber-500', title: 'text-amber-800' },
  danger: { wrap: 'border-red-200 bg-red-50', dot: 'bg-red-500', title: 'text-red-800' },
}

/** 取语气样式；未知语气回退为普通样式，避免后端新增语气时前端崩掉 */
function styleFor(level: AnnouncementLevel): (typeof LEVEL_STYLES)[AnnouncementLevel] {
  return LEVEL_STYLES[level] ?? LEVEL_STYLES.info
}

onMounted(async () => {
  try {
    const result = await fetchPublicAnnouncements()
    items.value = Array.isArray(result?.items) ? result.items : []
  } catch {
    // 公告拉取失败不影响宿主页面，直接当作"没有公告"处理
    items.value = []
  }
})
</script>

<template>
  <!-- 无可见公告时不渲染任何 DOM -->
  <div v-if="visible.length" class="space-y-2" role="region" aria-label="站点公告">
    <div
      v-for="item in visible"
      :key="item.id"
      class="flex items-start gap-3 rounded-lg border px-4 py-3 text-sm"
      :class="styleFor(item.level).wrap"
    >
      <span class="mt-1.5 h-2 w-2 shrink-0 rounded-full" :class="styleFor(item.level).dot" aria-hidden="true" />
      <div class="min-w-0 flex-1">
        <p class="font-medium" :class="styleFor(item.level).title">{{ item.title }}</p>
        <p v-if="item.content" class="mt-0.5 whitespace-pre-wrap leading-relaxed text-ink-200">{{ item.content }}</p>
      </div>
      <button
        type="button"
        class="shrink-0 rounded-md px-2 py-1 text-xs font-medium text-ink-300 transition-colors hover:bg-black/5 hover:text-ink-100"
        @click="dismiss(item)"
      >
        我知道了
      </button>
    </div>
  </div>
</template>
