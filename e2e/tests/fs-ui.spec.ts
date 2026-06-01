/**
 * File tree UI e2e tests.
 *
 * Drives a real browser against the embedded gmuxd frontend. Tests navigate
 * to the test project hub (where the file tree renders) and exercise the
 * tree interactions.
 *
 * @pierre/trees renders into a shadow DOM inside .ft-tree-inner. Playwright's
 * getByRole() and getByText() automatically pierce shadow DOM, so we use
 * ARIA role selectors for tree nodes. Elements in the Preact wrapper (header
 * buttons, delete modal, context menu) are in the light DOM and use CSS
 * selectors as before.
 */
import * as fs from 'fs'
import * as path from 'path'
import { test, expect } from '@playwright/test'
import { openApp } from '../helpers'

const PROJECT = 'test-project'

function workspace(): string {
  const w = process.env.GMUX_TEST_WORKSPACE
  if (!w) throw new Error('GMUX_TEST_WORKSPACE not set; global-setup did not run')
  return w
}

/**
 * Navigate to the project hub and wait for the file tree root to be visible.
 * 10 s timeout — the tree panel waits for the projects API to respond, which
 * can be slow when the daemon is busy after a previous test.
 */
async function openProjectHub(page: Parameters<typeof openApp>[0]) {
  await openApp(page, `/${PROJECT}`)
  await page.locator('.ft-root').waitFor({ state: 'visible', timeout: 5_000 })
}

// ── Rendering ─────────────────────────────────────────────────────────────────

test.describe('file tree rendering', () => {
  test('shows file tree panel on project hub', async ({ page }) => {
    fs.writeFileSync(path.join(workspace(), 'ui-seed.txt'), '')
    await openProjectHub(page)
    await expect(page.locator('.ft-header')).toBeVisible()
    await expect(page.locator('.ft-header-label')).toHaveText('Files')
  })

  test('lists seeded files', async ({ page }) => {
    fs.writeFileSync(path.join(workspace(), 'visible-file.ts'), '')
    await openProjectHub(page)
    // getByRole pierces the @pierre/trees shadow DOM automatically.
    await expect(page.getByRole('treeitem', { name: 'visible-file.ts' })).toBeVisible()
  })

  test('does not show file tree when no project is open', async ({ page }) => {
    await openApp(page, '/')
    await page.waitForTimeout(500)
    await expect(page.locator('.ft-root')).not.toBeVisible()
  })
})

// ── Expand / collapse ─────────────────────────────────────────────────────────

test.describe('directory expand/collapse', () => {
  test.beforeAll(() => {
    const dir = path.join(workspace(), 'expand-dir')
    fs.mkdirSync(dir, { recursive: true })
    fs.writeFileSync(path.join(dir, 'inner.ts'), '')
  })

  test('expands a directory and shows children', async ({ page }) => {
    await openProjectHub(page)
    await page.getByRole('treeitem', { name: 'expand-dir' }).click()
    await expect(page.getByRole('treeitem', { name: 'inner.ts' })).toBeVisible({ timeout: 3_000 })
  })

  test('collapses an expanded directory', async ({ page }) => {
    await openProjectHub(page)
    const dirItem = page.getByRole('treeitem', { name: 'expand-dir' })
    await dirItem.click()
    await page.getByRole('treeitem', { name: 'inner.ts' }).waitFor({ state: 'visible' })
    await dirItem.click()
    await expect(page.getByRole('treeitem', { name: 'inner.ts' })).not.toBeVisible()
  })
})

// ── New file inline input ─────────────────────────────────────────────────────

