import type { PlazaModel, ModelPlazaGroup } from '@/api/modelPlaza'
import type { UserPricingInterval } from '@/api/channels'

export type PlazaCurrency = 'CNY' | 'USD'
export const USD_TO_CNY = 6.70
export const TOKEN_UNIT = 1_000_000
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
export function displayPrice(value: number | null | undefined, offer: PlazaOffer, currency: PlazaCurrency, conversion?: number, original = false): number | null {
  if (value == null || !Number.isFinite(value)) return null
  if (original) return value * (currency === 'CNY' ? USD_TO_CNY : 1)
  const rate = offer.model.current_rate_multiplier
  // Older/unavailable backends must not silently display a guessed billing price.
  if (rate == null || !Number.isFinite(rate)) return null
  const charge = value * rate
  if (currency === 'USD') return charge
  return conversion != null && Number.isFinite(conversion) && conversion > 0 ? charge / conversion : null
}
export function formatPrice(value: number | null | undefined, unit = 1, currency: PlazaCurrency = 'USD'): string {
  if (value == null || !Number.isFinite(value)) return '—'
  return `${currency === 'CNY' ? '¥' : '$'}${new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(value * unit)}`
}
export function pricingRows(model: PlazaModel, original = false): Array<Partial<UserPricingInterval>> {
  // Official token prices cannot be compared with per-request/image/video charges.
  if (original && billingMode(model) !== 'token') return []
  const p = original ? model.official_pricing : model.pricing
  if (!p) return []
  return p.intervals?.length ? p.intervals : [p]
}
export function primaryPrice(offer: PlazaOffer, original = false) {
  const p = pricingRows(offer.model, original)[0]
  return billingMode(offer.model) === 'token' ? p?.input_price : p?.per_request_price
}
