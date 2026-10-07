<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import ChannelStatusCard from '@/components/user/monitor/ChannelStatusCard.vue'
import { useAuthStore } from '@/stores/auth'
import { useChannelMonitorFormat } from '@/composables/useChannelMonitorFormat'
import { extractApiErrorMessage } from '@/utils/apiError'
import * as api from '@/api/channelMonitorV2'
import type { MonitorFilter, MonitorMatrixResponse, MonitorMatrixRow, MonitorRange, MonitorSnapshot } from '@/api/channelMonitorV2'
import groupsAPI from '@/api/groups'
import { monitorBucketStarts } from '@/features/channel-monitor-v2/monitorTimeline'
import { monitorCardMetrics } from '@/features/channel-monitor-v2/monitorFormat'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const { t, locale } = useI18n()
const { providerLabel } = useChannelMonitorFormat()
const ranges: MonitorRange[] = ['90m', '24h', '7d', '30d']
const csv = (value: unknown) => typeof value === 'string' ? value.split(',').filter(Boolean) : []
const filter = computed<MonitorFilter>(() => ({
  range: ranges.includes(route.query.range as MonitorRange) ? route.query.range as MonitorRange : '90m',
  platforms: csv(route.query.platform),
  groupIds: csv(route.query.group).map(Number).filter(id => Number.isInteger(id) && id > 0),
  models: csv(route.query.model),
}))
const hasFilters = computed(() => filter.value.platforms.length + filter.value.groupIds.length + filter.value.models.length > 0)
const snapshot = ref<MonitorSnapshot | null>(null)
const matrix = ref<MonitorMatrixResponse | null>(null)
const loading = ref(true)
const error = ref('')
const rates = ref<Record<number, number>>({})
const now = ref(Date.now())
const nextRefresh = ref(Date.now())
const countdown = computed(() => Math.max(0, Math.ceil((nextRefresh.value - now.value) / 1000)))
const summary = computed(() => snapshot.value ? monitorCardMetrics(snapshot.value.metrics, snapshot.value.health) : null)
const starts = computed(() => matrix.value ? monitorBucketStarts(matrix.value.coverage) : [])
const sections = computed(() => {
  const result = new Map<string, MonitorMatrixRow[]>()
  for (const row of [...(matrix.value?.items || [])].sort((a, b) => (a.group_id || 0) - (b.group_id || 0))) {
    if (!row.group_id || row.group_id < 0) continue
    if (!result.has(row.platform)) result.set(row.platform, [])
    result.get(row.platform)!.push(row)
  }
  return [...result].map(([platform, rows]) => ({ platform, rows }))
    .sort((a, b) => Number(b.rows.length > 1) - Number(a.rows.length > 1))
})
const stale = computed(() => snapshot.value && Math.max(
  snapshot.value.coverage.aggregation_lag_seconds,
  (now.value - Date.parse(snapshot.value.coverage.computed_at)) / 1000,
) > Math.max(snapshot.value.coverage.bucket_seconds * 2, 600))

let controller: AbortController | null = null
let refreshTimer: ReturnType<typeof setTimeout> | undefined
let clockTimer: ReturnType<typeof setInterval> | undefined
let disposed = false

function scheduleRefresh() {
  if (refreshTimer) clearTimeout(refreshTimer)
  const seconds = snapshot.value?.coverage.bootstrap?.active ? 10 : Math.max(60, snapshot.value?.config.refresh_interval_seconds || 60)
  now.value = Date.now()
  nextRefresh.value = now.value + seconds * 1000
  refreshTimer = setTimeout(() => {
    if (document.hidden) scheduleRefresh()
    else void reload()
  }, seconds * 1000)
}
async function reload() {
  controller?.abort()
  if (refreshTimer) clearTimeout(refreshTimer)
  const request = new AbortController()
  controller = request
  loading.value = true
  error.value = ''
  try {
    const [nextSnapshot, nextMatrix] = await Promise.all([
      api.getSnapshot(filter.value, auth.isAdmin, request.signal),
      api.getMatrix(filter.value, 'platform_group', auth.isAdmin, request.signal),
    ])
    if (request.signal.aborted || disposed) return
    snapshot.value = nextSnapshot
    matrix.value = nextMatrix
  } catch (e) {
    if (!request.signal.aborted && !disposed) error.value = extractApiErrorMessage(e, t('channelMonitorV2.loadFailed'))
  } finally {
    if (controller === request && !disposed) {
      loading.value = false
      scheduleRefresh()
    }
  }
}
async function loadRates() {
  try {
    const [groups, custom] = await Promise.all([groupsAPI.getAvailable(), groupsAPI.getUserGroupRates()])
    if (disposed) return
    rates.value = Object.fromEntries(groups.map(group => [group.id, custom[group.id] ?? group.rate_multiplier])
      .filter(([, value]) => typeof value === 'number' && Number.isFinite(value) && value >= 0))
  } catch { /* Pricing is optional; never guess a multiplier when it is unavailable. */ }
}
function setRange(range: MonitorRange) {
  void router.replace({ query: { ...route.query, range } })
}
function clearFilters() {
  void router.replace({ query: { ...route.query, platform: undefined, group: undefined, model: undefined } })
}
function time(value: string) {
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? new Intl.DateTimeFormat(locale.value, { dateStyle: 'short', timeStyle: 'short' }).format(date) : '—'
}
watch(() => JSON.stringify(filter.value), () => {
  snapshot.value = null
  matrix.value = null
  void reload()
})
onMounted(() => {
  void reload()
  void loadRates()
  clockTimer = setInterval(() => { now.value = Date.now() }, 1000)
})
onBeforeUnmount(() => {
  disposed = true
  controller?.abort()
  if (refreshTimer) clearTimeout(refreshTimer)
  if (clockTimer) clearInterval(clockTimer)
})
</script>

