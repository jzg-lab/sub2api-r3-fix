import type { GroupLongContextPricing } from '@/types'

export interface GroupLongContextPricingForm {
  thresholdK: string
  models: { model: string; thresholdK: string }[]
}

export function longContextPricingToForm(config?: GroupLongContextPricing): GroupLongContextPricingForm {
  return {
    thresholdK: config?.threshold_tokens ? String(config.threshold_tokens / 1000) : '',
    models: Object.entries(config?.model_thresholds || {}).map(([model, tokens]) => ({ model, thresholdK: String(tokens / 1000) }))
  }
}

export function longContextPricingFromForm(form: GroupLongContextPricingForm): GroupLongContextPricing | null {
  const parse = (value: string): number | null => {
    const k = Number(value)
    return value.trim() !== '' && Number.isInteger(k) && k > 0 && k <= 2147483 ? k * 1000 : null
  }
  const threshold = form.thresholdK.trim() === '' ? 0 : parse(form.thresholdK)
  if (threshold === null) return null
  const models: Record<string, number> = Object.create(null)
  for (const row of form.models) {
    let model = row.model.trim().toLowerCase()
    if (model.startsWith('claude-')) model = model.replace(/\./g, '-')
    const tokens = parse(row.thresholdK)
    if (!model || model.length > 256 || /[\s*?]/.test(model) || tokens === null || Object.prototype.hasOwnProperty.call(models, model)) return null
    models[model] = tokens
  }
  return { threshold_tokens: threshold, model_thresholds: models }
}
