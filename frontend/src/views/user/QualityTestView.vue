<template>
  <AppLayout>
    <div class="mx-auto max-w-6xl space-y-6">
      <p class="max-w-3xl text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('qualityTest.hint') }}</p>
      <p v-if="loading" role="status" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <p v-if="error" role="alert" class="rounded-lg bg-red-50 px-4 py-3 text-sm text-red-600 dark:bg-red-500/10 dark:text-red-400">{{ error }}</p>
      <p v-if="notice" role="status" class="rounded-lg bg-emerald-50 px-4 py-3 text-sm text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-400">{{ notice }}</p>

      <div class="grid items-start gap-5 xl:grid-cols-[300px_minmax(0,1fr)]">
        <section class="card min-w-0 p-5 sm:p-6" aria-labelledby="quality-settings-heading">
          <h2 id="quality-settings-heading" class="mb-6 flex items-center gap-2 text-base font-semibold text-gray-900 dark:text-gray-100">
            <Icon name="cog" size="sm" class="text-gray-400" aria-hidden="true" />
            {{ t('qualityTest.settings') }}
          </h2>
          <div class="space-y-5">
            <div>
              <label for="self-quality-key" class="input-label">{{ t('nav.apiKeys') }}</label>
              <Select id="self-quality-key" v-model="keyId" :options="keyOptions" searchable :disabled="running || loading" :aria-label="t('nav.apiKeys')" />
              <button v-if="keys.length < totalKeys" class="btn btn-secondary mt-2" :disabled="loadingKeys || running" @click="loadKeys()">{{ t('qualityTest.moreKeys') }}</button>
              <RouterLink v-if="!loading && !keys.length" to="/keys" class="mt-2 inline-block text-sm text-primary-600 dark:text-primary-400">{{ t('qualityTest.createKey') }}</RouterLink>
            </div>
            <div>
              <label for="self-quality-model" class="input-label">{{ t('admin.accounts.manualQuality.model') }}</label>
              <input id="self-quality-model" v-model="model" class="input" maxlength="200" :disabled="running || loading" />
            </div>
            <fieldset>
              <legend class="input-label">{{ t('admin.accounts.manualQuality.template') }}</legend>
              <div class="space-y-2">
                <button v-for="option in kinds" :key="option.value" type="button" :aria-pressed="kind === option.value" :disabled="running || loading"
                  class="flex w-full items-center gap-3 rounded-lg border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:cursor-not-allowed disabled:opacity-60"
                  :class="kind === option.value ? 'border-primary-500 bg-primary-50 text-primary-700 dark:bg-primary-500/10 dark:text-primary-300' : 'border-gray-200 text-gray-600 hover:bg-gray-50 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700'"
                  @click="kind = option.value">
                  <Icon :name="option.value === 'candy' ? 'lightbulb' : 'edit'" size="md" class="shrink-0" aria-hidden="true" />
                  <span class="min-w-0 flex-1">
                    <span class="block text-sm font-medium">{{ option.label }}</span>
                    <span class="mt-0.5 block text-xs opacity-75">{{ t(`qualityTest.${option.value}Description`) }}</span>
                  </span>
                  <Icon v-if="kind === option.value" name="check" size="sm" class="shrink-0" aria-hidden="true" />
                </button>
              </div>
            </fieldset>
            <p class="border-t border-gray-100 pt-4 text-xs leading-5 text-gray-500 dark:border-dark-700 dark:text-gray-400">{{ t('qualityTest.billingHint') }}</p>
          </div>
        </section>

        <section class="card min-w-0">
          <div class="flex flex-wrap items-center justify-between gap-3 border-b border-gray-100 px-5 py-4 dark:border-dark-700 sm:px-6">
            <div class="flex gap-1 rounded-lg bg-gray-100 p-1 dark:bg-dark-900/60" role="group" :aria-label="t('qualityTest.title')">
              <button v-for="tab in panels" :key="tab" type="button" :aria-pressed="panel === tab" :aria-controls="`quality-${tab}-panel`"
                class="flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500"
                :class="panel === tab ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-700 dark:text-gray-100' : 'text-gray-500 hover:text-gray-900 dark:text-gray-400 dark:hover:text-gray-100'"
                @click="panel = tab">
                <Icon :name="tab === 'question' ? 'document' : 'chat'" size="sm" aria-hidden="true" />
                {{ t(`qualityTest.${tab}`) }}
              </button>
            </div>
            <button v-if="panel === 'question'" class="flex items-center gap-1.5 rounded text-xs font-medium text-gray-500 hover:text-primary-600 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:opacity-50 dark:text-gray-400 dark:hover:text-primary-400" :disabled="running || loading" @click="applyTemplate(defaults.find(value => value.kind === kind))">
              <Icon name="refresh" size="xs" aria-hidden="true" />{{ t('admin.accounts.manualQuality.restore') }}
            </button>
          </div>
          <div v-show="panel === 'question'" id="quality-question-panel" class="space-y-5 p-5 sm:p-6">
            <div>
              <div class="mb-2 flex items-center justify-between gap-3">
                <label for="self-quality-prompt" class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.accounts.manualQuality.prompt') }}</label>
                <span class="text-xs tabular-nums text-gray-400">{{ prompt.length.toLocaleString() }} / 16,000</span>
              </div>
              <textarea id="self-quality-prompt" v-model="prompt" class="input min-h-64 resize-y leading-7 sm:min-h-72" rows="10" maxlength="16000" :disabled="running || loading" />
            </div>
            <div v-if="kind === 'candy'" class="rounded-lg bg-gray-50 p-4 dark:bg-dark-900/50">
              <label for="self-quality-reference" class="input-label">{{ t('admin.accounts.manualQuality.reference') }}</label>
              <input id="self-quality-reference" v-model="reference" class="input" maxlength="1000" :disabled="running || loading" />
              <p class="mt-2 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.accounts.manualQuality.referenceHint') }}</p>
            </div>
          </div>
          <div v-show="panel === 'result'" id="quality-result-panel" class="min-h-80 overflow-y-auto p-5 sm:p-6 xl:max-h-[calc(100dvh-20rem)]">
            <div v-if="running" role="status" class="mb-4 flex items-start gap-3 rounded-lg bg-primary-50 p-4 dark:bg-primary-500/10">
              <Icon name="sync" size="sm" class="mt-0.5 shrink-0 animate-spin text-primary-500" aria-hidden="true" />
              <div>
                <p class="text-sm font-medium text-primary-700 dark:text-primary-300">{{ t('admin.accounts.manualQuality.running') }}</p>
                <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('qualityTest.waitHint') }}</p>
              </div>
            </div>
            <p v-if="task?.error" role="alert" class="mb-4 text-sm text-red-600 dark:text-red-400">{{ task.error }}</p>
            <ManualQualityResult v-if="panel === 'result' && task?.result" :result="task.result" />
            <div v-else-if="!running && !task?.error" class="py-5 text-center">
              <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('qualityTest.emptyResult') }}</p>
              <p class="mt-2 text-xs text-gray-400">{{ t('qualityTest.resultHint') }}</p>
            </div>
          </div>
          <p v-if="running && panel === 'question'" role="status" class="px-5 pb-4 text-xs leading-5 text-gray-500 dark:text-gray-400 sm:px-6">{{ t('qualityTest.waitHint') }}</p>
          <div class="flex flex-col gap-4 border-t border-gray-100 px-5 py-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between sm:px-6">
            <div v-if="panel === 'question'">
              <button class="text-sm font-medium text-gray-600 hover:text-primary-600 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:opacity-50 dark:text-gray-300 dark:hover:text-primary-400" :disabled="running || loading || !prompt.trim()" @click="saveTemplate">{{ t('qualityTest.save') }}</button>
              <p class="mt-1 text-xs text-gray-400">{{ t('qualityTest.localTemplateHint') }}</p>
            </div>
            <button v-else class="text-sm font-medium text-primary-600 dark:text-primary-400" @click="panel = 'question'">{{ t('qualityTest.backToQuestion') }}</button>
            <div class="flex shrink-0 items-center gap-2">
              <button v-if="running" class="btn btn-secondary" @click="task?.controller.abort()">{{ t('common.cancel') }}</button>
              <button class="btn btn-primary w-full gap-2 sm:w-auto" :disabled="running || loading || !selectedKey || !model.trim() || !prompt.trim()" @click="run">
                <Icon :name="running ? 'sync' : 'play'" size="sm" :class="{ 'animate-spin': running }" aria-hidden="true" />
                {{ t(running ? 'admin.accounts.manualQuality.running' : 'admin.accounts.manualQuality.run') }}
              </button>
            </div>
          </div>
        </section>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import ManualQualityResult from '@/components/common/ManualQualityResult.vue'
