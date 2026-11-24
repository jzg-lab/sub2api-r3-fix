import type { PlazaModel, ModelPlazaGroup } from '@/api/modelPlaza'
import type { UserPricingInterval } from '@/api/channels'

export const TOKEN_UNIT = 1_000_000
const BALANCE_USD_PER_CNY = 10
export type PriceField = 'input_price' | 'output_price' | 'cache_read_price' | 'cache_write_price'
export const priceFields: PriceField[] = ['input_price', 'output_price', 'cache_read_price', 'cache_write_price']
export interface PlazaOffer { group: ModelPlazaGroup; model: PlazaModel }
export interface PlazaEntry { key: string; offers: PlazaOffer[] }

export function modelKey(platform: string, name: string) { return JSON.stringify([platform, name]) }
export function monitorKey(platform: string, group: number | undefined, name: string | undefined) {
  return JSON.stringify([platform, group, name])
}
export function catalogue(groups: ModelPlazaGroup[]): PlazaEntry[] {
  const entries = new Map<string, PlazaEntry>()
  for (const group of groups) for (const model of group.models) {
    const key = modelKey(model.platform, model.name)
    const entry = entries.get(key) ?? { key, offers: [] }
    entry.offers.push({ group, model })
    entries.set(key, entry)
  }
  return [...entries.values()]
}
export function billingMode(model: PlazaModel) { return model.pricing?.billing_mode || 'token' }
export function displayPrice(value: number | null | undefined, offer: PlazaOffer, beforeMultiplier: boolean): number | null {
  if (value == null || !Number.isFinite(value)) return null
  if (beforeMultiplier) return value
  const rate = offer.model.current_rate_multiplier
  // Older/unavailable backends must not silently display a guessed billing price.
  if (rate == null || !Number.isFinite(rate)) return null
  return value / BALANCE_USD_PER_CNY * rate
}
export function formatPrice(value: number | null | undefined, unit = 1, currency: '$' | '¥' = '$'): string {
  if (value == null || !Number.isFinite(value)) return '—'
  return `${currency}${new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(value * unit)}`
}
export function pricingRows(model: PlazaModel): Array<Partial<UserPricingInterval>> {
  const p = model.pricing
  if (!p) return []
  return p.intervals?.length ? p.intervals : [p]
}
export function primaryPrice(offer: PlazaOffer) {
  const p = pricingRows(offer.model)[0]
  return billingMode(offer.model) === 'token' ? p?.input_price : p?.per_request_price
}
