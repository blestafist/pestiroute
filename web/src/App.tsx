import { useState } from 'react'
import { Link, NavLink, Route, Routes } from 'react-router-dom'
import { clearSession, fixtureAdapter, fixtureData, getSessionToken, setSessionToken, type SimulatedState } from './fixtures'

const pages = [
  ['Overview', '/', '01'], ['Connections', '/connections', '02'], ['Keys & Policies', '/keys', '03'],
  ['Usage', '/usage', '04'], ['Routing', '/routing', '05'], ['Settings & Diagnostics', '/settings', '06'],
] as const

function Screen({ title, children }: { title: string; children: React.ReactNode }) {
  return <section aria-labelledby="page-title"><p className="eyebrow">OPERATOR CONSOLE / {title.toUpperCase()}</p><h1 id="page-title">{title}</h1>{children}</section>
}

function Overview() { return <Screen title="Overview"><div className="cards"><article><span>Enabled accounts</span><strong>{fixtureData.overview.enabledAccounts ?? 'Unknown'}</strong></article><article><span>Recent requests · local ledger</span><strong>{fixtureData.overview.recentRequests ?? 'Unknown'}</strong></article></div><p className="note">Synthetic example data. Provider quota and live provider health are not shown.</p></Screen> }
function Connections() { return <Screen title="Connections"><p>Connection and capability overview</p><div className="table-wrap"><table><thead><tr><th scope="col">Connection</th><th scope="col">Capability</th></tr></thead><tbody>{fixtureData.connections.map(x => <tr key={x.name}><td>{x.name}</td><td>{x.capability}</td></tr>)}</tbody></table></div></Screen> }
function Keys() { return <Screen title="Keys & Policies"><p>Virtual keys and policy controls will appear here.</p></Screen> }
function Usage() { return <Screen title="Usage"><p>Usage records · unavailable counters remain unknown.</p><div className="table-wrap"><table><thead><tr><th scope="col">Model</th><th scope="col">Input tokens</th><th scope="col">Output tokens</th></tr></thead><tbody>{fixtureData.usage.map(x => <tr key={x.model}><td>{x.model}</td><td>{x.inputTokens ?? 'Unknown'}</td><td>{x.outputTokens ?? 'Unknown'}</td></tr>)}</tbody></table></div></Screen> }
function Routing() { return <Screen title="Routing"><p>Routes are configuration-backed. Changes require configuration and restart.</p><p>Capability state: {fixtureData.connections[1].capability}</p></Screen> }
function Settings() { return <Screen title="Settings & Diagnostics"><p>Local demo settings and safe diagnostic information.</p></Screen> }

export default function App() {
  const [theme, setTheme] = useState<'light' | 'dark'>('light')
  const [token, setToken] = useState(getSessionToken())
  const [demoToken, setDemoToken] = useState('')
  const [fixtureMode, setFixtureMode] = useState<SimulatedState>('ready')
  const fixtureResult = fixtureAdapter.read(fixtureData, fixtureMode)
  function login(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!demoToken.trim()) return
    setSessionToken(demoToken)
    setToken(demoToken)
    setDemoToken('')
  }
  function logout() { clearSession(); setToken(null); setDemoToken('') }
  return <div className={`app theme-${theme}`}>
    <div className="demo-banner" role="status">DEMO DATA — NOT LIVE</div>
    <header className="topbar"><Link className="brand" to="/">PestiRoute <span>CONTROL PLANE</span></Link><div className="top-status"><span><i className="status-dot" />Gateway demo</span><span>{token ? 'Demo operator session active' : 'No operator session'}</span><button onClick={() => setTheme(theme === 'light' ? 'dark' : 'light')} aria-label={`Switch to ${theme === 'light' ? 'dark' : 'light'} theme`}>{theme === 'light' ? '◐' : '☼'} Theme</button>{token ? <button onClick={logout}>Log out</button> : <form className="session-form" onSubmit={login}><label htmlFor="demo-token">Demo-only bearer token</label><input id="demo-token" type="password" autoComplete="off" value={demoToken} onChange={event => setDemoToken(event.target.value)} /><button type="submit">Start demo session</button></form>}</div></header>
    <div className="layout"><nav aria-label="Primary navigation" className="side-nav">{pages.map(([label, path, number]) => <NavLink key={path} to={path} end={path === '/'}><span>{number}</span>{label}</NavLink>)}</nav><main>
      <label className="fixture-control">Simulate fixture state <select aria-label="Simulate fixture state" value={fixtureMode} onChange={event => setFixtureMode(event.target.value as SimulatedState)}><option value="ready">Ready</option><option value="loading">Loading</option><option value="error">Error</option><option value="conflict">Conflict</option></select></label>
      {fixtureResult.status === 'loading' && <p className="fixture-state" role="status">Loading synthetic fixture data…</p>}
      {fixtureResult.status === 'error' && <p className="fixture-state error" role="alert">{fixtureResult.message}</p>}
      {fixtureResult.status === 'conflict' && <p className="fixture-state" role="status">Simulated revision conflict. Current fixture revision: {fixtureResult.currentRevision}.</p>}
      {fixtureResult.status === 'success' && <Routes><Route path="/" element={<Overview />} /><Route path="/connections" element={<Connections />} /><Route path="/keys" element={<Keys />} /><Route path="/usage" element={<Usage />} /><Route path="/routing" element={<Routing />} /><Route path="/settings" element={<Settings />} /><Route path="*" element={<Screen title="Not found"><p>This demo route does not exist.</p></Screen>} /></Routes>}
      <footer>Local fixture adapter · no gateway or provider connection</footer></main></div>
  </div>
}
