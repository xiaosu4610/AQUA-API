/**
 * 管理后台页面词条（简体中文）。
 *
 * 意图（Why）：
 *   为「页面级」文案预留独立文件，后续阶段迁移 views/admin/* 时在此补充。
 *
 * 流转（Flow）：
 *   i18n/index.ts 合并 → views 通过 $t('admin.<页>.<语义>') 读取。
 *
 * 扩展（Extend）：
 *   本阶段为空对象；填充时六种语言同步补齐（键集合由 index.ts 类型约束保证）。
 */
export default {}
