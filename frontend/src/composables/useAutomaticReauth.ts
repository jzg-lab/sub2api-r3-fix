import { onScopeDispose, ref, watch } from 'vue'
import { adminAPI } from '@/api/admin'
import type { AuthBrowserLogin } from '@/api/admin/accounts'
import type { OpenAITokenInfo, useOpenAIOAuth } from './useOpenAIOAuth'
import type { ReauthOperation, useReauthSession } from './useReauthSession'

export function useAutomaticReauth(
  session: ReturnType<typeof useReauthSession>,
  oauth: ReturnType<typeof useOpenAIOAuth>,
  apply: (operation: ReauthOperation, info: OpenAITokenInfo) => Promise<void>
) {
  const running = ref(false)
  const error = ref('')
  let controller: AbortController | undefined
  let activeLogin: AuthBrowserLogin | undefined
  const clearLogin = (login?: AuthBrowserLogin) => {
    if (!login) return
    login.email = login.password = login.totp_secret = ''
    delete login.use_stored_totp
  }
  const cancel = () => {
    if (controller) oauth.resetState()
    controller?.abort()
    controller = undefined
    clearLogin(activeLogin)
    activeLogin = undefined
    running.value = false
  }
  watch(session.generation, () => { cancel(); error.value = '' }, { flush: 'sync' })
  onScopeDispose(cancel)
  const start = async (login: AuthBrowserLogin) => {
    try {
      await session.run(async (operation) => {
        if (operation.account.platform !== 'openai' || operation.account.type !== 'oauth') return
        error.value = ''
        if (location.protocol !== 'https:' && !['localhost', '127.0.0.1', '[::1]'].includes(location.hostname)) {
          error.value = '账号密码授权只允许 HTTPS 或本机回环地址'
          return
        }
        const abort = new AbortController()
        controller = abort
        activeLogin = login
        running.value = true
        try {
          if (!await session.refreshForNewAuthorization(operation, adminAPI.accounts.getById)) return
          if (!session.isCurrent(operation) || abort.signal.aborted) return
          oauth.resetState()
          const generated = await oauth.generateAuthUrl(operation.account.proxy_id, undefined, {
            accountId: operation.account.id,
            expectedUpdatedAt: operation.expectedUpdatedAt,
            expectedAuthorizationRevision: operation.account.reauthorization_revision
          })
          if (!generated || !session.isCurrent(operation) || abort.signal.aborted) return
          const sessionID = oauth.sessionId.value
          const result = await adminAPI.accounts.automateAuthBrowser(sessionID, login, abort.signal)
          if (!session.isCurrent(operation) || abort.signal.aborted || sessionID !== oauth.sessionId.value) return
          if (!result.launched || !result.code || result.state !== oauth.oauthState.value) {
            throw new Error('授权未完成，未覆盖账号')
          }
          const info = await oauth.exchangeAuthCode(result.code, sessionID, result.state, operation.account.proxy_id)
          result.code = undefined
          if (!info || !session.isCurrent(operation) || abort.signal.aborted) return
          if (!info.reauthorization_proof) throw new Error('授权证明缺失，未覆盖账号')
          // A committed account update cannot be undone by canceling the browser.
          running.value = false
          await apply(operation, info)
        } catch {
          if (session.isCurrent(operation) && !abort.signal.aborted) {
            // Never display an arbitrary error which may retain login data.
            error.value = '自动授权未完成，账号未确认更新。请核对账号状态，或使用下方手动授权。'
          }
        } finally {
          if (controller === abort) {
            controller = undefined
            activeLogin = undefined
            running.value = false
          }
        }
      })
    } finally {
      clearLogin(login)
      login.password = login.totp_secret = ''
    }
  }
  return { start, cancel, running, error }
}
