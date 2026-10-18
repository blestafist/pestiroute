import { Fragment, useEffect, useRef, useState } from 'react'
import { Link, NavLink, Route, Routes, useLocation, useSearchParams } from 'react-router-dom'
import { clearSession, filterUsage, fixtureAdapter, fixtureData, getSessionToken, setSessionToken, type Account, type SimulatedState, type VirtualKey } from './fixtures'

const pages = [
  ['Overview', '/', '01'], ['Connections', '/connections', '02'], ['Keys & Policies', '/keys', '03'],
  ['Usage', '/usage', '04'], ['Routing', '/routing', '05'], ['Settings & Diagnostics', '/settings', '06'],
] as const

function Screen({ title, children }: { title: string; children: React.ReactNode }) {
  return <section aria-labelledby="page-title"><p className="eyebrow">OPERATOR CONSOLE / {title.toUpperCase()}</p><h1 id="page-title">{title}</h1>{children}</section>
}
function validDate(value: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(value) && !Number.isNaN(Date.parse(`${value}T00:00:00Z`)) && new Date(`${value}T00:00:00Z`).toISOString().slice(0, 10) === value
}

function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: React.ReactNode }) {
  const root = useRef<HTMLDivElement>(null)
  const close = useRef(onClose)
  close.current = onClose
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    root.current?.querySelector<HTMLElement>('[data-modal-title]')?.focus()
    function keydown(event: KeyboardEvent) {
      if (event.key === 'Escape') { event.preventDefault(); close.current(); return }
      if (event.key !== 'Tab' || !root.current) return
      const focusable = [...root.current.querySelectorAll<HTMLElement>('button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')]
      if (!focusable.length) { event.preventDefault(); return }
      const first = focusable[0], last = focusable[focusable.length - 1]
      if (event.shiftKey && (document.activeElement === first || document.activeElement === root.current.querySelector('[data-modal-title]'))) { event.preventDefault(); last.focus() }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
    }
    document.addEventListener('keydown', keydown)
    return () => { document.removeEventListener('keydown', keydown); previous?.focus() }
  }, [])
  return <div className="modal-overlay" onMouseDown={event => { if (event.target === event.currentTarget) onClose() }}><div className="modal" role="dialog" aria-modal="true" aria-label={title} ref={root}><h2 data-modal-title tabIndex={-1}>{title}</h2>{children}</div></div>
}