<template>
  <AppLayout>
    <div class="space-y-6 pb-8">
      <section class="card">
        <header class="flex flex-wrap items-center justify-between gap-4 border-b border-gray-100 px-5 py-4 dark:border-dark-700">
          <div>
            <h1 class="flex items-center gap-2 text-xl font-bold">
              <span class="rounded-lg bg-primary-50 p-2 text-primary-600 dark:bg-primary-900/30 dark:text-primary-300"><Icon name="chart" size="sm" /></span>
              {{ t('channelMonitorV2.cards.title') }}
            </h1>
            <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">
              {{ snapshot ? t('channelMonitorV2.updatedTo', { time: time(snapshot.coverage.data_through) }) : t('channelMonitorV2.cards.description') }}
            </p>
          </div>
          <div class="flex items-center gap-2">
            <router-link class="btn btn-secondary btn-sm" :to="{ query: { ...route.query, monitor_view: 'details' } }">{{ t('channelMonitorV2.cards.details') }}</router-link>
            <button class="btn btn-secondary btn-sm" type="button" :disabled="loading" @click="reload">
              <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />{{ t('common.refresh') }}
            </button>
          </div>
        </header>
        <div class="flex flex-wrap items-center gap-2 px-4 py-3">
          <button v-for="range in ranges" :key="range" class="tab !px-3 !py-1.5" :class="{ 'tab-active !bg-primary-50 !text-primary-700 dark:!bg-primary-900/30 dark:!text-primary-300': filter.range === range }" :aria-pressed="filter.range === range" type="button" @click="setRange(range)">{{ range }}</button>
          <span class="ml-auto text-xs text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.cards.rangeSummary', { range: filter.range }) }}<template v-if="summary"> · {{ t('channelMonitorV2.cards.availability') }} {{ summary.availability }} · {{ t('channelMonitorV2.metrics.cacheRate') }} {{ summary.cache }}</template></span>
        </div>
      </section>
      <div v-if="hasFilters" class="flex flex-wrap items-center gap-2 text-sm text-gray-500 dark:text-gray-400">
        {{ t('channelMonitorV2.cards.filtered') }}
        <button class="btn btn-secondary btn-sm" type="button" @click="clearFilters">{{ t('channelMonitorV2.clearFilters') }}</button>
      </div>
      <div v-if="error" role="alert" class="rounded-lg bg-red-50 p-4 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ error }}</div>
      <div v-if="snapshot && (!snapshot.coverage.coverage_complete || stale)" role="status" class="flex flex-wrap gap-2 text-xs">
        <span v-if="!snapshot.coverage.coverage_complete" class="badge badge-warning">{{ t('channelMonitorV2.partialCoverage') }}</span>
        <span v-if="snapshot.coverage.bootstrap?.active" class="badge badge-primary">{{ t('channelMonitorV2.bootstrap.progress', { percent: Math.round(snapshot.coverage.bootstrap.progress_percent) }) }}</span>
        <span v-if="stale" class="badge badge-warning">{{ t('channelMonitorV2.cards.stale') }}</span>
      </div>
      <div v-if="loading && !matrix" role="status" class="py-16 text-center text-sm text-gray-500">{{ t('common.loading') }}</div>
      <div v-else-if="matrix && !sections.length" class="card p-12 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('common.noData') }}</div>
      <div v-else class="channel-status-board grid grid-cols-1 gap-x-5 gap-y-7 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
        <section v-for="section in sections" :key="section.platform" class="min-w-0" :class="{ 'col-span-full': section.rows.length > 1 }">
          <h2 class="mb-3 flex items-center gap-2 px-1 text-xs font-semibold uppercase tracking-wide text-gray-600 dark:text-gray-300">
            {{ providerLabel(section.platform) }} <span class="rounded bg-gray-100 px-1.5 py-0.5 font-mono text-[10px] text-gray-500 dark:bg-dark-800">{{ section.rows.length }}</span>
          </h2>
          <div class="grid gap-5" :class="{ 'md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4': section.rows.length > 1 }">
            <ChannelStatusCard v-for="row in section.rows" :key="`${row.platform}:${row.group_id}`" :row="row" :starts="starts" :multiplier="rates[row.group_id!]" :countdown="countdown" />
          </div>
        </section>
      </div>
    </div>
  </AppLayout>
</template>