import { keysAPI } from '@/api/keys'
import { qualityTestTemplates, runUserQualityTest } from '@/api/qualityTest'
import type { QualityKind, QualityTemplate } from '@/api/admin/manualQuality'
import type { ApiKey } from '@/types'
import { useAuthStore } from '@/stores/auth'
import { DEFAULT_QUALITY_MODEL, useManualQualityStore } from '@/stores/manualQuality'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const auth = useAuthStore()
const tests = useManualQualityStore()
const taskKey = 'user'
const task = computed(() => tests.tasks[taskKey])
const running = computed(() => task.value?.running || false)
const panels = ['question', 'result'] as const
const panel = ref<typeof panels[number]>(task.value?.result || task.value?.error ? 'result' : 'question')
watch(running, (current, previous) => {
  if (previous && !current && (task.value?.result || task.value?.error)) panel.value = 'result'
})
const loading = ref(true)
const loadingKeys = ref(false)
const error = ref('')
const notice = ref('')
const keys = ref<ApiKey[]>([])
const totalKeys = ref(0)
let nextPage = 1
let active = true
const keyId = ref<number | string>(task.value?.keyId || '')
const selectedKey = computed(() => keys.value.find(value => value.id === Number(keyId.value)))
const keyOptions = computed(() => {
  const options = keys.value.map(key => ({ value: key.id, label: `${key.name}${key.group ? ` · ${key.group.name} · ${key.group.rate_multiplier}×` : ''}` }))
  if (task.value?.keyId && task.value.keyName && !options.some(value => value.value === task.value?.keyId)) options.unshift({ value: task.value.keyId, label: task.value.keyName })
  return options
})
const model = ref(task.value?.input?.model || DEFAULT_QUALITY_MODEL)
const kind = ref<QualityKind>(task.value?.input?.kind || 'candy')
const kinds = computed(() => (['candy', 'pelican'] as const).map(value => ({ value, label: t(`admin.accounts.manualQuality.${value}`) })))
const prompt = ref('')
const reference = ref('')
const defaults = ref<QualityTemplate[]>([])
const templates = ref<QualityTemplate[]>([])
const storageKey = `quality-test-templates:${auth.user?.id}`

