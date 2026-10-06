<template>
  <form class="space-y-3 border-b border-gray-200 pb-5 dark:border-dark-700" @submit.prevent="save">
    <div class="flex flex-wrap items-center gap-3">
      <label for="quality-scope" class="text-sm font-medium">{{ t('admin.plugins.qualityScope') }}</label>
      <select id="quality-scope" v-model="mode" class="input w-full sm:w-56" :disabled="!loaded || saving">
        <option value="all">{{ t('admin.plugins.qualityScopeAll') }}</option>
        <option value="selected">{{ t('admin.plugins.qualityScopeSelected') }}</option>
      </select>
      <button type="submit" class="btn btn-primary btn-sm" :disabled="!loaded || saving">
        <Icon name="check" size="sm" />{{ t('common.save') }}
      </button>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || saving" :title="t('common.refresh')" @click="load">
        <Icon name="refresh" size="sm" /><span class="sr-only">{{ t('common.refresh') }}</span>
      </button>
    </div>
    <fieldset v-if="loaded && mode === 'selected'" :disabled="saving" class="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
      <legend class="sr-only">{{ t('admin.plugins.qualityScopeSelected') }}</legend>
      <label v-for="group in choices" :key="group.id" class="flex min-w-0 items-start gap-2 text-sm">
        <input v-model="selected" type="checkbox" :value="group.id" class="mt-1 shrink-0" />
        <span class="min-w-0 break-words">{{ group.name }} <span class="text-gray-500">#{{ group.id }}</span></span>
      </label>
    </fieldset>
    <p v-if="error" role="alert" class="break-words text-sm text-red-600">{{ error }}</p>
  </form>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { openaiOperationsAPI } from '@/api/admin/openaiOperations'
import { groupsAPI } from '@/api/admin/groups'
import { useAppStore } from '@/stores'
import { extractApiErrorMessage } from '@/utils/apiError'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()
const mode = ref('all')
const selected = ref<number[]>([])
const groups = ref<{ id: number; name: string }[]>([])
const loading = ref(false)
const loaded = ref(false)
const saving = ref(false)
const error = ref('')
const choices = computed(() => [
  ...groups.value,
  ...selected.value.filter(id => !groups.value.some(group => group.id === id)).map(id => ({ id, name: '' }))
])

async function load() {
  loading.value = true
  loaded.value = false
  error.value = ''
  try {
    const [settings, available] = await Promise.all([
      openaiOperationsAPI.getSettings(), groupsAPI.getAll('openai')
    ])
    mode.value = settings.quality_protected_group_ids == null ? 'all' : 'selected'
    selected.value = settings.quality_protected_group_ids ?? []
    groups.value = available
    loaded.value = true
  } catch (err) {
    error.value = extractApiErrorMessage(err)
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!loaded.value || saving.value) return
  saving.value = true
  error.value = ''
  try {
    const settings = await openaiOperationsAPI.getSettings()
    await openaiOperationsAPI.setSettings({
      ...settings,
      quality_protected_group_ids: mode.value === 'all' ? null : selected.value
    })
    appStore.showSuccess(t('common.success'))
  } catch (err) {
    error.value = extractApiErrorMessage(err)
  } finally {
    saving.value = false
  }
}
onMounted(load)
</script>
