export type OpenAIReenableWatchOutcome = 'failed' | 'passed'

interface OpenAIReenableWatcher {
  accountId: number
  deadlineAt: number
  generation: number
  reenabledAt: number
  timer: ReturnType<typeof setTimeout> | null
}

interface OpenAIReenableLifecycleOptions<Health> {
  fetchHealth: (accountId: number) => Promise<Health | undefined>
  getProbeAt: (health: Health) => number
  classify: (health: Health) => OpenAIReenableWatchOutcome | null
  onOutcome: (accountId: number, outcome: OpenAIReenableWatchOutcome, health: Health) => void
  intervalMs: number
  deadlineMs: number
  now?: () => number
  setTimer?: typeof setTimeout
  clearTimer?: typeof clearTimeout
}

interface OpenAIReenableRequest<Result> {
  accountId: number
  execute: () => Promise<Result>
  getReenabledAt: (result: Result) => number
  onSuccess: (result: Result) => void
  onError: (error: unknown) => void
  onFinally: () => void
}

export const createOpenAIReenableLifecycle = <Health>(
  options: OpenAIReenableLifecycleOptions<Health>
) => {
  const now = options.now ?? Date.now
  const setTimer = options.setTimer ?? setTimeout
  const clearTimer = options.clearTimer ?? clearTimeout
  const watchers = new Map<number, OpenAIReenableWatcher>()
  const requests = new Map<number, symbol>()
  let disposed = false
  let nextGeneration = 0

  const isCurrent = (watcher: OpenAIReenableWatcher): boolean => {
    const current = watchers.get(watcher.accountId)
    return !disposed && current === watcher && current.generation === watcher.generation
  }

  const stopWatcher = (accountId: number, expected?: OpenAIReenableWatcher): boolean => {
    const watcher = watchers.get(accountId)
    if (!watcher || (expected && watcher !== expected)) return false
    if (watcher.timer !== null) {
      clearTimer(watcher.timer)
      watcher.timer = null
    }
    watchers.delete(accountId)
    return true
  }

  const schedule = (watcher: OpenAIReenableWatcher, delayMs: number) => {
    if (!isCurrent(watcher)) return
    const remainingMs = watcher.deadlineAt - now()
    if (remainingMs <= 0) {
      stopWatcher(watcher.accountId, watcher)
      return
    }
    watcher.timer = setTimer(() => {
      watcher.timer = null
      void poll(watcher)
    }, Math.min(delayMs, remainingMs))
  }

  const poll = async (watcher: OpenAIReenableWatcher) => {
    if (!isCurrent(watcher)) return
    if (now() >= watcher.deadlineAt) {
      stopWatcher(watcher.accountId, watcher)
      return
    }

    let health: Health | undefined
    try {
      health = await options.fetchHealth(watcher.accountId)
    } catch {
      if (isCurrent(watcher)) schedule(watcher, options.intervalMs)
      return
    }

    if (!isCurrent(watcher)) return
    if (now() >= watcher.deadlineAt) {
      stopWatcher(watcher.accountId, watcher)
      return
    }
    const probeAt = health ? options.getProbeAt(health) : Number.NaN
    if (!health || !Number.isFinite(probeAt) || probeAt <= watcher.reenabledAt) {
      schedule(watcher, options.intervalMs)
      return
    }

    const outcome = options.classify(health)
    if (outcome === null) {
      schedule(watcher, options.intervalMs)
      return
    }

    if (!isCurrent(watcher)) return
    options.onOutcome(watcher.accountId, outcome, health)
    if (isCurrent(watcher)) stopWatcher(watcher.accountId, watcher)
  }

  const watch = (accountId: number, reenabledAt: number): boolean => {
    if (disposed || !Number.isFinite(reenabledAt)) return false
    stopWatcher(accountId)
    const watcher: OpenAIReenableWatcher = {
      accountId,
      deadlineAt: now() + options.deadlineMs,
      generation: ++nextGeneration,
      reenabledAt,
      timer: null,
    }
    watchers.set(accountId, watcher)
    schedule(watcher, options.intervalMs)
    return true
  }

  const runRequest = async <Result>(request: OpenAIReenableRequest<Result>): Promise<void> => {
    if (disposed) return
    const generation = Symbol()
    requests.set(request.accountId, generation)
    const isRequestCurrent = () => !disposed && requests.get(request.accountId) === generation
    try {
      const result = await request.execute()
      if (!isRequestCurrent()) return
      request.onSuccess(result)
      if (!isRequestCurrent()) return
      watch(request.accountId, request.getReenabledAt(result))
    } catch (error) {
      if (isRequestCurrent()) request.onError(error)
    } finally {
      if (isRequestCurrent()) {
        try {
          request.onFinally()
        } finally {
          // The callback may have started another request for this account.
          if (isRequestCurrent()) requests.delete(request.accountId)
        }
      }
    }
  }

  const dispose = () => {
    if (disposed) return
    disposed = true
    requests.clear()
    for (const accountId of [...watchers.keys()]) {
      stopWatcher(accountId)
    }
  }

  return {
    dispose,
    runRequest,
    stop: stopWatcher,
    watch,
  }
}
