export type Capability = 'supported' | 'unsupported' | 'unknown'
export interface Account { id: string; connector: string; enabled: boolean; models: string[]; capabilities: Record<string, Capability>; expires: string; revision: number; auth: string[]; authStatus: string }
export interface VirtualKey { id: string; label: string; status: 'active' | 'revoked'; policy: string; revision: number; models: string[]; targets: string[]; rateLimit: number; created: string; revoked?: string }
export interface Attempt { target: string; outcome: string; error: string | null }
export interface UsageRecord { id: string; timestamp: string; model: string; account: string; status: 'success' | 'error'; inputTokens: number | null; outputTokens: number | null; route: string; attempts: Attempt[]; fallback: string }
export interface FixtureData { overview: { ready: boolean | null; enabledAccounts: number | null; recentRequests: number | null }; connections: Account[]; keys: VirtualKey[]; policies: { id: string; revision: number; models: string[]; targets: string[]; rateLimit: number }[]; usage: UsageRecord[]; routes: { protocol: string; model: string; target: string; capabilities: Record<string, Capability> }[]; diagnostics: { version: string; correlationId: string; endpoints: string[]; components: { name: string; ready: boolean | null }[]; settings: { name: string; value: string; restartRequired: boolean }[] } }
export const fixtureData: FixtureData = {
  overview: { ready: true, enabledAccounts: 1, recentRequests: null },
  connections: [
    { id: 'example', connector: 'OpenAI-compatible', enabled: true, models: ['demo-model'], capabilities: { streaming: 'supported', tools: 'unsupported', reasoning: 'unknown' }, expires: 'Never', revision: 1, auth: ['OAuth', 'Manual'], authStatus: 'Not connected' },
  ],
  keys: [{ id: 'key-demo', label: 'Demo key', status: 'active', policy: 'restricted-demo', revision: 1, models: ['demo-model'], targets: ['example'], rateLimit: 60, created: '2026-01-01' }],
  policies: [{ id: 'restricted-demo', revision: 1, models: ['demo-model'], targets: ['example'], rateLimit: 60 }],
  usage: [
    { id: 'req-104', timestamp: '2026-10-17T10:30:00Z', model: 'example-model', account: 'example', status: 'success', inputTokens: 120, outputTokens: 42, route: 'example', attempts: [{ target: 'example', outcome: 'success', error: null }], fallback: 'Not used' },
    { id: 'req-103', timestamp: '2026-10-17T10:20:00Z', model: 'example-model', account: 'example-fallback', status: 'success', inputTokens: null, outputTokens: null, route: 'example-fallback', attempts: [{ target: 'example', outcome: 'timeout', error: 'Synthetic fixture timeout' }, { target: 'example-fallback', outcome: 'success', error: null }], fallback: 'Retried after confirmed safe-retry disposition; final outcome: success' },
    { id: 'req-102', timestamp: '2026-10-17T10:10:00Z', model: 'other-model', account: 'example', status: 'success', inputTokens: 0, outputTokens: null, route: 'example', attempts: [{ target: 'example', outcome: 'success', error: null }], fallback: 'Not used' },
    { id: 'req-101', timestamp: '2026-10-17T10:00:00Z', model: 'other-model', account: 'example', status: 'error', inputTokens: null, outputTokens: null, route: 'example', attempts: [{ target: 'example', outcome: 'delivery unknown', error: 'Synthetic ambiguous timeout' }], fallback: 'Not retried: delivery outcome ambiguous' },
  ],
  routes: [{ protocol: 'Responses', model: 'example-model', target: 'example · OpenAI-compatible', capabilities: { streaming: 'supported', tools: 'unsupported', reasoning: 'unknown' } }],
  diagnostics: { version: 'demo-1.0.0', correlationId: 'demo-correlation-104', endpoints: ['http://127.0.0.1:8080/admin', 'http://127.0.0.1:8080/v1'], components: [{ name: 'Management API', ready: true }, { name: 'Ledger', ready: true }, { name: 'Provider health', ready: null }], settings: [{ name: 'Routing configuration', value: 'File-backed', restartRequired: true }, { name: 'Fixture adapter', value: 'Enabled', restartRequired: false }] },
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
export function filterUsage(records: UsageRecord[], filters: { model?: string; status?: string; account?: string; from?: string; to?: string }): UsageRecord[] {
  const from = filters.from ? Date.parse(`${filters.from}T00:00:00Z`) : -Infinity
  const to = filters.to ? Date.parse(`${filters.to}T23:59:59.999Z`) : Infinity
  return records.filter(record => {
    const time = Date.parse(record.timestamp)
    return (!filters.model || record.model === filters.model) && (!filters.status || record.status === filters.status) && (!filters.account || record.account === filters.account) && time >= from && time <= to
  })
}
export function setSessionToken(token: string | null): void { sessionToken = token }
export function getSessionToken(): string | null { return sessionToken }
let sessionToken: string | null = null
export function clearSession(): void { sessionToken = null }
