import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
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
  await expect(page.getByText('Gateway readiness')).toBeVisible()
  await expect(page.getByText(/Gateway vdev/)).toBeVisible()
  await expect(page.getByText('m12-connector')).toBeVisible()
  await page.getByRole('link', { name: 'Settings & Diagnostics' }).click()
  await expect(page.getByRole('heading', { name: 'Settings & Diagnostics' })).toBeVisible()
  await expect(page.locator('.diagnostic-summary')).toContainText('unknown')

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
  await expect(dialog.getByRole('button', { name: 'Copy key' })).toBeFocused()
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
  await expect(page.locator('tbody tr').filter({ hasText: 'm12-model' }).filter({ hasText: 'succeeded' }).first()).toBeVisible()

  await page.getByRole('link', { name: 'Keys & Policies' }).click()
  await page.getByRole('button', { name: 'Revoke', exact: true }).click()
  const revokeDialog = page.getByRole('dialog', { name: /Revoke/ })
  await expect(revokeDialog).toBeVisible()
  await expect(revokeDialog.getByRole('heading')).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(revokeDialog).toBeHidden()
  await expect(page.getByRole('button', { name: 'Revoke', exact: true })).toBeFocused()
  await page.getByRole('button', { name: 'Revoke', exact: true }).click()
  await expect(revokeDialog.getByRole('heading')).toBeFocused()
  await revokeDialog.getByRole('button', { name: /Confirm revoke/ }).click()
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

