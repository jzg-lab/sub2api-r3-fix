import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { runUserQualityTest } from '../qualityTest'

const fetchMock = vi.fn()
const input = { model: 'gpt-6.1-sol', kind: 'candy' as const, prompt: 'edited question', reference_answer: 'secret reference' }
const delta = (text: string, finish: string | null = null) => `data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: text }, finish_reason: finish }] })}\r\n\r\n`
function stream(chunks: string[]) {
  const encoder = new TextEncoder()
  // One byte at a time exercises CRLF and multibyte UTF-8 across network reads.
  const bytes = encoder.encode(chunks.join(''))
  return { ok: true, body: new ReadableStream({ start(controller) { for (const byte of bytes) controller.enqueue(Uint8Array.of(byte)); controller.close() } }) }
}
beforeEach(() => { vi.stubGlobal('fetch', fetchMock); fetchMock.mockReset() })
afterEach(() => vi.unstubAllGlobals())

describe('user quality test gateway', () => {
  it('uses the user key and edited question without sending the reference; collects complete UTF-8 output and usage', async () => {
    fetchMock.mockResolvedValue(stream([delta('答案：21', 'stop'), 'data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}\r\n\r\n', 'data: [DONE]\r\n\r\n']))
    const { result } = await runUserQualityTest('sk-own', input, new AbortController().signal)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toMatch(/\/v1\/chat\/completions$/)
    expect(init.headers.Authorization).toBe('Bearer sk-own')
    expect(JSON.parse(init.body)).toMatchObject({ model: 'gpt-6.1-sol', messages: [{ role: 'user', content: 'edited question' }], stream: true })
    expect(init.body).not.toContain('secret reference')
    expect(result).toMatchObject({ status: 'completed', answer: '答案：21', reference_answer: 'secret reference', usage: { prompt_tokens: 5, completion_tokens: 3 } })
  })

  it.each([
    ['truncated output', [delta('partial', 'length'), 'data: [DONE]\n\n']],
    ['disconnection', [delta('partial')]],
    ['missing terminal marker', [delta('partial', 'stop')]],
    ['empty answer', [delta('', 'stop'), 'data: [DONE]\n\n']],
    ['stream error', [delta('partial'), 'data: {"error":{"message":"upstream failed"}}\n\n']]
  ])('does not report %s as a complete answer', async (_, chunks) => {
    fetchMock.mockResolvedValue(stream(chunks))
    const { result } = await runUserQualityTest('sk-own', input, new AbortController().signal)
    expect(result.status).toBe('failed')
    expect(result.error).toBeTruthy()
  })

  it('reports normal gateway authentication/quota errors without replaying the request', async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 403, json: async () => ({ error: { message: 'quota exceeded' } }) })
    await expect(runUserQualityTest('sk-own', input, new AbortController().signal)).rejects.toThrow('quota exceeded')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
