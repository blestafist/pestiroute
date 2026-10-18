export type Page<T> = { items: T[]; limit: number; offset: number; total: number }
export type Status = { version: string; readiness: 'ready' | 'unavailable' | 'unknown'; components: Component[]; routes: Route[] }
export type Component = { id: string; kind: string; implementation: string; health: 'ready' | 'unavailable' | 'unknown'; protocols?: string[]; capabilities: Record<string, 'supported' | 'unsupported' | 'unknown'> }
export type Route = { protocol: string; mode: string; model: string; adapter_id: string; connector_id: string; account_id: string }
export type Account = { id: string; connector: string; enabled: boolean; created_at: string; updated_at: string }
export type Credential = { id: string; account_id: string; revision: number; expires_at: string | null; updated_at: string }
export type Policy = { id: string; revision: number; enabled: boolean; models: string[]; connectors: string[]; rpm: number; tpm: number; created_at: string }
export type Key = { id: string; key_id: string; policy_id: string; policy_revision: number; revision: number; enabled: boolean; revoked: boolean; created_at: string; revoked_at: string | null }
export type IssuedKey = { secret: string; key: Key }
export type UsageAttempt = { id: string; request_id: string; ordinal: number; account_id: string; connector: string; route_id: string; state: string; estimate_tokens: number; committed: boolean; error_category: string | null; error_reason: string | null; dispatched_at: string | null; finished_at: string | null; usage: { input_tokens: number | null; output_tokens: number | null; reasoning_tokens: number | null; cached_tokens: number | null; source: string; completeness: string; recorded_at: string } | null; reservation: { estimated_tokens: number; actual_tokens: number | null; effective_charge: number; state: string } }
export type UsageRequest = { id: string; key_id: string; policy_id: string; protocol: string; model: string; route_id: string; key_revision: number; policy_revision: number; accepted_at: string; state: string; finished_at: string | null; attempts: UsageAttempt[] }
export type UsageSummary = { requests: number; attempts: number; input_tokens: number | null; output_tokens: number | null; reasoning_tokens: number | null; cached_tokens: number | null; estimated_tokens: number; actual_tokens: number | null; effective_charge: number }
export type AuthResult = { status: 'complete' | 'continue'; session_id: string | null; expires_at: string | null; user_action: { verification_uri?: string; user_code?: string | null; poll_interval_seconds?: number | null } | null }
export type AuthRefresh = { status: 'refreshed'; revision: number }
export type ErrorEnvelope = { error: { code: string; message: string; request_id: string } }

export class ApiError extends Error {
  constructor(readonly status: number, readonly code: string, message: string, readonly requestId: string) { super(message); this.name = 'ApiError' }
}

let token: string | null = null
export function setOperatorToken(value: string) { token = value }
export function clearOperatorToken() { token = null }
export function hasOperatorToken() { return token !== null }

export class AdminApi {
  constructor(private readonly base = '/admin/v1', private readonly request: typeof fetch = (...args) => fetch(...args)) {}

