import type { ModelPlazaGroup, ModelPlazaResponse } from '@/api/modelPlaza'
export function plazaFixture(): ModelPlazaResponse {
  const group: ModelPlazaGroup = {
    id: 1, name: 'Standard', description: 'Configured prices', platform: 'openai', subscription_type: 'standard',
    rate_multiplier: 0.5, peak_rate_enabled: false, peak_start: '', peak_end: '', peak_rate_multiplier: 1,
    is_exclusive: false, image_rate_independent: false, image_rate_multiplier: 1, long_context_pricing_enabled: true,
    models: [{ name: 'gpt-5', platform: 'openai', current_rate_multiplier: 0.5,
      pricing: { billing_mode: 'token', input_price: 3e-6, output_price: 15e-6, cache_write_price: null, cache_read_price: 3e-7, image_input_price: null, image_output_price: null, per_request_price: null, intervals: [] },
      official_pricing: { input_price: 5e-6, output_price: 20e-6, cache_write_price: null, cache_read_price: 5e-7 }
    }]
  }
  return { description: '', balance_recharge_multiplier: 10, groups: [group] }
}
