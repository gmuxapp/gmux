import type { Page } from '@playwright/test'

export interface TermState {
  termCols: number | undefined
  termRows: number | undefined
}

/** Read the xterm terminal dimensions exposed via window.__gmuxTerm. */
export async function getTermState(page: Page): Promise<TermState> {
  return page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    return {
      termCols: term?.cols as number | undefined,
      termRows: term?.rows as number | undefined,
    }
  })
}

/** Check whether the resize pill is visible. */
export async function isPillVisible(page: Page): Promise<boolean> {
  return page.locator('.terminal-resize-overlay').isVisible().catch(() => false)
}

/** Click the first session in the sidebar and wait for the terminal to appear. */
export async function selectFirstSession(page: Page): Promise<void> {
  await page.locator('.session-item').first().click()
  await page.locator('.xterm').waitFor({ state: 'visible', timeout: 5_000 })
  // Give the WS connection time to establish and replay scrollback
  await page.waitForTimeout(1500)
}

/** Click the resize pill. */
export async function clickPill(page: Page): Promise<void> {
  await page.locator('.terminal-resize-overlay').click()
  await page.waitForTimeout(1000)
}
