<template>
  <BaseDialog :show="show" :title="t('deployment.title')" width="wide" @close="$emit('close')">
    <div class="space-y-5">
      <div class="flex items-center justify-between gap-3 border-b border-gray-100 pb-3 dark:border-dark-700">
        <p class="flex min-w-0 items-center gap-2 text-sm text-gray-500 dark:text-dark-400" role="status">
          <Icon :name="disconnected ? 'exclamationCircle' : 'server'" size="sm" class="shrink-0" />
          {{ disconnected ? t('deployment.disconnected') : loading ? t('deployment.loading') : t('deployment.status') }}
        </p>
        <button type="button" class="btn btn-ghost h-8 w-8 shrink-0 p-1.5" :disabled="loading || busy" :title="t('deployment.refresh')" :aria-label="t('deployment.refresh')" @click="refresh(true)">
          <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />
        </button>
      </div>
      <p v-if="error" role="alert" class="break-words text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-if="snapshot?.catalog_error" role="alert" class="text-sm text-amber-700 dark:text-amber-400">{{ t('deployment.catalogUnavailable') }}</p>
      <p v-if="snapshot && !snapshot.enabled" class="text-sm text-amber-700 dark:text-amber-400">{{ t('deployment.unavailable') }}</p>
      <template v-if="snapshot?.enabled">
        <section v-if="snapshot.job" class="space-y-4 border-b border-gray-200 pb-5 dark:border-dark-700" :aria-label="t('deployment.currentTask')">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div class="min-w-0">
              <p class="mb-1 text-xs text-gray-500 dark:text-dark-400">{{ t('deployment.currentTask') }}</p>
              <h4 class="break-all font-mono text-base font-semibold text-gray-900 dark:text-white">{{ snapshot.job.version }}</h4>
            </div>
            <span class="badge max-w-full py-1" :class="phaseTone" role="status">
              <Icon :name="phaseIcon" size="sm" class="shrink-0" />
              {{ phaseLabel(snapshot.job.phase) }}
            </span>
          </div>
          <p v-if="snapshot.job.error_code" class="text-sm text-red-600 dark:text-red-400">{{ errorLabel(snapshot.job.error_code) }}</p>
          <details class="text-xs text-gray-500 dark:text-dark-400">
            <summary class="cursor-pointer">{{ t('deployment.details') }}</summary>
            <dl class="mt-2 grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1.5">
              <dt>{{ t('deployment.source') }}</dt><dd class="break-all font-mono">{{ snapshot.job.source }}</dd>
              <dt>{{ t('deployment.taskId') }}</dt><dd class="break-all font-mono">{{ snapshot.job.id }}</dd>
            </dl>
          </details>
          <p v-if="snapshot.job.reused" class="text-xs text-gray-500">{{ t('deployment.reused') }}</p>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <p v-if="snapshot.job.expires_at" class="flex items-start gap-1.5 text-xs text-gray-500 dark:text-dark-400"><Icon name="clock" size="sm" class="shrink-0" />{{ t('deployment.expires', { time: new Date(snapshot.job.expires_at * 1000).toLocaleString() }) }}</p>
            <button v-if="snapshot.job.actions.includes('activate')" type="button" class="btn btn-primary ml-auto" :disabled="busy || disconnected" @click="beginConfirmation('activate')">
              <Icon name="swap" size="sm" />{{ t('deployment.activate') }}
            </button>
          </div>
        </section>
        <section v-if="previousJob?.previous_source" class="flex flex-wrap items-center justify-between gap-4 border-b border-gray-200 pb-5 dark:border-dark-700" :aria-label="t('deployment.previousVersion')">
          <div class="min-w-0 space-y-1">
            <h4 class="mb-2 flex items-center gap-2 text-sm font-medium text-gray-700 dark:text-dark-300"><Icon name="arrowLeft" size="sm" class="text-gray-400 dark:text-dark-400" />{{ t('deployment.previousVersion') }}</h4>
            <div class="flex flex-wrap items-center gap-2">
              <p class="break-all font-mono text-sm font-semibold">{{ previousJob.previous_version || previousJob.previous_source.slice(0, 12) }}</p>
              <span class="badge" :class="rollbackJob ? 'badge-success' : 'badge-gray'">{{ t(rollbackJob ? 'deployment.rollbackAvailable' : 'deployment.rollbackUnavailable') }}</span>
            </div>
            <details class="pt-1 text-xs text-gray-500 dark:text-dark-400">
              <summary class="cursor-pointer">{{ t('deployment.details') }}</summary>
              <p class="mt-2 break-all font-mono">{{ previousJob.previous_source }}</p>
            </details>
            <p v-if="rollbackJob?.expires_at && rollbackJob.id !== snapshot.job?.id" class="pt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('deployment.expires', { time: new Date(rollbackJob.expires_at * 1000).toLocaleString() }) }}</p>
          </div>
          <button v-if="rollbackJob" type="button" class="btn btn-secondary" :disabled="busy || disconnected" @click="beginConfirmation('rollback')">
            <Icon name="arrowLeft" size="sm" />{{ t('deployment.rollback') }}
          </button>
        </section>
        <section :aria-label="t('deployment.candidates')">
          <h4 class="mb-1 flex items-center gap-2 text-sm font-medium text-gray-700 dark:text-dark-300"><Icon name="download" size="sm" class="text-gray-400 dark:text-dark-400" />{{ t('deployment.candidates') }}</h4>
          <p v-if="!snapshot.candidates.length" class="py-5 text-sm text-gray-500">{{ t('deployment.empty') }}</p>
          <ul class="divide-y divide-gray-100 dark:divide-dark-700">
            <li v-for="candidate in snapshot.candidates" :key="candidate.id" class="flex flex-wrap items-start justify-between gap-4 py-4">
              <div class="min-w-0 flex-1 basis-64 space-y-2">
                <div class="flex flex-wrap items-center gap-2">
                  <h4 class="break-all font-mono text-sm font-semibold text-gray-900 dark:text-white">{{ candidate.version }}</h4>
                  <span class="badge" :class="candidate.channel === 'special' ? 'badge-warning' : 'badge-gray'">{{ t(`deployment.channels.${candidate.channel}`) }}</span>
                </div>
                <p class="break-words whitespace-pre-wrap text-sm leading-relaxed text-gray-600 dark:text-dark-300">{{ candidate.summary }}</p>
                <p v-if="candidate.channel === 'special'" class="text-xs leading-relaxed text-amber-700 dark:text-amber-400">{{ candidate.reason || t('deployment.special') }}</p>
                <details class="text-xs text-gray-500 dark:text-dark-400">
                  <summary class="cursor-pointer">{{ t('deployment.details') }}</summary>
                  <p class="mt-2 break-all font-mono">{{ candidate.source }}</p>
                </details>
              </div>
              <button v-if="candidate.channel !== 'special'" type="button" class="btn shrink-0" :class="!snapshot.job ? 'btn-primary' : 'btn-secondary'" :disabled="busy || disconnected || jobActive || (snapshot.job?.candidate_id === candidate.id && ['PREPARED', 'ACCEPTED'].includes(snapshot.job.phase))" @click="prepare(candidate.id)">
                <Icon name="download" size="sm" />{{ t('deployment.prepare') }}
              </button>
            </li>
          </ul>
        </section>
      </template>
    </div>
  </BaseDialog>
  <BaseDialog :show="confirm !== null" :title="t(confirm === 'rollback' ? 'deployment.rollback' : 'deployment.activate')" width="narrow" :z-index="60" @close="confirm = null">
    <p v-if="confirm === 'rollback'" class="mb-2 text-xs text-gray-500 dark:text-dark-400">{{ t('deployment.previousVersion') }}</p>
    <p class="break-all font-mono font-semibold">{{ confirm === 'rollback' ? (confirmJob?.previous_version || t('deployment.targetCommit')) : confirmJob?.version }}</p>
    <p class="mt-2 break-all font-mono text-xs text-gray-500">{{ confirm === 'rollback' ? confirmJob?.previous_source : confirmJob?.source }}</p>
    <p class="mt-4 text-sm">{{ t(confirmJob?.live_handoff ? (confirm === 'rollback' ? 'deployment.liveRollbackConfirm' : 'deployment.liveHandoffConfirm') : confirm === 'rollback' ? 'deployment.rollbackConfirm' : confirmJob?.mode === 'handoff' ? 'deployment.handoffConfirm' : 'deployment.activateConfirm') }}</p>
    <template #footer>
      <button type="button" class="btn btn-secondary" :disabled="busy" @click="confirm = null">{{ t('common.cancel') }}</button>
      <button type="button" class="btn btn-primary" :disabled="busy || disconnected" @click="executeConfirmed">
        <Icon name="check" size="sm" />{{ t('common.confirm') }}
      </button>
    </template>
  </BaseDialog>
  <TotpStepUpDialog :controller="stepUp" />
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { deploymentAction, getDeployment, type DeploymentJob, type DeploymentSnapshot } from '@/api/admin/deployment'
import { isStepUpBlocked, isStepUpCancelled, stepUpBlockReason, useStepUp } from '@/composables/useStepUp'
import BaseDialog from './BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'

