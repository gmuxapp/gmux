/**
 * File tree UI e2e tests.
 *
 * Drives a real browser against the embedded gmuxd frontend. Tests navigate
 * to the test project hub (where the file tree renders) and exercise the
 * tree interactions that are not covered by the API spec.
 *
 * NOTE: The file tree is now powered by @pierre/trees which renders inside a
 * shadow DOM. Playwright's getByRole() / getByText() pierce shadow DOM
 * automatically. For direct CSS queries inside the shadow DOM use
 * locator('pierce/.some-class').
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

/**
 * Find a tree item button by its filename/dirname. Uses getByRole which
 * automatically pierces the @pierre/trees shadow DOM.
 */
function treeItem(page: Parameters<typeof openApp>[0], name: string) {
  // @pierre/trees renders each file/directory with role="treeitem".
  // Scope to .ft-tree to avoid matching unrelated items in the page.
  return page.locator('.ft-tree').getByRole('treeitem', { name, exact: true })
}

/**
 * Open the context menu for a tree item.
 * Dispatches contextmenu event directly on the element to avoid coordinate
 * ambiguity with the virtualised tree layout.
 */
async function openContextMenu(page: Parameters<typeof openApp>[0], itemName: string) {
  const item = page.locator(`.ft-tree [data-item-path="${itemName}"]`)
  await item.scrollIntoViewIfNeeded()
  // Dispatch contextmenu directly so the event fires on the exact element,
  // bypassing any bounding-rect offset ambiguity from the virtualised list.
  await item.dispatchEvent('contextmenu', { bubbles: true, cancelable: true })
  // Wait for the context menu to appear (rendered in the light DOM slot).
  await page.locator('.ft-ctx-menu').waitFor({ state: 'visible', timeout: 3_000 })
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
    // getByRole pierces the @pierre/trees shadow DOM.
    await expect(treeItem(page, 'visible-file.ts')).toBeVisible({ timeout: 5_000 })
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
    // initialExpansion:1 means depth-1 directories are open by default.
    // The directory button is a peer of the child file buttons.
    await expect(treeItem(page, 'inner.ts')).toBeVisible({ timeout: 5_000 })
  })

  test('collapses an expanded directory', async ({ page }) => {
    await openProjectHub(page)
    // Ensure the child is visible first (depth-1 expansion is on by default).
    await expect(treeItem(page, 'inner.ts')).toBeVisible({ timeout: 5_000 })
    // Click the directory to collapse it.
    await treeItem(page, 'expand-dir').click()
    await expect(treeItem(page, 'inner.ts')).not.toBeVisible({ timeout: 3_000 })
  })
})

// ── New file inline input ─────────────────────────────────────────────────────

