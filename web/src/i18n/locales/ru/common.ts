/**
 * 通用词条（俄语 / Русский）。
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
    save: 'Сохранить',
    cancel: 'Отмена',
    confirm: 'Подтвердить',
    close: 'Закрыть',
    delete: 'Удалить',
    edit: 'Изменить',
    create: 'Создать',
    copy: 'Копировать',
    copied: 'Скопировано',
    refresh: 'Обновить',
    retry: 'Перезагрузить',
    search: 'Поиск',
    reset: 'Сбросить',
    submit: 'Отправить',
    back: 'Назад',
    view: 'Просмотр',
    open: 'Открыть',
    more: 'Ещё',
  },
  state: {
    loading: 'Загрузка…',
    empty: 'Нет данных',
  },
  unit: {
    items: 'шт.',
    days: 'дн.',
  },
  toast: {
    operationFailed: 'Произошла ошибка. Повторите попытку позже.',
  },
  language: {
    label: 'Язык',
    switch: 'Сменить язык',
  },
  confirm: {
    defaultMessage: 'Вы уверены, что хотите выполнить это действие?',
  },
}
