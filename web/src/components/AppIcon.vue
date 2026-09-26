<script setup lang="ts">
/**
 * 内联图标组件：全站唯一的图标渲染入口。
 *
 * 意图（Why）：
 *   1) 不使用任何图片或图标字体（自托管网关可能部署在内网，避免外链依赖）；
 *   2) 图标为原创几何线条 SVG，规避第三方图标库的许可问题；
 *   3) 统一描边宽度与线帽，保证全站图标粗细一致、视觉克制。
 *
 * 流转（Flow）：
 *   各 .vue → <AppIcon name="key" /> → 查 icons.ts 的路径表 → 渲染 <svg>
 *
 * 扩展（Extend）：
 *   新增图标请在 components/icons.ts 中登记（类型与路径两处同步），本文件无需改动。
 */
import { computed } from 'vue'

import { ICON_PATHS, type IconName } from './icons'

const props = withDefaults(
  defineProps<{
    /** 图标名（必须存在于 icons.ts 的 ICON_PATHS） */
    name: IconName
    /** 尺寸（像素），默认 18，与正文行高协调 */
    size?: number | string
    /** 描边宽度，默认 1.7：深色底上比 1.5 更清晰，又不至于厚重 */
    strokeWidth?: number | string
  }>(),
  { size: 18, strokeWidth: 1.7 },
)

const paths = computed(() => ICON_PATHS[props.name] ?? [])
</script>

<template>
  <svg
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    :stroke-width="strokeWidth"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    class="shrink-0"
  >
    <path v-for="(d, index) in paths" :key="index" :d="d" />
  </svg>
</template>