test.describe('new file flow', () => {
  test('shows inline input when + file is clicked', async ({ page }) => {
    await openProjectHub(page)
    await page.locator('button[title="New file"]').click()
    // @pierre/trees renders the rename input inside its shadow DOM.
    // Use data-item-rename-input to avoid matching the search textbox.
    const input = page.locator('[data-item-rename-input="true"]')
    await expect(input).toBeVisible({ timeout: 3_000 })
    await expect(input).toBeFocused()
  })

  test('Escape cancels without creating a file', async ({ page }) => {
    await openProjectHub(page)
    // Count items before.
    const countBefore = await page.locator('.ft-tree-inner').getByRole('treeitem').count()
    await page.locator('button[title="New file"]').click()
    const input = page.locator('[data-item-rename-input="true"]')
    await input.waitFor({ state: 'visible', timeout: 3_000 })
    await page.keyboard.press('Escape')
    await expect(input).not.toBeVisible({ timeout: 3_000 })
    // With removeIfCanceled:true the placeholder disappears — treeitem count unchanged.
    await expect(page.locator('.ft-tree-inner').getByRole('treeitem')).toHaveCount(countBefore)
  })

  test('Enter creates the file via API and adds it to the tree', async ({ page }) => {
    await openProjectHub(page)
    await page.locator('button[title="New file"]').click()
    const input = page.locator('[data-item-rename-input="true"]')
    await input.waitFor({ state: 'visible', timeout: 3_000 })
    await input.fill('e2e-new-file.ts')
    await page.keyboard.press('Enter')
    // File should appear in the tree.
    await expect(treeItem(page, 'e2e-new-file.ts')).toBeVisible({ timeout: 5_000 })
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
    await expect(treeItem(page, 'to-delete.txt')).toBeVisible({ timeout: 5_000 })
    await openContextMenu(page, 'to-delete.txt')
    // Delete button is inside the shadow DOM context menu.
    await page.locator('.ft-tree-inner').locator('.ft-ctx-item--danger').click()
    await expect(page.locator('.ft-modal')).toBeVisible({ timeout: 3_000 })
    await expect(page.locator('.ft-modal-body')).toContainText('to-delete.txt')
  })

  test('Cancel dismisses modal without deleting', async ({ page }) => {
    await openProjectHub(page)
    await expect(treeItem(page, 'to-delete.txt')).toBeVisible({ timeout: 5_000 })
    await openContextMenu(page, 'to-delete.txt')
    await page.locator('.ft-tree-inner').locator('.ft-ctx-item--danger').click()
    await page.locator('.ft-modal').waitFor({ state: 'visible', timeout: 3_000 })
    await page.locator('.ft-modal-cancel').click()
    await expect(page.locator('.ft-modal')).not.toBeVisible()
    expect(fs.existsSync(path.join(workspace(), 'to-delete.txt'))).toBe(true)
  })

  test('Confirm deletes the file and removes it from the tree', async ({ page }) => {
    // Use a fresh file so the previous cancel test doesn't interfere.
    fs.writeFileSync(path.join(workspace(), 'to-delete-confirm.txt'), '')
    await openProjectHub(page)
    await expect(treeItem(page, 'to-delete-confirm.txt')).toBeVisible({ timeout: 5_000 })
    await openContextMenu(page, 'to-delete-confirm.txt')
    await page.locator('.ft-tree-inner').locator('.ft-ctx-item--danger').click()
    await page.locator('.ft-modal').waitFor({ state: 'visible', timeout: 3_000 })
    // Assert the modal targets the right file before confirming.
    await expect(page.locator('.ft-modal-body')).toContainText('to-delete-confirm.txt')
    await page.locator('.ft-modal-confirm').click()
    // Wait for modal to close (confirms the delete was triggered).
    await expect(page.locator('.ft-modal')).not.toBeVisible({ timeout: 3_000 })
    await expect(treeItem(page, 'to-delete-confirm.txt')).not.toBeVisible({ timeout: 5_000 })
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
    await expect(treeItem(page, 'before-rename.txt')).toBeVisible({ timeout: 5_000 })
    await openContextMenu(page, 'before-rename.txt')
    // Rename is the first context menu item (no extra class).
    await page.locator('.ft-tree-inner').locator('.ft-ctx-item').first().click()
    // Rename input appears inside the shadow DOM.
    await expect(page.locator('[data-item-rename-input="true"]')).toBeVisible({ timeout: 3_000 })
  })

  test('Enter commits rename and updates tree', async ({ page }) => {
    await openProjectHub(page)
    await expect(treeItem(page, 'before-rename.txt')).toBeVisible({ timeout: 5_000 })
    await openContextMenu(page, 'before-rename.txt')
    await page.locator('.ft-tree-inner').locator('.ft-ctx-item').first().click()
    const input = page.locator('[data-item-rename-input="true"]')
    await input.waitFor({ state: 'visible', timeout: 3_000 })
    await input.fill('after-rename.txt')
    await page.keyboard.press('Enter')
    await expect(treeItem(page, 'after-rename.txt')).toBeVisible({ timeout: 5_000 })
    expect(fs.existsSync(path.join(workspace(), 'after-rename.txt'))).toBe(true)
    expect(fs.existsSync(path.join(workspace(), 'before-rename.txt'))).toBe(false)
  })
})
