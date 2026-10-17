<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { MonitorMatrixRow } from '@/api/channelMonitorV2'
import { healthStateClass, monitorAvailabilityState, monitorCardMetrics, monitorPerformanceScore } from '@/features/channel-monitor-v2/monitorFormat'
import { useChannelMonitorFormat } from '@/composables/useChannelMonitorFormat'
import ProviderIcon from './ProviderIcon.vue'

const props = defineProps<{
  row: MonitorMatrixRow
  starts: string[]
  multiplier?: number
  countdown: number
}>()
const { t, locale } = useI18n()
const { providerLabel, providerBadgeClass } = useChannelMonitorFormat()
const metrics = computed(() => monitorCardMetrics(props.row.metrics, props.row.health))
const active = ref<number | null>(null)
const slots = computed(() => {
  const byTime = new Map(props.row.buckets.map(bucket => [Date.parse(bucket.bucket_start), bucket]))
  return props.starts.map(start => {
    const bucket = byTime.get(Date.parse(start))
    const availability = bucket ? monitorAvailabilityState(bucket.metrics, bucket.health) : 'unknown'
    const score = bucket ? monitorPerformanceScore(bucket.metrics, bucket.health) : null
    return {
      start, bucket, availability,
      performance: availability === 'unknown' ? 'empty' : score == null ? 'unknown' : 'known',
      height: availability === 'unknown' ? 15 : score == null ? 50 : 15 + score * 0.85,
    }
  })
})
function time(value: string) {
  return new Intl.DateTimeFormat(locale.value, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(new Date(value))
}
function detail(index: number) {
  const slot = slots.value[index]
  if (!slot) return ''
  if (!slot.bucket) return t('channelMonitorV2.cards.noDataAt', { time: time(slot.start) })
  const m = monitorCardMetrics(slot.bucket.metrics, slot.bucket.health)
  return `${time(slot.start)} · ${t('channelMonitorV2.cards.availability')} ${m.availability} · ${t('channelMonitorV2.metrics.cacheRate')} ${m.cache} · ${t('channelMonitorV2.metrics.ttftP50')} ${m.ttft}`
}

</script>

<template>
  <article class="channel-status-card card min-w-0 p-[18px]" :data-group-id="row.group_id">
    <header class="card-heading flex items-start gap-3">
      <div class="provider-tile rounded-md p-2.5" :class="providerBadgeClass(row.platform)">
        <ProviderIcon :provider="row.platform" :size="24" />
      </div>
      <div class="min-w-0 flex-1">
        <h3 class="break-words text-base font-semibold text-gray-900 dark:text-white">{{ row.group_name || `#${row.group_id}` }}</h3>
        <div class="mt-1.5 flex flex-wrap gap-1.5 text-[10px]">
          <span class="rounded px-1.5 py-0.5" :class="providerBadgeClass(row.platform)">{{ providerLabel(row.platform) }}</span>
          <span v-if="multiplier != null" class="rounded bg-gray-100 px-1.5 py-0.5 font-mono text-gray-600 dark:bg-slate-700 dark:text-gray-300">
            {{ t('channelMonitorV2.cards.multiplier', { value: multiplier }) }}
          </span>
        </div>
      </div>
    </header>
    <dl class="card-metrics grid grid-cols-3 gap-2">
      <div>
        <dt class="text-[10px] font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.metrics.cacheRate') }}</dt>
        <dd class="mt-2 font-mono text-lg font-semibold tabular-nums">{{ metrics.cache }}</dd>
      </div>
      <div>
        <dt class="text-[10px] font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.cards.availability') }}</dt>
        <dd class="mt-2 font-mono text-lg font-semibold tabular-nums" :class="{
          'text-emerald-600 dark:text-emerald-400': row.health.error_rate === 'healthy',
          'text-amber-600 dark:text-amber-400': row.health.error_rate === 'warning',
          'text-red-600 dark:text-red-400': row.health.error_rate === 'critical'
        }">{{ metrics.availability }}</dd>
      </div>
      <div>
        <dt class="text-[10px] font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.metrics.ttftP50') }}</dt>
        <dd class="mt-2 font-mono text-lg font-semibold tabular-nums">{{ metrics.ttft }}</dd>
      </div>
    </dl>
    <div class="relative border-t border-gray-100 pt-3 dark:border-dark-700">
      <div class="mb-2 flex justify-between gap-2 text-[10px] text-gray-500 dark:text-gray-400">
        <span>{{ t('channelMonitorV2.cards.historyCount', { count: starts.length }) }}</span>
        <span>{{ t('channelMonitorV2.cards.refreshIn', { seconds: countdown }) }}</span>
      </div>
      <div class="mb-1 flex flex-wrap items-center justify-between gap-x-2 gap-y-1 text-[9px] text-gray-500 dark:text-gray-400">
        <span class="inline-flex items-center gap-1">
          <i class="h-1.5 w-1.5 rounded-full health-healthy" />
          <i class="h-1.5 w-1.5 rounded-full health-warning" />
          <i class="h-1.5 w-1.5 rounded-full health-critical" />
          {{ t('channelMonitorV2.cards.availabilityColorLegend') }}
        </span>
        <span>{{ t('channelMonitorV2.cards.performanceHeightLegend') }}</span>
      </div>
      <div class="flex h-5 items-end gap-[3px]" @mouseleave="active = null">
        <button
          v-for="(slot, index) in slots" :key="slot.start" type="button"
          class="status-slot group flex items-end h-full min-w-0 flex-1 rounded-[1px] focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500"
          :aria-label="detail(index)"
          @mouseenter="active = index" @focus="active = index" @blur="active = null" @click="active = index"
          @keydown.esc="active = null"
        >
          <span
            :data-state="slot.availability"
            :data-performance="slot.performance"
            class="block w-full rounded-[1px] transition-transform group-hover:-translate-y-0.5 group-focus-visible:-translate-y-0.5"
            :class="healthStateClass(slot.availability)"
            :style="{ height: `${slot.height}%` }"
          />
        </button>
      </div>
      <div v-if="active != null" role="tooltip" class="pointer-events-none absolute bottom-full left-0 z-30 mb-2 w-full rounded-lg border border-gray-200 bg-white p-3 text-xs leading-relaxed text-gray-700 shadow-lg dark:border-dark-600 dark:bg-dark-800 dark:text-gray-200">
        {{ detail(active) }}
      </div>
      <div class="mt-2 flex justify-between text-[9px] text-gray-400">
        <span :title="starts[0] ? time(starts[0]) : undefined">PAST</span>
        <span :title="starts.length ? time(starts[starts.length - 1]) : undefined">NOW</span>
      </div>
    </div>
  </article>