test.describe('new file flow', () => {
  test('shows inline input when + file is clicked', async ({ page }) => {
    await openProjectHub(page)
    // The search textbox is always present inside the shadow DOM (count = 1).
    // After clicking "New file" a rename input appears (count becomes 2).
    const initialCount = await page.getByRole('textbox').count()
    await page.locator('.ft-header-btn[title="New file"]').click()
    // Wait for a new textbox to appear (the inline rename input).
    await expect(page.getByRole('textbox')).toHaveCount(initialCount + 1, { timeout: 3_000 })
    // @pierre/trees focuses the input inside shadow DOM via element.focus(). Playwright
    // reports the frame as 'inactive' unless we send a real pointer event. Click the input
    // to ensure true focus before asserting toBeFocused().
    const renameInput = page.locator('[data-item-rename-input]')
    await renameInput.click()
    await expect(renameInput).toBeFocused()
  })

  test('Escape cancels without creating a file', async ({ page }) => {
    await openProjectHub(page)
    const beforeCount = await page.getByRole('textbox').count()
    await page.locator('.ft-header-btn[title="New file"]').click()
    // Wait for the inline input to actually appear before reading initialBoxCount.
    await expect(page.getByRole('textbox')).toHaveCount(beforeCount + 1, { timeout: 3_000 })
    const initialBoxCount = await page.getByRole('textbox').count()
    await page.keyboard.press('Escape')
    // Inline input gone.
    await expect(page.getByRole('textbox')).toHaveCount(initialBoxCount - 1, { timeout: 3_000 })
    // No __new-file__ placeholder should exist in the tree or on disk.
    await expect(page.getByRole('treeitem', { name: '__new-file__' })).not.toBeVisible()
    expect(fs.existsSync(path.join(workspace(), '__new-file__'))).toBe(false)
  })

  test('Enter creates the file via API and adds it to the tree', async ({ page }) => {
    await openProjectHub(page)
    await page.locator('.ft-header-btn[title="New file"]').click()
    // The inline rename input should be focused — just type directly.
    // Click the rename input to ensure pointer-event focus (shadow DOM in headless Chrome
    // does not propagate focus to document.activeElement until a pointer event is sent).
    const renameInput = page.locator('[data-item-rename-input]')
    await renameInput.waitFor({ state: 'visible', timeout: 3_000 })
    await renameInput.click()
    await renameInput.fill('e2e-new-file.ts')
    await page.keyboard.press('Enter')
    await expect(
      page.getByRole('treeitem', { name: 'e2e-new-file.ts' }),
    ).toBeVisible({ timeout: 5_000 })
    expect(fs.existsSync(path.join(workspace(), 'e2e-new-file.ts'))).toBe(true)
})

// ── Delete modal ──────────────────────────────────────────────────────────────

test.describe('delete confirmation modal', () => {
  test.beforeAll(() => {
    fs.writeFileSync(path.join(workspace(), 'to-delete.txt'), '')
  })

  test('shows modal when delete is clicked', async ({ page }) => {
    await openProjectHub(page)
    // Right-click opens the context menu rendered by the composition callback
    // (light DOM, class ft-ctx-menu). The danger button triggers delete.
    await page.getByRole('treeitem', { name: 'to-delete.txt' }).click({ button: 'right' })
    await page.locator('.ft-ctx-item--danger').click()
    await expect(page.locator('.ft-modal')).toBeVisible()
    await expect(page.locator('.ft-modal-body')).toContainText('to-delete.txt')
  })

  test('Cancel dismisses modal without deleting', async ({ page }) => {
    await openProjectHub(page)
    await page.getByRole('treeitem', { name: 'to-delete.txt' }).click({ button: 'right' })
    await page.locator('.ft-ctx-item--danger').click()
    await page.locator('.ft-modal-cancel').click()
    await expect(page.locator('.ft-modal')).not.toBeVisible()
    expect(fs.existsSync(path.join(workspace(), 'to-delete.txt'))).toBe(true)
  })

  test('Confirm deletes the file and removes it from the tree', async ({ page }) => {
    fs.writeFileSync(path.join(workspace(), 'to-delete-confirm.txt'), '')
    await openProjectHub(page)
    await page.getByRole('treeitem', { name: 'to-delete-confirm.txt' }).click({ button: 'right' })
    await page.locator('.ft-ctx-item--danger').click()
    await page.locator('.ft-modal-confirm').click()
    await expect(
      page.getByRole('treeitem', { name: 'to-delete-confirm.txt' }),
    ).not.toBeVisible({ timeout: 5_000 })
    expect(fs.existsSync(path.join(workspace(), 'to-delete-confirm.txt'))).toBe(false)
  })
})

// ── Rename ────────────────────────────────────────────────────────────────────

test.describe('inline rename', () => {
  test.beforeAll(() => {
    fs.writeFileSync(path.join(workspace(), 'before-rename.txt'), '')
  })

  test('shows inline input when rename is clicked', async ({ page }) => {
    await openProjectHub(page)
    const initialBoxCount = await page.getByRole('textbox').count()
    await page.getByRole('treeitem', { name: 'before-rename.txt' }).click({ button: 'right' })
    await page.locator('.ft-ctx-item', { hasText: 'Rename' }).click()
    // A new textbox (inline rename input) should appear.
    await expect(page.getByRole('textbox')).toHaveCount(initialBoxCount + 1, { timeout: 3_000 })
  })

  test('Enter commits rename and updates tree', async ({ page }) => {
    // Re-create the file — the previous test may have consumed it via the
    // rename flow if it ran to completion, and beforeAll only runs once.
    fs.writeFileSync(path.join(workspace(), 'before-rename.txt'), '')
    await openProjectHub(page)
    const boxCountBefore = await page.getByRole('textbox').count()
    await page.getByRole('treeitem', { name: 'before-rename.txt' }).click({ button: 'right' })
    await page.locator('.ft-ctx-item', { hasText: 'Rename' }).click()
    // Click rename input to ensure pointer-event focus, then fill the new name.
    const renameInput = page.locator('[data-item-rename-input]')
    await expect(page.getByRole('textbox')).toHaveCount(boxCountBefore + 1, { timeout: 3_000 })
    await renameInput.click()
    await renameInput.fill('after-rename.txt')
    await page.keyboard.press('Enter')
    await expect(
      page.getByRole('treeitem', { name: 'after-rename.txt' }),
    ).toBeVisible({ timeout: 5_000 })
    expect(fs.existsSync(path.join(workspace(), 'after-rename.txt'))).toBe(true)
    expect(fs.existsSync(path.join(workspace(), 'before-rename.txt'))).toBe(false)
  })
})

})