function applyTemplate(template?: QualityTemplate) {
  prompt.value = template?.prompt || ''
  reference.value = template?.reference_answer || ''
  notice.value = ''
}
watch(prompt, () => {
  reference.value = ''
  notice.value = ''
}, { flush: 'sync' })
watch(kind, () => {
  applyTemplate(templates.value.find(value => value.kind === kind.value))
  panel.value = 'question'
})

async function loadKeys() {
  if (loadingKeys.value) return
  loadingKeys.value = true
  try {
    error.value = ''
    const page = await keysAPI.list(nextPage, 100, { status: 'active' })
    if (!active) return
    keys.value.push(...page.items)
    totalKeys.value = page.total
    nextPage++
    if (!keyId.value && keys.value.length) keyId.value = keys.value[0].id
  } catch (cause) {
    if (active) error.value = extractApiErrorMessage(cause)
  } finally {
    loadingKeys.value = false
  }
}
function saveTemplate() {
  error.value = ''
  const saved = { kind: kind.value, prompt: prompt.value, reference_answer: kind.value === 'candy' ? reference.value : '' }
  const updated = [...templates.value.filter(value => value.kind !== kind.value), saved]
  try {
    localStorage.setItem(storageKey, JSON.stringify(updated))
    templates.value = updated
    notice.value = t('qualityTest.saved')
  } catch (cause) {
    error.value = extractApiErrorMessage(cause)
  }
}
function run() {
  const key = selectedKey.value
  if (running.value || loading.value || !key || !model.value.trim() || !prompt.value.trim()) return
  error.value = ''
  notice.value = ''
  panel.value = 'question'
  const input = { model: model.value.trim(), kind: kind.value, prompt: prompt.value, reference_answer: kind.value === 'candy' ? reference.value : '' }
  void tests.start(taskKey, `${key.name} · ${input.model} · ${t(`admin.accounts.manualQuality.${input.kind}`)}`, signal => runUserQualityTest(key.key, input, signal))
  tests.tasks[taskKey].input = input
  tests.tasks[taskKey].keyId = key.id
  tests.tasks[taskKey].keyName = key.name
}
onMounted(async () => {
  const [response] = await Promise.allSettled([qualityTestTemplates(), loadKeys()])
  if (!active) return
  if (response.status === 'fulfilled') {
    defaults.value = response.value
    templates.value = response.value
    try {
      const saved: unknown = JSON.parse(localStorage.getItem(storageKey) || 'null')
      if (Array.isArray(saved)) {
        templates.value = defaults.value.map(fallback => saved.find(value =>
          value?.kind === fallback.kind &&
          typeof value.prompt === 'string' && value.prompt.trim() && value.prompt.length <= 16000 &&
          typeof value.reference_answer === 'string' && value.reference_answer.length <= 1000
        ) || fallback)
      }
    } catch { /* Invalid local drafts fall back to the server defaults. */ }
    applyTemplate(task.value?.input || templates.value.find(value => value.kind === kind.value))
  } else error.value = extractApiErrorMessage(response.reason)
  loading.value = false
})
onBeforeUnmount(() => { active = false })
</script>
