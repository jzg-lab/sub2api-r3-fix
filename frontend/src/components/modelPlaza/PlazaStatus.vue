<template>
  <div class="plaza-status" :class="{ expanded }">
    <button class="plaza-history" :aria-label="t('modelPlaza.catalog.statusDetails')" @click="$emit('details')">
      <span class="plaza-status-label"><span>{{ t('modelPlaza.catalog.successRate') }}</span><b>{{ successRate }}</b></span>
      <span class="plaza-timeline">
        <i v-for="(bucket, index) in timeline" :key="index" :class="bucket.health" :title="bucket.label"></i>
      </span>
    </button>
    <div class="plaza-metric"><span>{{ t('modelPlaza.catalog.ttft') }}</span><strong>{{ ttft }}</strong></div>
    <div class="plaza-metric" :title="t('modelPlaza.catalog.tpsHint')"><span>TPS</span><strong>{{ row?.output_tps != null ? row.output_tps.toFixed(1) : '—' }}</strong></div>
    <button v-if="!expanded" class="plaza-detail-button" @click="$emit('details')">{{ t('modelPlaza.catalog.details') }} <Icon name="chevronRight" size="xs" /></button>
  </div>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { MonitorCoverage, MonitorMatrixRow } from '@/api/channelMonitorV2'
import { monitorAvailabilityState } from '@/features/channel-monitor-v2/monitorFormat'
import { monitorBucketStarts } from '@/features/channel-monitor-v2/monitorTimeline'
const props = defineProps<{ row?: MonitorMatrixRow; coverage?: MonitorCoverage; expanded?: boolean }>()
defineEmits<{ details: [] }>()
const { t } = useI18n()
const successRate = computed(() => {
  if (!props.row || monitorAvailabilityState(props.row.metrics, props.row.health) === 'unknown') return '—'
  return `${((props.row.metrics.success_rate ?? 1 - props.row.metrics.error_rate) * 100).toFixed(1)}%`
})
const ttft = computed(() => {
  const ms = props.row?.metrics.ttft.p50_ms
  return ms == null ? '—' : `${(ms / 1000).toFixed(2)}s`
})
const timeline = computed(() => {
  const starts = props.coverage ? monitorBucketStarts(props.coverage) : []
  if (!starts.length) return Array.from({ length: 24 }, () => ({ health: 'unknown', label: t('modelPlaza.catalog.unknown') }))
  const byTime = new Map((props.row?.buckets ?? []).map(bucket => [Date.parse(bucket.bucket_start), bucket]))
  return starts.map(start => {
    const bucket = byTime.get(Date.parse(start))
    const health = bucket ? monitorAvailabilityState(bucket.metrics, bucket.health) : 'unknown'
    return { health, label: `${new Date(start).toLocaleString()} · ${t(`modelPlaza.catalog.${health}`)}` }
  })
})
</script>
