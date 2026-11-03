import { apiClient, buildGatewayUrl } from './client'
import type { QualityResult, QualityTemplate } from './admin/manualQuality'

export async function qualityTestTemplates(): Promise<QualityTemplate[]> {
  return (await apiClient.get<QualityTemplate[]>('/user/quality-test/templates')).data
}

// Use the regular gateway so ownership, quota, IP rules, routing and billing all apply.
export async function runUserQualityTest(apiKey: string, input: QualityTemplate & { model: string }, signal: AbortSignal): Promise<{ result: QualityResult }> {
  const started = Date.now()
  const result: QualityResult = {
    ...input, account_id: 0, run_id: crypto.randomUUID(), started_at: new Date(started).toISOString(),
    duration_ms: 0, status: 'running', answer: '', upstream_model: '',
    parameters: { protocol: 'chat_completions', model: input.model, stream: true }
  }
  const response = await fetch(buildGatewayUrl('/v1/chat/completions'), {
    method: 'POST', signal,
    headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: input.model, messages: [{ role: 'user', content: input.prompt }], stream: true, stream_options: { include_usage: true } })
  })
  if (!response.ok) {
    const body = await response.json().catch(() => null)
    throw new Error(body?.error?.message || body?.message || `HTTP ${response.status}`)
  }
  if (!response.body) throw new Error('No response body')
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let bytes = 0
  let finished = ''
  let ended = false
  try {
    while (!ended) {
      const { value, done } = await reader.read()
      if (done) break
      bytes += value.byteLength
      if (bytes > 2 * 1024 * 1024) throw new Error('Test response is too large')
      buffer += decoder.decode(value, { stream: true })
      buffer = buffer.replace(/\r\n/g, '\n')
      let boundary: number
      while ((boundary = buffer.indexOf('\n\n')) !== -1) {
        const frame = buffer.slice(0, boundary)
        buffer = buffer.slice(boundary + 2)
        const data = frame.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n')
        if (!data) continue
        if (data === '[DONE]') { ended = true; break }
        const chunk = JSON.parse(data)
        if (chunk.error) throw new Error(chunk.error.message || 'Test failed')
        const choice = chunk.choices?.find((value: { index: number }) => value.index === 0)
        if (typeof choice?.delta?.content === 'string') result.answer += choice.delta.content
        if (result.answer.length > 256 * 1024) throw new Error('Test answer is too large')
        if (choice?.finish_reason) finished = choice.finish_reason
        if (chunk.usage) result.usage = Object.fromEntries(Object.entries(chunk.usage).filter((entry): entry is [string, number] => typeof entry[1] === 'number'))
      }
    }
    result.status = ended && finished === 'stop' && result.answer.trim() ? 'completed' : 'failed'
    if (result.status === 'failed') result.error = 'No complete text response was received.'
  } catch (cause) {
    // Keep a partial answer for review, but never report it as a completed test.
    result.status = 'failed'
    result.error = cause instanceof Error ? cause.message : 'The test was interrupted.'
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
  result.duration_ms = Date.now() - started
  return { result }
}
