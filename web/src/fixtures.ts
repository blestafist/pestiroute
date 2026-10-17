export type Capability = 'supported' | 'unsupported' | 'unknown'
export interface Account { id: string; connector: string; enabled: boolean; models: string[]; capabilities: Record<string, Capability>; expires: string; revision: number; auth: string[]; authStatus: string }
export interface VirtualKey { id: string; label: string; status: 'active' | 'revoked'; policy: string; revision: number; models: string[]; targets: string[]; rateLimit: number; created: string; revoked?: string }
export interface FixtureData { overview: { enabledAccounts: number | null; recentRequests: number | null }; connections: Account[]; keys: VirtualKey[]; policies: { id: string; revision: number; models: string[]; targets: string[]; rateLimit: number }[]; usage: { model: string; inputTokens: number | null; outputTokens: number | null }[] }
export const fixtureData: FixtureData = {
  overview: { enabledAccounts: 2, recentRequests: null },
  connections: [
    { id: 'example', connector: 'OpenAI-compatible', enabled: true, models: ['demo-model'], capabilities: { streaming: 'supported', tools: 'unsupported', reasoning: 'unknown' }, expires: 'Never', revision: 1, auth: ['OAuth', 'Manual'], authStatus: 'Not connected' },
  ],
  keys: [{ id: 'key-demo', label: 'Demo key', status: 'active', policy: 'restricted-demo', revision: 1, models: ['demo-model'], targets: ['example'], rateLimit: 60, created: '2026-01-01' }],
  policies: [{ id: 'restricted-demo', revision: 1, models: ['demo-model'], targets: ['example'], rateLimit: 60 }],
  usage: [{ model: 'example-model', inputTokens: 120, outputTokens: null }],
}
export type SimulatedState = 'ready' | 'loading' | 'error' | 'conflict'
export type FixtureState<T> = { status: 'loading' } | { status: 'error'; message: string } | { status: 'conflict'; currentRevision: number } | { status: 'success'; data: T }
export const fixtureAdapter = {
  policyRevision: 1,
  compareAndSwapPolicy(expectedRevision: number): { ok: true; revision: number } | { ok: false; currentRevision: number } {
    if (expectedRevision !== this.policyRevision) return { ok: false, currentRevision: this.policyRevision }
    return { ok: true, revision: this.policyRevision }
  },
  simulateConcurrentPolicyUpdate(): number { this.policyRevision += 1; return this.policyRevision },
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
