<script setup lang="ts">
/**
 * 语言切换器（顶栏控件）。
 *
 * 意图（Why）：
 *   六种语言随时可切；把「地球图标 + 当前语言简称 + 下拉」收敛成一个小组件，
 *   顶栏调用处保持干净，也便于移动端换成紧凑形态。
 *   下拉里每种语言都用「母语自称」显示（简体中文 / English / Français / Русский /
 *   Español / العربية）—— 用户找自己的语言时按名字扫读，比看英文名更直觉。
 *
 * 流转（Flow）：
 *   点击 → 展开下拉 → 选择 → i18n/index.ts 的 setLocale()
 *   → 同步 i18n.locale / localStorage / <html lang/dir> → 全站文案与方向即时更新。
 *
 * 扩展（Extend）：
 *   新增语言只在 i18n/index.ts 的 SUPPORTED_LOCALES 里加一项，本组件无需改动。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import AppIcon from './AppIcon.vue'
import { SUPPORTED_LOCALES, setLocale } from '@/i18n'

const { t, locale } = useI18n()

/** 下拉展开状态 */
const open = ref(false)
/** 组件根节点：用于判断点击是否落在控件之外（点外部即收起） */
const root = ref<HTMLElement | null>(null)

/** 当前语言的母语名（用于 title 提示） */
const currentName = computed(
  () => SUPPORTED_LOCALES.find((item) => item.code === locale.value)?.name ?? '',
)

/** 当前语言简称：取语言主码大写（zh-CN → ZH），在顶栏里保持紧凑 */
const shortLabel = computed(() => String(locale.value).split('-')[0].toUpperCase())

function choose(code: string): void {
  setLocale(code)
  open.value = false
}

function onDocumentClick(event: MouseEvent): void {
  if (open.value && root.value && !root.value.contains(event.target as Node)) open.value = false
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') open.value = false
}

onMounted(() => {
  document.addEventListener('click', onDocumentClick)
  document.addEventListener('keydown', onKeydown)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <div ref="root" class="relative">
    <button
      type="button"
      class="btn btn-ghost btn-sm"
      :title="currentName"
      :aria-label="t('common.language.switch')"
      aria-haspopup="listbox"
      :aria-expanded="open"
      @click="open = !open"
    >
      <AppIcon name="globe" :size="15" />
      <span class="hidden text-xs font-medium sm:inline">{{ shortLabel }}</span>
    </button>

    <!-- 下拉：end-0 使面板在 RTL 下自动贴到另一半，无需两套样式 -->
    <ul
      v-if="open"
      class="absolute end-0 z-50 mt-1.5 min-w-[9rem] overflow-hidden rounded-lg border border-ink-700 bg-white py-1 shadow-pop"
      role="listbox"
      :aria-label="t('common.language.label')"
    >
      <li v-for="item in SUPPORTED_LOCALES" :key="item.code">
        <button
          type="button"
          class="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-start text-sm transition-colors hover:bg-ink-850"
          :class="item.code === locale ? 'font-medium text-brand-700' : 'text-ink-200'"
          role="option"
          :aria-selected="item.code === locale"
          @click="choose(item.code)"
        >
          <!-- dir=auto 让每条语言名按自身书写方向渲染（阿拉伯语名不会在中文界面里倒排） -->
          <span dir="auto">{{ item.name }}</span>
          <AppIcon v-if="item.code === locale" name="check" :size="14" />
        </button>
      </li>
    </ul>
  </div>
</template>
