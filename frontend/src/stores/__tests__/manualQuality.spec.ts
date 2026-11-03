import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { i18n, loadLocaleMessages } from '@/i18n'
import type { QualityResult } from '@/api/admin/manualQuality'
import { useAppStore } from '../app'
import { useManualQualityStore } from '../manualQuality'

const result: QualityResult = {
  account_id: 0,
  run_id: 'run',
  model: 'gpt-6.1-sol',
  upstream_model: '',
  kind: 'candy',
  prompt: 'question',
  reference_answer: '21',
  answer: '21',
  started_at: '2026-11-03T00:00:00Z',
  duration_ms: 1,
  status: 'completed'
}

beforeEach(async () => {
  await loadLocaleMessages('en')
  setActivePinia(createPinia())
})
afterEach(() => vi.useRealTimers())

describe('manual quality task lifecycle', () => {
  it('replaces the previous notice for a repeated test without removing other notices', async () => {
    const tests = useManualQualityStore()
    const app = useAppStore()
    const unrelated = app.showToast('info', 'Other notification')
    const execute = async () => ({ result })
    await tests.start('user', 'First', execute)
    const previous = tests.tasks.user.notificationId
    await tests.start('admin:7:model:candy', 'Admin', execute)
    const adminNotice = tests.tasks['admin:7:model:candy'].notificationId
    await tests.start('user', 'Second', execute)
    expect(app.toasts.map(toast => toast.id)).not.toContain(previous)
    expect(app.toasts.map(toast => toast.id)).toEqual([unrelated, adminNotice, tests.tasks.user.notificationId])
    tests.reset()
    expect(app.toasts.map(toast => toast.id)).toEqual([unrelated])
  })

  it('keeps a partial answer after cancellation and warns that upstream may still charge', async () => {
    const tests = useManualQualityStore()
    let finish!: (value: { result: QualityResult }) => void
    const request = tests.start('user', 'User', () => new Promise(resolve => { finish = resolve }))
    tests.tasks.user.controller.abort()
    finish({ result: { ...result, answer: 'partial reasoning', status: 'failed', error: 'AbortError' } })
    const task = await request
    expect(task.result?.answer).toBe('partial reasoning')
    expect(task.error).toBeTruthy()
    expect(task.error).toBe(i18n.global.t('admin.accounts.manualQuality.canceled'))
    expect(useAppStore().toasts[0].type).toBe('warning')
  })

  it('aborts after ten minutes and suppresses duplicate starts while waiting', async () => {
    vi.useFakeTimers()
    const tests = useManualQualityStore()
    const execute = vi.fn((signal: AbortSignal) => new Promise<{ result: QualityResult }>((_, reject) => {
      signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })
    }))
    const request = tests.start('user', 'User', execute)
    await tests.start('user', 'Duplicate', execute)
    expect(execute).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(10 * 60 * 1000)
    const task = await request
    expect(task.controller.signal.aborted).toBe(true)
    expect(task.running).toBe(false)
    expect(task.error).toBeTruthy()
    expect(useAppStore().toasts).toHaveLength(1)
  })
})