const props = defineProps<{ show: boolean }>()
defineEmits<{ close: [] }>()
const { t, te } = useI18n()
const stepUp = useStepUp()
const snapshot = ref<DeploymentSnapshot | null>(null)
const loading = ref(false)
const busy = ref(false)
const disconnected = ref(false)
const error = ref('')
const requestUncertain = ref(false)
const confirm = ref<'activate' | 'rollback' | null>(null)
const confirmJob = ref<DeploymentJob | null>(null)
const settled = new Set(['PREPARATION_FAILED', 'BLOCKED', 'PREPARED', 'EXPIRED', 'ACCEPTED', 'ROLLED_BACK', 'RECOVERED', 'DEFERRED', 'PREFLIGHT_FAILED', 'LAUNCH_FAILED'])
const jobActive = computed(() => {
  const job = snapshot.value?.job
  return !!job && (!settled.has(job.phase) || ['AUXILIARY_SERVICES_PENDING', 'PREVIOUS_REQUESTS_PENDING'].includes(job.error_code || ''))
})
const rollbackJob = computed(() => [snapshot.value?.rollback_job, snapshot.value?.job].find(job => job?.actions.includes('rollback')) ?? null)
const previousJob = computed(() => rollbackJob.value ?? snapshot.value?.job)
const phaseKind = computed(() => {
  const phase = snapshot.value?.job?.phase || ''
  if (phase === 'ACCEPTED') return 'success'
  if (['PREPARATION_FAILED', 'BLOCKED', 'PREFLIGHT_FAILED', 'LAUNCH_FAILED', 'NEEDS_ATTENTION'].includes(phase)) return 'error'
  if (['ROLLED_BACK', 'RECOVERED', 'RECOVERING', 'EXPIRED', 'DEFERRED'].includes(phase)) return 'warning'
  return 'active'
})
const phaseTone = computed(() => ({
  success: 'badge-success',
  error: 'badge-danger',
  warning: 'badge-warning',
  active: 'badge-primary'
}[phaseKind.value]))
const phaseIcon = computed(() => ({ success: 'check', error: 'exclamationCircle', warning: 'arrowLeft', active: 'clock' } as const)[phaseKind.value])
let poll: ReturnType<typeof setTimeout> | undefined
let generation = 0

