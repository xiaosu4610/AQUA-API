// 本文件是 AQUA-API 前端的构建与开发服务器配置。
//
// 意图（Why）：
//   前端源码与后端同仓库共存：开发期用 Vite dev server 提供热更新，
//   通过代理把 /api 与 /v1 转发到本机后端（127.0.0.1:8787），
//   避免跨域配置；生产期把构建产物输出到 web/dist，
//   由后端 go:embed 直接嵌入并提供静态文件（无需 nginx 单独托管前端）。
//
// 流转（Flow）：
//   npm run dev   → 浏览器 :5173 → /api/* 代理到后端 8787
//   npm run build → web/dist/index.html + web/dist/assets/*
//                 → Go 构建时 go:embed web/dist 嵌入二进制
//
// 扩展（Extend）：
//   新增代理前缀：在 server.proxy 里追加一项。
//   调整产物路径：改 build.outDir，同时需同步后端 go:embed 的路径指令。
import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

/** 后端监听地址：契约约定为 127.0.0.1:8787，由 nginx 反向代理对外。 */
const BACKEND_TARGET = 'http://127.0.0.1:8787'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      // 统一用 @/ 指向 src，避免深层相对路径（../../..）难以阅读
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: BACKEND_TARGET, changeOrigin: false },
      '/v1': { target: BACKEND_TARGET, changeOrigin: false },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // echarts 体积较大，单独分块便于浏览器缓存（改业务代码不失效图表缓存）
    rollupOptions: {
      output: {
        manualChunks: {
          echarts: ['echarts/core', 'echarts/charts', 'echarts/components', 'echarts/renderers'],
          vendor: ['vue', 'vue-router', 'pinia', 'axios'],
        },
      },
    },
    chunkSizeWarningLimit: 1000,
  },
})
