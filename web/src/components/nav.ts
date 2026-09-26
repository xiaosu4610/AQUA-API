/**
 * 导航数据结构（侧边栏与布局共用）。
 *
 * 意图（Why）：
 *   导航项的类型要被 layouts 与 AppShell 同时引用；
 *   放在普通 TS 模块可避免从 .vue 的 <script setup> 导出类型（那会削弱 SFC 编译约束）。
 *
 * 流转（Flow）：
 *   layouts/ConsoleLayout.vue、AdminLayout.vue → 声明 NavGroup[] → AppShell 渲染
 *
 * 扩展（Extend）：
 *   新增导航项只需在对应 layout 的数组里追加一项，图标名须已在 components/icons.ts 登记。
 */
import type { IconName } from './icons'

/** 单个导航项 */
export interface NavItem {
  /** 展示文案 */
  label: string
  /** 目标路由（可带 hash，如 /#quickstart） */
  to: string
  /** 图标名 */
  icon: IconName
}

/** 导航分组（可带分组标题，用于区分「资源 / 运维」等功能域） */
export interface NavGroup {
  /** 分组标题（可选；不传则不渲染标题） */
  title?: string
  items: NavItem[]
}
