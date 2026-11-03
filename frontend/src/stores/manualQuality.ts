import { defineStore } from 'pinia'
import { markRaw, ref, watch } from 'vue'
import { useAuthStore } from './auth'
import { useAppStore } from './app'
import { i18n } from '@/i18n'
import { manualQualityAPI, type QualityKind, type QualityResult, type QualityTemplate } from '@/api/admin/manualQuality'
import { extractApiErrorMessage } from '@/utils/apiError'

export const DEFAULT_QUALITY_MODEL = 'gpt-6.1-sol'
export const adminQualityKey = (id: number, model: string, kind: QualityKind) => `admin:${id}:${model}:${kind}`

export interface QualityTask {
  running: boolean
  result: QualityResult | null
  error: string
  saved: boolean
  controller: AbortController
  notificationId?: string
  keyName?: string
  keyId?: number
  input?: QualityTemplate & { model: string }
}
type Outcome = { result: QualityResult; saved?: boolean; error?: string }

export const useManualQualityStore = defineStore('manualQuality', () => {
  const auth = useAuthStore()
  const app = useAppStore()
  const tasks = ref<Record<string, QualityTask>>({})
  function reset() {
    const previous = tasks.value
    tasks.value = {}
    Object.values(previous).forEach(task => {
      if (task.notificationId) app.hideToast(task.notificationId)
      task.controller.abort()
    })
  }
  watch(() => `${auth.user?.id}:${auth.user?.role}:${auth.isAuthenticated}`, reset)

  async function start(key: string, label: string, execute: (signal: AbortSignal) => Promise<Outcome>) {
    if (tasks.value[key]?.running) return tasks.value[key]
    const previousNotification = tasks.value[key]?.notificationId
    if (previousNotification) app.hideToast(previousNotification)
    tasks.value[key] = { running: true, result: null, error: '', saved: false, controller: markRaw(new AbortController()) }
    const task = tasks.value[key]
    const timeout = setTimeout(() => task.controller.abort(), 10 * 60 * 1000)
    try {
      const outcome = await execute(task.controller.signal)
      if (tasks.value[key] !== task) return task
      task.result = outcome.result
      task.saved = outcome.saved === true
      if (outcome.saved === false) task.error = outcome.error || i18n.global.t('admin.accounts.manualQuality.saveFailed')
      if (task.controller.signal.aborted && task.result.status !== 'completed') {
        task.error = i18n.global.t('admin.accounts.manualQuality.canceled')
      }
    } catch (cause) {
      if (tasks.value[key] !== task) return task
      task.error = task.controller.signal.aborted
        ? i18n.global.t('admin.accounts.manualQuality.canceled')
        : extractApiErrorMessage(cause)
    } finally {
      clearTimeout(timeout)
      task.running = false
      if (tasks.value[key] === task) {
        const success = !task.error && task.result?.status === 'completed'
        task.notificationId = app.showToast(success ? 'success' : 'warning', i18n.global.t(success ? 'qualityTest.completed' : 'qualityTest.failed', { name: label }))
      }
    }
    return task
  }

  function runAdmin(id: number, name: string, input: QualityTemplate & { model: string }) {
    const key = adminQualityKey(id, input.model, input.kind)
    const promise = start(key, `${name} · ${input.model} · ${i18n.global.t(`admin.accounts.manualQuality.${input.kind}`)}`, signal => manualQualityAPI.run(id, input, signal))
    tasks.value[key].input = input
    return promise
  }

  return { tasks, start, runAdmin, reset }
})
