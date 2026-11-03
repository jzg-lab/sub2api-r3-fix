<template>
  <section class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" aria-live="polite">
    <div class="flex flex-wrap items-center justify-between gap-3 text-sm">
      <div class="flex min-w-0 flex-wrap items-center gap-2">
      <strong class="rounded-md bg-gray-100 px-2 py-1 text-xs dark:bg-dark-700">{{ t(`admin.accounts.manualQuality.status.${result.status}`) }}</strong>
      <span class="break-all text-gray-600 dark:text-gray-300">{{ t(`admin.accounts.manualQuality.${result.kind}`) }} · {{ result.model }}</span>
      </div>
      <span class="text-xs text-gray-400">{{ new Date(result.started_at).toLocaleString() }} · {{ (result.duration_ms / 1000).toFixed(1) }}s</span>
    </div>
    <p v-if="result.error" class="text-sm text-red-600 dark:text-red-400">{{ result.error }}</p>
    <p v-if="comparison !== null" class="text-sm text-gray-500">{{ t(comparison ? 'admin.accounts.manualQuality.match' : 'admin.accounts.manualQuality.noMatch') }}</p>
    <img v-if="svgURL" :src="svgURL" :alt="t('admin.accounts.manualQuality.preview')" class="max-h-[28rem] w-full rounded-lg bg-white p-3" />
    <p v-else-if="result.kind === 'pelican' && result.status === 'completed'" class="text-sm text-gray-500">{{ t('admin.accounts.manualQuality.noSVG') }}</p>
    <details v-if="svgURL" class="rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600">
      <summary class="cursor-pointer text-gray-500 dark:text-gray-400">{{ t('qualityTest.svgSource') }}</summary>
      <pre class="mt-3 max-h-96 overflow-auto whitespace-pre-wrap break-words text-xs leading-6">{{ result.answer }}</pre>
    </details>
    <pre v-else class="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-4 text-sm leading-7 dark:bg-dark-900/50">{{ result.answer }}</pre>
    <details class="border-t border-gray-100 pt-3 text-sm text-gray-500 dark:border-dark-700 dark:text-gray-400">
      <summary class="cursor-pointer">{{ t('admin.accounts.manualQuality.details') }}</summary>
      <p class="mt-2">{{ t('admin.accounts.manualQuality.upstreamModel') }}: {{ result.upstream_model || '—' }}</p>
      <pre class="whitespace-pre-wrap break-words">{{ result.prompt }}</pre>
      <p>{{ t('admin.accounts.manualQuality.reference') }}: {{ result.reference_answer || '—' }}</p>
      <pre class="whitespace-pre-wrap break-words">{{ JSON.stringify(result.parameters || {}, null, 2) }}</pre>
      <p>{{ t('admin.accounts.manualQuality.usage') }}: {{ result.usage ? JSON.stringify(result.usage) : '—' }}</p>
    </details>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { QualityResult } from '@/api/admin/manualQuality'
import { qualitySVGDataURL, qualityAnswerMatches } from '@/utils/manualQuality'

const props = defineProps<{ result: QualityResult }>()
const { t } = useI18n()
const svgURL = computed(() => props.result.kind === 'pelican' ? qualitySVGDataURL(props.result.answer) : null)
const comparison = computed(() => props.result.kind === 'candy' && props.result.status === 'completed' ? qualityAnswerMatches(props.result.answer, props.result.reference_answer) : null)
</script>
