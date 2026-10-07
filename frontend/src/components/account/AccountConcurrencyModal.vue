<template>
  <BaseDialog
    :show="account !== null"
    :title="t('admin.accounts.capacity.editConcurrency')"
    width="narrow"
    :close-on-escape="!saving"
    :show-close-button="!saving"
    @close="close"
  >
    <form id="account-concurrency-form" class="space-y-4" @submit.prevent="save">
      <p class="break-all text-sm text-gray-600 dark:text-gray-300">{{ account?.name }}</p>
      <div>
        <label for="account-concurrency-limit" class="input-label">{{ t('admin.accounts.concurrency') }}</label>
        <input
          id="account-concurrency-limit"
          v-model.number="limit"
          type="number"
          min="1"
          :max="MAX_ACCOUNT_CONCURRENCY"
          step="1"
          required
          :disabled="saving"
          :aria-invalid="!valid"
          class="input"
        />
      </div>
      <p v-if="!valid" role="alert" class="text-sm text-red-600">{{ t('admin.accounts.capacity.invalidConcurrency') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    </form>
    <template #footer>
      <button type="button" class="btn btn-secondary" :disabled="saving" @click="close">{{ t('common.cancel') }}</button>
      <button type="submit" form="account-concurrency-form" class="btn btn-primary" :disabled="saving || !valid">
        {{ saving ? t('common.saving') : t('common.save') }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { MAX_ACCOUNT_CONCURRENCY } from '@/constants/account'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { Account } from '@/types'

const props = defineProps<{ account: Account | null }>()
const emit = defineEmits<{
  (event: 'close'): void
  (event: 'updated', account: Account): void
}>()
const { t } = useI18n()
const appStore = useAppStore()
const limit = ref<number | string>(1)
const saving = ref(false)
const error = ref('')
const valid = computed(() => typeof limit.value === 'number' && Number.isInteger(limit.value) &&
  limit.value >= 1 && limit.value <= MAX_ACCOUNT_CONCURRENCY)

watch(() => props.account?.id, () => {
  limit.value = props.account?.concurrency ?? 1
  error.value = ''
}, { immediate: true })

function close() {
  if (!saving.value) emit('close')
}

async function save() {
  if (!props.account || saving.value || !valid.value) return
  const accountID = props.account.id
  const concurrency = Number(limit.value)
  saving.value = true
  error.value = ''
  try {
    // Do not round-trip credentials or a stale account snapshot.
    const updated = await adminAPI.accounts.update(accountID, { concurrency })
    emit('updated', updated)
    appStore.showSuccess(t('admin.accounts.capacity.concurrencySaved'))
    if (props.account?.id === accountID) emit('close')
  } catch (err) {
    if (props.account?.id === accountID) {
      error.value = extractApiErrorMessage(err, t('admin.accounts.capacity.concurrencyFailed'))
    }
  } finally {
    saving.value = false
  }
}
</script>
