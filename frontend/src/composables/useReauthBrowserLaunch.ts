import { computed, onScopeDispose, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import { extractApiErrorCode, extractI18nErrorMessage } from '@/utils/apiError'
import type { useOpenAIOAuth } from './useOpenAIOAuth'
import type { useReauthSession } from './useReauthSession'

const recoverableReasons = new Set([
  'AUTH_BROWSER_SESSION_NOT_FOUND',
  'AUTH_BROWSER_SESSION_EXPIRED',
  'AUTH_BROWSER_SESSION_INVALID',
  'AUTH_BROWSER_PROXY_ROUTE_STALE'
])

export function useReauthBrowserLaunch(
  session: ReturnType<typeof useReauthSession>,
  oauth: Pick<ReturnType<typeof useOpenAIOAuth>, 'sessionId' | 'generateAuthUrl'>,
  clearCallback: () => void
) {
  const appStore = useAppStore()
  const { t } = useI18n()
  const launching = ref(false)
  const confirmedSessionId = ref('')
  const ready = computed(() =>
    !!confirmedSessionId.value && confirmedSessionId.value === oauth.sessionId.value
  )
  const invalidate = () => { confirmedSessionId.value = '' }
  watch([oauth.sessionId, session.generation], invalidate, { flush: 'sync' })
  watch(session.busy, (busy) => {
    if (!busy) launching.value = false
  }, { flush: 'sync' })
  onScopeDispose(() => {
    launching.value = false
    invalidate()
  })

  const launch = () => session.run(async (operation) => {
    if (operation.account.platform !== 'openai') return
    let sessionId = oauth.sessionId.value
    if (!sessionId) {
      appStore.showError('授权会话缺失，请先重新生成授权链接')
      return
    }
    invalidate()
    clearCallback()
    launching.value = true
    let refreshed = false
    try {
      while (session.isCurrent(operation)) {
        try {
          const result = await adminAPI.accounts.launchAuthBrowser(sessionId)
          if (!session.isCurrent(operation) || oauth.sessionId.value !== sessionId) return
          if (result.already_running) {
            appStore.showSuccess('激活浏览器正在启动，请勿重复点击')
          } else if (result.launched === true) {
            confirmedSessionId.value = sessionId
            appStore.showSuccess(`激活浏览器已启动（${result.proxy_name}），请在弹出的窗口完成登录`)
          } else {
            appStore.showError(`激活浏览器启动失败：${result.output || '未知原因'}`)
          }
          return
        } catch (error: unknown) {
          if (!session.isCurrent(operation) || oauth.sessionId.value !== sessionId) return
          const reason = extractApiErrorCode(error)
          if (!refreshed && reason && recoverableReasons.has(reason)) {
            refreshed = true
            const generated = await oauth.generateAuthUrl(operation.account.proxy_id, undefined, {
              accountId: operation.account.id,
              expectedUpdatedAt: operation.expectedUpdatedAt,
              expectedAuthorizationRevision: operation.account.reauthorization_revision
            })
            if (!generated || !session.isCurrent(operation)) return
            sessionId = oauth.sessionId.value
            if (sessionId) continue
          }
          appStore.showError(`弹出激活浏览器失败：${extractI18nErrorMessage(
            error, t, 'admin.accounts.oauth.openai.errors', '未知原因'
          )}`)
          return
        }
      }
    } finally {
      if (session.isCurrent(operation)) launching.value = false
    }
  })

  return { launching, launch, ready, invalidate }
}
