/**
 * 简体中文（zh-CN）词条聚合入口。
 *
 * 意图（Why）：
 *   按功能域拆分为多个文件（common / components / portal / admin），
 *   便于多人并行编辑不同域，此处只做聚合。
 *
 * 流转（Flow）：
 *   ./{common,components,portal,admin}.ts → 本文件默认导出 → i18n/index.ts
 *
 * 扩展（Extend）：
 *   新增语言目录时，复制本文件结构并同步补齐各域词条。
 */
import admin from './admin'
import common from './common'
import components from './components'
import portal from './portal'

export default { common, components, portal, admin }