function Overview() { return <Screen title="Overview"><div className="cards"><article><span>Gateway readiness</span><strong>{fixtureData.overview.ready === null ? 'Unknown' : fixtureData.overview.ready ? 'Ready' : 'Not ready'}</strong></article><article><span>Enabled accounts</span><strong>{fixtureData.overview.enabledAccounts ?? 'Unknown'}</strong></article><article><span>Recent requests · local ledger</span><strong>{fixtureData.overview.recentRequests ?? 'Unknown'}</strong></article></div><h2>Components</h2><p>{fixtureData.diagnostics.components.map(c => `${c.name}: ${c.ready === null ? 'Unknown' : c.ready ? 'Ready' : 'Not ready'}`).join(' · ')}</p><h2>Recent activity</h2>{fixtureData.usage.map(r => <p key={r.id}><Link to={`/usage?model=${encodeURIComponent(r.model)}&status=${r.status}`}>{r.id} · {r.status} · {r.model}</Link></p>)}<p className="note">Synthetic example data. Provider quota and live provider health are not shown.</p></Screen> }
function Connections() {
  const [accounts, setAccounts] = useState<Account[]>(fixtureData.connections)
  const [selected, setSelected] = useState(accounts[0]?.id ?? '')
  const [credential, setCredential] = useState('')
  const [id, setId] = useState('')
  const [connector, setConnector] = useState('OpenAI-compatible')
  const account = accounts.find(item => item.id === selected)
  function saveCredential(event: React.FormEvent) { event.preventDefault(); if (!credential || !account) return; setCredential(''); setAccounts(accounts.map(x => x.id === account.id ? { ...x, expires: 'Updated just now', revision: x.revision + 1 } : x)) }
  function createAccount(event: React.FormEvent) { event.preventDefault(); if (!id.trim()) return; const next: Account = { id: id.trim(), connector, enabled: true, models: [], capabilities: { streaming: 'unknown' }, expires: 'Unknown', revision: 1, auth: ['OAuth', 'Manual'], authStatus: 'Not connected' }; setAccounts([...accounts, next]); setSelected(next.id); setId(''); setCredential('') }
  return <Screen title="Connections"><div className="table-wrap"><table><thead><tr><th>Account</th><th>Connector</th><th>Status</th><th>Credential expiry</th><th>Revision</th><th>Capabilities</th></tr></thead><tbody>{accounts.map(x => <tr key={x.id}><td><button onClick={() => setSelected(x.id)}>{x.id}</button></td><td>{x.connector}</td><td>{x.enabled ? 'Enabled' : 'Disabled'}</td><td>{x.expires}</td><td>{x.revision}</td><td>{Object.entries(x.capabilities).map(([name, value]) => <span className={`badge capability-${value}`} key={name}>{name}: {value}</span>)}</td></tr>)}</tbody></table></div>
    <form onSubmit={createAccount}><h2>Create account</h2><label>Account ID <input value={id} onChange={e => setId(e.target.value)} required /></label><label>Connector <select value={connector} onChange={e => setConnector(e.target.value)}><option>OpenAI-compatible</option><option>Claude Code</option></select></label><label>Credential <input type="password" autoComplete="new-password" value={credential} onChange={e => setCredential(e.target.value)} /></label><button>Create account</button></form>
    {account && <section aria-label={`Account details for ${account.id}`}><h2>{account.id}</h2><button onClick={() => setAccounts(accounts.map(x => x.id === account.id ? { ...x, enabled: !x.enabled, revision: x.revision + 1 } : x))}>{account.enabled ? 'Disable' : 'Enable'} account</button><form onSubmit={saveCredential}><label>Replace credential (write-only) <input type="password" autoComplete="new-password" value={credential} onChange={e => setCredential(e.target.value)} /></label><button>Replace credential</button></form><p>Advertised authentication</p>{account.auth.map(action => <button key={action} onClick={() => setAccounts(accounts.map(x => x.id === account.id ? { ...x, authStatus: `${action} started` } : x))}>{action}</button>)}<p role="status">{account.authStatus}</p></section>}
  </Screen>
}
function Keys() {
  const [keys, setKeys] = useState<VirtualKey[]>(fixtureData.keys)
  const [label, setLabel] = useState('')
  const [models, setModels] = useState('demo-model')
  const [targets, setTargets] = useState('example')
  const [rate, setRate] = useState('60')
  const [secret, setSecret] = useState<string | null>(null)
  const [createdName, setCreatedName] = useState('')
  const [revoke, setRevoke] = useState<VirtualKey | null>(null)
  const [conflict, setConflict] = useState(false)
  const [policyRevision, setPolicyRevision] = useState(fixtureAdapter.policyRevision)
  const [copyStatus, setCopyStatus] = useState('')
  const policy = fixtureData.policies[0]
  function create(event: React.FormEvent) { event.preventDefault(); const result = fixtureAdapter.compareAndSwapPolicy(policyRevision); if (!result.ok) { setConflict(true); return } const key: VirtualKey = { id: `key-${keys.length + 1}`, label: label.trim(), status: 'active', policy: policy.id, revision: result.revision, models: models.split(',').map(x => x.trim()).filter(Boolean), targets: targets.split(',').map(x => x.trim()).filter(Boolean), rateLimit: Number(rate), created: new Date().toISOString().slice(0, 10) }; setKeys([...keys, key]); setCreatedName(key.label); setSecret(`pr_demo_${Math.random().toString(36).slice(2)}`); setLabel(''); setConflict(false) }
  async function copy() { if (!secret) return; try { if (!navigator.clipboard?.writeText) throw new Error('Clipboard unavailable'); await navigator.clipboard.writeText(secret); setCopyStatus('Key copied to clipboard.') } catch { setCopyStatus('Could not copy automatically. Select and copy the key manually.') } }
  return <Screen title="Keys & Policies"><p>Policy restrictions are shown in plain language. Creation uses revision-checked fixture data.</p><section aria-label={`Policy ${policy.id}`}><h2>{policy.id} · revision {policyRevision}</h2><p>Models: {policy.models.join(', ')}; targets: {policy.targets.join(', ')}; rate limit: {policy.rateLimit}/min.</p></section><button onClick={() => fixtureAdapter.simulateConcurrentPolicyUpdate()}>Simulate concurrent policy update</button><div className="table-wrap"><table><thead><tr><th>Key</th><th>Status</th><th>Policy / revision</th><th>Restrictions</th><th>Created / revoked</th><th /></tr></thead><tbody>{keys.map(key => <tr key={key.id}><td>{key.label} ({key.id})</td><td>{key.status}</td><td>{key.policy} / {key.revision}</td><td>Models: {key.models.join(', ') || 'None'}; targets: {key.targets.join(', ') || 'None'}; {key.rateLimit}/min</td><td>{key.created}{key.revoked ? ` / ${key.revoked}` : ''}</td><td>{key.status === 'active' && <button onClick={() => setRevoke(key)}>Revoke</button>}</td></tr>)}</tbody></table></div>
    <form onSubmit={create}><h2>Create restricted key</h2><label>Label <input value={label} onChange={e => setLabel(e.target.value)} required /></label><label>Models (comma separated) <input value={models} onChange={e => setModels(e.target.value)} /></label><label>Targets (comma separated) <input value={targets} onChange={e => setTargets(e.target.value)} /></label><label>Requests per minute <input type="number" min="1" value={rate} onChange={e => setRate(e.target.value)} /></label><button>Create key</button></form>
    {conflict && <section role="alert"><p>Revision conflict. Your entered values are preserved; review the latest revision before retrying.</p><button onClick={() => { setPolicyRevision(fixtureAdapter.policyRevision); setConflict(false) }}>Refresh revision {fixtureAdapter.policyRevision}</button></section>}
    {secret && <Modal title={`Copy your new key: ${createdName}`} onClose={() => { setSecret(null); setCopyStatus('') }}><p>This secret is shown once. Copy it now; it cannot be retrieved later.</p><code>{secret}</code><p role="status">{copyStatus}</p><button onClick={() => void copy()}>Copy key</button><button onClick={() => { setSecret(null); setCopyStatus('') }}>I have copied it — dismiss</button></Modal>}
    {revoke && <Modal title={`Revoke ${revoke.label}?`} onClose={() => setRevoke(null)}><p>This will revoke {revoke.id}.</p><button onClick={() => setRevoke(null)}>Cancel</button><button onClick={() => { setKeys(keys.map(x => x.id === revoke.id ? { ...x, status: 'revoked', revoked: new Date().toISOString().slice(0, 10) } : x)); setRevoke(null) }}>Confirm revoke {revoke.label}</button></Modal>}
  </Screen>
}
function Usage() {
  const [params, setParams] = useSearchParams()
  const modelValue = params.get('model') ?? '', statusValue = params.get('status') ?? '', accountValue = params.get('account') ?? ''
  const models = [...new Set(fixtureData.usage.map(x => x.model))], accounts = [...new Set(fixtureData.usage.map(x => x.account))]
  const statuses: string[] = ['success', 'error']
  const model = models.includes(modelValue) ? modelValue : '', status = statuses.includes(statusValue) ? statusValue : '', account = accounts.includes(accountValue) ? accountValue : ''
  let from = params.get('from') ?? '', to = params.get('to') ?? ''
  if (!validDate(from)) from = ''
  if (!validDate(to)) to = ''
  const invalidRange = Boolean(from && to && from > to)
  const sizeParam = Number(params.get('size')), size = [2, 5].includes(sizeParam) ? sizeParam : 2
  const pagesFor = (count: number) => Math.max(1, Math.ceil(count / size))
  const filtered = invalidRange ? [] : filterUsage(fixtureData.usage, { model, status, account, from, to }), pages = pagesFor(filtered.length)
  const pageParam = Number(params.get('page')), page = Number.isSafeInteger(pageParam) && pageParam > 0 ? Math.min(pageParam, pages) : 1
  useEffect(() => {
    const next = new URLSearchParams(params)
    let changed = false
    for (const key of ['from', 'to']) {
      const value = next.get(key) ?? ''
      if (value && !validDate(value)) { next.delete(key); changed = true }
    }
    for (const [key, value, allowed] of [['model', modelValue, models], ['account', accountValue, accounts], ['status', statusValue, statuses]] as const) {
      if (value && !allowed.includes(value)) { next.delete(key); changed = true }
    }
    if (params.has('size') && sizeParam !== size) { next.set('size', String(size)); changed = true }
    if (params.has('page') && String(pageParam) !== String(page)) { next.set('page', String(page)); changed = true }
    if (changed) setParams(next, { replace: true })
  }, [accountValue, accounts, modelValue, models, page, pageParam, params, setParams, size, sizeParam, statusValue])
  const [selected, setSelected] = useState<string | null>(null)
  function update(key: string, value: string) { const next = new URLSearchParams(params); value ? next.set(key, value) : next.delete(key); if (['model', 'status', 'account', 'from', 'to'].includes(key)) next.delete('page'); setParams(next) }
  return <Screen title="Usage"><p>Local ledger request history · token counts may be unknown.</p><label>Time from <input aria-label="Time from" type="date" value={from} onChange={e => update('from', e.target.value)} /></label> <label>Time to <input aria-label="Time to" type="date" value={to} onChange={e => update('to', e.target.value)} /></label> <label>Account <select aria-label="Filter by account" value={account} onChange={e => update('account', e.target.value)}><option value="">All accounts</option>{accounts.map(x => <option key={x}>{x}</option>)}</select></label> <label>Model <select aria-label="Filter by model" value={model} onChange={e => update('model', e.target.value)}><option value="">All models</option>{models.map(x => <option key={x}>{x}</option>)}</select></label> <label>Outcome <select aria-label="Filter by outcome" value={status} onChange={e => update('status', e.target.value)}><option value="">All outcomes</option><option value="success">Success</option><option value="error">Error</option></select></label>
    {invalidRange ? <p role="alert">Invalid time range: the start date must be on or before the end date. Correct either date to view requests.</p> : filtered.length === 0 ? <p role="status">No requests match active filters (model: {model || 'all'}, status: {status || 'all'}, account: {account || 'all'}, time: {from || 'any'}–{to || 'any'}).</p> : <><div className="table-wrap"><table><thead><tr><th>Request</th><th>Time</th><th>Model</th><th>Account</th><th>Status</th><th>Input tokens</th><th>Output tokens</th><th>Attempts</th><th>Details</th></tr></thead><tbody>{filtered.slice((page - 1) * size, page * size).map(r => <Fragment key={r.id}><tr><td>{r.id}</td><td>{r.timestamp}</td><td>{r.model}</td><td>{r.account}</td><td>{r.status}</td><td>{r.inputTokens ?? 'Unknown'}</td><td>{r.outputTokens ?? 'Unknown'}</td><td>{r.attempts.length}</td><td><button onClick={() => setSelected(selected === r.id ? null : r.id)}>{selected === r.id ? 'Hide' : 'Show'} details</button></td></tr>{selected === r.id && <tr><td colSpan={9}><strong>Route/target:</strong> {r.route}<p><strong>Fallback:</strong> {r.fallback}</p><strong>Attempts</strong>{r.attempts.map((a, i) => <p key={i}>{a.target}: {a.outcome}{a.error ? ` — ${a.error}` : ''}</p>)}</td></tr>}</Fragment>)}</tbody></table></div><p>Page {page} of {pages} · {filtered.length} requests</p><button disabled={page <= 1} onClick={() => update('page', String(page - 1))}>Previous</button> <button disabled={page >= pages} onClick={() => update('page', String(page + 1))}>Next</button> <label>Rows per page <select aria-label="Rows per page" value={size} onChange={e => { const next = new URLSearchParams(params); next.set('size', e.target.value); next.delete('page'); setParams(next) }}><option value="2">2</option><option value="5">5</option></select></label></>}</Screen>
}
function Routing() { return <Screen title="Routing"><p role="note">Configuration-backed topology. Changes require configuration and restart; routes are read-only.</p>{fixtureData.routes.map(r => <article key={`${r.protocol}-${r.model}`}><h2>{r.protocol} · {r.model}</h2><p>Target: {r.target}</p><p>Capabilities: {Object.entries(r.capabilities).map(([name, value]) => <span className={`badge capability-${value}`} key={name}>{name}: {value}</span>)}</p><Link to="/connections">Connections</Link> · <Link to={`/usage?model=${encodeURIComponent(r.model)}`}>Usage</Link></article>)}</Screen> }
function Settings({ onLogout }: { onLogout: () => void }) { const [copy, setCopy] = useState(''); async function copyId() { try { await navigator.clipboard.writeText(fixtureData.diagnostics.correlationId); setCopy('Correlation ID copied.') } catch { setCopy('Clipboard unavailable; correlation ID is safe to copy manually.') } } return <Screen title="Settings & Diagnostics"><h2>Gateway</h2><p>Version: {fixtureData.diagnostics.version}</p><h2>Components</h2>{fixtureData.diagnostics.components.map(c => <p key={c.name}>{c.name}: {c.ready === null ? 'Unknown' : c.ready ? 'Ready' : 'Not ready'}</p>)}<h2>Endpoints</h2>{fixtureData.diagnostics.endpoints.map(e => <p key={e}>{e}</p>)}<h2>Runtime settings</h2>{fixtureData.diagnostics.settings.map(s => <p key={s.name}>{s.name}: {s.value} {s.restartRequired && <span className="badge">Restart required</span>}</p>)}<p>Diagnostic correlation ID: <code>{fixtureData.diagnostics.correlationId}</code></p><button onClick={() => void copyId()}>Copy correlation ID</button><p role="status">{copy}</p><button onClick={onLogout}>Log out and clear cached views</button></Screen> }

