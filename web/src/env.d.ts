/// <reference types="vite/client" />

// 本文件为 TypeScript 的环境声明补充。
//
// 意图（Why）：
//   让 TS 认识 Vite 注入的 import.meta.env 等类型；
//   .vue 单文件组件的类型由 vue-tsc 原生解析，无需再写宽松的 declare module 垫片
//   （写了反而会削弱 SFC 的类型检查）。
//
// 流转（Flow）：
//   tsconfig.json → include 匹配本文件 → 全局生效
//
// 扩展（Extend）：
//   新增自定义环境变量时，在此处补充 ImportMetaEnv 声明，保持类型与 .env 同步。

interface ImportMetaEnv {
  /** 可选：覆盖 API 基地址（默认同源 /api，由后端或 Vite 代理转发） */
  readonly VITE_API_BASE?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
