/**
 * 通用词条（英语 / English）。
 *
 * 意图（Why）：
 *   「动作 / 状态 / 单位 / 提示」等跨页面复用短词；六种语言键集合必须一致。
 *
 * 流转（Flow）：
 *   i18n/index.ts 合并六种语言词条 → 组件通过 $t('common.action.save') 读取。
 *
 * 扩展（Extend）：
 *   新增词条时六种语言同步补齐（键集合由 index.ts 类型约束强制）。
 */
export default {
  action: {
    save: 'Save',
    cancel: 'Cancel',
    confirm: 'Confirm',
    close: 'Close',
    delete: 'Delete',
    edit: 'Edit',
    create: 'Create',
    copy: 'Copy',
    copied: 'Copied',
    refresh: 'Refresh',
    retry: 'Reload',
    search: 'Search',
    reset: 'Reset',
    submit: 'Submit',
    back: 'Back',
    view: 'View',
    open: 'Open',
    more: 'More',
  },
  state: {
    loading: 'Loading…',
    empty: 'No data',
  },
  unit: {
    items: 'items',
    days: 'days',
  },
  toast: {
    operationFailed: 'Something went wrong. Please try again later.',
  },
  language: {
    label: 'Language',
    switch: 'Switch language',
  },
  confirm: {
    defaultMessage: 'Are you sure you want to proceed?',
  },
}