export default function App() {
  const location = useLocation()
  const [lastUsageQuery, setLastUsageQuery] = useState('')
  const [theme, setTheme] = useState<'light' | 'dark'>('light')
  const [token, setToken] = useState(getSessionToken())
  const [demoToken, setDemoToken] = useState('')
  const [fixtureMode, setFixtureMode] = useState<SimulatedState>('ready')
  const [viewSession, setViewSession] = useState(0)
  useEffect(() => { if (location.pathname === '/usage') setLastUsageQuery(location.search) }, [location.pathname, location.search])
  const fixtureResult = fixtureAdapter.read(fixtureData, fixtureMode)
  function login(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!demoToken.trim()) return
    setSessionToken(demoToken)
    setToken(demoToken)
    setDemoToken('')
  }
  function logout() { clearSession(); setToken(null); setDemoToken(''); setViewSession(value => value + 1) }
  return <div className={`app theme-${theme}`}>
    <div className="demo-banner" role="status">DEMO DATA — NOT LIVE</div>
    <header className="topbar"><Link className="brand" to="/">PestiRoute <span>CONTROL PLANE</span></Link><div className="top-status"><span><i className="status-dot" />Gateway demo</span><span>{token ? 'Demo operator session active' : 'No operator session'}</span><button onClick={() => setTheme(theme === 'light' ? 'dark' : 'light')} aria-label={`Switch to ${theme === 'light' ? 'dark' : 'light'} theme`}>{theme === 'light' ? '◐' : '☼'} Theme</button>{token ? <button onClick={logout}>Log out</button> : <form className="session-form" onSubmit={login}><label htmlFor="demo-token">Demo-only bearer token</label><input id="demo-token" type="password" autoComplete="off" value={demoToken} onChange={event => setDemoToken(event.target.value)} /><button type="submit">Start demo session</button></form>}</div></header>
     <div className="layout"><nav aria-label="Primary navigation" className="side-nav">{pages.map(([label, path, number]) => <NavLink key={path} to={path === '/usage' && lastUsageQuery ? `${path}${lastUsageQuery}` : path} end={path === '/'}><span>{number}</span>{label}</NavLink>)}</nav><main>
      <label className="fixture-control">Simulate fixture state <select aria-label="Simulate fixture state" value={fixtureMode} onChange={event => setFixtureMode(event.target.value as SimulatedState)}><option value="ready">Ready</option><option value="loading">Loading</option><option value="error">Error</option><option value="conflict">Conflict</option></select></label>
      {fixtureResult.status === 'loading' && <p className="fixture-state" role="status">Loading synthetic fixture data…</p>}
      {fixtureResult.status === 'error' && <p className="fixture-state error" role="alert">{fixtureResult.message}</p>}
      {fixtureResult.status === 'conflict' && <p className="fixture-state" role="status">Simulated revision conflict. Current fixture revision: {fixtureResult.currentRevision}.</p>}
       {fixtureResult.status === 'success' && <Routes key={viewSession}><Route path="/" element={<Overview />} /><Route path="/connections" element={<Connections />} /><Route path="/keys" element={<Keys />} /><Route path="/usage" element={<Usage />} /><Route path="/routing" element={<Routing />} /><Route path="/settings" element={<Settings onLogout={logout} />} /><Route path="*" element={<Screen title="Not found"><p>This demo route does not exist.</p></Screen>} /></Routes>}
      <footer>Local fixture adapter · no gateway or provider connection</footer></main></div>
  </div>
}
