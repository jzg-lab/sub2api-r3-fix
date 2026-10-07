<template>
  <div class="provider-filter" :class="{ 'provider-filter--compact': compact }" role="group" :aria-label="t('keys.providerLabel')">
    <div class="mb-2 flex items-center justify-between gap-2">
      <span class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('keys.providerLabel') }}</span>
      <button type="button" class="text-xs text-primary-600 dark:text-primary-400 hover:underline" :aria-pressed="modelValue === null" @click="$emit('update:modelValue', null)">{{ t('keys.allGroups') }}</button>
    </div>
    <div class="grid grid-cols-4 gap-2">
      <button
        v-for="provider in providers" :key="provider.value" type="button"
        class="provider-card relative flex min-w-0 flex-col items-center justify-center gap-2 rounded-lg border transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500"
        :class="modelValue === provider.value ? 'border-primary-500 bg-primary-50 text-primary-700 dark:bg-primary-900/20 dark:text-primary-300' : 'border-gray-300 bg-white text-gray-700 hover:border-primary-400 dark:border-dark-500 dark:bg-dark-800 dark:text-gray-200'"
        :aria-pressed="modelValue === provider.value"
        @click="$emit('update:modelValue', modelValue === provider.value ? null : provider.value)"
      >
        <span class="flex gap-1" aria-hidden="true">
          <span v-for="platform in provider.icons" :key="platform" class="provider-icon rounded-lg" :class="platformBadgeClass(platform)"><PlatformIcon :platform="platform" size="lg" /></span>
        </span>
        <span class="provider-label">{{ provider.label }}</span>
        <Icon v-if="modelValue === provider.value" name="check" size="xs" class="absolute right-1 top-1" aria-hidden="true" />
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import Icon from '@/components/icons/Icon.vue'
import { platformBadgeClass } from '@/utils/platformColors'
import type { GroupPlatform } from '@/types'
import type { KeyGroupProvider } from '@/utils/keyGroupProvider'

defineProps<{ modelValue: KeyGroupProvider | null; compact?: boolean }>()
defineEmits<{ 'update:modelValue': [value: KeyGroupProvider | null] }>()
const { t } = useI18n()
const providers = computed<Array<{ value: KeyGroupProvider; label: string; icons: GroupPlatform[] }>>(() => [
  { value: 'anthropic', label: 'Anthropic', icons: ['anthropic'] },
  { value: 'openai', label: 'OpenAI', icons: ['openai'] },
  { value: 'cn', label: t('keys.providerCN'), icons: ['deepseek', 'kimi'] },
  { value: 'other', label: t('keys.providerOther'), icons: ['gemini', 'grok'] },
])
</script>

<style scoped>
.provider-card { min-height: 96px; padding: 12px 4px; }
.provider-icon { display: grid; place-items: center; width: 28px; height: 28px; }
.provider-label { font-size: 12px; font-weight: 600; overflow-wrap: anywhere; }
.provider-filter--compact .provider-card { min-height: 72px; padding: 8px 4px; }
.provider-filter--compact .provider-icon { width: 24px; height: 24px; }
@media (max-width: 639px) { .provider-label { font-size: 10px; } }
</style>
