import type { Plugin } from 'vite'

interface Chunk {
  type: string
  isEntry?: boolean
  imports?: string[]
  dynamicImports?: string[]
  modules?: Record<string, unknown>
}

export function eagerPaymentSdks(bundle: Record<string, Chunk>): string[] {
  const pending = Object.keys(bundle).filter((key) => bundle[key].isEntry)
  const visited = new Set<string>()
  const found = new Set<string>()
  while (pending.length) {
    const key = pending.pop()!
    if (visited.has(key)) continue
    visited.add(key)
    const chunk = bundle[key]
    if (!chunk || chunk.type !== 'chunk') continue
    for (const id of Object.keys(chunk.modules || {})) {
      const normalized = id.replaceAll('\\', '/')
      if (normalized.includes('/@airwallex/components-sdk/')) found.add('airwallex')
      if (normalized.includes('/@stripe/stripe-js/')) found.add('stripe')
    }
    pending.push(...(chunk.imports || []))
  }
  return [...found].sort()
}

export function lazyPaymentChunkGuard(): Plugin {
  return {
    name: 'lazy-payment-chunk-guard',
    apply: 'build',
    generateBundle(_options, bundle) {
      const eager = eagerPaymentSdks(bundle)
      if (eager.length) this.error(`Payment SDK in an eager entry dependency: ${eager.join(', ')}`)
    }
  }
}
