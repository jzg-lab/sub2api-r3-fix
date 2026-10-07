<script setup lang="ts">
import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAdminSettingsStore } from '@/stores/adminSettings'
import OpsOpenAIDowngradeCard from './ops/components/OpsOpenAIDowngradeCard.vue'

const { t } = useI18n()
const settings = useAdminSettingsStore()
onMounted(() => void settings.fetch())
</script>

<template>
  <AppLayout>
    <div class="space-y-5">
      <header>
        <h1 class="page-title">{{ t('admin.ops.openaiDowngrade.title') }}</h1>
        <p class="page-description">{{ t('admin.ops.openaiDowngrade.description') }}</p>
      </header>
      <OpsOpenAIDowngradeCard v-if="settings.opsMonitoringEnabled" />
      <div v-else class="card p-6 text-sm text-gray-500 dark:text-gray-400" role="status">
        {{ t('admin.ops.openaiDowngrade.disabled') }}
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.openaiDowngrade.limits') }}</p>
    </div>
  </AppLayout>
</template>
