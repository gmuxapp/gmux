import { test, expect } from '@playwright/test'
import * as fs from 'node:fs'
import * as path from 'node:path'
import { apiGet, pollUntil } from '../helpers'
import {
  type AdapterKind,
  type FixtureSpec,
  appendToSession,
  writeFakeSession,
} from '../fixtures'

/**
 * Watcher-driven conversation discovery spec.
 *
 * Each test creates a fresh adapter session file under the daemon's
 * fakeHome at runtime (after gmuxd is already up) and asserts that
 * the public API reflects the change within a tight timeout. The
 * watcher path should be sub-second; 2s is the ceiling that catches
 * regression to a periodic scan (which would land on a tick \u22650.5s
 * away if reintroduced, and likely \u226530s).
 *
 * The harness's only alive session is `shell`. Pi, claude, and codex
 * never have alive sessions in this test daemon. That means tests
 * 1\u20133 implicitly verify always-on root watches: if the rewrite
 * regressed to session-gated roots, the pi/claude/codex watches
 * wouldn't exist, and these tests would time out.
 */

interface ConvBody {
  data: { kind: string; title: string; cwd: string; slug: string }
}

const TEST_HOME = (): string => {
  const home = process.env.GMUX_TEST_HOME
  if (!home) throw new Error('GMUX_TEST_HOME not set; global-setup did not run')
  return home
}

/** Poll /v1/conversations/{kind}/{slug} until it returns 200, or timeout. */
async function awaitIndexed(kind: AdapterKind, slug: string, timeoutMs = 2_000): Promise<ConvBody['data']> {
  return pollUntil(
    async () => {
      const { status, body } = await apiGet<ConvBody>(`/v1/conversations/${kind}/${slug}`)
      if (status !== 200) return null
      return body.data
    },
    { timeoutMs, description: `${kind}/${slug} reachable via API` },
  )
}

/** Poll until /v1/conversations/{kind}/{slug} returns 404, or timeout. */
async function awaitRemoved(kind: AdapterKind, slug: string, timeoutMs = 2_000): Promise<void> {
  await pollUntil(
    async () => {
      const { status } = await apiGet<unknown>(`/v1/conversations/${kind}/${slug}`)
      return status === 404 ? true : null
    },
    { timeoutMs, description: `${kind}/${slug} removed from API` },
  )
}

/**
 * Build a unique fixture for a test. Synthetic cwd embeds the test
 * name + a random suffix so concurrent tests (or repeat runs) don't
 * collide on slug/path.
 */
function uniqueFixture(kind: AdapterKind, label: string): FixtureSpec {
  const tag = `${label}-${Math.random().toString(36).slice(2, 10)}`
  return {
    kind,
    cwd: `/var/gmux-e2e/discovery/${tag}`,
    toolID: `disc-${tag}`,
    title: `${kind} discovery ${tag}`,
  }
}

test.describe('conversation discovery (watcher-driven)', () => {
  // Track files created in each test for cleanup. Unique slugs make
  // tests independent regardless, but cleanup keeps the daemon's
  // index from accumulating cruft over a long test run.
  const filesCreated: string[] = []
  test.afterEach(() => {
    while (filesCreated.length) {
      const p = filesCreated.pop()!
      try { fs.unlinkSync(p) } catch { /* already gone */ }
    }
  })

  for (const kind of ['pi', 'claude', 'codex'] as const) {
    test(`${kind}: write session file -> reachable in API within 2s`, async () => {
      const spec = uniqueFixture(kind, 'create')
      const { filePath, expectedSlug } = writeFakeSession(TEST_HOME(), spec)
      filesCreated.push(filePath)

      const result = await awaitIndexed(kind, expectedSlug)
      expect(result.kind).toBe(kind)
      expect(result.title).toBe(spec.title)
      expect(result.cwd).toBe(spec.cwd)
      expect(result.slug).toBe(expectedSlug)
    })
  }

  test('claude: appending custom-title updates API title within 2s', async () => {
    // Initial fixture: title comes from the user message.
    const spec = uniqueFixture('claude', 'retitle')
    const { filePath, expectedSlug } = writeFakeSession(TEST_HOME(), spec)
    filesCreated.push(filePath)

    // Wait for initial indexing.
    const initial = await awaitIndexed('claude', expectedSlug)
    expect(initial.title).toBe(spec.title)

    // Append a `custom-title` line. Claude's parser prefers
    // custom-title over first-user-message text, so the indexed
    // title should update on re-parse. The slug stays stable across
    // updates (Upsert preserves it via byToolID, so existing URLs
    // don't break) — we query the same slug and expect a new title.
    const newTitle = 'claude custom retitled'
    appendToSession(filePath, { type: 'custom-title', customTitle: newTitle })

    await pollUntil(
      async () => {
        const { status, body } = await apiGet<ConvBody>(`/v1/conversations/claude/${expectedSlug}`)
        if (status !== 200) return null
        return body.data.title === newTitle ? body.data : null
      },
      { description: `claude/${expectedSlug} title updated to ${newTitle}` },
    )
  })

  test('pi: deleting session file -> 404 within 2s', async () => {
    const spec = uniqueFixture('pi', 'delete')
    const { filePath, expectedSlug } = writeFakeSession(TEST_HOME(), spec)
    // Don't push to filesCreated; the test deletes the file itself.

    await awaitIndexed('pi', expectedSlug)

    // Remove triggers fsnotify Remove -> RemoveByPath.
    fs.unlinkSync(filePath)

    await awaitRemoved('pi', expectedSlug)
  })

  test('pi: write into a brand-new cwd subdirectory -> reachable within 2s', async () => {
    // The synthetic cwd is unique per test; its encoded subdirectory
    // doesn't exist yet under $HOME/.pi/agent/sessions/. Creating it
    // exercises the always-on root-watch path:
    //   1. mkdir of new subdir -> Create event under root
    //   2. handleNewSubdirLocked adds a watch on the new subdir
    //   3. write of .jsonl inside -> Create event picked up by the new watch
    //
    // If the rewrite regressed to session-gated root watches, step 1
    // wouldn't fire (root not watched without an alive pi session),
    // and the test would time out.
    const spec = uniqueFixture('pi', 'newdir')
    const { filePath, expectedSlug } = writeFakeSession(TEST_HOME(), spec)
    filesCreated.push(filePath)

    // Sanity check that we did create a previously-nonexistent subdir.
    const subdir = path.dirname(filePath)
    expect(fs.existsSync(subdir)).toBe(true)

    const result = await awaitIndexed('pi', expectedSlug)
    expect(result.cwd).toBe(spec.cwd)
  })
})
