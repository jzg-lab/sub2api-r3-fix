<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  opsAPI,
  type OpsOpenAIDowngradeAccountStat,
  type OpsOpenAIDowngradeDashboard,
  type OpsOpenAIDowngradeEvent
} from '@/api/admin/ops'
import { formatDateTime } from '../utils/opsFormatters'

interface Props {
  refreshToken: number
}

const props = defineProps<Props>()
const { t } = useI18n()
const loading = ref(false)
const errorMessage = ref('')
const snapshot = ref<OpsOpenAIDowngradeDashboard | null>(null)
let refreshTimer: ReturnType<typeof setInterval> | undefined

const buckets = computed(() => snapshot.value?.buckets ?? [])
const accounts = computed(() => snapshot.value?.account_stats ?? [])
const events = computed(() => snapshot.value?.recent_events ?? [])

function stateLabel(state: string): string {
  const labels: Record<string, string> = {
    on_duty: t('admin.ops.openaiDowngrade.states.onDuty'),
    circuit_open: t('admin.ops.openaiDowngrade.states.circuitOpen'),
    reprobe: t('admin.ops.openaiDowngrade.states.reprobe'),
    pending_replace: t('admin.ops.openaiDowngrade.states.pendingReplace')
  }
  return labels[state] || state
}

function stateClass(state: string): string {
  if (state === 'on_duty') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
  if (state === 'circuit_open') return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  if (state === 'reprobe') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
}

function eventLabel(event: OpsOpenAIDowngradeEvent): string {
  const labels: Record<string, string> = {
    circuit_open: t('admin.ops.openaiDowngrade.events.circuitOpen'),
    bucket_reprobe: t('admin.ops.openaiDowngrade.events.bucketReprobe'),
    bucket_rescue: t('admin.ops.openaiDowngrade.events.bucketRescue'),
    replace_required: t('admin.ops.openaiDowngrade.events.replaceRequired'),
    recovered: t('admin.ops.openaiDowngrade.events.recovered')
  }
  return labels[event.event_type] || event.event_type
}

function percent(value: number): string {
  return `${Math.round(value * 100)}%`
}

function avgTokens(account: OpsOpenAIDowngradeAccountStat): string {
  return account.avg_reasoning_tokens == null ? '-' : Math.round(account.avg_reasoning_tokens).toLocaleString()
}

async function load(): Promise<void> {
  loading.value = true
  errorMessage.value = ''
  try {
    snapshot.value = await opsAPI.getOpenAIDowngradeDashboard()
  } catch (error: any) {
    errorMessage.value = error?.message || t('admin.ops.openaiDowngrade.loadFailed')
  } finally {
    loading.value = false
  }
}

function restartTimer(): void {
  if (refreshTimer) clearInterval(refreshTimer)
  refreshTimer = setInterval(() => void load(), 60_000)
}

onMounted(() => {
  void load()
  restartTimer()
})

onBeforeUnmount(() => {
  if (refreshTimer) clearInterval(refreshTimer)
})

watch(() => props.refreshToken, () => void load())
</script>

