import { test, expect } from '@playwright/test'
import { openApp, gotoTestSession } from '../helpers'

/**
 * Regression test for the "whole terminal takes one bright colour" bug.
 *
 * wterm's renderer sets the `.term-grid` container's inline `background`
 * to the bottom-right cell's background colour (a heuristic for apps that
 * paint a uniform full-screen background). TUIs like pi colour their
 * footer/status bottom line, so the bottom-right cell is coloured and that
 * colour bleeds across the entire terminal behind the (transparent) rows.
 *
 * gmux overrides the grid container background so it never inherits that
 * cell colour; real per-row/per-cell backgrounds still paint normally.
 */
test.describe('terminal grid background', () => {
  test('coloured bottom-right cell does not bleed across the terminal', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    // Drive the renderer into the bug state: position the cursor at the
    // bottom-right cell, set a bright background, print a space, reset.
    await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const rows: number = term.rows
      const cols: number = term.cols
      const seq = `\x1b[${rows};${cols}H\x1b[48;2;180;60;200m \x1b[0m`
      term.write(new TextEncoder().encode(seq))
    })

    // The renderer flushes on an animation frame, so wait until it has
    // applied the inline grid background (confirms the bug path ran).
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const grid = term.element.querySelector('.term-grid') as HTMLElement
      return grid.style.background.includes('180, 60, 200')
    }, undefined, { timeout: 5_000 })

    // …but the *effective* grid background must stay transparent, so the
    // colour never fills the terminal. (CSS override beats the inline style.)
    const computed = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const grid = term.element.querySelector('.term-grid') as HTMLElement
      return getComputedStyle(grid).backgroundColor
    })
    expect(computed).toBe('rgba(0, 0, 0, 0)')
  })
})
