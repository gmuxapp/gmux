/**
 * Git API e2e tests.
 *
 * Pure HTTP — no browser. Tests the GET /v1/git/{slug}/diff and
 * GET /v1/git/{slug}/status endpoints against the test gmuxd.
 *
 * Note: the test project workspace lives under a system tmpdir (not ~/),
 * so tilde-expansion is covered at the unit-test level (diff_test.go).
 * These tests cover endpoint routing, default-cwd behaviour, and the
 * out-of-root 400 rejection.
 */
import * as path from 'path'
import { test, expect } from '@playwright/test'
import { apiGet } from '../helpers'

const PROJECT = 'test-project'

function workspace(): string {
  const w = process.env.GMUX_TEST_WORKSPACE
  if (!w) throw new Error('GMUX_TEST_WORKSPACE not set; global-setup did not run')
  return w
}

// ── GET /v1/git/{slug}/diff ────────────────────────────────────────────────

test.describe('GET /v1/git/{slug}/diff', () => {
  test('returns 200 with no cwd (defaults to project root)', async () => {
    const { status } = await apiGet(`/v1/git/${PROJECT}/diff`)
    // The test workspace is not a git repo, so output is empty — but the
    // handler must return 200, not 400.
    expect(status).toBe(200)
  })

  test('returns 200 for explicit absolute cwd matching project root', async () => {
    const ws = workspace()
    const { status } = await apiGet(`/v1/git/${PROJECT}/diff?cwd=${encodeURIComponent(ws)}`)
    expect(status).toBe(200)
  })

  test('returns 200 for cwd pointing at a subdirectory of the project root', async () => {
    const ws = workspace()
    const sub = path.join(ws, 'subdir')
    // The subdir doesn't need to exist for git to accept -C; a non-repo just
    // returns an empty diff anyway.
    const { status } = await apiGet(`/v1/git/${PROJECT}/diff?cwd=${encodeURIComponent(sub)}`)
    expect(status).toBe(200)
  })

  test('returns 400 for cwd outside the project root', async () => {
    const { status } = await apiGet(`/v1/git/${PROJECT}/diff?cwd=${encodeURIComponent('/etc/passwd')}`)
    expect(status).toBe(400)
  })

  test('returns 400 for cwd that is a prefix-match false positive', async () => {
    const ws = workspace()
    // "/tmp/gmux-e2e-abc123/workspace-evil" must not be accepted just because
    // it shares the "/tmp/gmux-e2e-abc123/workspace" prefix.
    const evil = ws + '-evil'
    const { status } = await apiGet(`/v1/git/${PROJECT}/diff?cwd=${encodeURIComponent(evil)}`)
    expect(status).toBe(400)
  })

  test('returns 404 for an unknown project slug', async () => {
    const { status } = await apiGet('/v1/git/no-such-project/diff')
    expect(status).toBe(404)
  })
})

// ── GET /v1/git/{slug}/status ──────────────────────────────────────────────

test.describe('GET /v1/git/{slug}/status', () => {
  test('returns 200 with numeric fields', async () => {
    const { status, body } = await apiGet<{
      ok: boolean
      data: { files: number; insertions: number; deletions: number }
    }>(`/v1/git/${PROJECT}/status`)
    expect(status).toBe(200)
    expect(body.ok).toBe(true)
    expect(typeof body.data.files).toBe('number')
    expect(typeof body.data.insertions).toBe('number')
    expect(typeof body.data.deletions).toBe('number')
  })

  test('returns 404 for an unknown project slug', async () => {
    const { status } = await apiGet('/v1/git/no-such-project/status')
    expect(status).toBe(404)
  })
})
