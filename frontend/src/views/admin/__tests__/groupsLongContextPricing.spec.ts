import { describe, expect, it } from 'vitest'
import { longContextPricingFromForm, longContextPricingToForm } from '../groupsLongContextPricing'
import en from '@/i18n/locales/en/admin/overview'
import zh from '@/i18n/locales/zh/admin/overview'

describe('group long-context thresholds', () => {
  it('inherits old settings and round-trips defaults and model overrides', () => {
    expect(longContextPricingToForm()).toEqual({ thresholdK: '', models: [] })
    expect(longContextPricingFromForm(longContextPricingToForm())).toEqual({ threshold_tokens: 0, model_thresholds: {} })
    const config = { threshold_tokens: 128000, model_thresholds: { 'model-a': 256000 } }
    expect(longContextPricingToForm(config)).toEqual({ thresholdK: '128', models: [{ model: 'model-a', thresholdK: '256' }] })
    expect(longContextPricingFromForm(longContextPricingToForm(config))).toEqual(config)
  })

  it.each(['0', '-1', '1.5', 'NaN', 'Infinity', '2147484'])('rejects invalid K value %s', value => {
    expect(longContextPricingFromForm({ thresholdK: value, models: [] })).toBeNull()
    expect(longContextPricingFromForm({ thresholdK: '', models: [{ model: 'model', thresholdK: value }] })).toBeNull()
  })

  it('requires complete unique exact model overrides and normalizes Claude spelling', () => {
    for (const models of [
      [{ model: '', thresholdK: '128' }], [{ model: 'model', thresholdK: '' }],
      [{ model: 'model*', thresholdK: '128' }], [{ model: 'bad model', thresholdK: '128' }],
      [{ model: 'Model', thresholdK: '128' }, { model: 'model', thresholdK: '256' }]
    ]) expect(longContextPricingFromForm({ thresholdK: '', models })).toBeNull()
    expect(longContextPricingFromForm({ thresholdK: '', models: [{ model: ' Claude-Sonnet-4.6 ', thresholdK: '256' }] }))
      .toEqual({ threshold_tokens: 0, model_thresholds: { 'claude-sonnet-4-6': 256000 } })
  })

  it('documents preserved rates, exemptions and channel precedence in both languages', () => {
    expect(zh.groups.modelPricing.thresholdHint).toContain('倍率、边界和账号豁免保持原规则')
    expect(en.groups.modelPricing.thresholdHint).toContain('Explicit channel pricing intervals take precedence')
  })
})
