export type SuggestionStatus = 'pending' | 'accepted' | 'rejected'
export type StatusFilter = SuggestionStatus | 'all'
export type Signal = 'correction' | 'rediscovery' | 'failure_fix' | 'convention' | 'workflow'
export type Scope = 'machine' | 'repo'
export type Placement = 'always_on' | 'path_scoped' | 'skill'
export type DecisionAction = 'accept' | 'reject'

export interface Evidence {
  excerpt: string
  sessionId: string
  tool: 'cursor' | 'claude'
}

export interface Suggestion {
  id: string
  createdAt: string
  status: SuggestionStatus
  title: string
  signal: Signal
  scope: Scope
  placement: Placement
  sensitivity: boolean
  confidence: number
  project: string
  repoRoot: string
  targetPath: string
  body: string
  rationale: string
  evidence: Evidence[]
}

export interface Stats {
  pending: number
  accepted: number
  rejected: number
  sessions: number
  projects: number
}

export interface Project {
  name: string
  repoRoot: string
  pending: number
}

export interface DecisionResponse {
  ok: boolean
  writtenPath?: string
}

const CAPABILITY_HEADER = 'X-Temper-Capability'

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, init)
  if (!res.ok) {
    throw new Error(`${init?.method ?? 'GET'} ${path} failed: ${res.status}`)
  }
  return res.json() as Promise<T>
}

// The capability authorizes a decision. It belongs to the running temper process, so it is
// fetched from it and kept in memory only — persisting it would outlive the process that issued it.
let capability: Promise<string> | null = null

function getCapability(): Promise<string> {
  if (!capability) {
    capability = request<{ capability: string }>('/api/capability')
      .then(({ capability: token }) => token)
      .catch((err: unknown) => {
        capability = null // a failed bootstrap must not poison every later decision
        throw err
      })
  }
  return capability
}

export function getStats(): Promise<Stats> {
  return request<Stats>('/api/stats')
}

export function getSuggestions(status: StatusFilter): Promise<{ suggestions: Suggestion[] }> {
  return request<{ suggestions: Suggestion[] }>(`/api/suggestions?status=${status}`)
}

export function getProjects(): Promise<{ projects: Project[] }> {
  return request<{ projects: Project[] }>('/api/projects')
}

export async function postDecision(
  id: string,
  action: DecisionAction,
  body?: string,
): Promise<DecisionResponse> {
  const token = await getCapability()
  return request<DecisionResponse>(`/api/suggestions/${id}/decision`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', [CAPABILITY_HEADER]: token },
    body: JSON.stringify(body !== undefined ? { action, body } : { action }),
  })
}
