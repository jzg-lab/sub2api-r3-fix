import { apiClient, buildApiUrl } from '../client'
import { ADMIN_UI_REQUEST_HEADER } from '../adminUIRequest'

export type QualityKind = 'candy' | 'pelican'
export interface QualityTemplate {
  kind: QualityKind
  prompt: string
  reference_answer: string
}
export interface QualityResult extends QualityTemplate {
  account_id: number
  model: string
  run_id: string
  started_at: string
  duration_ms: number
  status: 'running' | 'completed' | 'failed' | 'unknown'
  answer: string
  error?: string
  upstream_model: string
  parameters?: Record<string, unknown>
  usage?: Record<string, number>
}
const base = '/admin/accounts'
export const manualQualityAPI = {
  async templates() {
    return (await apiClient.get<{ templates: QualityTemplate[]; defaults: QualityTemplate[] }>(`${base}/manual-quality/templates`)).data
  },
  async save(template: QualityTemplate) {
    return (await apiClient.put<QualityTemplate>(`${base}/manual-quality/templates`, template)).data
  },
  async result(id: number, model: string, kind: QualityKind) {
    return (await apiClient.get<QualityResult | null>(`${base}/${id}/manual-quality`, { params: { model, kind } })).data
  },
  async summaries(ids: number[]) {
    return (await apiClient.get<QualityResult[]>(`${base}/manual-quality/summaries`, { params: { ids: ids.join(',') } })).data
  },
  async run(id: number, input: QualityTemplate & { model: string }, signal: AbortSignal) {
    const response = await fetch(buildApiUrl(`${base}/${id}/manual-quality`), {
      method: 'POST', signal,
      headers: { Authorization: `Bearer ${localStorage.getItem('auth_token')}`, 'Content-Type': 'application/json', [ADMIN_UI_REQUEST_HEADER]: '1' },
      body: JSON.stringify(input)
    })
    if (!response.ok) {
      const error = await response.json().catch(() => null) as { message?: string } | null
      throw new Error(error?.message || `HTTP ${response.status}`)
    }
    if (!response.body) throw new Error('No response body')
    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    try {
      while (true) {
        const { done, value } = await reader.read()
        if (done) throw new Error('The connection closed before the result arrived. Reopen to check the saved result.')
        buffer += decoder.decode(value, { stream: true })
        if (buffer.length > 2 * 1024 * 1024) throw new Error('Test response is too large')
        let boundary: number
        while ((boundary = buffer.indexOf('\n\n')) !== -1) {
          const frame = buffer.slice(0, boundary)
          buffer = buffer.slice(boundary + 2)
          const data = frame.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n')
          if (!data) continue
          const result = JSON.parse(data) as { result: QualityResult | null; saved: boolean; error?: string }
          if (!result.result) throw new Error(result.error || 'Test failed')
          return { ...result, result: result.result }
        }
      }
    } finally { await reader.cancel().catch(() => {}); reader.releaseLock() }
  }
}
