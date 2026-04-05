import { test, expect } from '@playwright/test'
import { openApp, selectFirstSession } from '../helpers'

test.describe('connection lost', () => {
  test('shows disconnected pill when backend stops', async ({ page }) => {
    await openApp(page)
    await selectFirstSession(page)

    // Verify we're connected — no disconnected pill
    const pill = page.locator('.terminal-disconnected-pill')
    await expect(pill).not.toBeVisible()

    // Kill the gmuxd process via the page's fetch (triggers connection drop)
    // Instead, we can just close the WebSocket from the page side to simulate
    // a backend going away. But for a real test, we kill the process.
    const fs = await import('fs')
    const os = await import('os')
    const path = await import('path')
    const stateFile = path.join(os.tmpdir(), 'gmux-e2e-state.json')
    const state = JSON.parse(fs.readFileSync(stateFile, 'utf8'))
    process.kill(state.pids[0], 'SIGTERM')

    // Disconnected pill should appear
    await expect(pill).toBeVisible({ timeout: 10_000 })
    await expect(pill).toContainText('Connection lost')
  })
})
