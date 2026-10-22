import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import VersionBadge from '@/components/common/VersionBadge.vue'

const mocks = vi.hoisted(() => ({
  app: {
    versionLoading: false,
    versionLoaded: true,
    currentVersion: '0.2.17+r17.1',
    latestVersion: '0.2.17+r17.1',
    hasUpdate: false,
    versionWarning: '',
    buildType: 'release',
    releaseInfo: null,
    fetchVersion: vi.fn(),
    clearVersionCache: vi.fn()
  },
  auth: { isAdmin: true },
  getRollbackVersions: vi.fn(),
  performUpdate: vi.fn(),
  rollback: vi.fn(),
  restartService: vi.fn(),
  getDeployment: vi.fn()
}))

vi.mock('@/stores', () => ({
  useAppStore: () => mocks.app,
  useAuthStore: () => mocks.auth
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/system', () => ({
  getRollbackVersions: mocks.getRollbackVersions,
  performUpdate: mocks.performUpdate,
  rollback: mocks.rollback,
  restartService: mocks.restartService
}))
vi.mock('@/api/admin/deployment', () => ({ getDeployment: mocks.getDeployment }))
vi.mock('@/components/common/DeploymentDialog.vue', () => ({ default: { template: '<div data-testid="deployment-dialog" />' } }))

const guideURL = 'https://github.com/jzg-lab/sub2api-r3-fix/blob/main/docs/merge-deploy-guide.md'
let wrapper: VueWrapper

async function openBadge() {
  wrapper = mount(VersionBadge, { global: { stubs: { Icon: true } } })
  await wrapper.get('button').trigger('click')
  await flushPromises()
}

beforeEach(() => {
  vi.clearAllMocks()
  mocks.app.hasUpdate = false
  mocks.app.versionWarning = ''
  mocks.app.buildType = 'release'
  mocks.auth.isAdmin = true
  mocks.getDeployment.mockResolvedValue({ enabled: false, candidates: [] })
})

afterEach(() => wrapper?.unmount())

describe('VersionBadge update boundaries', () => {
  it('opens the managed deployment dialog instead of legacy mutation controls', async () => {
    mocks.getDeployment.mockResolvedValue({ enabled: true, candidates: [] })
    await openBadge()
    expect(wrapper.find('[data-testid="deployment-dialog"]').exists()).toBe(true)
    expect(mocks.performUpdate).not.toHaveBeenCalled()
    expect(mocks.getRollbackVersions).not.toHaveBeenCalled()
  })

  it('does not fall back to binary updates when the deployment executor is unavailable', async () => {
    mocks.getDeployment.mockRejectedValue(new Error('offline'))
    await openBadge()
    expect(wrapper.find('[data-testid="deployment-dialog"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('version.updateNow')
  })

  it('does not claim latest or offer updates when version checking is uncertain', async () => {
    mocks.app.versionWarning = 'Release identity requires verification'
    mocks.app.hasUpdate = true
    await openBadge()
    expect(wrapper.text()).toContain(mocks.app.versionWarning)
    expect(wrapper.text()).not.toContain('version.upToDate')
    expect(wrapper.text()).not.toContain('version.updateNow')
    expect(wrapper.get('a').attributes('href')).toBe(guideURL)
  })

  it('points source builds to the project deployment guide without mutation actions', async () => {
    mocks.app.buildType = 'source'
    mocks.app.hasUpdate = true
    await openBadge()
    expect(wrapper.text()).not.toContain('version.updateNow')
    expect(wrapper.get('a').attributes('href')).toBe(guideURL)
    expect(mocks.performUpdate).not.toHaveBeenCalled()
    expect(mocks.getRollbackVersions).not.toHaveBeenCalled()
  })

  it('does not generate official install or Docker commands for project rollbacks', async () => {
    mocks.getRollbackVersions.mockResolvedValue({
      versions: [{ version: '0.2.16', published_at: '', html_url: '' }]
    })
    await openBadge()
    await wrapper.findAll('button').find(button => button.text() === 'version.rollback')!.trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text().includes('v0.2.16'))!.trigger('click')
    expect(wrapper.find('code').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('Wei-Shaw')
    expect(wrapper.html()).not.toContain('weishaw')
    expect(wrapper.get('a').attributes('href')).toBe(guideURL)
  })
})
