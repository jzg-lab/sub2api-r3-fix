import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { extractI18nErrorMessage } from '@/utils/apiError'

export interface OpenAITokenInfo {
  initial_authorization_proof?: string
  reauthorization_proof?: string
  proxy_id?: number
  access_token?: string
  refresh_token?: string
  client_id?: string
  id_token?: string
  token_type?: string
  expires_in?: number
  expires_at?: number
  scope?: string
  email?: string
  name?: string
  plan_type?: string
  subscription_expires_at?: string
  privacy_mode?: string
  // OpenAI specific IDs (extracted from ID Token)
  chatgpt_account_id?: string
  chatgpt_user_id?: string
  organization_id?: string
  [key: string]: unknown
}

export type OpenAIOAuthPlatform = 'openai'

export function useOpenAIOAuth() {
  const appStore = useAppStore()
  const { t } = useI18n()
  const endpointPrefix = '/admin/openai'

  // State
  const authUrl = ref('')
  const sessionId = ref('')
  const oauthState = ref('')
  const authorizationProxyId = ref<number | null>(null)
  const loading = ref(false)
  const error = ref('')
  let requestVersion = 0
  const proxyRequiredMessage = () =>
    t('admin.accounts.oauth.openai.errors.OPENAI_OAUTH_PROXY_REQUIRED')
  const validProxyId = (proxyId?: number | null): proxyId is number =>
    Number.isSafeInteger(proxyId) && (proxyId ?? 0) > 0
  const rejectProxyContinuity = () => {
    error.value = proxyRequiredMessage()
    appStore.showError(error.value)
  }
  const clearAuthorizationSession = () => {
    authUrl.value = ''
    sessionId.value = ''
    oauthState.value = ''
    authorizationProxyId.value = null
  }

  // Reset state
  const resetState = () => {
    requestVersion++
    clearAuthorizationSession()
    loading.value = false
    error.value = ''
  }

  // Generate auth URL for OpenAI OAuth
  const generateAuthUrl = async (
    proxyId?: number | null,
    redirectUri?: string,
    reauthorization?: { accountId: number; expectedUpdatedAt: string }
  ): Promise<boolean> => {
    const version = ++requestVersion
    clearAuthorizationSession()
    error.value = ''
    if (!validProxyId(proxyId)) {
      loading.value = false
      rejectProxyContinuity()
      return false
    }
    loading.value = true

    try {
      const payload: Record<string, unknown> = { proxy_id: proxyId }
      if (redirectUri) {
        payload.redirect_uri = redirectUri
      }
      if (reauthorization) {
        payload.account_id = reauthorization.accountId
        payload.expected_updated_at = reauthorization.expectedUpdatedAt
      }

      const response = await adminAPI.accounts.generateAuthUrl(
        `${endpointPrefix}/generate-auth-url`,
        payload
      )
      if (version !== requestVersion) return false
      authUrl.value = response.auth_url
      sessionId.value = response.session_id
      authorizationProxyId.value = proxyId
      try {
        const parsed = new URL(response.auth_url)
        oauthState.value = parsed.searchParams.get('state') || ''
      } catch {
        oauthState.value = ''
      }
      return true
    } catch (err: any) {
      if (version !== requestVersion) return false
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.oauth.openai.errors',
        t('admin.accounts.oauth.openai.failedToGenerateUrl')
      )
      appStore.showError(error.value)
      return false
    } finally {
      if (version === requestVersion) loading.value = false
    }
  }

  // Exchange auth code for tokens
  const exchangeAuthCode = async (
    code: string,
    currentSessionId: string,
    state: string,
    proxyId?: number | null
  ): Promise<OpenAITokenInfo | null> => {
    if (loading.value) return null
    const version = ++requestVersion
    if (!code.trim() || !currentSessionId || !state.trim()) {
      loading.value = false
      error.value = 'Missing auth code, session ID, or state'
      return null
    }
    const boundProxyId = authorizationProxyId.value
    if (
      currentSessionId !== sessionId.value ||
      !validProxyId(boundProxyId) ||
      !validProxyId(proxyId) ||
      proxyId !== boundProxyId
    ) {
      loading.value = false
      rejectProxyContinuity()
      return null
    }

    loading.value = true
    error.value = ''

    try {
      const payload: { session_id: string; code: string; state: string; proxy_id?: number } = {
        session_id: currentSessionId,
        code: code.trim(),
        state: state.trim()
      }
      payload.proxy_id = boundProxyId

      const tokenInfo = await adminAPI.accounts.exchangeCode(`${endpointPrefix}/exchange-code`, payload)
      if (version !== requestVersion) return null
      const result = tokenInfo as OpenAITokenInfo
      if (!Number.isSafeInteger(result.proxy_id) || (result.proxy_id ?? 0) <= 0) {
        throw new Error('Authorization response is missing its proxy assignment')
      }
      if (result.proxy_id !== boundProxyId) {
        throw new Error('Authorization response proxy does not match the authorization session')
      }
      return result
    } catch (err: any) {
      if (version !== requestVersion) return null
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.oauth.openai.errors',
        t('admin.accounts.oauth.openai.failedToExchangeCode')
      )
      appStore.showError(error.value)
      return null
    } finally {
      if (version === requestVersion) {
        // The server consumes the session before exchange. Even a lost response
        // is not permission to replay the old code or reuse its browser proof.
        clearAuthorizationSession()
        loading.value = false
      }
    }
  }

  // Validate refresh token and get full token info
  // clientId: 指定 OAuth client_id（用于第三方渠道获取的 RT，如 app_LlGpXReQgckcGGUo2JrYvtJK）
  const validateRefreshToken = async (
    refreshToken: string,
    proxyId?: number | null,
    clientId?: string
  ): Promise<OpenAITokenInfo | null> => {
    const version = ++requestVersion
    if (!refreshToken.trim()) {
      loading.value = false
      error.value = 'Missing refresh token'
      return null
    }

    loading.value = true
    error.value = ''

    try {
      // Use dedicated refresh-token endpoint
      const tokenInfo = await adminAPI.accounts.refreshOpenAIToken(
        refreshToken.trim(),
        proxyId,
        `${endpointPrefix}/refresh-token`,
        clientId
      )
      if (version !== requestVersion) return null
      return tokenInfo as OpenAITokenInfo
    } catch (err: any) {
      if (version !== requestVersion) return null
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.oauth.openai.errors',
        t('admin.accounts.oauth.openai.failedToValidateRT')
      )
      appStore.showError(error.value)
      return null
    } finally {
      if (version === requestVersion) loading.value = false
    }
  }

  // Build credentials for OpenAI OAuth account (aligned with backend BuildAccountCredentials)
  const buildCredentials = (tokenInfo: OpenAITokenInfo): Record<string, unknown> => {
    const creds: Record<string, unknown> = {
      access_token: tokenInfo.access_token,
      expires_at: tokenInfo.expires_at
    }

    // 仅在返回了新的 refresh_token 时才写入，防止用空值覆盖已有令牌
    if (tokenInfo.refresh_token) {
      creds.refresh_token = tokenInfo.refresh_token
    }
    if (tokenInfo.id_token) {
      creds.id_token = tokenInfo.id_token
    }
    if (tokenInfo.email) {
      creds.email = tokenInfo.email
    }
    if (tokenInfo.chatgpt_account_id) {
      creds.chatgpt_account_id = tokenInfo.chatgpt_account_id
    }
    if (tokenInfo.chatgpt_user_id) {
      creds.chatgpt_user_id = tokenInfo.chatgpt_user_id
    }
    if (tokenInfo.organization_id) {
      creds.organization_id = tokenInfo.organization_id
    }
    if (tokenInfo.plan_type) {
      creds.plan_type = tokenInfo.plan_type
    }
    if (tokenInfo.subscription_expires_at) {
      creds.subscription_expires_at = tokenInfo.subscription_expires_at
    }
    if (tokenInfo.client_id) {
      creds.client_id = tokenInfo.client_id
    }

    return creds
  }

  // Build extra info from token response
  const buildExtraInfo = (tokenInfo: OpenAITokenInfo): Record<string, string> | undefined => {
    const extra: Record<string, string> = {}
    if (tokenInfo.email) {
      extra.email = tokenInfo.email
    }
    if (tokenInfo.name) {
      extra.name = tokenInfo.name
    }
    if (tokenInfo.privacy_mode) {
      extra.privacy_mode = tokenInfo.privacy_mode
    }
    return Object.keys(extra).length > 0 ? extra : undefined
  }

  return {
    // State
    authUrl,
    sessionId,
    oauthState,
    authorizationProxyId,
    loading,
    error,
    // Methods
    resetState,
    generateAuthUrl,
    exchangeAuthCode,
    validateRefreshToken,
    buildCredentials,
    buildExtraInfo
  }
}
