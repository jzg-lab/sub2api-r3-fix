import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createOpenAIReenableLifecycle } from '../openAIReenableLifecycle'

interface Health {
  lastProbeAt: number
  state: 'qualification' | 'pending_replace' | 'on_duty'
}

const deferred = <T>() => {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, reject, resolve }
}

const flushPromises = async () => {
  await Promise.resolve()
  await Promise.resolve()
}

const createLifecycle = (
  fetchHealth: (accountId: number) => Promise<Health | undefined>,
  onOutcome = vi.fn()
) => createOpenAIReenableLifecycle<Health>({
  fetchHealth,
  getProbeAt: (health) => health.lastProbeAt,
  classify: (health) => {
    if (health.state === 'pending_replace') return 'failed'
    if (health.state === 'on_duty') return 'passed'
    return null
  },
  onOutcome,
  intervalMs: 20_000,
  deadlineMs: 15 * 60_000,
})

describe('openAI reenable lifecycle', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-09-28T14:00:00Z'))
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('polls single-flight and measures the 15 minute deadline by elapsed time', async () => {
    const pending = deferred<Health | undefined>()
    const fetchHealth = vi.fn(() => pending.promise)
    const onOutcome = vi.fn()
    const lifecycle = createLifecycle(fetchHealth, onOutcome)

    expect(lifecycle.watch(7, Date.now() - 1)).toBe(true)
    vi.advanceTimersByTime(20_000)
    expect(fetchHealth).toHaveBeenCalledTimes(1)

    vi.advanceTimersByTime(14 * 60_000 + 40_000)
    expect(fetchHealth).toHaveBeenCalledTimes(1)

    pending.resolve({ lastProbeAt: Date.now(), state: 'on_duty' })
    await flushPromises()
    expect(onOutcome).not.toHaveBeenCalled()

    vi.advanceTimersByTime(60_000)
    expect(fetchHealth).toHaveBeenCalledTimes(1)
  })

  it('ignores an old in-flight response after a replacement watcher starts', async () => {
    const oldResponse = deferred<Health | undefined>()
    const fetchHealth = vi.fn()
      .mockImplementationOnce(() => oldResponse.promise)
      .mockImplementationOnce(async () => ({
        lastProbeAt: Date.now() + 1,
        state: 'on_duty',
      }))
    const onOutcome = vi.fn()
    const lifecycle = createLifecycle(fetchHealth, onOutcome)

    lifecycle.watch(7, Date.now() - 1)
    vi.advanceTimersByTime(20_000)
    expect(fetchHealth).toHaveBeenCalledTimes(1)

    lifecycle.watch(7, Date.now())
    oldResponse.resolve({ lastProbeAt: Date.now() + 1, state: 'pending_replace' })
    await flushPromises()
    expect(onOutcome).not.toHaveBeenCalled()

    vi.advanceTimersByTime(20_000)
    await flushPromises()
    expect(fetchHealth).toHaveBeenCalledTimes(2)
    expect(onOutcome).toHaveBeenCalledTimes(1)
    expect(onOutcome).toHaveBeenCalledWith(
      7,
      'passed',
      expect.objectContaining({ state: 'on_duty' })
    )
  })

  it('drops a watcher response that arrives after disposal', async () => {
    const pending = deferred<Health | undefined>()
    const onOutcome = vi.fn()
    const lifecycle = createLifecycle(vi.fn(() => pending.promise), onOutcome)

    lifecycle.watch(7, Date.now() - 1)
    vi.advanceTimersByTime(20_000)
    lifecycle.dispose()
    pending.resolve({ lastProbeAt: Date.now() + 1, state: 'pending_replace' })
    await flushPromises()

    expect(onOutcome).not.toHaveBeenCalled()
  })

  it('fails closed for non-finite probe and reenable timestamps', async () => {
    const fetchHealth = vi.fn()
      .mockResolvedValue({ lastProbeAt: Number.NaN, state: 'pending_replace' })
    const onOutcome = vi.fn()
    const lifecycle = createLifecycle(fetchHealth, onOutcome)

    expect(lifecycle.watch(7, Number.NaN)).toBe(false)
    vi.advanceTimersByTime(20_000)
    expect(fetchHealth).not.toHaveBeenCalled()

    expect(lifecycle.watch(7, Date.now() - 1)).toBe(true)
    vi.advanceTimersByTime(20_000)
    await flushPromises()

    expect(fetchHealth).toHaveBeenCalledTimes(1)
    expect(onOutcome).not.toHaveBeenCalled()
  })

  it('does not notify or create a watcher when the reenable API finishes after disposal', async () => {
    const request = deferred<{ reenabled_at: number }>()
    const fetchHealth = vi.fn<() => Promise<Health | undefined>>()
    const onSuccess = vi.fn()
    const onError = vi.fn()
    const onFinally = vi.fn()
    const lifecycle = createLifecycle(fetchHealth)

    const running = lifecycle.runRequest({
      accountId: 7,
      execute: () => request.promise,
      getReenabledAt: (result) => result.reenabled_at,
      onSuccess,
      onError,
      onFinally,
    })
    lifecycle.dispose()
    request.resolve({ reenabled_at: Date.now() })
    await running

    expect(onSuccess).not.toHaveBeenCalled()
    expect(onError).not.toHaveBeenCalled()
    expect(onFinally).not.toHaveBeenCalled()
    vi.advanceTimersByTime(60_000)
    expect(fetchHealth).not.toHaveBeenCalled()
  })
})