</template>

<style scoped>
.channel-status-card {
  min-height: 300px;
  border-radius: 8px;
  background: linear-gradient(165deg, #f2f5f6, #fff 60%);
  box-shadow: inset 0 1px 0 #ffffff80, 0 2px 4px #20242603;
}
.dark .channel-status-card {
  border-color: #353e43;
  background: linear-gradient(165deg, #272d30, #1e2224 60%);
  box-shadow: inset 0 1px 0 #ffffff0c, 0 2px 4px #00000014;
}
.card-heading { padding-bottom: 12px; border-bottom: 1px solid #e9edef; }
.dark .card-heading { border-color: #30383c; }
.provider-tile { flex-shrink: 0; box-shadow: inset 0 0 0 1px #ffffff12; }
.card-metrics { margin: 20px 0 24px; }
.card-metrics > div { min-width: 0; padding: 8px 6px; }
.card-metrics dd { overflow-wrap: anywhere; }
.health-healthy { color: #2acb95; background: #2acb95; }
.health-warning { color: #fbbf24; background: #fbbf24; }
.health-critical { color: #f87171; background: #f87171; }
.health-unknown { background: #cbd5e1; }
.dark .health-unknown { background: #58636c; }
.status-slot > span[data-performance='unknown'] { background: transparent; outline: 1px dashed currentColor; outline-offset: -1px; }
</style>
