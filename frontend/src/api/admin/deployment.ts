import { apiClient } from '../client'

export interface DeploymentCandidate {
  id: string
  version: string
  source: string
  summary: string
  channel: 'ordinary' | 'special'
  reason?: string
}

export interface DeploymentJob {
  mode?: 'single' | 'handoff'
  id: string
  candidate_id: string
  version: string
  source: string
  phase: string
  previous_source?: string
  previous_version?: string
  expires_at?: number
  actions: Array<'activate' | 'rollback'>
  error_code?: string
  reused?: boolean
}

export interface DeploymentSnapshot {
  enabled: boolean
  catalog_error?: string
  candidates: DeploymentCandidate[]
  job?: DeploymentJob | null
  rollback_job?: DeploymentJob | null
}

export async function getDeployment(force = false): Promise<DeploymentSnapshot> {
  const { data } = await apiClient.get<DeploymentSnapshot>('/admin/system/deployment', { params: force ? { force: 'true' } : undefined })
  return data
}

export async function deploymentAction(action: 'prepare' | 'activate' | 'rollback', id: string): Promise<DeploymentSnapshot> {
  const { data } = await apiClient.post<DeploymentSnapshot>(`/admin/system/deployment/${action}`, { id })
  return data
}
