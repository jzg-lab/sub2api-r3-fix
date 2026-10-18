<template>
  <BaseDialog :show="show" :title="t('admin.accounts.environmentAudit.title')" width="extra-wide" @close="close">
    <div class="min-w-0 space-y-4 text-sm">
      <div class="flex flex-wrap items-center gap-3 border-b border-gray-200 pb-3 dark:border-dark-600">
        <span>OpenAI OAuth</span>
        <span v-if="report" class="text-gray-500">{{ t('admin.accounts.environmentAudit.count', { count: report.accounts.length }) }}</span>
        <button type="button" class="btn btn-secondary ml-auto h-9 w-9 shrink-0 p-0"
          :title="t('common.refresh')" :aria-label="t('common.refresh')" :disabled="loading" @click="load">
          <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />
        </button>
      </div>
      <p v-if="failed" role="alert" class="text-red-600 dark:text-red-400">{{ t('admin.accounts.environmentAudit.loadFailed') }}</p>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <template v-else-if="report">
        <section v-if="report.collisions.length" class="min-w-0" data-testid="collisions">
          <h3 class="mb-2 font-medium text-amber-700 dark:text-amber-400">{{ t('admin.accounts.environmentAudit.collisions') }}</h3>
          <ul class="space-y-1">
            <li v-for="group in report.collisions" :key="group.kind + ':' + group.account_ids.join(',')" class="break-words">
              {{ collisionLabel(group.kind) }}:
              <span class="font-mono">{{ group.account_ids.map(id => `#${id}`).join(', ') }}</span>
            </li>
          </ul>
        </section>
        <p v-else-if="report.accounts.length" data-testid="no-collisions" class="text-gray-500">{{ t('admin.accounts.environmentAudit.noCollisions') }}</p>
        <section v-if="report.import_batches.length" class="min-w-0" data-testid="import-batches">
          <h3 class="mb-2 font-medium">{{ t('admin.accounts.environmentAudit.importBatches') }}</h3>
          <ul class="space-y-1 font-mono">
            <li v-for="group in report.import_batches" :key="group.account_ids.join(',')" class="break-words">
              {{ group.account_ids.map(id => `#${id}`).join(', ') }}
            </li>
          </ul>
        </section>
        <div v-if="report.accounts.length" class="max-h-[50vh] overflow-auto" tabindex="0"
          role="region" :aria-label="t('admin.accounts.environmentAudit.title')">
          <table class="w-full min-w-[960px] table-fixed text-left">
            <thead class="border-b border-gray-200 dark:border-dark-600">
              <tr>
                <th v-for="column in columns" :key="column" scope="col" class="px-2 py-2 align-top font-medium">
                  {{ t(`admin.accounts.environmentAudit.${column}`) }}
                </th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="account in report.accounts" :key="account.account_id" :data-account-id="account.account_id">
                <th scope="row" class="px-2 py-3 align-top font-medium">#{{ account.account_id }}</th>
                <td class="break-words px-2 py-3 align-top">
                  <div>{{ account.configured_mode || unknown }}</div>
                  <div class="text-gray-500">{{ account.effective_mode || unknown }}</div>
                </td>
                <td class="px-2 py-3 align-top">{{ t(`admin.accounts.environmentAudit.${account.seed_present ? 'present' : 'missing'}`) }}</td>
                <td class="break-words px-2 py-3 align-top">{{ sourceLabel(account.identity_source) }}</td>
                <td class="break-words px-2 py-3 align-top" data-field="identity-generated">{{ timestamp(account.identity_generated_at) }}</td>
                <td class="break-words px-2 py-3 align-top">{{ timestamp(account.account_created_at) }}</td>
                <td class="px-2 py-3 align-top" data-field="login-ip">
                  {{ t(`admin.accounts.environmentAudit.${account.login_ip_recorded ? 'recorded' : 'unknown'}`) }}
                </td>
                <td class="break-words px-2 py-3 align-top">{{ account.proxy_id === null ? unknown : `#${account.proxy_id}` }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else data-testid="empty" class="py-4 text-gray-500">{{ t('admin.accounts.environmentAudit.empty') }}</p>
        <p class="break-words text-xs text-gray-500">{{ t('admin.accounts.environmentAudit.observedAt') }} {{ timestamp(report.generated_at) }}</p>
      </template>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { accountsAPI, type AccountEnvironmentAudit } from '@/api/admin/accounts'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ close: [] }>()
const { t } = useI18n()
const report = ref<AccountEnvironmentAudit | null>(null)
const loading = ref(false)
const failed = ref(false)
const unknown = computed(() => t('admin.accounts.environmentAudit.unknown'))
const columns = ['account', 'modes', 'seed', 'source', 'identityGenerated', 'accountCreated', 'loginIP', 'proxy'] as const
let generation = 0
let controller: AbortController | null = null

function timestamp(value: string | null): string {
  return value && Number.isFinite(Date.parse(value)) ? formatDateTime(value) : unknown.value
}

function sourceLabel(source: string): string {
  const keys: Record<string, string> = { seed_derived: 'seedDerived', configured_device: 'configuredDevice' }
  return t(`admin.accounts.environmentAudit.${keys[source] || 'unknown'}`)
}

function collisionLabel(kind: string): string {
  const keys: Record<string, string> = {
    duplicate_seed: 'duplicateSeed',
    duplicate_effective_installation: 'duplicateInstallation'
  }
  return t(`admin.accounts.environmentAudit.${keys[kind] || 'unknown'}`)
}

function reset() {
  generation++
  controller?.abort()
  controller = null
  loading.value = false
  failed.value = false
  report.value = null
}

async function load() {
  if (!props.show || loading.value) return
  const current = ++generation
  const request = new AbortController()
  controller = request
  loading.value = true
  failed.value = false
  report.value = null
  try {
    const result = await accountsAPI.getEnvironmentAudit(request.signal)
    if (current !== generation || request.signal.aborted || !props.show) return
    if (!result || !Array.isArray(result.accounts) || !Array.isArray(result.collisions) ||
      !Array.isArray(result.import_batches) || typeof result.generated_at !== 'string') {
      throw new Error('invalid_environment_audit')
    }
    report.value = result
  } catch {
    if (current === generation && !request.signal.aborted && props.show) failed.value = true
  } finally {
    // A closed or superseded request must not clear the next request's loading state.
    if (current === generation) {
      loading.value = false
      controller = null
    }
  }
}

function close() {
  reset()
  emit('close')
}

watch(() => props.show, show => {
  reset()
  if (show) void load()
}, { immediate: true })
onBeforeUnmount(reset)
</script>
