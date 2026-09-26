/**
 * 通用词条（法语 / Français）。
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
    save: 'Enregistrer',
    cancel: 'Annuler',
    confirm: 'Confirmer',
    close: 'Fermer',
    delete: 'Supprimer',
    edit: 'Modifier',
    create: 'Créer',
    copy: 'Copier',
    copied: 'Copié',
    refresh: 'Actualiser',
    retry: 'Recharger',
    search: 'Rechercher',
    reset: 'Réinitialiser',
    submit: 'Envoyer',
    back: 'Retour',
    view: 'Voir',
    open: 'Ouvrir',
    more: 'Plus',
  },
  state: {
    loading: 'Chargement…',
    empty: 'Aucune donnée',
  },
  unit: {
    items: 'éléments',
    days: 'jours',
  },
  toast: {
    operationFailed: 'Une erreur est survenue. Veuillez réessayer plus tard.',
  },
  language: {
    label: 'Langue',
    switch: 'Changer de langue',
  },
  confirm: {
    defaultMessage: 'Voulez-vous vraiment effectuer cette action ?',
  },
}
