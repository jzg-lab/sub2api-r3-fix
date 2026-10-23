import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import DeploymentDialog from '@/components/common/DeploymentDialog.vue'
import type { DeploymentSnapshot } from '@/api/admin/deployment'

const mocks = vi.hoisted(() => ({ get: vi.fn(), action: vi.fn(), stepUp: vi.fn() }))
vi.mock('@/api/admin/deployment', () => ({ getDeployment: mocks.get, deploymentAction: mocks.action }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
vi.mock('@/composables/useStepUp', () => ({ useStepUp: () => ({ run: mocks.stepUp }), isStepUpCancelled: () => false, isStepUpBlocked: () => false, stepUpBlockReason: () => '' }))
vi.mock('@/components/auth/TotpStepUpDialog.vue', () => ({ default: { template: '<div />' } }))

const candidate = { id: 'release-1', version: 'v0.2.17+r17.1', source: 'b'.repeat(40), summary: 'Reviewed fix', channel: 'ordinary' }
const job = { id: 'job-1', candidate_id: candidate.id, version: candidate.version, source: candidate.source, previous_source: 'a'.repeat(40), phase: 'PREPARED', actions: ['activate'], expires_at: 2000000000 }
let wrapper: VueWrapper
async function open(state: DeploymentSnapshot = { enabled: true, candidates: [candidate], job: null }) {
  mocks.get.mockResolvedValue(state)
  wrapper = mount(DeploymentDialog, { props: { show: true }, global: { stubs: {
    Icon: true,
    BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' }
  } } })
  await flushPromises()
}
function button(text: string) { return wrapper.findAll('button').find(b => b.text().includes(text))! }

beforeEach(() => {
  vi.useFakeTimers()
  vi.clearAllMocks()
  mocks.stepUp.mockImplementation((action: () => Promise<unknown>) => action())
})
afterEach(() => { wrapper?.unmount(); vi.useRealTimers() })

describe('managed deployment controls', () => {
  it('shows the actual previous version, not the release being rolled back', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, previous_version: 'v0.2.16+r17.9', phase: 'ACCEPTED', actions: ['rollback'] } })
    const previous = wrapper.get('section[aria-label="deployment.previousVersion"]')
    expect(previous.text()).toContain('v0.2.16+r17.9')
    expect(previous.text()).not.toContain(candidate.version)
    await button('deployment.rollback').trigger('click')
    expect(wrapper.text().match(/v0.2.16\+r17.9/g)).toHaveLength(2)
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('keeps previous identity visible after expiry without enabling rollback', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, phase: 'ACCEPTED', actions: [] } })
    const previous = wrapper.get('section[aria-label="deployment.previousVersion"]')
    expect(previous.text()).toContain(job.previous_source)
    expect(previous.text()).toContain('deployment.rollbackUnavailable')
    expect(button('deployment.rollback')).toBeUndefined()
  })

  it('requires an explicit rollback action even when a rollback record exists', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, phase: 'EXPIRED', actions: [] }, rollback_job: { ...job, phase: 'EXPIRED', actions: [] } })
    expect(button('deployment.rollback')).toBeUndefined()
  })

  it('shows the actual prepared stage and the maintenance confirmation', async () => {
    await open({ enabled: true, candidates: [candidate], job })
    expect(wrapper.text()).toContain('deployment.phases.PREPARED')
    expect(wrapper.text()).not.toContain('deployment.phases.ACCEPTED')
    await button('deployment.activate').trigger('click')
    expect(wrapper.text()).toContain('deployment.activateConfirm')
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('uses the pause-switch-resume confirmation for a handoff installation', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, mode: 'handoff' } })
    await button('deployment.activate').trigger('click')
    expect(wrapper.text()).toContain('deployment.handoffConfirm')
    expect(wrapper.text()).not.toContain('deployment.activateConfirm')
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it.each(['ROLLED_BACK', 'NEEDS_ATTENTION', 'DEFERRED', 'RECOVERED'])('does not show a forward-success rail for %s', async phase => {
    await open({ enabled: true, candidates: [], job: { ...job, phase, actions: [] } })
    expect(wrapper.find('ol').exists()).toBe(false)
    expect(wrapper.text()).toContain(`deployment.phases.${phase}`)
  })

  it('shows live confirmation only for a verified live handoff', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, mode: 'handoff', live_handoff: true } })
    await button('deployment.activate').trigger('click')
    expect(wrapper.text()).toContain('deployment.liveHandoffConfirm')
    expect(wrapper.text()).not.toContain('deployment.handoffConfirm')
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('keeps polling and prevents another prepare while old requests finish', async () => {
    await open({ enabled: true, candidates: [{ ...candidate, id: 'release-2' }], job: {
      ...job, phase: 'ACCEPTED', actions: [], error_code: 'PREVIOUS_REQUESTS_PENDING'
    } })
    expect(wrapper.text()).toContain('deployment.errors.PREVIOUS_REQUESTS_PENDING')
    expect(button('deployment.prepare').attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(3100)
    expect(mocks.get.mock.calls.length).toBeGreaterThan(1)
    expect(mocks.action).not.toHaveBeenCalled()
  })


  it('prepares without activating, using the existing step-up flow', async () => {
    await open()
    mocks.action.mockResolvedValue({ enabled: true, candidates: [candidate], job: { ...job, phase: 'BUILDING', actions: [] } })
    await button('deployment.prepare').trigger('click')
    await flushPromises()
    expect(mocks.stepUp).toHaveBeenCalledOnce()
    expect(mocks.action.mock.calls).toEqual([['prepare', 'release-1']])
  })

  it('requires an explicit confirmation before activation', async () => {
    await open({ enabled: true, candidates: [], job })
    await button('deployment.activate').trigger('click')
    expect(mocks.action).not.toHaveBeenCalled()
    mocks.action.mockResolvedValue({ enabled: true, candidates: [], job: { ...job, phase: 'DRAINING', actions: [] } })
    await button('common.confirm').trigger('click')
    await flushPromises()
    expect(mocks.action.mock.calls).toEqual([['activate', 'job-1']])
  })

  it('queries persisted state on reconnect and never automatically resends a mutation', async () => {
    await open()
    mocks.action.mockRejectedValue(new Error('response lost'))
    mocks.get.mockRejectedValueOnce(new Error('maintenance'))
    await button('deployment.prepare').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('deployment.disconnected')
    mocks.get.mockResolvedValue({ enabled: true, candidates: [], job: { ...job, phase: 'ACCEPTED', actions: [] } })
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()
    expect(wrapper.text()).toContain('deployment.phases.ACCEPTED')
    expect(mocks.action).toHaveBeenCalledOnce()
  })

  it('keeps polling auxiliary recovery after the main deployment is accepted', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, phase: 'ACCEPTED', actions: [], error_code: 'AUXILIARY_SERVICES_PENDING' } })
    expect(wrapper.text()).toContain('deployment.errors.AUXILIARY_SERVICES_PENDING')
    expect(button('deployment.rollback')).toBeUndefined()
    mocks.get.mockResolvedValue({ enabled: true, candidates: [], job: { ...job, phase: 'ACCEPTED', actions: ['rollback'] } })
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()
    expect(wrapper.text()).not.toContain('deployment.errors.AUXILIARY_SERVICES_PENDING')
    expect(button('deployment.rollback')).toBeDefined()
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('does not offer normal preparation for a special update', async () => {
    await open({ enabled: true, candidates: [{ ...candidate, channel: 'special' }], job: null })
    expect(wrapper.text()).toContain('deployment.special')
    expect(button('deployment.prepare')).toBeUndefined()
  })

  it('removes stale confirmation when backup expiry is observed', async () => {
    await open({ enabled: true, candidates: [], job })
    await button('deployment.activate').trigger('click')
    mocks.get.mockResolvedValue({ enabled: true, candidates: [], job: { ...job, phase: 'EXPIRED', actions: [] } })
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()
    expect(button('common.confirm')).toBeUndefined()
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('closing the dialog stops polling without cancelling the server task', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, phase: 'BUILDING', actions: [] } })
    await wrapper.setProps({ show: false })
    const count = mocks.get.mock.calls.length
    await vi.advanceTimersByTimeAsync(9000)
    expect(mocks.get).toHaveBeenCalledTimes(count)
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('cancels confirmation when another administrator replaces the job', async () => {
    await open({ enabled: true, candidates: [], job })
    await button('deployment.activate').trigger('click')
    mocks.get.mockResolvedValue({ enabled: true, candidates: [], job: { ...job, id: 'job-2' } })
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()
    expect(button('common.confirm')).toBeUndefined()
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('closes confirmation before waiting for step-up authentication', async () => {
    await open({ enabled: true, candidates: [], job })
    mocks.stepUp.mockImplementation(() => new Promise(() => {}))
    await button('deployment.activate').trigger('click')
    await button('common.confirm').trigger('click')
    expect(button('common.confirm')).toBeUndefined()
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('keeps the accepted rollback available after a newer preparation fails', async () => {
    const rollback = { ...job, id: 'job-accepted', phase: 'ACCEPTED', actions: ['rollback'] as const }
    await open({ enabled: true, candidates: [], job: { ...job, phase: 'PREPARATION_FAILED', actions: [] }, rollback_job: { ...rollback, actions: ['rollback'] } })
    await button('deployment.rollback').trigger('click')
    expect(mocks.action).not.toHaveBeenCalled()
    mocks.action.mockResolvedValue({ enabled: true, candidates: [], job: { ...rollback, phase: 'DRAINING', actions: [] } })
    await button('common.confirm').trigger('click')
    await flushPromises()
    expect(mocks.action.mock.calls).toEqual([['rollback', 'job-accepted']])
  })

  it('polls and dismisses a saved rollback confirmation when its backup expires', async () => {
    const state: DeploymentSnapshot = { enabled: true, candidates: [], job: { ...job, phase: 'PREPARATION_FAILED', actions: [] }, rollback_job: { ...job, id: 'job-old', phase: 'ACCEPTED', actions: ['rollback'] } }
    await open(state)
    await button('deployment.rollback').trigger('click')
    mocks.get.mockResolvedValue({ ...state, rollback_job: null })
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()
    expect(button('common.confirm')).toBeUndefined()
    expect(button('deployment.rollback')).toBeUndefined()
    expect(mocks.action).not.toHaveBeenCalled()
  })

  it('dismisses rollback confirmation if the previous commit changes', async () => {
    await open({ enabled: true, candidates: [], job: { ...job, phase: 'ACCEPTED', actions: ['rollback'] } })
    await button('deployment.rollback').trigger('click')
    mocks.get.mockResolvedValue({ enabled: true, candidates: [], job: { ...job, phase: 'ACCEPTED', previous_source: 'c'.repeat(40), actions: ['rollback'] } })
    await vi.advanceTimersByTimeAsync(3000)
    expect(button('common.confirm')).toBeUndefined()
    expect(mocks.action).not.toHaveBeenCalled()
  })
})
