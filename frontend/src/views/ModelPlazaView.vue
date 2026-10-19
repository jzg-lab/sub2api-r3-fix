<template>
  <div class="plaza-page">
    <div class="plaza-shell">
      <PlazaNavBar />
      <main>
        <ModelPlazaContent :response="data" :loading="loading" :error="loadFailed"
          :updated-at="updatedAt" :monitor="monitor" :monitor-unavailable="monitorUnavailable"
          @retry="refresh" />
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import PlazaNavBar from '@/components/modelPlaza/PlazaNavBar.vue'
import ModelPlazaContent from '@/components/modelPlaza/ModelPlazaContent.vue'
import { refreshModelPlaza, type ModelPlazaResponse } from '@/api/modelPlaza'
import { getMatrix, getSnapshot, type MonitorMatrixResponse } from '@/api/channelMonitorV2'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import '@/components/modelPlaza/plaza.css'

const appStore = useAppStore()
const auth = useAuthStore()
const { locale } = useI18n()
const data = ref<ModelPlazaResponse | null>(null)
const loading = ref(true)
const loadFailed = ref(false)
const updatedAt = ref<Date | null>(null)
const monitor = ref<MonitorMatrixResponse | null>(null)
const monitorUnavailable = ref(false)
let etag: string | undefined
let controller: AbortController | undefined
let monitorController: AbortController | undefined
let timer: ReturnType<typeof setInterval> | undefined
let monitorDue = 0
let disposed = false

async function refresh() {
  if (disposed || document.hidden || controller) return
  const request = new AbortController()
  controller = request
  try {
    const next = await refreshModelPlaza(etag, request.signal)
    if (request.signal.aborted) return
    if (next.data) data.value = next.data
    etag = next.etag
    updatedAt.value = new Date()
    loadFailed.value = false
  } catch (error) {
    if (request.signal.aborted) return
    loadFailed.value = true
    const status = (error as { status?: number }).status
    // Permission changes must also remove previously visible groups and prices.
    if (status === 401 || status === 403 || status === 404) {
      data.value = null
      etag = undefined
      monitor.value = null
    }
  } finally {
    if (controller === request) {
      controller = undefined
      loading.value = false
    }
  }
}

async function refreshMonitor() {
  if (disposed || document.hidden || monitorController || Date.now() < monitorDue) return
  if (!auth.isAuthenticated || appStore.cachedPublicSettings?.channel_monitor_enabled === false) {
    monitor.value = null
    monitorUnavailable.value = true
    return
  }
  const request = new AbortController()
  monitorController = request
  monitorDue = Date.now() + 60_000
  try {
    const filter = { range: '24h' as const, platforms: [], groupIds: [], models: [] }
    const [snapshot, matrix] = await Promise.all([
      getSnapshot(filter, false, request.signal),
      getMatrix(filter, 'platform_group_model', false, request.signal, true)
    ])
    if (request.signal.aborted) return
    monitor.value = matrix
    monitorUnavailable.value = false
    monitorDue = Date.now() + Math.max(60, snapshot.config.refresh_interval_seconds) * 1000
  } catch {
    if (!request.signal.aborted) {
      monitor.value = null
      monitorUnavailable.value = true
    }
  } finally {
    if (monitorController === request) monitorController = undefined
  }
}
function resume() {
  if (document.hidden) return
  monitorDue = 0
  void refresh()
  void refreshMonitor()
}
watch([() => auth.user?.id, locale], () => {
  controller?.abort()
  monitorController?.abort()
  controller = undefined
  monitorController = undefined
  etag = undefined
  data.value = null
  monitor.value = null
  loading.value = true
  resume()
})
onMounted(() => {
  void appStore.fetchPublicSettings()
  resume()
  timer = setInterval(() => { void refresh(); void refreshMonitor() }, 3000)
  window.addEventListener('focus', resume)
  window.addEventListener('online', resume)
  document.addEventListener('visibilitychange', resume)
})
onBeforeUnmount(() => {
  disposed = true
  clearInterval(timer)
  controller?.abort()
  monitorController?.abort()
  window.removeEventListener('focus', resume)
  window.removeEventListener('online', resume)
  document.removeEventListener('visibilitychange', resume)
})
</script>
