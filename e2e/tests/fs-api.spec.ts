/**
 * Filesystem API e2e tests.
 *
 * Pure HTTP — no browser needed. All requests go through the bearer-auth
 * helpers directly to the test gmuxd. The workspace is an empty directory
 * seeded by global-setup at $GMUX_TEST_WORKSPACE.
 *
 * Each describe block creates its own subdirectory under the workspace so
 * tests don't interfere with each other even if they run in unexpected order.
 */
import * as fs from 'fs'
import * as path from 'path'
import { test, expect } from '@playwright/test'
import { apiGet, apiPost, apiDelete } from '../helpers'

const PROJECT = 'test-project'
const fsBase = () => `/v1/fs/${PROJECT}`

function workspace(): string {
  const w = process.env.GMUX_TEST_WORKSPACE
  if (!w) throw new Error('GMUX_TEST_WORKSPACE not set; global-setup did not run')
  return w
}

// Each test group seeds a unique subdirectory so concurrent-ish runs don't collide.
function testDir(label: string): string {
  const dir = path.join(workspace(), label)
  fs.mkdirSync(dir, { recursive: true })
  return dir
}

// ── List ──────────────────────────────────────────────────────────────────────

test.describe('GET /v1/fs/{slug} — list directory', () => {
  test('lists root workspace contents', async () => {
    // Seed a known file directly so we have something to assert on.
    fs.writeFileSync(path.join(workspace(), 'list-seed.txt'), 'hello')
    const { status, body } = await apiGet<{ ok: boolean; data: Array<{ name: string; type: string }> }>(`${fsBase()}`)
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    const names = body.data.map(e => e.name)
    expect(names).toContain('list-seed.txt')
  })

  test('lists a subdirectory', async () => {
    const dir = testDir('list-subdir')
    fs.writeFileSync(path.join(dir, 'child.ts'), '')
    const { status, body } = await apiGet<{ ok: boolean; data: Array<{ name: string; type: string }> }>(
      `${fsBase()}?path=list-subdir`,
    )
    expect(status).toBe(200)
    expect(body.data.map(e => e.name)).toContain('child.ts')
  })

  test('returns dirs before files', async () => {
    const dir = testDir('list-order')
    fs.writeFileSync(path.join(dir, 'aaa.txt'), '')
    fs.mkdirSync(path.join(dir, 'zzz-dir'))
    const { body } = await apiGet<{ ok: boolean; data: Array<{ name: string; type: string }> }>(
      `${fsBase()}?path=list-order`,
    )
    const entries = body.data
    const firstDir = entries.findIndex(e => e.type === 'dir')
    const firstFile = entries.findIndex(e => e.type === 'file')
    expect(firstDir).toBeLessThan(firstFile)
  })

  test('excludes hidden files', async () => {
    const dir = testDir('list-hidden')
    fs.writeFileSync(path.join(dir, '.hidden'), '')
    fs.writeFileSync(path.join(dir, 'visible.txt'), '')
    const { body } = await apiGet<{ ok: boolean; data: Array<{ name: string }> }>(
      `${fsBase()}?path=list-hidden`,
    )
    const names = body.data.map(e => e.name)
    expect(names).not.toContain('.hidden')
    expect(names).toContain('visible.txt')
  })

  test('rejects path traversal', async () => {
    const { status } = await apiGet(`${fsBase()}?path=../../../etc`)
    expect(status).toBe(400)
  })

  test('rejects absolute path', async () => {
    const { status } = await apiGet(`${fsBase()}?path=${encodeURIComponent('/etc/passwd')}`)
    expect(status).toBe(400)
  })
})

// ── Mkdir ─────────────────────────────────────────────────────────────────────

test.describe('POST /v1/fs/{slug}/mkdir', () => {
  test('creates a directory', async () => {
    const { status, body } = await apiPost<{ ok: boolean }>(`${fsBase()}/mkdir`, { path: 'mkdir-test/nested' })
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    expect(fs.existsSync(path.join(workspace(), 'mkdir-test/nested'))).toBe(true)
  })

  test('rejects path traversal', async () => {
    const { status } = await apiPost(`${fsBase()}/mkdir`, { path: '../../evil' })
    expect(status).toBe(400)
  })
})