<template>
  <section class="card p-4 md:p-5">
    <div class="mb-4 flex flex-wrap items-center justify-between gap-3">
      <div>
        <h3 class="text-sm font-bold text-gray-900 dark:text-white">
          {{ t('admin.ops.openaiDowngrade.title') }}
        </h3>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.ops.openaiDowngrade.summary', {
            probes: snapshot?.probe_count_24h ?? 0,
            success: snapshot?.success_count_24h ?? 0
          }) }}
        </p>
      </div>
      <div class="flex flex-wrap gap-2 text-xs">
        <span class="rounded-md bg-red-50 px-2 py-1 text-red-700 dark:bg-red-900/20 dark:text-red-300">
          {{ t('admin.ops.openaiDowngrade.degraded', { count: snapshot?.degraded_account_count ?? 0 }) }}
        </span>
        <span class="rounded-md bg-amber-50 px-2 py-1 text-amber-700 dark:bg-amber-900/20 dark:text-amber-300">
          {{ t('admin.ops.openaiDowngrade.fullBuckets', { count: snapshot?.at_capacity_bucket_count ?? 0 }) }}
        </span>
      </div>
    </div>

    <div v-if="errorMessage" class="mb-4 rounded-md bg-red-50 px-3 py-2 text-xs text-red-600 dark:bg-red-900/20 dark:text-red-400">
      {{ errorMessage }}
    </div>
    <div v-if="loading && !snapshot" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('admin.ops.loadingText') }}
    </div>
    <div v-else class="space-y-5">
      <div>
        <div class="mb-2 text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
          {{ t('admin.ops.openaiDowngrade.buckets') }}
        </div>
        <div v-if="buckets.length" class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
          <div v-for="bucket in buckets" :key="bucket.proxy_id" class="rounded-md border border-gray-200 p-3 dark:border-dark-700">
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="truncate text-sm font-semibold text-gray-900 dark:text-white">{{ bucket.name || `#${bucket.proxy_id}` }}</div>
                <div class="mt-1 font-mono text-xs text-gray-500 dark:text-gray-400">{{ bucket.exit_ip || '-' }}</div>
              </div>
              <span :class="bucket.at_capacity ? 'text-red-600 dark:text-red-300' : 'text-emerald-600 dark:text-emerald-300'" class="text-xs">
                {{ bucket.account_ids.length }}/{{ bucket.capacity }}
              </span>
            </div>
            <div class="mt-3 grid grid-cols-4 gap-2 text-center text-xs">
              <div><div class="font-semibold text-emerald-600">{{ bucket.healthy_count }}</div><div class="text-gray-500">{{ t('admin.ops.openaiDowngrade.healthy') }}</div></div>
              <div><div class="font-semibold text-red-600">{{ bucket.circuit_count }}</div><div class="text-gray-500">{{ t('admin.ops.openaiDowngrade.circuit') }}</div></div>
              <div><div class="font-semibold text-amber-600">{{ bucket.reprobe_count }}</div><div class="text-gray-500">{{ t('admin.ops.openaiDowngrade.reprobe') }}</div></div>
              <div><div class="font-semibold text-gray-500">{{ bucket.replace_count }}</div><div class="text-gray-500">{{ t('admin.ops.openaiDowngrade.replace') }}</div></div>
            </div>
          </div>
        </div>
        <div v-else class="py-4 text-sm text-gray-500 dark:text-gray-400">{{ t('common.noData') }}</div>
      </div>

      <div>
        <div class="mb-2 text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
          {{ t('admin.ops.openaiDowngrade.accounts') }}
        </div>
        <div class="overflow-x-auto rounded-md border border-gray-200 dark:border-dark-700">
          <table class="min-w-full text-left text-xs">
            <thead class="bg-gray-50 dark:bg-dark-800">
              <tr class="text-gray-500 dark:text-gray-400">
                <th class="px-3 py-2 font-semibold">{{ t('admin.ops.openaiDowngrade.account') }}</th>
                <th class="px-3 py-2 font-semibold">{{ t('admin.ops.openaiDowngrade.state') }}</th>
                <th class="px-3 py-2 font-semibold">{{ t('admin.ops.openaiDowngrade.successRate') }}</th>
                <th class="px-3 py-2 font-semibold">{{ t('admin.ops.openaiDowngrade.reasoning') }}</th>
                <th class="px-3 py-2 font-semibold">{{ t('admin.ops.openaiDowngrade.probes') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="account in accounts" :key="account.account_id" class="border-t border-gray-100 dark:border-dark-800">
                <td class="px-3 py-2 font-mono text-gray-700 dark:text-gray-200">{{ account.account_id }}</td>
                <td class="px-3 py-2"><span class="rounded-md px-2 py-1" :class="stateClass(account.state)">{{ stateLabel(account.state) }}</span></td>
                <td class="px-3 py-2 text-gray-700 dark:text-gray-200">{{ percent(account.success_rate_24h) }}</td>
                <td class="px-3 py-2 text-gray-700 dark:text-gray-200">{{ avgTokens(account) }}</td>
                <td class="px-3 py-2 text-gray-700 dark:text-gray-200">{{ account.probe_count_24h }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div>
        <div class="mb-2 text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
          {{ t('admin.ops.openaiDowngrade.timeline') }}
        </div>
        <div v-if="events.length" class="max-h-48 space-y-2 overflow-y-auto">
          <div v-for="event in events" :key="event.id" class="flex flex-wrap items-center gap-2 text-xs">
            <span class="font-mono text-gray-500 dark:text-gray-400">{{ formatDateTime(event.created_at) }}</span>
            <span class="font-medium text-gray-800 dark:text-gray-200">{{ eventLabel(event) }}</span>
            <span v-if="event.account_id" class="text-gray-500 dark:text-gray-400">#{{ event.account_id }}</span>
          </div>
        </div>
        <div v-else class="py-4 text-sm text-gray-500 dark:text-gray-400">{{ t('common.noData') }}</div>
      </div>
    </div>
  </section>
</template>
