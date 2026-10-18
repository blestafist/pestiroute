import { afterEach, describe, expect, it, vi } from 'vitest'
import { AdminApi, clearOperatorToken, setOperatorToken } from './api'

describe('admin API client', () => {
  afterEach(clearOperatorToken)
  it('adds the in-memory bearer token and omits cookies', async () => {
    setOperatorToken('operator-token')
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ version: '1', readiness: 'unknown', components: [], routes: [] }), { status: 200 }))
    await new AdminApi('/admin/v1', fetcher).status()
    const [, init] = fetcher.mock.calls[0]
    expect(new Headers(init.headers).get('Authorization')).toBe('Bearer operator-token')
    expect(init.credentials).toBe('omit')
  })
  it('parses the standard error envelope with status and request id', async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: { code: 'revision_conflict', message: 'Revision is stale', request_id: 'req-123' } }), { status: 409 }))
    await expect(new AdminApi('/admin/v1', fetcher).status()).rejects.toMatchObject({ status: 409, code: 'revision_conflict', message: 'Revision is stale', requestId: 'req-123' })
  })
  it('targets the accepted account and usage endpoints with encoded path/query values', async () => {
    const fetcher = vi.fn().mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ items: [], limit: 50, offset: 0, total: 0 }), { status: 200 })))
    const api = new AdminApi('/admin/v1', fetcher)
    await api.account('a/b')
    await api.usageRequests({ account_id: 'a b', limit: 10 })
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual(['/admin/v1/accounts/a%2Fb', '/admin/v1/usage/requests?account_id=a+b&limit=10'])
  })
})
