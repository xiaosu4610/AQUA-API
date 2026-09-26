/**
 * 图表通用配色与样式常量。
 *
 * 意图（Why）：
 *   仪表盘与门户共 4 处图表，若各自定义颜色会出现「同一指标在不同页颜色不同」的问题；
 *   集中定义保证「请求=青色、Token=紫色、额度=绿色」在全站一致，降低读图成本。
 *
 * 流转（Flow）：
 *   views/* 构造 EChartsOption → 引用本文件的常量 → components/EChart.vue 渲染
 *
 * 扩展（Extend）：
 *   新增指标配色请在 CHART_PALETTE 追加，并同步 views 中的显式取色；
 *   颜色值需与 tailwind.config.js 的品牌色保持同一色系。
 */

/** 图表主色序列（顺序即默认取色顺序：青 → 靛 → 绿 → 琥珀 → 粉 → 蓝） */
export const CHART_PALETTE = ['#22d3ee', '#818cf8', '#34d399', '#fbbf24', '#f472b6', '#60a5fa']

/** 坐标轴标签样式：比正文弱一档，避免图表抢主体内容的视觉权重 */
export const AXIS_LABEL_STYLE = { color: '#6f7f96', fontSize: 11 } as const

/** 坐标轴线样式 */
export const AXIS_LINE_STYLE = { lineStyle: { color: 'rgba(148,163,184,0.18)' } } as const

/** 网格分割线：虚线 + 低透明度，深色底上不喧宾夺主 */
export const SPLIT_LINE_STYLE = {
  lineStyle: { color: 'rgba(148,163,184,0.12)', type: 'dashed' as const },
} as const

/**
 * 统一的 tooltip 外观（与 .card 的圆角/描边语言一致）。
 *
 * 注意：此处刻意不加 `as const` —— padding 若被推断为 readonly 元组，
 * 将无法赋值给 echarts 的 `number | number[]` 类型。
 */
export const TOOLTIP_STYLE = {
  backgroundColor: 'rgba(13,21,31,0.96)',
  borderColor: 'rgba(38,50,74,1)',
  borderWidth: 1,
  padding: [8, 12],
  textStyle: { color: '#e6ebf2', fontSize: 12 },
  extraCssText: 'border-radius:10px;box-shadow:0 12px 32px -12px rgba(0,0,0,.8);',
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