  private async call<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = new Headers(init.headers)
    if (token) headers.set('Authorization', `Bearer ${token}`)
    if (init.body !== undefined) headers.set('Content-Type', 'application/json')
    let response: Response
    try { response = await this.request(`${this.base}${path}`, { ...init, headers, credentials: 'omit' }) }
    catch { throw new ApiError(0, 'disconnected', 'The administration API could not be reached. Check the gateway and retry.', '') }
    if (!response.ok) {
      let e: ErrorEnvelope | undefined
      try { e = await response.json() as ErrorEnvelope } catch { /* malformed error body */ }
      const requestId = e?.error?.request_id ?? response.headers.get('X-Request-ID') ?? ''
      throw new ApiError(response.status, e?.error?.code ?? 'http_error', e?.error?.message ?? `The request failed (HTTP ${response.status}).`, requestId)
    }
    if (response.status === 204) return undefined as T
    return await response.json() as T
  }
  private query(params: Record<string, string | number | boolean | null | undefined>) { const q = new URLSearchParams(); for (const [key, value] of Object.entries(params)) if (value !== null && value !== undefined && value !== '') q.set(key, String(value)); return q.size ? `?${q}` : '' }
  private json(method: string, body?: unknown): RequestInit { return { method, ...(body === undefined ? {} : { body: JSON.stringify(body) }) } }
  status() { return this.call<Status>('/status') }
  accounts(params: { connector?: string; enabled?: boolean; limit?: number; offset?: number } = {}) { return this.call<Page<Account>>(`/accounts${this.query(params)}`) }
  account(id: string) { return this.call<Account>(`/accounts/${encodeURIComponent(id)}`) }
  createAccount(body: { id: string; connector: string; enabled?: boolean }) { return this.call<Account>('/accounts', this.json('POST', body)) }
  setAccountEnabled(id: string, enabled: boolean) { return this.call<Account>(`/accounts/${encodeURIComponent(id)}/enabled`, this.json('PUT', { enabled })) }
  createCredential(account: string, body: { id: string; secret: string; expires_at?: string | null }) { return this.call<Credential>(`/accounts/${encodeURIComponent(account)}/credentials`, this.json('POST', body)) }
  replaceCredential(account: string, id: string, body: { secret: string; expected_revision: number; expires_at?: string | null }) { return this.call<Credential>(`/accounts/${encodeURIComponent(account)}/credentials/${encodeURIComponent(id)}`, this.json('PUT', body)) }
  policies(params: { enabled?: boolean; limit?: number; offset?: number } = {}) { return this.call<Page<Policy>>(`/policies${this.query(params)}`) }
  policy(id: string, revision?: number) { return this.call<Policy>(`/policies/${encodeURIComponent(id)}${this.query({ revision })}`) }
  createPolicy(body: { id: string; models: string[]; connectors: string[]; rpm: number; tpm: number }) { return this.call<Policy>('/policies', this.json('POST', body)) }
  updatePolicy(id: string, body: { expected_revision: number; models?: string[]; connectors?: string[]; rpm?: number; tpm?: number; enabled?: false }) { return this.call<Policy>(`/policies/${encodeURIComponent(id)}`, this.json('PUT', body)) }
  keys(params: { policy_id?: string; enabled?: boolean; revoked?: boolean; limit?: number; offset?: number } = {}) { return this.call<Page<Key>>(`/keys${this.query(params)}`) }
  key(id: string) { return this.call<Key>(`/keys/${encodeURIComponent(id)}`) }
  createKey(body: { policy_id: string; policy_revision: number }) { return this.call<IssuedKey>('/keys', this.json('POST', body)) }
  revokeKey(id: string) { return this.call<Key>(`/keys/${encodeURIComponent(id)}`, { method: 'DELETE' }) }
  setKeyPolicy(id: string, body: { policy_id: string; policy_revision: number; expected_revision: number }) { return this.call<Key>(`/keys/${encodeURIComponent(id)}/policy`, this.json('PUT', body)) }
  usageRequests(params: { key_id?: string; account_id?: string; since?: string; until?: string; limit?: number; offset?: number } = {}) { return this.call<Page<UsageRequest>>(`/usage/requests${this.query(params)}`) }
  usageAttempts(params: { request_id?: string; account_id?: string; since?: string; until?: string; limit?: number; offset?: number } = {}) { return this.call<Page<UsageAttempt>>(`/usage/attempts${this.query(params)}`) }
  usageSummary(params: { key_id?: string; account_id?: string; since?: string; until?: string } = {}) { return this.call<UsageSummary>(`/usage/summary${this.query(params)}`) }
  startAuth(account_id: string) { return this.call<AuthResult>('/auth/sessions', this.json('POST', { account_id })) }
  continueAuth(id: string, callback_input: string) { return this.call<AuthResult>(`/auth/sessions/${encodeURIComponent(id)}/continue`, this.json('POST', { callback_input })) }
  refreshAuth(account: string) { return this.call<AuthRefresh>(`/auth/accounts/${encodeURIComponent(account)}/refresh`, this.json('POST')) }
}

export const adminApi = new AdminApi()