test('all live screens fit audited viewports in light and dark themes', async ({ page }) => {
  const dir = process.env.M12_VISUAL_DIR ?? join(root, 'web/test-results/m12-010')
  mkdirSync(dir, { recursive: true })
  await page.goto(`${fixture.admin}/admin/`)
  await page.getByLabel('Operator bearer token').fill(fixture.operator_token)
  await page.getByRole('button', { name: 'Connect' }).click()

  // Seed a real ledger event independently of the preceding lifecycle test.
  await page.getByRole('link', { name: 'Keys & Policies' }).click()
  const issueKey = async (width: number) => {
    await page.setViewportSize({ width, height: 900 })
    await page.getByLabel('Target Policy ID').fill('m12-route-policy')
    await page.getByLabel('Policy revision').fill('1')
    await page.getByRole('button', { name: 'Create key' }).click()
    const dialog = page.getByRole('dialog', { name: 'Copy your new key secret' })
    await expect(dialog).toBeVisible()
    const box = await dialog.boundingBox()
    expect(box).not.toBeNull()
    expect(box!.x).toBeGreaterThanOrEqual(0)
    expect(box!.y).toBeGreaterThanOrEqual(0)
    expect(box!.x + box!.width).toBeLessThanOrEqual(width)
    expect(box!.y + box!.height).toBeLessThanOrEqual(900)
    await expect(dialog.getByRole('heading')).toBeFocused()
    await page.keyboard.press('Tab')
    await expect(dialog.getByRole('button', { name: 'Copy key' })).toBeFocused()
    await page.keyboard.press('Tab')
    await expect(dialog.getByRole('button', { name: 'Dismiss secret' })).toBeFocused()
    await page.keyboard.press('Tab')
    await expect(dialog.getByRole('button', { name: 'Copy key' })).toBeFocused()
    await page.keyboard.press('Shift+Tab')
    await expect(dialog.getByRole('button', { name: 'Dismiss secret' })).toBeFocused()
    const secret = await dialog.locator('code').textContent()
    expect(secret).toBeTruthy()
    await page.keyboard.press('Escape')
    await expect(dialog).toBeHidden()
    await expect(page.getByRole('button', { name: 'Create key' })).toBeFocused()
    return secret!
  }
  const secret = await issueKey(375)
  await issueKey(420)
  const inference = await page.request.post(`${fixture.client}/v1/responses`, {
    headers: { Authorization: `Bearer ${secret}` },
    data: { model: 'm12-model', input: 'viewport audit seed' },
  })
  expect(inference.status()).toBe(200)

  const sizes = [375, 420, 760, 1024, 1440]
  const screens = ['Overview', 'Connections', 'Keys & Policies', 'Usage', 'Routing', 'Settings & Diagnostics']
  for (const theme of ['light', 'dark']) {
    if (theme === 'dark') await page.getByRole('button', { name: 'Switch to dark theme' }).click()
    for (const width of sizes) {
      await page.setViewportSize({ width, height: 900 })
      for (const screen of screens) {
        await page.getByRole('link', { name: screen }).click()
        await expect(page.locator('#page-title')).toHaveText(screen)
        await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
        const textRatio = await page.locator('#page-title').evaluate(element => {
          const rgb = (value: string) => value.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []
          const luminance = (value: string) => {
            const components = rgb(value).map(channel => { const c = channel / 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4 })
            return components[0] * 0.2126 + components[1] * 0.7152 + components[2] * 0.0722
          }
          const style = getComputedStyle(element)
          let background = style.backgroundColor
          for (let parent = element.parentElement; parent && background.startsWith('rgba') && Number(background.match(/[\d.]+/g)?.[3]) === 0; parent = parent.parentElement) background = getComputedStyle(parent).backgroundColor
          const foregroundLum = luminance(style.color), backgroundLum = luminance(background)
          return (Math.max(foregroundLum, backgroundLum) + 0.05) / (Math.min(foregroundLum, backgroundLum) + 0.05)
        })
        expect(textRatio, `${theme} ${screen} heading contrast`).toBeGreaterThanOrEqual(4.5)
        const badge = page.locator('.badge').first()
        await expect(badge).toBeVisible()
        const badgeRatio = await badge.evaluate(element => {
          const rgb = (value: string) => value.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []
          const luminance = (value: string) => rgb(value).map(channel => { const c = channel / 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4 }).reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0)
          const style = getComputedStyle(element), foreground = luminance(style.color), background = luminance(style.backgroundColor)
          return (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05)
        })
        expect(badgeRatio, `${theme} ${screen} badge contrast`).toBeGreaterThanOrEqual(4.5)
        const capabilityBadge = page.locator('.capability-list .badge, .route-capabilities .badge').first()
        if (await capabilityBadge.count()) {
          const capabilityRatio = await capabilityBadge.evaluate(element => {
            const rgb = (value: string) => value.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []
            const luminance = (value: string) => rgb(value).map(channel => { const c = channel / 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4 }).reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0)
            const style = getComputedStyle(element), foreground = luminance(style.color), background = luminance(style.backgroundColor)
            return (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05)
          })
          expect(capabilityRatio, `${theme} ${screen} capability badge contrast`).toBeGreaterThanOrEqual(4.5)
        }
        const actionButton = page.locator('main button:not(:disabled)').first()
        const buttonRatio = await actionButton.evaluate(element => {
          const rgb = (value: string) => value.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []
          const luminance = (value: string) => rgb(value).map(channel => { const c = channel / 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4 }).reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0)
          const style = getComputedStyle(element), foreground = luminance(style.color), background = luminance(style.backgroundColor)
          return (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05)
        })
        expect(buttonRatio, `${theme} ${screen} enabled action text contrast`).toBeGreaterThanOrEqual(4.5)
        if (screen === 'Connections' || screen === 'Usage') {
          const input = page.locator('input').first()
          const inputRatio = await input.evaluate(element => {
            const rgb = (value: string) => value.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []
            const luminance = (value: string) => rgb(value).map(channel => { const c = channel / 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4 }).reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0)
            const style = getComputedStyle(element), foreground = luminance(style.color), background = luminance(style.backgroundColor)
            return (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05)
          })
          expect(inputRatio, `${theme} ${screen} input contrast`).toBeGreaterThanOrEqual(4.5)
        }
        await page.keyboard.press('Tab')
        const focus = await page.evaluate(() => {
          const element = document.activeElement!, style = getComputedStyle(element)
          const rgb = (value: string) => value.match(/[\d.]+/g)?.slice(0, 3).map(Number) ?? []
          const luminance = (value: string) => rgb(value).map(channel => { const c = channel / 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4 }).reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0)
          let background = style.backgroundColor
          for (let parent = element.parentElement; parent && background.startsWith('rgba') && Number(background.match(/[\d.]+/g)?.[3]) === 0; parent = parent.parentElement) background = getComputedStyle(parent).backgroundColor
          const focusLum = luminance(style.outlineColor), backgroundLum = luminance(background)
          return {
            visible: element.matches(':focus-visible'), width: parseFloat(style.outlineWidth), style: style.outlineStyle,
            color: style.outlineColor,
            contrast: (Math.max(focusLum, backgroundLum) + 0.05) / (Math.min(focusLum, backgroundLum) + 0.05),
          }
        })
        expect(focus.visible, `${theme} ${screen} keyboard focus-visible`).toBe(true)
        expect(focus.width).toBeGreaterThanOrEqual(2)
        expect(focus.style).not.toBe('none')
        expect(focus.color).not.toBe('rgba(0, 0, 0, 0)')
        expect(focus.contrast, `${theme} ${screen} focus contrast`).toBeGreaterThanOrEqual(3)
        await page.keyboard.press('Shift+Tab')
        if (screen === 'Connections' && width <= 760) {
          const table = await page.locator('.table-wrap').boundingBox()
          const bounds = await page.locator('.table-wrap').evaluate(element => ({ client: element.clientWidth, scroll: element.scrollWidth }))
          expect(bounds.scroll).toBeGreaterThan(bounds.client)
          expect(table!.x + table!.width).toBeLessThanOrEqual(width)
        }
        if (screen === 'Connections') {
          const columns = await page.locator('.management-grid').first().evaluate(element => getComputedStyle(element).gridTemplateColumns.split(' ').length)
          expect(columns).toBe(width <= 760 ? 1 : 2)
        }
        if (screen === 'Usage') {
          const columns = await page.locator('[aria-labelledby="usage-filters"] .management-grid').evaluate(element => getComputedStyle(element).gridTemplateColumns.split(' ').length)
          expect(columns).toBe(width <= 420 ? 1 : width <= 760 ? 2 : 4)
        }
        if (screen === 'Overview' && width <= 420) {
          const id = await page.locator('.activity-id').first().boundingBox()
          const model = await page.locator('.activity-model').first().boundingBox()
          expect(id).not.toBeNull()
          expect(model).not.toBeNull()
          expect(id!.x + id!.width).toBeLessThanOrEqual(model!.x)
        }
        await page.screenshot({ path: `${dir}/${theme}-${width}-${screen.toLowerCase().replaceAll(/[^a-z]+/g, '-')}.png`, fullPage: true })
      }
    }
    if (theme === 'dark') await page.getByRole('button', { name: 'Switch to light theme' }).click()
  }
})
