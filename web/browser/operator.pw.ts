import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'
import { expect, test } from '@playwright/test'

type Fixture = { admin: string; client: string; operator_token: string }

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
let fixtureProcess: ChildProcessWithoutNullStreams
let fixture: Fixture

test.beforeAll(async () => {
  const gateway = process.env.M12_E2E_GATEWAY
  if (!gateway) throw new Error('Set M12_E2E_GATEWAY to a release build made with --with-ui')
  fixtureProcess = spawn('python3', ['scripts/e2e-m12-webui.py', '--serve'], {
    cwd: root,
    env: { ...process.env, M12_E2E_GATEWAY: gateway },
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  const lines = createInterface({ input: fixtureProcess.stdout })
  fixture = await new Promise<Fixture>((resolveFixture, reject) => {
    const timeout = setTimeout(() => reject(new Error('Gateway browser fixture startup timed out')), 60_000)
    lines.once('line', line => {
      clearTimeout(timeout)
      try { resolveFixture(JSON.parse(line) as Fixture) }
      catch (error) { reject(error) }
    })
    fixtureProcess.once('exit', code => {
      clearTimeout(timeout)
      reject(new Error(`Gateway browser fixture exited early (${code})`))
    })
    fixtureProcess.stderr.on('data', chunk => process.stderr.write(chunk))
  })
})

test.afterAll(async () => {
  if (!fixtureProcess) return
  fixtureProcess.stdin.end()
  if (fixtureProcess.exitCode === null) {
    await new Promise<void>(resolveExit => fixtureProcess.once('exit', () => resolveExit()))
  }
})

test('packaged entry routing and complete live operator workflow', async ({ page, request }) => {
  const removedFallback = await page.request.get(`${fixture.admin}/admin/fallback.html`)
  expect(await removedFallback.text()).not.toContain('WebUI unavailable')
  await page.goto(`${fixture.admin}/admin/keys`)
  await expect(page.getByRole('heading', { name: 'PestiRoute administration' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/admin/keys')
  await page.getByLabel('Operator bearer token').fill(fixture.operator_token)
  await page.getByRole('button', { name: 'Connect' }).click()
  await expect(page.getByRole('heading', { name: 'Keys & Policies' })).toBeVisible()
  await page.reload()
  await expect(page.getByRole('heading', { name: 'PestiRoute administration' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/admin/keys')
  await page.getByLabel('Operator bearer token').fill(fixture.operator_token)
  await page.getByRole('button', { name: 'Connect' }).click()
  await expect(page.getByRole('heading', { name: 'Keys & Policies' })).toBeVisible()

  await page.getByRole('link', { name: 'Overview' }).click()
  await expect(page.getByText(/Gateway readiness: unknown · Version dev/)).toBeVisible()
  await expect(page.getByText(/m12-connector \(connector\)/)).toBeVisible()
  await page.getByRole('link', { name: 'Settings & Diagnostics' }).click()
  await expect(page.getByText(/Readiness: unknown; components: 2; routes: 1/)).toBeVisible()

  await page.getByRole('link', { name: 'Connections' }).click()
  await page.getByLabel('Account ID', { exact: true }).fill('m12-browser-account')
  await page.getByLabel('Connector', { exact: true }).fill('m12-connector')
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByText('Account created.')).toBeVisible()
  await page.getByLabel('Credential account ID').fill('m12-browser-account')
  await page.getByLabel('New credential ID').fill('m12-browser-credential')
  await page.getByLabel('New write-only credential').fill('synthetic-browser-credential')
  await page.getByRole('button', { name: 'Create credential' }).click()
  await expect(page.getByText('Credential created.')).toBeVisible()
  await expect(page.getByLabel('New write-only credential')).toHaveValue('')

  await page.getByRole('link', { name: 'Keys & Policies' }).click()
  await page.getByLabel('Policy ID', { exact: true }).fill('m12-browser-policy')
  await page.getByLabel('Models (comma separated)').fill('m12-model')
  await page.getByLabel('Connectors (comma separated)').fill('m12-connector')
  await page.getByLabel('Requests per minute').fill('10')
  await page.getByLabel('Tokens per minute').fill('1000')
  await page.getByRole('button', { name: 'Create policy' }).click()
  await expect(page.getByText('Policy created.')).toBeVisible()
  await page.getByLabel('Target Policy ID').fill('m12-browser-policy')
  await page.getByLabel('Policy revision').fill('1')
  await page.getByRole('button', { name: 'Create key' }).click()
  const dialog = page.getByRole('dialog', { name: 'Copy your new key secret' })
  await expect(dialog).toBeVisible()
  const secret = await dialog.locator('code').textContent()
  expect(secret).toBeTruthy()
  await expect(dialog.getByRole('heading')).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(dialog.getByRole('button', { name: 'Dismiss secret' })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()

  const inference = await request.post(`${fixture.client}/v1/responses`, {
    headers: { Authorization: `Bearer ${secret}` },
    data: { model: 'm12-model', input: 'browser synthetic request' },
  })
  expect(inference.status()).toBe(200)
  expect(await inference.text()).toContain('m12-fake-response')
  await page.getByRole('link', { name: 'Usage' }).click()
  await expect(page.getByText(/m12-model · succeeded · 1 attempts/)).toBeVisible()

  await page.getByRole('link', { name: 'Keys & Policies' }).click()
  await page.getByRole('button', { name: 'Revoke', exact: true }).click()
  await expect(page.getByText('Key revoked.')).toBeVisible()
  const afterRevoke = await request.post(`${fixture.client}/v1/responses`, {
    headers: { Authorization: `Bearer ${secret}` },
    data: { model: 'm12-model', input: 'must be rejected' },
  })
  expect(afterRevoke.status()).toBe(401)

  await page.getByRole('button', { name: 'Log out' }).click()
  await expect(page.getByRole('heading', { name: 'PestiRoute administration' })).toBeVisible()
  await expect(page.getByLabel('Operator bearer token')).toHaveValue('')
  expect(await page.evaluate(() => ({ local: localStorage.length, session: sessionStorage.length, cookies: document.cookie })))
    .toEqual({ local: 0, session: 0, cookies: '' })
})
