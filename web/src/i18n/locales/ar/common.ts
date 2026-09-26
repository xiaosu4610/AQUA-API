/**
 * 通用词条（阿拉伯语 / العربية，RTL）。
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
    save: 'حفظ',
    cancel: 'إلغاء',
    confirm: 'تأكيد',
    close: 'إغلاق',
    delete: 'حذف',
    edit: 'تعديل',
    create: 'إنشاء',
    copy: 'نسخ',
    copied: 'تم النسخ',
    refresh: 'تحديث',
    retry: 'إعادة التحميل',
    search: 'بحث',
    reset: 'إعادة الضبط',
    submit: 'إرسال',
    back: 'رجوع',
    view: 'عرض',
    open: 'فتح',
    more: 'المزيد',
  },
  state: {
    loading: 'جارٍ التحميل…',
    empty: 'لا توجد بيانات',
  },
  unit: {
    items: 'عناصر',
    days: 'أيام',
  },
  toast: {
    operationFailed: 'حدث خطأ. يرجى المحاولة مرة أخرى لاحقًا.',
  },
  language: {
    label: 'اللغة',
    switch: 'تغيير اللغة',
  },
  confirm: {
    defaultMessage: 'هل أنت متأكد من تنفيذ هذا الإجراء؟',
  },
}
