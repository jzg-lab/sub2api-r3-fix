import { onScopeDispose, ref, watch } from 'vue'
import type { Account } from '@/types'

export interface ReauthOperation {
  account: Account
  expectedUpdatedAt: string
}

export function useReauthSession(props: { show: boolean; account: Account | null }) {
  const busy = ref(false)
  const generation = ref(0)
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
    [() => props.show, () => props.account?.id, () => props.account?.platform, () => props.account?.proxy_id],
    () => {
      invalidate()
      open = props.show && !!props.account
      expectedUpdatedAt = props.account?.updated_at || ''
    },
    { immediate: true, flush: 'sync' }
  )
  onScopeDispose(invalidate)

  const isCurrent = (operation: ReauthOperation) =>
    open && props.show && active === operation &&
    props.account?.id === operation.account.id &&
    props.account?.platform === operation.account.platform &&
    props.account?.proxy_id === operation.account.proxy_id

  const run = async (action: (operation: ReauthOperation) => Promise<void>) => {
    if (!open || !props.show || !props.account || busy.value) return
    const operation = { account: { ...props.account }, expectedUpdatedAt }
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

  return { busy, generation, invalidate, isCurrent, run }
}
