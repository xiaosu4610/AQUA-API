// PostCSS 配置：把 Tailwind 指令与浏览器前缀处理接入 Vite 的样式管线。
//
// 意图（Why）：
//   Tailwind 需要经 PostCSS 编译才会生成真实 CSS，autoprefixer 补齐浏览器前缀。
//
// 流转（Flow）：
//   .vue / .css 中的 @tailwind 指令 → tailwindcss 插件展开 → autoprefixer 补前缀 → 打包输出
//
// 扩展（Extend）：
//   新增 PostCSS 插件（如 cssnano）时在此追加，顺序即执行顺序。
export default {
  plugins: {
    tailwindcss: {},
    autoprefixer: {},
  },
}
