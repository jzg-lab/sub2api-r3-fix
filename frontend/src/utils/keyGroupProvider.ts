import type { Group } from '@/types'

export type KeyGroupProvider = 'anthropic' | 'openai' | 'cn' | 'other'

export function keyGroupProvider(group: Pick<Group, 'name' | 'platform'>): KeyGroupProvider {
  if (['kimi', 'zhipu', 'deepseek', 'minimax'].includes(group.platform)) return 'cn'
  // ponytail: legacy compatible groups named 国产 lack a display category; add one if naming stops being sufficient.
  if (group.platform === 'openai' && group.name.includes('国产')) return 'cn'
  if (group.platform === 'anthropic') return 'anthropic'
  if (group.platform === 'openai') return 'openai'
  return 'other'
}
