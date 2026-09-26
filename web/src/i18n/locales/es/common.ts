/**
 * 通用词条（西班牙语 / Español）。
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
    save: 'Guardar',
    cancel: 'Cancelar',
    confirm: 'Confirmar',
    close: 'Cerrar',
    delete: 'Eliminar',
    edit: 'Editar',
    create: 'Crear',
    copy: 'Copiar',
    copied: 'Copiado',
    refresh: 'Actualizar',
    retry: 'Recargar',
    search: 'Buscar',
    reset: 'Restablecer',
    submit: 'Enviar',
    back: 'Atrás',
    view: 'Ver',
    open: 'Abrir',
    more: 'Más',
  },
  state: {
    loading: 'Cargando…',
    empty: 'Sin datos',
  },
  unit: {
    items: 'elementos',
    days: 'días',
  },
  toast: {
    operationFailed: 'Se produjo un error. Inténtalo de nuevo más tarde.',
  },
  language: {
    label: 'Idioma',
    switch: 'Cambiar idioma',
  },
  confirm: {
    defaultMessage: '¿Seguro que quieres realizar esta acción?',
  },
}
