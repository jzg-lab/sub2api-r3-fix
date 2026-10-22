import { createServer } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath } from 'node:url'

// A separate dev server: no production config, proxy, credentials or executor.
const root = fileURLToPath(new URL('../', import.meta.url))
const candidates = [
  { id: 'preview-2', version: 'v0.2.17+r17.2', source: 'b'.repeat(40), channel: 'ordinary', summary: '模型广场展示修复。无迁移、插件或运行环境变更。' },
  { id: 'preview-3', version: 'v0.2.17+r17.3', source: 'c'.repeat(40), channel: 'special', summary: '数据库结构调整。', reason: '涉及数据库迁移，需专项审核，不能直接上线。' }
]
let job, accepted, started, phases, scenario, grantUntil, counts, sequence
function reset(value = 'normal') {
  scenario = value
  job = null
  accepted = null
  started = 0
  phases = []
  grantUntil = 0
  sequence = 0
  counts = { prepare: 0, activate: 0, rollback: 0 }
  if (scenario === 'previous-version') {
    job = { ...candidates[0], mode: 'handoff', id: 'preview-job-' + (++sequence), candidate_id: candidates[0].id,
      previous_source: 'a'.repeat(40), previous_version: 'v0.2.17+r17.1', phase: 'ACCEPTED', expires_at: Date.now() / 1000 + 7200, actions: ['rollback'] }
  }
}
reset()
function snapshot() {
  if (phases.length) {
    const index = Math.min(Math.floor((Date.now() - started) / 2200), phases.length - 1)
    job.phase = phases[index]
    job.actions = []
    if (index === phases.length - 1) {
      phases = []
      job.actions = job.phase === 'PREPARED' ? ['activate'] : job.phase === 'ACCEPTED' ? ['rollback'] : []
      if (job.phase === 'PREPARED') job.expires_at = Date.now() / 1000 + 7200
      if (job.phase === 'ACCEPTED') accepted = structuredClone(job)
      if (job.phase === 'ROLLED_BACK') accepted = null
      if (job.phase === 'PREPARATION_FAILED') job.error_code = 'VERIFYING_FAILED'
      if (job.phase === 'EXPIRED') job.expires_at = Date.now() / 1000 - 1
    }
  }
  return { enabled: true, candidates, job, rollback_job: phases.length ? null : job?.actions.includes('rollback') ? job : accepted }
}
function start(steps) { phases = steps; started = Date.now(); job.actions = []; job.phase = phases[0] }
const server = await createServer({
  root, configFile: false, envFile: false,
  define: { 'import.meta.env.VITE_API_BASE_URL': JSON.stringify('/api/v1') },
  resolve: { alias: { '@': root + 'src' } },
  server: { host: '127.0.0.1', port: 5182, strictPort: false },
  plugins: [vue(), {
    name: 'local-deployment-simulation',
    configureServer(vite) {
      vite.middlewares.use(async (req, res, next) => {
        const path = new URL(req.url, 'http://localhost').pathname
        if (path === '/') { res.writeHead(302, { Location: '/dev/deployment-preview.html' }); res.end(); return }
        if (!path.startsWith('/api/') && !path.startsWith('/__preview/')) return next()
        const reply = (data, status = 200) => { res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(data)) }
        try {
          let raw = ''
          for await (const chunk of req) { raw += chunk; if (raw.length > 1024) return reply({ message: 'Request too large' }, 413) }
          const body = raw ? JSON.parse(raw) : {}
          if (req.method === 'POST' && path === '/__preview/reset' && ['normal', 'failed', 'expired', 'auto-rollback', 'long-session', 'previous-version'].includes(body.scenario)) {
            reset(body.scenario); return reply({ code: 0, data: snapshot() })
          }
          if (req.method === 'GET' && path === '/__preview/counts') return reply(counts)
          if (req.method === 'GET' && path === '/api/v1/admin/system/deployment') return reply({ code: 0, data: snapshot() })
          if (req.method === 'POST' && path === '/api/v1/user/totp/step-up') {
            if (body.code !== '123456') return reply({ message: '本地模拟验证码为 123456' }, 400)
            grantUntil = Date.now() + 120000
            return reply({ code: 0, data: { expires_at: grantUntil / 1000 } })
          }
          const action = path.match(/^\/api\/v1\/admin\/system\/deployment\/(prepare|activate|rollback)$/)?.[1]
          if (req.method !== 'POST' || !action) return reply({ message: 'Local preview has no such API' }, 404)
          if (Date.now() >= grantUntil) return reply({ code: 'STEP_UP_REQUIRED' }, 403)
          const state = snapshot()
          if (phases.length && !(action === 'rollback' && state.rollback_job?.id === body.id)) return reply({ code: 'DEPLOYMENT_CONFLICT' }, 409)
          if (action === 'prepare' && body.id === candidates[0].id) {
            job = { ...candidates[0], mode: 'handoff', id: 'preview-job-' + (++sequence), candidate_id: body.id, previous_source: 'a'.repeat(40), previous_version: 'v0.2.17+r17.1', expires_at: 0, actions: [] }
            start(['QUEUED', 'BUILDING', 'VERIFYING', 'BACKUP', scenario === 'failed' ? 'PREPARATION_FAILED' : scenario === 'expired' ? 'EXPIRED' : 'PREPARED'])
          } else if (action === 'activate' && body.id === job?.id && job.actions.includes('activate')) {
            start(scenario === 'auto-rollback'
              ? ['STAGING', 'PAUSING', 'DRAINING', 'SWITCHING', 'RESUMING', 'RECOVERING', 'RECOVERED']
              : scenario === 'long-session'
                ? ['STAGING', 'PAUSING', 'DRAINING', 'DEFERRED']
                : ['STAGING', 'PAUSING', 'DRAINING', 'SWITCHING', 'RESUMING', 'PUBLIC_VERIFY', 'ACCEPTED'])
          } else if (action === 'rollback' && body.id === state.rollback_job?.id) {
            job = structuredClone(state.rollback_job)
            start(['STAGING', 'PAUSING', 'DRAINING', 'SWITCHING', 'RESUMING', 'PUBLIC_VERIFY', 'ROLLED_BACK'])
          } else return reply({ code: 'DEPLOYMENT_CONFLICT' }, 409)
          counts[action]++
          return reply({ code: 0, data: snapshot() })
        } catch { reply({ message: 'Invalid preview request' }, 400) }
      })
    }
  }]
})
await server.listen()
server.printUrls()
