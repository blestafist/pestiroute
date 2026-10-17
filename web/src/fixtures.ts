export type Capability = 'supported' | 'unsupported' | 'unknown'

export interface FixtureData {
  overview: { enabledAccounts: number | null; recentRequests: number | null }
  connections: { name: string; capability: Capability }[]
  usage: { model: string; inputTokens: number | null; outputTokens: number | null }[]
}

// UI-local synthetic data, not an HTTP/API contract.
export const fixtureData: FixtureData = {
  overview: { enabledAccounts: 2, recentRequests: null },
  connections: [{ name: 'Example connection', capability: 'supported' }, { name: 'Unsupported example', capability: 'unsupported' }, { name: 'Unknown connection', capability: 'unknown' }],
  usage: [{ model: 'example-model', inputTokens: 120, outputTokens: null }],
}

export type SimulatedState = 'ready' | 'loading' | 'error' | 'conflict'
export type FixtureState<T> = { status: 'loading' } | { status: 'error'; message: string } | { status: 'conflict'; currentRevision: number } | { status: 'success'; data: T }

export const fixtureAdapter = {
  read<T>(data: T, state: SimulatedState): FixtureState<T> {
    switch (state) {
      case 'loading': return { status: 'loading' }
      case 'error': return { status: 'error', message: 'Synthetic fixture unavailable. Choose Ready to retry.' }
      case 'conflict': return { status: 'conflict', currentRevision: 2 }
      case 'ready': return { status: 'success', data }
    }
  },
}
export function setSessionToken(token: string | null): void { sessionToken = token }
export function getSessionToken(): string | null { return sessionToken }
let sessionToken: string | null = null
export function clearSession(): void { sessionToken = null }
