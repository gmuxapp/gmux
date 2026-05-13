/**
 * File tree UI e2e tests.
 *
 * Drives a real browser against the embedded gmuxd frontend. Tests navigate
 * to the test project hub (where the file tree renders) and exercise the
 * tree interactions that are not covered by the API spec.
 */
import * as fs from 'fs'
import * as path from 'path'
import { test, expect } from '@playwright/test'
import { openApp, apiPost } from '../helpers'

const PROJECT = 'test-project'

function workspace(): string {
  const w = process.env.GMUX_TEST_WORKSPACE
  if (!w) throw new Error('GMUX_TEST_WORKSPACE not set; global-setup did not run')
  return w
}

/** Navigate to the project hub and wait for the file tree root to be visible. */
async function openProjectHub(page: Parameters<typeof openApp>[0]) {
  await openApp(page, `/${PROJECT}`)
  await page.locator('.ft-root').waitFor({ state: 'visible', timeout: 5_000 })
}

// ── Rendering ─────────────────────────────────────────────────────────────────

test.describe('file tree rendering', () => {
  test('shows file tree panel on project hub', async ({ page }) => {
    // Seed a file so the tree has something to show.
    fs.writeFileSync(path.join(workspace(), 'ui-seed.txt'), '')
    await openProjectHub(page)
    await expect(page.locator('.ft-header')).toBeVisible()
    await expect(page.locator('.ft-header-label')).toHaveText('Files')
  })

  test('lists seeded files', async ({ page }) => {
    fs.writeFileSync(path.join(workspace(), 'visible-file.ts'), '')
    await openProjectHub(page)
    await expect(page.locator('.ft-node-name', { hasText: 'visible-file.ts' })).toBeVisible()
  })

  test('does not show file tree when no project is open', async ({ page }) => {
    await openApp(page, '/')
    // Home route — no project active, tree must not render.
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
    const dirNode = page.locator('.ft-node', { hasText: 'expand-dir' }).first()
    await dirNode.click()
    await expect(page.locator('.ft-node-name', { hasText: 'inner.ts' })).toBeVisible({ timeout: 3_000 })
  })

  test('collapses an expanded directory', async ({ page }) => {
    await openProjectHub(page)
    const dirNode = page.locator('.ft-node', { hasText: 'expand-dir' }).first()
    // Expand
    await dirNode.click()
    await page.locator('.ft-node-name', { hasText: 'inner.ts' }).waitFor({ state: 'visible' })
    // Collapse
    await dirNode.click()
    await expect(page.locator('.ft-node-name', { hasText: 'inner.ts' })).not.toBeVisible()
  })
})

// ── New file inline input ─────────────────────────────────────────────────────

test.describe('new file flow', () => {
  test('shows inline input when + file is clicked', async ({ page }) => {
    await openProjectHub(page)
    await page.locator('.ft-header').hover()
    await page.locator('.ft-header-btn', { hasText: '' }).first().click()
    await expect(page.locator('.ft-inline-input')).toBeVisible()
    await expect(page.locator('.ft-inline-input')).toBeFocused()
  })

  test('Escape cancels without creating a file', async ({ page }) => {
    await openProjectHub(page)
    const countBefore = await page.locator('.ft-node-name').count()
    await page.locator('.ft-header').hover()
    await page.locator('.ft-header-btn').first().click()
    await page.locator('.ft-inline-input').waitFor({ state: 'visible' })
    await page.keyboard.press('Escape')
    await expect(page.locator('.ft-inline-input')).not.toBeVisible()
    // Node count should be unchanged (no file was created).
    await expect(page.locator('.ft-node-name')).toHaveCount(countBefore)
  })

  test('Enter creates the file via API and adds it to the tree', async ({ page }) => {
    await openProjectHub(page)
    await page.locator('.ft-header').hover()
    await page.locator('.ft-header-btn').first().click()
    await page.locator('.ft-inline-input').fill('e2e-new-file.ts')
    await page.keyboard.press('Enter')
    // File should appear in the tree.
    await expect(
      page.locator('.ft-node-name', { hasText: 'e2e-new-file.ts' }),
    ).toBeVisible({ timeout: 5_000 })
    // And exist on disk.
    expect(fs.existsSync(path.join(workspace(), 'e2e-new-file.ts'))).toBe(true)
  })
})

// ── Delete modal ──────────────────────────────────────────────────────────────

test.describe('delete confirmation modal', () => {
  test.beforeAll(() => {
    fs.writeFileSync(path.join(workspace(), 'to-delete.txt'), '')
  })

  test('shows modal when delete is clicked', async ({ page }) => {
    await openProjectHub(page)
    const fileNode = page.locator('.ft-node', { hasText: 'to-delete.txt' }).first()
    await fileNode.hover()
    await fileNode.locator('.ft-action-delete').click()
    await expect(page.locator('.ft-modal')).toBeVisible()
    await expect(page.locator('.ft-modal-body')).toContainText('to-delete.txt')
  })

  test('Cancel dismisses modal without deleting', async ({ page }) => {
    await openProjectHub(page)
    const fileNode = page.locator('.ft-node', { hasText: 'to-delete.txt' }).first()
    await fileNode.hover()
    await fileNode.locator('.ft-action-delete').click()
    await page.locator('.ft-modal-cancel').click()
    await expect(page.locator('.ft-modal')).not.toBeVisible()
    expect(fs.existsSync(path.join(workspace(), 'to-delete.txt'))).toBe(true)
  })

  test('Confirm deletes the file and removes it from the tree', async ({ page }) => {
    // Use a fresh file so the previous cancel test doesn't interfere.
    fs.writeFileSync(path.join(workspace(), 'to-delete-confirm.txt'), '')
    await openProjectHub(page)
    const fileNode = page.locator('.ft-node', { hasText: 'to-delete-confirm.txt' }).first()
    await fileNode.hover()
    await fileNode.locator('.ft-action-delete').click()
    await page.locator('.ft-modal-confirm').click()
    await expect(
      page.locator('.ft-node-name', { hasText: 'to-delete-confirm.txt' }),
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
    const fileNode = page.locator('.ft-node', { hasText: 'before-rename.txt' }).first()
    await fileNode.hover()
    await fileNode.locator('button[title="Rename"]').click()
    await expect(page.locator('.ft-inline-input')).toBeVisible()
  })

  test('Enter commits rename and updates tree', async ({ page }) => {
    await openProjectHub(page)
    const fileNode = page.locator('.ft-node', { hasText: 'before-rename.txt' }).first()
    await fileNode.hover()
    await fileNode.locator('button[title="Rename"]').click()
    const input = page.locator('.ft-inline-input')
    await input.fill('after-rename.txt')
    await page.keyboard.press('Enter')
    await expect(
      page.locator('.ft-node-name', { hasText: 'after-rename.txt' }),
    ).toBeVisible({ timeout: 5_000 })
    expect(fs.existsSync(path.join(workspace(), 'after-rename.txt'))).toBe(true)
    expect(fs.existsSync(path.join(workspace(), 'before-rename.txt'))).toBe(false)
  })
})
