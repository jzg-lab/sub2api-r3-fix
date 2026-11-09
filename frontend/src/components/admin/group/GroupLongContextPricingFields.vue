<template>
  <div class="mt-3 space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-800">
    <label class="block">
      <span class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.groups.modelPricing.threshold') }}</span>
      <input
        :value="modelValue.thresholdK"
        type="number" min="1" max="2147483" step="1"
        class="input mt-1 max-w-xs" data-testid="long-context-default"
        :placeholder="t('admin.groups.modelPricing.inheritThreshold')"
        @input="emit('update:modelValue', { ...modelValue, thresholdK: ($event.target as HTMLInputElement).value })"
      />
    </label>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.modelPricing.thresholdHint') }}</p>
    <div class="flex items-center justify-between gap-2">
      <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.groups.modelPricing.modelThresholds') }}</span>
      <button type="button" class="btn btn-secondary btn-sm" data-testid="long-context-add" @click="addModel">
        {{ t('admin.groups.modelPricing.addModelThreshold') }}
      </button>
    </div>
    <div v-for="(row, index) in modelValue.models" :key="index" class="flex flex-wrap items-end gap-2">
      <label class="min-w-0 flex-[2] basis-48">
        <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.modelPricing.billingModel') }}</span>
        <input :value="row.model" type="text" maxlength="256" class="input mt-1" data-testid="long-context-model"
          @input="updateRow(index, 'model', ($event.target as HTMLInputElement).value)" />
      </label>
      <label class="min-w-0 flex-1 basis-24">
        <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.modelPricing.threshold') }}</span>
        <input :value="row.thresholdK" type="number" min="1" max="2147483" step="1" class="input mt-1" data-testid="long-context-model-threshold"
          @input="updateRow(index, 'thresholdK', ($event.target as HTMLInputElement).value)" />
      </label>
      <button type="button" class="btn btn-secondary" :aria-label="t('common.delete')" @click="removeRow(index)">{{ t('common.delete') }}</button>
    </div>
    <p v-if="modelValue.models.length" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.modelPricing.modelThresholdHint') }}</p>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { GroupLongContextPricingForm } from '@/views/admin/groupsLongContextPricing'

const { t } = useI18n()
const props = defineProps<{ modelValue: GroupLongContextPricingForm }>()
const emit = defineEmits<{ 'update:modelValue': [value: GroupLongContextPricingForm] }>()

function addModel() {
  emit('update:modelValue', { ...props.modelValue, models: [...props.modelValue.models, { model: '', thresholdK: '' }] })
}
function updateRow(index: number, field: 'model' | 'thresholdK', value: string) {
  const models = props.modelValue.models.map((row, i) => i === index ? { ...row, [field]: value } : row)
  emit('update:modelValue', { ...props.modelValue, models })
}
function removeRow(index: number) {
  emit('update:modelValue', { ...props.modelValue, models: props.modelValue.models.filter((_, i) => i !== index) })
}
</script>
