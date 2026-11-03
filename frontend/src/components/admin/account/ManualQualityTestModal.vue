<template>
  <BaseDialog :show="true" :title="`${t('admin.accounts.manualQuality.title')} · ${account.name}`" width="wide" @close="emit('close')">
    <div class="space-y-4">
      <p class="text-sm text-gray-500">{{ t('admin.accounts.manualQuality.hint') }}</p>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-if="notice" role="status" class="text-sm text-emerald-600 dark:text-emerald-400">{{ notice }}</p>
      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="quality-model" class="input-label">{{ t('admin.accounts.manualQuality.model') }}</label>
          <Select id="quality-model" v-model="model" :options="models" searchable creatable :disabled="running || loading" :aria-label="t('admin.accounts.manualQuality.model')" :placeholder="t('admin.accounts.manualQuality.modelHint')" />
        </div>
        <div>
          <label for="quality-kind" class="input-label">{{ t('admin.accounts.manualQuality.template') }}</label>
          <Select id="quality-kind" v-model="kind" :options="kinds" :disabled="running || loading" :aria-label="t('admin.accounts.manualQuality.template')" />
        </div>
      </div>
      <div>
        <label for="quality-prompt" class="input-label">{{ t('admin.accounts.manualQuality.prompt') }}</label>
        <textarea id="quality-prompt" v-model="prompt" class="input w-full font-mono text-sm" rows="10" :disabled="running || loading" maxlength="16000" />
      </div>
      <div v-if="kind === 'candy'">
        <label for="quality-reference" class="input-label">{{ t('admin.accounts.manualQuality.reference') }}</label>
        <input id="quality-reference" v-model="reference" class="input w-full" maxlength="1000" :disabled="running || loading" />
        <p class="mt-1 text-xs text-gray-500">{{ t('admin.accounts.manualQuality.referenceHint') }}</p>
      </div>
      <div class="flex flex-wrap gap-2">
        <button class="btn btn-secondary" :disabled="running || loading" @click="restore">{{ t('admin.accounts.manualQuality.restore') }}</button>
        <button class="btn btn-secondary" :disabled="running || loading || saving || !prompt.trim()" @click="save">{{ t('admin.accounts.manualQuality.save') }}</button>
        <button class="btn btn-primary" :disabled="running || loading || saving || !model || !prompt.trim()" @click="run">{{ t(running ? 'admin.accounts.manualQuality.running' : 'admin.accounts.manualQuality.run') }}</button>
        <button v-if="running" class="btn btn-secondary" @click="task?.controller.abort()">{{ t('common.cancel') }}</button>
        <button v-if="running" class="btn btn-secondary" @click="emit('close')">{{ t('qualityTest.background') }}</button>
      </div>
      <p v-if="running" role="status" class="text-sm text-gray-500">{{ t('admin.accounts.manualQuality.waitHint') }}</p>
      <ManualQualityResult v-if="result" :result="result" />
      <button v-if="result?.status === 'running'" class="btn btn-secondary" :disabled="running" @click="loadResult">{{ t('common.refresh') }}</button>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { adminQualityKey, DEFAULT_QUALITY_MODEL, useManualQualityStore } from '@/stores/manualQuality'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import { getAvailableModels } from '@/api/admin/accounts'
import { manualQualityAPI, type QualityKind, type QualityResult, type QualityTemplate } from '@/api/admin/manualQuality'
import ManualQualityResult from '@/components/common/ManualQualityResult.vue'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'