function phaseLabel(phase: string) {
  return te(`deployment.phases.${phase}`) ? t(`deployment.phases.${phase}`) : phase
}

function errorLabel(code: string) {
  return te(`deployment.errors.${code}`) ? t(`deployment.errors.${code}`) : t('deployment.checkFailed')
}

function schedule() {
  clearTimeout(poll)
  if (props.show && (jobActive.value || disconnected.value || snapshot.value?.job?.phase === 'PREPARED' || snapshot.value?.job?.error_code === 'AUXILIARY_SERVICES_PENDING' || rollbackJob.value)) {
    poll = setTimeout(() => refresh(), 3000)
  }
}

async function refresh(force = false) {
  if (loading.value || busy.value) return
  const current = generation
  loading.value = true
  try {
    const result = await getDeployment(force)
    if (current !== generation) return
    snapshot.value = result
    disconnected.value = false
    if (requestUncertain.value) { error.value = ''; requestUncertain.value = false }
    const target = confirm.value === 'rollback' ? rollbackJob.value : result.job
    if (confirm.value && (target?.id !== confirmJob.value?.id || target?.source !== confirmJob.value?.source || target?.previous_source !== confirmJob.value?.previous_source || !target?.actions.includes(confirm.value))) confirm.value = null
  } catch {
    if (current === generation) disconnected.value = true
  } finally {
    loading.value = false
    if (current === generation) schedule()
    else if (props.show) void refresh()
  }
}

async function act(action: 'prepare' | 'activate' | 'rollback', id: string) {
  if (busy.value || disconnected.value) return
  busy.value = true
  error.value = ''
  clearTimeout(poll)
  try {
    snapshot.value = await stepUp.run(() => deploymentAction(action, id))
  } catch (err) {
    if (isStepUpBlocked(err)) {
      error.value = t(stepUpBlockReason(err) === 'STEP_UP_ADMIN_API_KEY_FORBIDDEN' ? 'stepUp.adminApiKeyForbidden' : 'stepUp.notEnabled')
    } else if (!isStepUpCancelled(err)) {
      error.value = t('deployment.actionUncertain')
      requestUncertain.value = true
      disconnected.value = true
    }
  } finally {
    busy.value = false
    confirm.value = null
    // A lost response is not a failed deployment. Read the persisted job; never resend automatically.
    await refresh()
  }
}

async function prepare(id: string) { await act('prepare', id) }
function beginConfirmation(action: 'activate' | 'rollback') {
  const target = action === 'rollback' ? rollbackJob.value : snapshot.value?.job
  if (!target?.actions.includes(action)) return
  confirmJob.value = { ...target }
  confirm.value = action
}
async function executeConfirmed() {
  const job = confirmJob.value
  const action = confirm.value
  const target = action === 'rollback' ? rollbackJob.value : snapshot.value?.job
  if (action && job && target?.id === job.id && target.source === job.source && target.previous_source === job.previous_source && target.actions.includes(action)) {
    // Let the existing step-up dialog own the foreground while authenticating.
    confirm.value = null
    await act(action, job.id)
  }
}

watch(() => props.show, show => {
  generation++
  clearTimeout(poll)
  confirm.value = null
  if (show) void refresh()
}, { immediate: true })
onBeforeUnmount(() => { generation++; clearTimeout(poll) })
</script>
