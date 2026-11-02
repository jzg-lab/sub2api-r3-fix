<template>
  <BaseDialog :show="true" :title="`${t('admin.accounts.manualQuality.title')} · ${account.name}`" width="wide" :close-on-escape="!running" :show-close-button="!running" @close="emit('close')">
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
        <button v-if="running" class="btn btn-secondary" @click="controller?.abort()">{{ t('common.cancel') }}</button>
      </div>
      <p v-if="running" role="status" class="text-sm text-gray-500">{{ t('admin.accounts.manualQuality.waitHint') }}</p>
      <section v-if="result" class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" aria-live="polite">
        <div class="flex flex-wrap gap-2 text-sm">
          <strong>{{ t(`admin.accounts.manualQuality.status.${result.status}`) }}</strong>
          <span>{{ t(`admin.accounts.manualQuality.${result.kind}`) }} · {{ result.model }}</span>
          <span>{{ new Date(result.started_at).toLocaleString() }} · {{ (result.duration_ms / 1000).toFixed(1) }}s</span>
        </div>
        <p v-if="result.error" class="text-sm text-red-600 dark:text-red-400">{{ result.error }}</p>
        <p v-if="comparison !== null" class="text-sm text-gray-500">{{ t(comparison ? 'admin.accounts.manualQuality.match' : 'admin.accounts.manualQuality.noMatch') }}</p>
        <img v-if="svgURL" :src="svgURL" :alt="t('admin.accounts.manualQuality.preview')" class="max-h-[28rem] w-full rounded-lg bg-white p-3" />
        <p v-else-if="result.kind === 'pelican' && result.status === 'completed'" class="text-sm text-gray-500">{{ t('admin.accounts.manualQuality.noSVG') }}</p>
        <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800">{{ result.answer }}</pre>
        <details class="text-sm">
          <summary class="cursor-pointer">{{ t('admin.accounts.manualQuality.details') }}</summary>
          <p class="mt-2">{{ t('admin.accounts.manualQuality.upstreamModel') }}: {{ result.upstream_model || '—' }}</p>
          <pre class="whitespace-pre-wrap break-words">{{ result.prompt }}</pre>
          <p>{{ t('admin.accounts.manualQuality.reference') }}: {{ result.reference_answer || '—' }}</p>
          <pre class="whitespace-pre-wrap break-words">{{ JSON.stringify(result.parameters || {}, null, 2) }}</pre>
          <p>{{ t('admin.accounts.manualQuality.usage') }}: {{ result.usage ? JSON.stringify(result.usage) : '—' }}</p>
        </details>
        <button v-if="result.status === 'running'" class="btn btn-secondary" :disabled="running" @click="loadResult">{{ t('common.refresh') }}</button>
      </section>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import { getAvailableModels } from '@/api/admin/accounts'
import { manualQualityAPI, type QualityKind, type QualityResult, type QualityTemplate } from '@/api/admin/manualQuality'
import { qualitySVGDataURL, qualityAnswerMatches } from '@/utils/manualQuality'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'

const props = defineProps<{ account: Account; initialResult?: QualityResult }>()
const emit = defineEmits<{ close: []; result: [result: QualityResult] }>()
const { t } = useI18n()
const loading = ref(true)
const saving = ref(false)
const running = ref(false)
const error = ref('')
const notice = ref('')
const model = ref(props.initialResult?.model || '')
const kind = ref<QualityKind>(props.initialResult?.kind || 'candy')
const prompt = ref('')
const reference = ref('')
const models = ref<{ value: string; label: string }[]>([])
const templates = ref<QualityTemplate[]>([])
const defaults = ref<QualityTemplate[]>([])
const result = ref<QualityResult | null>(null)
let controller: AbortController | undefined
let resultSequence = 0
let active = true
const kinds = computed(() => (['candy', 'pelican'] as const).map(value => ({ value, label: t(`admin.accounts.manualQuality.${value}`) })))
const svgURL = computed(() => result.value?.kind === 'pelican' ? qualitySVGDataURL(result.value.answer) : null)
const comparison = computed(() => result.value?.kind === 'candy' && result.value.status === 'completed' ? qualityAnswerMatches(result.value.answer, result.value.reference_answer) : null)

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
  result.value = null
  if (!model.value || loading.value || running.value) return
  try {
    const saved = await manualQualityAPI.result(props.account.id, model.value, kind.value)
    if (active && sequence === resultSequence) result.value = saved
  } catch (cause) { if (active && sequence === resultSequence) error.value = extractApiErrorMessage(cause) }
}
async function save() {
  if (saving.value || running.value) return
  saving.value = true; error.value = ''; notice.value = ''
  const input = { kind: kind.value, prompt: prompt.value, reference_answer: kind.value === 'candy' ? reference.value : '' }
  try {
    const saved = await manualQualityAPI.save(input)
    templates.value = templates.value.map(value => value.kind === saved.kind ? saved : value)
    notice.value = t('admin.accounts.manualQuality.saved')
  } catch (cause) { error.value = extractApiErrorMessage(cause) }
  finally { saving.value = false }
}
async function run() {
  if (running.value || loading.value || !model.value || !prompt.value.trim()) return
  ++resultSequence
  running.value = true; error.value = ''; notice.value = ''; result.value = null
  controller = new AbortController()
  try {
    const response = await manualQualityAPI.run(props.account.id, {
      kind: kind.value, model: model.value, prompt: prompt.value,
      reference_answer: kind.value === 'candy' ? reference.value : ''
    }, controller.signal)
    if (!active) return
    result.value = response.result
    if (response.saved) emit('result', response.result)
    else error.value = response.error || t('admin.accounts.manualQuality.saveFailed')
  } catch (cause) {
    if (!active) return
    error.value = controller.signal.aborted ? t('admin.accounts.manualQuality.canceled') : extractApiErrorMessage(cause)
  } finally { running.value = false }
}
onMounted(async () => {
  const [templateResponse, modelResponse] = await Promise.allSettled([
    manualQualityAPI.templates(), getAvailableModels(props.account.id)
  ])
  if (!active) return
  if (templateResponse.status === 'fulfilled') {
    templates.value = templateResponse.value.templates
    defaults.value = templateResponse.value.defaults
    applyTemplate(templates.value.find(value => value.kind === kind.value))
  } else error.value = extractApiErrorMessage(templateResponse.reason)
  if (modelResponse.status === 'fulfilled') {
    models.value = modelResponse.value.map(value => ({ value: value.id, label: value.display_name || value.id }))
    if (!model.value && models.value.length) model.value = models.value[0].value
  } else if (!error.value) error.value = t('admin.accounts.manualQuality.modelLoadFailed')
  loading.value = false
  await loadResult()
})
onBeforeUnmount(() => { active = false; ++resultSequence; controller?.abort() })
</script>