const props = defineProps<{ account: Account; initialResult?: QualityResult }>()
const emit = defineEmits<{ close: []; result: [result: QualityResult] }>()
const { t } = useI18n()
const loading = ref(true)
const saving = ref(false)
const tests = useManualQualityStore()
const task = computed(() => tests.tasks[adminQualityKey(props.account.id, model.value, kind.value)])
const running = computed(() => task.value?.running || false)
const localError = ref('')
const error = computed(() => task.value?.error || localError.value)
const notice = ref('')
const accountTasks = Object.entries(tests.tasks).filter(([key]) => key.startsWith(`admin:${props.account.id}:`)).map(([, task]) => task)
const pending = accountTasks.find(task => task.running) || accountTasks.filter(task => task.result).sort((a, b) => (b.result?.started_at || '').localeCompare(a.result?.started_at || ''))[0]
const model = ref(pending?.input?.model || DEFAULT_QUALITY_MODEL)
const kind = ref<QualityKind>(pending?.input?.kind || props.initialResult?.kind || 'candy')
const prompt = ref('')
const reference = ref('')
const models = ref<{ value: string; label: string }[]>([])
const templates = ref<QualityTemplate[]>([])
const defaults = ref<QualityTemplate[]>([])
const savedResult = ref<QualityResult | null>(null)
const result = computed(() => {
  const local = task.value?.result
  const saved = savedResult.value
  if (!local) return saved
  if (task.value?.saved && saved && Date.parse(saved.started_at) >= Date.parse(local.started_at)) return saved
  return local
})
let resultSequence = 0
let active = true
const kinds = computed(() => (['candy', 'pelican'] as const).map(value => ({ value, label: t(`admin.accounts.manualQuality.${value}`) })))

function applyTemplate(template?: QualityTemplate) {
  prompt.value = template?.prompt || ''
  reference.value = template?.reference_answer || ''
}
function restore() { applyTemplate(defaults.value.find(value => value.kind === kind.value)); notice.value = '' }
watch(prompt, () => { reference.value = ''; notice.value = '' }, { flush: 'sync' })
watch(kind, () => { applyTemplate(templates.value.find(value => value.kind === kind.value)); void loadResult() })
watch(model, () => { void loadResult() })

async function loadResult() {
  const sequence = ++resultSequence
  savedResult.value = null
  if (!model.value || loading.value || running.value) return
  try {
    const saved = await manualQualityAPI.result(props.account.id, model.value, kind.value)
    if (active && sequence === resultSequence) savedResult.value = saved
  } catch (cause) { if (active && sequence === resultSequence) localError.value = extractApiErrorMessage(cause) }
}
async function save() {
  if (saving.value || running.value) return
  saving.value = true
  localError.value = ''
  notice.value = ''
  const input = { kind: kind.value, prompt: prompt.value, reference_answer: kind.value === 'candy' ? reference.value : '' }
  try {
    const saved = await manualQualityAPI.save(input)
    templates.value = templates.value.map(value => value.kind === saved.kind ? saved : value)
    notice.value = t('admin.accounts.manualQuality.saved')
  } catch (cause) { localError.value = extractApiErrorMessage(cause) }
  finally { saving.value = false }
}
async function run() {
  if (running.value || loading.value || !model.value || !prompt.value.trim()) return
  ++resultSequence
  localError.value = ''
  notice.value = ''
  savedResult.value = null
  const finished = await tests.runAdmin(props.account.id, props.account.name, {
    kind: kind.value, model: model.value, prompt: prompt.value,
    reference_answer: kind.value === 'candy' ? reference.value : ''
  })
  if (active && finished.saved && finished.result) emit('result', finished.result)
}
onMounted(async () => {
  const [templateResponse, modelResponse] = await Promise.allSettled([
    manualQualityAPI.templates(), getAvailableModels(props.account.id)
  ])
  if (!active) return
  if (templateResponse.status === 'fulfilled') {
    templates.value = templateResponse.value.templates
    defaults.value = templateResponse.value.defaults
    applyTemplate(task.value?.input || templates.value.find(value => value.kind === kind.value))
  } else localError.value = extractApiErrorMessage(templateResponse.reason)
  if (modelResponse.status === 'fulfilled') {
    models.value = modelResponse.value.map(value => ({ value: value.id, label: value.display_name || value.id }))
    if (!models.value.some(value => value.value === model.value)) {
      models.value.unshift({ value: model.value, label: model.value })
    }
  } else if (!error.value) localError.value = t('admin.accounts.manualQuality.modelLoadFailed')
  loading.value = false
  await loadResult()
})
onBeforeUnmount(() => { active = false; ++resultSequence })
</script>
