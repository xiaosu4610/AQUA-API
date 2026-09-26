// Tailwind 配置：定义 AQUA-API 的深色设计令牌。
//
// 意图（Why）：
//   把「颜色/字体/圆角」等设计决策集中成令牌（token），
//   组件里只使用语义化类名（bg-ink-900 / text-brand-300），
//   避免颜色散落在各处导致视觉不一致，也便于后续整体换肤。
//
// 流转（Flow）：
//   tailwind.config.js → postcss（tailwindcss 插件）→ src/style.css 的 @tailwind 指令
//   → 生成工具类 → 各 .vue 模板使用
//
// 扩展（Extend）：
//   新增色板：在 theme.extend.colors 下加一组（如 warn），并同步 style.css 的 @layer components
//   里已有语义类；品牌色统一用 brand-*，中性色统一用 ink-*（数值越大越深）。
/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,ts}'],
  theme: {
    extend: {
      colors: {
        // 品牌色（青蓝，呼应 "AQUA"）：用于主按钮、强调、图表主色
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
        // 中性色（深色主题的层级：950 页面底色 → 700 边框 → 300 次要文字）
        ink: {
          50: '#f5f7fa',
          100: '#e6ebf2',
          200: '#cbd5e1',
          300: '#9aa8bc',
          400: '#6f7f96',
          500: '#4c5b70',
          600: '#33415a',
          700: '#26324a',
          750: '#1c2739',
          800: '#16202f',
          850: '#111a26',
          900: '#0d151f',
          950: '#080d14',
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
        // 卡片与弹窗的层次感：深色主题下用更黑的投影 + 极细描边
        panel: '0 1px 0 0 rgba(255,255,255,0.03) inset, 0 12px 32px -12px rgba(0,0,0,0.7)',
        pop: '0 24px 60px -20px rgba(0,0,0,0.85)',
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
