<template>
  <div v-if="loading && !stats" class="h-12 w-32 animate-pulse rounded bg-gray-100 dark:bg-dark-700" :aria-label="t('common.loading')"></div>
  <span v-else-if="unavailable" class="text-xs text-gray-500">{{ t('admin.accounts.recentStats.unavailable') }}</span>
  <span v-else-if="!stats || stats.attempts === 0" class="text-xs text-gray-400">{{ t('admin.accounts.recentStats.noData') }}</span>
  <div v-else class="min-w-36 space-y-1 text-xs tabular-nums">
    <div class="flex items-center justify-between gap-3">
      <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.recentStats.success') }}</span>
      <span class="font-medium text-gray-900 dark:text-gray-100">{{ percent(stats.success_rate) }} <span class="font-normal text-gray-500">({{ stats.successes }}/{{ stats.attempts }})</span></span>
    </div>
    <div class="flex items-center justify-between gap-3">
      <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.recentStats.cache') }}</span>
      <span class="rounded bg-purple-50 px-1.5 text-purple-700 dark:bg-purple-900/25 dark:text-purple-300">{{ percent(stats.cache_hit_rate) }}</span>
    </div>
    <div class="flex items-center justify-between gap-3">
      <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.recentStats.ttft') }}</span>
      <span class="text-gray-700 dark:text-gray-200">{{ stats.ttft_avg_ms == null ? '—' : `${(stats.ttft_avg_ms / 1000).toFixed(2)} s` }}</span>
    </div>
    <div v-if="stats.attempts < 10" class="text-gray-400">{{ t('admin.accounts.recentStats.fewSamples') }}</div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { AccountRecentStats } from '@/api/admin/accounts'

defineProps<{ stats?: AccountRecentStats | null; loading?: boolean; unavailable?: boolean }>()
const { t } = useI18n()
const percent = (value: number | null) => value == null ? '—' : `${(value * 100).toFixed(2)}%`
</script>
