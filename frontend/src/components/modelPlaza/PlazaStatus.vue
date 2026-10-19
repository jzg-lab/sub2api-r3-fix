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
const props = defineProps<{ row?: MonitorMatrixRow; coverage?: MonitorCoverage; expanded?: boolean }>()
defineEmits<{ details: [] }>()
const { t } = useI18n()
const successRate = computed(() => {
  if (!props.row || props.row.health.overall === 'unknown') return '—'
  return `${((props.row.metrics.success_rate ?? 1 - props.row.metrics.error_rate) * 100).toFixed(1)}%`
})
const ttft = computed(() => {
  const ms = props.row?.metrics.ttft.p50_ms
  return ms == null ? '—' : `${(ms / 1000).toFixed(2)}s`
})
const timeline = computed(() => {
  const coverage = props.coverage
  const start = Date.parse(coverage?.requested_start ?? '')
  const end = Date.parse(coverage?.requested_end ?? coverage?.data_through ?? '')
  const count = 30
  const slots = Array.from({ length: count }, () => ({ health: 'unknown', label: t('modelPlaza.catalog.unknown') }))
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return slots
  const ranks: Record<string, number> = { unknown: 0, healthy: 1, warning: 2, critical: 3 }
  for (const bucket of props.row?.buckets ?? []) {
    const index = Math.floor((Date.parse(bucket.bucket_start) - start) / (end - start) * count)
    if (index < 0 || index >= count) continue
    const health = bucket.health.overall
    if (ranks[health] > ranks[slots[index].health]) slots[index] = { health, label: `${new Date(bucket.bucket_start).toLocaleString()} · ${t(`modelPlaza.catalog.${health}`)}` }
  }
  return slots
})
</script>
