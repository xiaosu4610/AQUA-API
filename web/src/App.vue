<script setup lang="ts">
/**
 * 应用根组件：只负责挂载全局浮层与路由出口。
 *
 * 意图（Why）：
 *   提示（Toast）与确认弹窗（Confirm）需要脱离具体页面层级（避免被 overflow 裁剪），
 *   因此统一挂在根组件，配合 Teleport 渲染到 body。
 *
 * 流转（Flow）：
 *   main.ts → App.vue
 *     → <RouterView />         渲染当前页面（含其布局组件）
 *     → <ToastHost />          全局提示队列
 *     → <ConfirmHost />        全局确认弹窗
 *
 * 扩展（Extend）：
 *   新增全局浮层（如「系统公告」）在此追加，并保持顺序：内容 → 提示 → 弹窗。
 */
import ConfirmHost from '@/components/ConfirmHost.vue'
import ToastHost from '@/components/ToastHost.vue'
</script>

<template>
  <RouterView />
  <ToastHost />
  <ConfirmHost />
</template>
