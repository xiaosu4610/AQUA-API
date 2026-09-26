/**
 * 通用词条（简体中文）。
 *
 * 意图（Why）：
 *   「动作 / 状态 / 单位 / 提示」这类跨页面复用的短词集中在此，
 *   避免「保存」「取消」在多个组件里各写一份中文，改文案时漏改。
 *
 * 流转（Flow）：
 *   i18n/index.ts 合并六种语言词条 → 组件通过 $t('common.action.save') 读取。
 *
 * 扩展（Extend）：
 *   新增词条时六种语言的同一路径都要补齐（index.ts 用类型约束强制键集合一致）；
 *   键名规则：common.<语义组>.<语义>。
 */
export default {
  action: {
    save: '保存',
    cancel: '取消',
    confirm: '确认',
    close: '关闭',
    delete: '删除',
    edit: '编辑',
    create: '创建',
    copy: '复制',
    copied: '已复制',
    refresh: '刷新',
    retry: '重新加载',
    search: '搜索',
    reset: '重置',
    submit: '提交',
    back: '返回',
    view: '查看',
    open: '打开',
    more: '更多',
  },
  state: {
    loading: '加载中…',
    empty: '暂无数据',
  },
  unit: {
    items: '条',
    days: '天',
  },
  toast: {
    operationFailed: '操作失败，请稍后重试',
  },
  language: {
    label: '语言',
    switch: '切换语言',
  },
  confirm: {
    defaultMessage: '确认执行该操作？',
  },
}
