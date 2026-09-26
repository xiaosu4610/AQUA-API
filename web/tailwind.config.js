// Tailwind 配置：定义 AQUA-API 的亮色设计令牌。
//
// 意图（Why）：
//   把「颜色/字体/圆角」等设计决策集中成令牌（token），
//   组件里只使用语义化类名（bg-ink-900 / text-brand-700），
//   避免颜色散落在各处导致视觉不一致，也便于后续整体换肤。
//
// 关于 ink 色阶的方向（重要，改色前必读）：
//   本项目采用「数值越大越接近背景」的语义约定：
//     ink-950 = 最接近页面底色，ink-50 = 与底色对比最强。
//   因此亮色主题下 950 是最浅（近白）、50 是最深（近黑），
//   与 Tailwind 默认色阶的方向相反。这样约定的好处是：
//   切换主题时只需替换色值，组件里的 ink-* 类名完全不用动。
//
// 流转（Flow）：
//   tailwind.config.js → postcss（tailwindcss 插件）→ src/style.css 的 @tailwind 指令
//   → 生成工具类 → 各 .vue 模板使用
//
// 扩展（Extend）：
//   新增色板：在 theme.extend.colors 下加一组（如 warn），并同步 style.css 的 @layer components
//   里已有语义类；品牌色统一用 brand-*，中性色统一用 ink-*。
/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,ts}'],
  theme: {
    extend: {
      colors: {
        // 品牌色（青蓝，呼应 "AQUA"）：用于主按钮、强调、图表主色。
        // 文字类用法请取 600/700（亮底上对比度足够），
        // 500 及更浅的色阶仅用于底色与描边（配透明度）。
        brand: {
          50: '#ecfeff',
          100: '#cffafe',
          200: '#a5f3fc',
          300: '#67e8f9',
          400: '#22d3ee',
          500: '#06b6d4',
          600: '#0891b2',
          700: '#0e7490',
          800: '#155e75',
          900: '#164e63',
        },
        // 中性色：语义为「数值越大越接近页面底色」（亮色主题 → 越大越浅）。
        // 950/900/850 用于页面底、卡片底与浅色填充；
        // 500-700 用于边框与分隔；50-300 用于各级文字。
        ink: {
          50: '#0f172a', // 最强对比：标题
          100: '#1e293b', // 正文
          200: '#334155', // 次级正文
          300: '#475569', // 次要文字（说明、表头）
          400: '#64748b', // 更弱文字（提示、占位说明）
          500: '#94a3b8', // 输入框占位符、禁用态
          600: '#b8c4d4', // 强边框（复选框等需要明确边界处）
          700: '#cbd6e4', // 常规边框
          750: '#d6e0ec', // hover 底色 / 次级按钮 hover
          800: '#e0e8f2', // 次级按钮底、浅色标签底
          850: '#e9eff7', // 表头底、chip 底、hover 底色
          900: '#ffffff', // 卡片 / 输入框底
          950: '#f5f8fc', // 页面底色（极浅冷调，营造通透感）
        },
      },
      fontFamily: {
        sans: [
          'Inter',
          '-apple-system',
          'BlinkMacSystemFont',
          '"Segoe UI"',
          '"PingFang SC"',
          '"Hiragino Sans GB"',
          '"Microsoft YaHei"',
          'sans-serif',
        ],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
      },
      boxShadow: {
        // 卡片与弹窗的层次感：亮色主题下投影要更轻更散，
        // 否则会在浅色背景上形成生硬的黑色边缘。
        panel: '0 1px 2px 0 rgba(15,23,42,0.04), 0 12px 32px -16px rgba(15,23,42,0.16)',
        pop: '0 24px 60px -24px rgba(15,23,42,0.28)',
      },
      keyframes: {
        'fade-in': { from: { opacity: '0' }, to: { opacity: '1' } },
        'slide-up': { from: { opacity: '0', transform: 'translateY(6px)' }, to: { opacity: '1', transform: 'translateY(0)' } },
        'slide-in-right': { from: { transform: 'translateX(100%)' }, to: { transform: 'translateX(0)' } },
      },
      animation: {
        'fade-in': 'fade-in 160ms ease-out',
        'slide-up': 'slide-up 200ms ease-out',
        'slide-in-right': 'slide-in-right 220ms cubic-bezier(0.22, 1, 0.36, 1)',
      },
    },
  },
  plugins: [],
}
