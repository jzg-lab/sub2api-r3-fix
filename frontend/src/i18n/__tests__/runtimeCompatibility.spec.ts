import { describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import { i18n, loadLocaleMessages } from '../index'

describe('i18n runtime compatibility', () => {
  it('loads both real catalogs and translates without dynamic code generation', async () => {
    await loadLocaleMessages('en')
    await loadLocaleMessages('zh')
    const originalLocale = i18n.global.locale.value
    const dynamicCode = vi.spyOn(globalThis, 'Function').mockImplementation(() => {
      throw new Error('CSP blocks unsafe-eval')
    })
    try {
      i18n.global.locale.value = 'en'
      expect(i18n.global.t('common.save')).toBe('Save')
      i18n.global.locale.value = 'zh'
      expect(i18n.global.t('common.save')).toBe('保存')
      const translator = createI18n({
        legacy: false,
        locale: 'en',
        messages: { en: { greeting: 'Hello {name}', items: 'one item | {count} items' } }
      })
      expect(translator.global.t('greeting', { name: 'Ada' })).toBe('Hello Ada')
      expect(translator.global.t('items', 2)).toBe('2 items')
      expect(dynamicCode).not.toHaveBeenCalled()
      translator.dispose()
    } finally {
      dynamicCode.mockRestore()
      i18n.global.locale.value = originalLocale
    }
  })
})
