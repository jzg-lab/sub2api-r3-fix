<template>
  <div class="flex flex-wrap items-center gap-1" @click.stop @pointerdown.stop>
    <form v-if="editing" class="flex items-center gap-1" @submit.prevent="save">
      <input
        ref="input"
        v-model.number="draft"
        type="number"
        :min="field === 'concurrency' ? 1 : 0"
        step="1"
        required
        :disabled="saving"
        :aria-label="label"
        class="input !h-7 !w-20 !px-2 !py-0 text-xs tabular-nums"
        @keydown.esc.prevent="cancel"
      />
      <button type="submit" :disabled="saving" :title="t('common.save')" :aria-label="t('common.save')" class="flex h-7 w-7 shrink-0 items-center justify-center rounded text-primary-600 hover:bg-primary-50 disabled:opacity-50 dark:hover:bg-dark-700">
        <Icon :name="saving ? 'refresh' : 'check'" size="sm" :class="{ 'animate-spin': saving }" />
      </button>
      <button type="button" :disabled="saving" :title="t('common.cancel')" :aria-label="t('common.cancel')" class="flex h-7 w-7 shrink-0 items-center justify-center rounded text-gray-500 hover:bg-gray-100 disabled:opacity-50 dark:hover:bg-dark-700" @click="cancel">
        <Icon name="x" size="sm" />
      </button>
    </form>
    <template v-else>
      <slot><span class="text-sm tabular-nums text-gray-700 dark:text-gray-300">{{ account[field] }}</span></slot>
      <button type="button" :title="editLabel" :aria-label="editLabel" class="flex h-7 w-7 shrink-0 items-center justify-center rounded text-gray-400 hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700" @click="startEditing">
        <Icon name="edit" size="xs" />
      </button>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import Icon from '@/components/icons/Icon.vue'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'

const props = defineProps<{ account: Account; field: 'concurrency' | 'priority' }>()
const emit = defineEmits<{
  updated: [account: Account]
  editing: [editing: boolean]
}>()
const { t } = useI18n()
const appStore = useAppStore()
const editing = ref(false)
const saving = ref(false)
const draft = ref<number | string>(0)
const input = ref<HTMLInputElement | null>(null)
const label = computed(() => t(`admin.accounts.${props.field}`))
const editLabel = computed(() => `${t('common.edit')} ${label.value}`)

const startEditing = async () => {
  draft.value = props.account[props.field]
  editing.value = true
  emit('editing', true)
  await nextTick()
  input.value?.focus()
  input.value?.select()
}
const cancel = () => {
  if (saving.value) return
  editing.value = false
  emit('editing', false)
}
const save = async () => {
  if (saving.value) return
  const value = draft.value
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < (props.field === 'concurrency' ? 1 : 0)) {
    appStore.showError(t('admin.accounts.schedulingInvalid'))
    return
  }
  if (value === props.account[props.field]) {
    cancel()
    return
  }
  saving.value = true
  try {
    // The partial bulk endpoint avoids overwriting other concurrently edited fields.
    const result = await adminAPI.accounts.bulkUpdate([props.account.id], { [props.field]: value })
    if (result.success !== 1 || result.failed !== 0) {
      throw new Error(result.results?.find(item => !item.success)?.error || t('common.error'))
    }
    emit('updated', { ...props.account, [props.field]: value })
    editing.value = false
    emit('editing', false)
    appStore.showSuccess(t('common.success'))
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('common.error')))
  } finally {
    saving.value = false
  }
}
onBeforeUnmount(() => {
  if (editing.value) emit('editing', false)
})
</script>
