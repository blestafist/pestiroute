import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it } from 'vitest'
import App from './App'
import { clearSession, fixtureAdapter, fixtureData, getSessionToken } from './fixtures'

function renderApp(path = '/') { return render(<MemoryRouter initialEntries={[path]}><App /></MemoryRouter>) }

describe('operator demo shell', () => {
  afterEach(clearSession)
  it.each([['/', 'Overview'], ['/connections', 'Connections'], ['/keys', 'Keys & Policies'], ['/usage', 'Usage'], ['/routing', 'Routing'], ['/settings', 'Settings & Diagnostics']])('renders %s and marks its navigation link', (path, title) => {
    renderApp(path)
    expect(screen.getByRole('heading', { name: title })).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Primary navigation' })).getByRole('link', { name: new RegExp(title) })).toHaveAttribute('aria-current', 'page')
    expect(screen.getByText('DEMO DATA — NOT LIVE')).toHaveAttribute('role', 'status')
  })
  it('switches themes and supports keyboard tab traversal by accessible names', async () => {
    const user = userEvent.setup()
    renderApp()
    await user.click(screen.getByRole('link', { name: /Overview/ }))
    await user.tab()
    expect(screen.getByRole('link', { name: /Connections/ })).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('link', { name: /Keys & Policies/ })).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('link', { name: /Usage/ })).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('link', { name: /Routing/ })).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('link', { name: /Settings & Diagnostics/ })).toHaveFocus()
    const button = screen.getByRole('button', { name: 'Switch to dark theme' })
    await user.click(button)
    expect(document.querySelector('.theme-dark')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Switch to light theme' })).toBeInTheDocument()
  })
  it('renders selectable loading, error and conflict fixture states', async () => {
    const user = userEvent.setup()
    renderApp()
    const selector = screen.getByRole('combobox', { name: 'Simulate fixture state' })
    await user.selectOptions(selector, 'loading')
    expect(screen.getByText('Loading synthetic fixture data…')).toHaveAttribute('role', 'status')
    await user.selectOptions(selector, 'error')
    expect(screen.getByRole('alert')).toHaveTextContent('Choose Ready to retry')
    await user.selectOptions(selector, 'conflict')
    expect(screen.getByText(/Current fixture revision: 2/)).toHaveAttribute('role', 'status')
    await user.selectOptions(selector, 'ready')
    expect(screen.getByRole('heading', { name: 'Overview' })).toBeInTheDocument()
  })
  it('keeps nullable counters and tri-state capabilities, and clears demo token on logout', async () => {
    const user = userEvent.setup()
    renderApp('/usage')
    expect(screen.getAllByText('Unknown').length).toBeGreaterThan(0)
    expect(fixtureData.connections.map(x => x.capability)).toEqual(['supported', 'unsupported', 'unknown'])
    const ready = fixtureAdapter.read(fixtureData, 'ready')
    expect(ready.status).toBe('success')
    if (ready.status !== 'success') throw new Error('Expected fixture success')
    expect(ready.data.usage[0].outputTokens).toBeNull()
    const tokenInput = screen.getByLabelText('Demo-only bearer token')
    await user.type(tokenInput, 'demo-secret')
    await user.click(screen.getByRole('button', { name: 'Start demo session' }))
    expect(getSessionToken()).toBe('demo-secret')
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    expect(screen.queryByText('demo-secret')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Log out' }))
    expect(getSessionToken()).toBeNull()
    expect(screen.getByLabelText('Demo-only bearer token')).toHaveValue('')
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })
})
