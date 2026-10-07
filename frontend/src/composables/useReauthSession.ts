import { onScopeDispose, ref, shallowRef, watch } from 'vue'
import type { Account } from '@/types'

export interface ReauthOperation {
  account: Account
  expectedUpdatedAt: string
}

export function useReauthSession(props: { show: boolean; account: Account | null }) {
  const busy = ref(false)
  const generation = ref(0)
  const account = shallowRef<Account | null>(null)
  let active: ReauthOperation | undefined
  let open = false
  let expectedUpdatedAt = ''

  const invalidate = () => {
    generation.value++
    active = undefined
    busy.value = false
    open = false
  }

  watch(
    [() => props.show, () => props.account?.id, () => props.account?.platform, () => props.account?.type, () => props.account?.proxy_id],
    () => {
      invalidate()
      open = props.show && !!props.account
      account.value = props.account ? { ...props.account } : null
      expectedUpdatedAt = props.account?.updated_at || ''
    },
    { immediate: true, flush: 'sync' }
  )
  onScopeDispose(invalidate)

  const isCurrent = (operation: ReauthOperation) =>
    open && props.show && active === operation &&
    props.account?.id === operation.account.id &&
    props.account?.platform === operation.account.platform &&
    props.account?.type === operation.account.type &&
    props.account?.proxy_id === operation.account.proxy_id

  // Only a newly generated authorization may adopt a newer account revision.
  const refreshForNewAuthorization = async (
    operation: ReauthOperation,
    load: (id: number) => Promise<Account>
  ) => {
    if (!isCurrent(operation)) return false
    const fresh = await load(operation.account.id)
    if (!isCurrent(operation)) return false
    if (!fresh || fresh.id !== operation.account.id ||
        fresh.platform !== operation.account.platform || fresh.type !== operation.account.type ||
        fresh.proxy_id !== operation.account.proxy_id || !fresh.updated_at) {
      throw { reason: 'OAUTH_REAUTH_STALE_ACCOUNT' }
    }
    account.value = { ...fresh }
    expectedUpdatedAt = fresh.updated_at
    operation.account = account.value
    operation.expectedUpdatedAt = expectedUpdatedAt
    return true
  }

  const run = async (action: (operation: ReauthOperation) => Promise<void>) => {
    if (!open || !props.show || !account.value || busy.value) return
    const operation = { account: { ...account.value }, expectedUpdatedAt }
    active = operation
    busy.value = true
    try {
      await action(operation)
    } finally {
      if (active === operation) {
        active = undefined
        busy.value = false
      }
    }
  }

  return { busy, generation, account, invalidate, isCurrent, run, refreshForNewAuthorization }
}
