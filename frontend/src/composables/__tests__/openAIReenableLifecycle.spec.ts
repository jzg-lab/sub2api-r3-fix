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

const createRequest = (accountId: number) => {
  const pending = deferred<{ reenabled_at: number }>()
  return {
    pending,
    request: {
      accountId,
      execute: vi.fn(() => pending.promise),
      getReenabledAt: (result: { reenabled_at: number }) => result.reenabled_at,
      onSuccess: vi.fn(),
      onError: vi.fn(),
      onFinally: vi.fn(),
    },
  }
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

  it.each([7, 8])('keeps both accounts independent when account %i finishes first', async (firstId) => {
    const fetchHealth = vi.fn(async (accountId: number): Promise<Health> => ({
      lastProbeAt: Date.now(),
      state: accountId === 7 ? 'pending_replace' : 'on_duty',
    }))
    const onOutcome = vi.fn()
    const lifecycle = createLifecycle(fetchHealth, onOutcome)
    const requests = [createRequest(7), createRequest(8)]
    const running = requests.map(({ request }) => lifecycle.runRequest(request))
    const firstIndex = firstId === 7 ? 0 : 1
    const secondIndex = 1 - firstIndex
    const result = { reenabled_at: Date.now() }

    requests[firstIndex].pending.resolve(result)
    await running[firstIndex]
    expect(requests[firstIndex].request.onSuccess.mock.calls).toEqual([[result]])
    expect(requests[firstIndex].request.onFinally).toHaveBeenCalledTimes(1)
    expect(requests[secondIndex].request.onSuccess).not.toHaveBeenCalled()
    expect(requests[secondIndex].request.onFinally).not.toHaveBeenCalled()

    requests[secondIndex].pending.resolve(result)
    await running[secondIndex]
    for (const { request } of requests) {
      expect(request.onSuccess.mock.calls).toEqual([[result]])
      expect(request.onError).not.toHaveBeenCalled()
      expect(request.onFinally).toHaveBeenCalledTimes(1)
    }
    await vi.advanceTimersByTimeAsync(20_000)
    expect(fetchHealth).toHaveBeenCalledTimes(2)
    expect(onOutcome).toHaveBeenCalledTimes(2)
    expect(onOutcome).toHaveBeenCalledWith(
      7, 'failed', expect.objectContaining({ state: 'pending_replace' })
    )
    expect(onOutcome).toHaveBeenCalledWith(
      8, 'passed', expect.objectContaining({ state: 'on_duty' })
    )
    lifecycle.dispose()
  })

  it.each([true, false])('keeps account errors independent (failure first: %s)', async (failureFirst) => {
    const failed = createRequest(7)
    const passed = createRequest(8)
    const fetchHealth = vi.fn(async (): Promise<Health> => ({
      lastProbeAt: Date.now(),
      state: 'on_duty',
    }))
    const onOutcome = vi.fn()
    const lifecycle = createLifecycle(fetchHealth, onOutcome)
    const failedRun = lifecycle.runRequest(failed.request)
    const passedRun = lifecycle.runRequest(passed.request)
    const error = new Error('reenable failed')
    const result = { reenabled_at: Date.now() }

    if (failureFirst) {
      failed.pending.reject(error)
      await failedRun
      expect(passed.request.onFinally).not.toHaveBeenCalled()
      passed.pending.resolve(result)
      await passedRun
    } else {
      passed.pending.resolve(result)
      await passedRun
      expect(failed.request.onFinally).not.toHaveBeenCalled()
      failed.pending.reject(error)
      await failedRun
    }

    expect(failed.request.onError.mock.calls).toEqual([[error]])
    expect(failed.request.onSuccess).not.toHaveBeenCalled()
    expect(failed.request.onFinally).toHaveBeenCalledTimes(1)
    expect(passed.request.onSuccess.mock.calls).toEqual([[result]])
    expect(passed.request.onError).not.toHaveBeenCalled()
    expect(passed.request.onFinally).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(20_000)
    expect(fetchHealth.mock.calls).toEqual([[8]])
    expect(onOutcome).toHaveBeenCalledWith(
      8, 'passed', expect.objectContaining({ state: 'on_duty' })
    )
    lifecycle.dispose()
  })

  it.each(['success', 'error'])('drops an older same-account %s without clearing its replacement', async (outcome) => {
    const stale = createRequest(7)
    const current = createRequest(7)
    const fetchHealth = vi.fn(async (): Promise<Health> => ({
      lastProbeAt: Date.now(),
      state: 'on_duty',
    }))
    const lifecycle = createLifecycle(fetchHealth)
    const staleRun = lifecycle.runRequest(stale.request)
    const currentRun = lifecycle.runRequest(current.request)
    if (outcome === 'success') {
      stale.pending.resolve({ reenabled_at: Date.now() })
    } else {
      stale.pending.reject(new Error('stale error'))
    }
    await staleRun

    expect(stale.request.onSuccess).not.toHaveBeenCalled()
    expect(stale.request.onError).not.toHaveBeenCalled()
    expect(stale.request.onFinally).not.toHaveBeenCalled()
    expect(current.request.onFinally).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(20_000)
    expect(fetchHealth).not.toHaveBeenCalled()

    const result = { reenabled_at: Date.now() }
    current.pending.resolve(result)
    await currentRun
    expect(current.request.onSuccess.mock.calls).toEqual([[result]])
    expect(current.request.onFinally).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(20_000)
    expect(fetchHealth.mock.calls).toEqual([[7]])
    lifecycle.dispose()
  })

  it.each(['success', 'error'])('does not reuse a completed request identity for a stale %s', async (outcome) => {
    const stale = createRequest(7)
    const completed = createRequest(7)
    const current = createRequest(7)
    const lifecycle = createLifecycle(vi.fn())
    const staleRun = lifecycle.runRequest(stale.request)
    const completedRun = lifecycle.runRequest(completed.request)
    completed.pending.resolve({ reenabled_at: Date.now() })
    await completedRun
    const currentRun = lifecycle.runRequest(current.request)

    if (outcome === 'success') {
      stale.pending.resolve({ reenabled_at: Date.now() })
    } else {
      stale.pending.reject(new Error('stale error'))
    }
    await staleRun
    expect(stale.request.onSuccess).not.toHaveBeenCalled()
    expect(stale.request.onError).not.toHaveBeenCalled()
    expect(stale.request.onFinally).not.toHaveBeenCalled()

    const result = { reenabled_at: Date.now() }
    current.pending.resolve(result)
    await currentRun
    expect(current.request.onSuccess.mock.calls).toEqual([[result]])
    expect(current.request.onFinally).toHaveBeenCalledTimes(1)
    lifecycle.dispose()
  })

  it('drops failures and future requests after disposal', async () => {
    const first = createRequest(7)
    const second = createRequest(8)
    const future = createRequest(9)
    const fetchHealth = vi.fn()
    const lifecycle = createLifecycle(fetchHealth)
    const firstRun = lifecycle.runRequest(first.request)
    const secondRun = lifecycle.runRequest(second.request)

    lifecycle.dispose()
    first.pending.reject(new Error('late error'))
    second.pending.resolve({ reenabled_at: Date.now() })
    await Promise.all([firstRun, secondRun])
    await lifecycle.runRequest(future.request)
    for (const { request } of [first, second, future]) {
      expect(request.onSuccess).not.toHaveBeenCalled()
      expect(request.onError).not.toHaveBeenCalled()
      expect(request.onFinally).not.toHaveBeenCalled()
    }
    expect(future.request.execute).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(20_000)
    expect(fetchHealth).not.toHaveBeenCalled()
  })

  it.each([false, true])('preserves a request started from onFinally (callback throws: %s)', async (throws) => {
    const first = createRequest(7)
    const next = createRequest(7)
    const fetchHealth = vi.fn()
    const lifecycle = createLifecycle(fetchHealth)
    let nextRun: Promise<void> | undefined
    const error = new Error('cleanup failed')
    first.request.onFinally.mockImplementation(() => {
      nextRun = lifecycle.runRequest(next.request)
      if (throws) throw error
    })
    const firstRun = lifecycle.runRequest(first.request)
    first.pending.resolve({ reenabled_at: Date.now() })
    if (throws) {
      await expect(firstRun).rejects.toBe(error)
    } else {
      await firstRun
    }
    expect(first.request.onFinally).toHaveBeenCalledTimes(1)
    expect(next.request.execute).toHaveBeenCalledTimes(1)

    const result = { reenabled_at: Date.now() }
    next.pending.resolve(result)
    await nextRun
    expect(next.request.onSuccess.mock.calls).toEqual([[result]])
    expect(next.request.onFinally).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(20_000)
    expect(fetchHealth.mock.calls).toEqual([[7]])
    lifecycle.dispose()
  })
})