// ── Create file ───────────────────────────────────────────────────────────────

test.describe('POST /v1/fs/{slug}/create', () => {
  test('creates an empty file', async () => {
    const { status, body } = await apiPost<{ ok: boolean }>(`${fsBase()}/create`, { path: 'create-test/new.ts' })
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    const stat = fs.statSync(path.join(workspace(), 'create-test/new.ts'))
    expect(stat.size).toBe(0)
  })

  test('returns 409 if file already exists', async () => {
    fs.mkdirSync(path.join(workspace(), 'create-exists'), { recursive: true })
    fs.writeFileSync(path.join(workspace(), 'create-exists/dup.ts'), 'content')
    const { status } = await apiPost(`${fsBase()}/create`, { path: 'create-exists/dup.ts' })
    expect(status).toBe(409)
  })

  test('rejects path traversal', async () => {
    const { status } = await apiPost(`${fsBase()}/create`, { path: '../evil.sh' })
    expect(status).toBe(400)
  })
})

// ── Rename ────────────────────────────────────────────────────────────────────

test.describe('POST /v1/fs/{slug}/rename', () => {
  test('renames a file', async () => {
    const dir = testDir('rename-test')
    fs.writeFileSync(path.join(dir, 'before.txt'), 'data')
    const { status, body } = await apiPost<{ ok: boolean }>(`${fsBase()}/rename`, {
      from: 'rename-test/before.txt',
      to: 'rename-test/after.txt',
    })
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    expect(fs.existsSync(path.join(dir, 'before.txt'))).toBe(false)
    expect(fs.existsSync(path.join(dir, 'after.txt'))).toBe(true)
  })

  test('rejects traversal in from', async () => {
    const { status } = await apiPost(`${fsBase()}/rename`, { from: '../../etc/passwd', to: 'safe.txt' })
    expect(status).toBe(400)
  })
})

// ── Move ──────────────────────────────────────────────────────────────────────

test.describe('POST /v1/fs/{slug}/move', () => {
  test('moves a file into a directory', async () => {
    const base = testDir('move-test')
    fs.writeFileSync(path.join(base, 'file.ts'), '')
    fs.mkdirSync(path.join(base, 'subdir'))
    const { status, body } = await apiPost<{ ok: boolean }>(`${fsBase()}/move`, {
      from: 'move-test/file.ts',
      to: 'move-test/subdir',
    })
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    expect(fs.existsSync(path.join(base, 'subdir', 'file.ts'))).toBe(true)
    expect(fs.existsSync(path.join(base, 'file.ts'))).toBe(false)
  })
})

// ── Delete ────────────────────────────────────────────────────────────────────

test.describe('DELETE /v1/fs/{slug}/item', () => {
  test('deletes a file', async () => {
    const dir = testDir('delete-file')
    fs.writeFileSync(path.join(dir, 'target.txt'), '')
    const { status, body } = await apiDelete<{ ok: boolean }>(`${fsBase()}/item`, {
      path: 'delete-file/target.txt',
      recursive: false,
    })
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    expect(fs.existsSync(path.join(dir, 'target.txt'))).toBe(false)
  })

  test('deletes a directory recursively', async () => {
    const dir = testDir('delete-dir')
    fs.writeFileSync(path.join(dir, 'child.txt'), '')
    const { status, body } = await apiDelete<{ ok: boolean }>(`${fsBase()}/item`, {
      path: 'delete-dir',
      recursive: true,
    })
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    expect(fs.existsSync(dir)).toBe(false)
  })

  test('returns 404 for missing path', async () => {
    const { status } = await apiDelete(`${fsBase()}/item`, { path: 'does-not-exist.txt', recursive: false })
    expect(status).toBe(404)
  })

  test('refuses to delete the project root', async () => {
    // path='' resolves to the root itself
    const { status } = await apiDelete(`${fsBase()}/item`, { path: '', recursive: true })
    expect(status).toBe(400)
  })

  test('rejects traversal', async () => {
    const { status } = await apiDelete(`${fsBase()}/item`, { path: '../../etc', recursive: true })
    expect(status).toBe(400)
  })
})
