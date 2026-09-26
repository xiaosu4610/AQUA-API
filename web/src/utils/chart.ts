/**
 * 图表通用配色与样式常量。
 *
 * 意图（Why）：
 *   仪表盘与门户共 4 处图表，若各自定义颜色会出现「同一指标在不同页颜色不同」的问题；
 *   集中定义保证「请求=青色、Token=紫色、额度=绿色」在全站一致，降低读图成本。
 *
 * 关于亮色主题下的取色（重要）：
 *   亮色背景上必须使用「中等偏深」的颜色。原先为深色底选的浅色（如 #22d3ee）在
 *   白底上对比度只有 1.6:1，几乎看不清，因此整体下沉一个明度档，
 *   同时保持色相不变以维持原有的"指标—颜色"记忆。
 *
 * 流转（Flow）：
 *   views/* 构造 EChartsOption → 引用本文件的常量 → components/EChart.vue 渲染
 *
 * 扩展（Extend）：
 *   新增指标配色请在 CHART_PALETTE 追加，并同步 views 中的显式取色；
 *   颜色值需与 tailwind.config.js 的品牌色保持同一色系。
 */

/** 图表主色序列（顺序即默认取色顺序：青 → 靛 → 绿 → 琥珀 → 粉 → 蓝） */
export const CHART_PALETTE = ['#0891b2', '#6366f1', '#059669', '#d97706', '#db2777', '#2563eb']

/** 坐标轴标签样式：比正文弱一档，避免图表抢主体内容的视觉权重 */
export const AXIS_LABEL_STYLE = { color: '#475569', fontSize: 11 } as const

/** 坐标轴线样式：亮色下线条需要比深色主题更实一些才看得见 */
export const AXIS_LINE_STYLE = { lineStyle: { color: 'rgba(100,116,139,0.35)' } } as const

/** 网格分割线：虚线 + 低透明度，浅色底上不喧宾夺主 */
export const SPLIT_LINE_STYLE = {
  lineStyle: { color: 'rgba(100,116,139,0.22)', type: 'dashed' as const },
} as const

/**
 * 统一的 tooltip 外观（与 .card 的圆角/描边语言一致）。
 *
 * 亮色主题下用「白色浮层 + 深色文字」，与页面的玻璃质感卡片保持同一视觉语言；
 * 若沿用深色 tooltip，会在浅色页面里显得突兀且像一块"黑洞"。
 *
 * 注意：此处刻意不加 `as const` —— padding 若被推断为 readonly 元组，
 * 将无法赋值给 echarts 的 `number | number[]` 类型。
 */
export const TOOLTIP_STYLE = {
  backgroundColor: 'rgba(255,255,255,0.98)',
  borderColor: 'rgba(224,232,242,1)',
  borderWidth: 1,
  padding: [8, 12],
  textStyle: { color: '#1e293b', fontSize: 12 },
  extraCssText: 'border-radius:10px;box-shadow:0 12px 32px -12px rgba(15,23,42,.25);',
}

/** 面积图渐变（用于折线下方的填充，让趋势更易读） */
export function areaGradient(color: string): Record<string, unknown> {
  return {
    type: 'linear',
    x: 0,
    y: 0,
    x2: 0,
    y2: 1,
    colorStops: [
      { offset: 0, color: `${color}40` },
      { offset: 1, color: `${color}00` },
    ],
  }
}
