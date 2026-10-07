<template>
  <section class="signal-charts" data-signal-charts :aria-busy="loading">
    <div class="signal-chart-toolbar">
      <div class="signal-date-control">
        <DateRangePicker :start-date="startDate" :end-date="endDate" @update:startDate="$emit('update:startDate', $event)" @update:endDate="$emit('update:endDate', $event)" @change="$emit('dateRangeChange', $event)" />
      </div>
      <div class="signal-chart-actions">
        <div class="signal-chart-mode" role="group" :aria-label="t('dashboard.tokenUsageTrend')">
          <button :aria-pressed="mode === 'total'" @click="mode = 'total'">{{ t('dashboard.totalUsage') }}</button>
          <button :aria-pressed="mode === 'breakdown'" @click="mode = 'breakdown'">{{ t('dashboard.tokenDetails') }}</button>
        </div>
        <Select class="signal-granularity" :aria-label="t('dashboard.granularity')" :model-value="granularity" :options="[{value:'day', label:t('dashboard.day')}, {value:'hour', label:t('dashboard.hour')}]" @update:model-value="$emit('update:granularity', $event)" @change="$emit('granularityChange')" />
      </div>
    </div>
    <div class="signal-trend">
      <TokenUsageTrend :trend-data="trend" :loading="loading" :mode="mode" />
    </div>
    <details class="signal-model-details">
      <summary>{{ t('dashboard.modelDistribution') }} · {{ t('dashboard.details') }}</summary>
      <!-- Model Distribution Chart -->
      <div class="relative min-w-0 overflow-hidden py-4">
        <div v-if="loading" class="absolute inset-0 z-10 flex items-center justify-center bg-white/50 backdrop-blur-sm dark:bg-dark-800/50">
          <LoadingSpinner size="md" />
        </div>
        <h3 class="mb-4 text-sm font-semibold text-gray-900 dark:text-white">{{ t('dashboard.modelDistribution') }}</h3>
        <div class="flex flex-col items-center gap-4 sm:flex-row sm:gap-6">
          <div class="h-48 w-48 shrink-0">
            <Doughnut v-if="modelData" :data="modelData" :options="doughnutOptions" />
            <div v-else class="flex h-full items-center justify-center text-sm text-gray-500 dark:text-gray-400">{{ t('dashboard.noDataAvailable') }}</div>
          </div>
          <div class="max-h-48 w-full min-w-0 flex-1 overflow-auto">
            <table class="w-full text-xs">
              <thead>
                <tr class="text-gray-500 dark:text-gray-400">
                  <th class="pb-2 text-left">{{ t('dashboard.model') }}</th>
                  <th class="pb-2 text-right">{{ t('dashboard.requests') }}</th>
                  <th class="pb-2 text-right">{{ t('dashboard.tokens') }}</th>
                  <th class="pb-2 text-right">{{ t('dashboard.actual') }}</th>
                  <th class="pb-2 text-right">{{ t('dashboard.standard') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="model in models" :key="model.model" class="border-t border-gray-100 dark:border-dark-700">
                  <td class="max-w-[100px] truncate py-1.5 font-medium text-gray-900 dark:text-white" :title="model.model">{{ model.model }}</td>
                  <td class="py-1.5 text-right text-gray-600 dark:text-gray-400">{{ formatNumber(model.requests) }}</td>
                  <td class="py-1.5 text-right text-gray-600 dark:text-gray-400">{{ formatTokens(model.total_tokens) }}</td>
                  <td class="py-1.5 text-right text-green-600 dark:text-green-400">${{ formatCost(model.actual_cost) }}</td>
                  <td class="py-1.5 text-right text-gray-400 dark:text-gray-500">${{ formatCost(model.cost) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

    </details>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import DateRangePicker from '@/components/common/DateRangePicker.vue'
import Select from '@/components/common/Select.vue'
import { Doughnut } from 'vue-chartjs'
import TokenUsageTrend from '@/components/charts/TokenUsageTrend.vue'
import type { TrendDataPoint, ModelStat } from '@/types'
import { formatCostFixed as formatCost, formatNumberLocaleString as formatNumber, formatTokensK as formatTokens } from '@/utils/format'
import { Chart as ChartJS, CategoryScale, LinearScale, PointElement, LineElement, ArcElement, Title, Tooltip, Legend, Filler } from 'chart.js'
ChartJS.register(CategoryScale, LinearScale, PointElement, LineElement, ArcElement, Title, Tooltip, Legend, Filler)

const props = defineProps<{ loading: boolean, startDate: string, endDate: string, granularity: string, trend: TrendDataPoint[], models: ModelStat[] }>()
defineEmits(['update:startDate', 'update:endDate', 'update:granularity', 'dateRangeChange', 'granularityChange', 'refresh'])
const { t } = useI18n()
const mode = ref<'total' | 'breakdown'>('total')

const modelData = computed(() => !props.models?.length ? null : {
  labels: props.models.map((m: ModelStat) => m.model),
  datasets: [{
    data: props.models.map((m: ModelStat) => m.total_tokens),
    backgroundColor: ['#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6', '#ec4899', '#06b6d4', '#84cc16']
  }]
})

const doughnutOptions = {
  responsive: true,
  maintainAspectRatio: false,
  plugins: {
    legend: { display: false },
    tooltip: {
      callbacks: {
        label: (context: any) => `${context.label}: ${formatTokens(context.parsed)} tokens`
      }
    }
  }
}
</script>
<style scoped>
.signal-charts { margin-top: 20px; min-width: 0; }
.signal-chart-toolbar { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; padding-bottom: 14px; }
.signal-chart-actions { display: flex; align-items: center; gap: 10px; min-width: 0; }
.signal-chart-mode { display: flex; padding: 3px; border: 1px solid var(--signal-line); border-radius: 6px; background: var(--signal-inset); }
.signal-chart-mode button { min-height: 30px; padding: 4px 12px; border-radius: 4px; font-size: 12px; color: var(--signal-muted); }
.signal-chart-mode [aria-pressed='true'] { background: var(--signal-raised); color: var(--signal-text); box-shadow: 0 1px 3px #00000010; }
.signal-granularity { width: 98px; }
.signal-trend { min-width: 0; max-width: 100%; overflow: hidden; }
.signal-trend :deep(canvas) { max-width: 100%; }
.signal-trend :deep(.card) { background: transparent; border: 0; border-radius: 0; padding: 16px 0 0; box-shadow: none; border-top: 1px solid var(--signal-line); }
.signal-trend :deep(.h-48) { height: 260px; }
@media (max-width: 639px) {
  .signal-chart-actions { flex-wrap: wrap; }
  .signal-chart-mode button { min-height: 38px; }
  .signal-trend :deep(.h-48) { height: 220px; }
}
</style>
