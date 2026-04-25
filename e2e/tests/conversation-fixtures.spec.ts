import { test, expect } from '@playwright/test'
import { apiGet, pollUntil } from '../helpers'
import { SMOKE_FIXTURES, slugify } from '../fixtures'

/**
 * Smoke spec: pre-seeded fixtures (written by global-setup before gmuxd
 * starts) must be reachable at /v1/conversations/{kind}/{slug} once the
 * bootstrap scan completes.
 *
 * Purpose: detect drift between the TS fixtures in `e2e/fixtures.ts`
 * and the Go parsers in `packages/adapter/adapters/*.go`. If a parser
 * grows a new required field and the TS fixture doesn't update, this
 * spec fails first with a clear signal pointing at fixture validity,
 * before the discovery spec's tests run.
 *
 * The tests assert via the public API only (no introspection). The
 * polling timeout is generous because we only need the bootstrap scan
 * to have completed once at startup; in practice it's done before the
 * first test even runs.
 */
test.describe('conversation fixtures (bootstrap scan)', () => {
  // Each smoke fixture rendered as one parameterized test, so a
  // failure names the offending kind directly.
  for (const fixture of SMOKE_FIXTURES) {
    test(`${fixture.kind}: pre-seeded fixture reachable via API`, async () => {
      const expectedSlug = slugify(fixture.title)
      // Bootstrap is one-shot at daemon start, but we still poll: the
      // first test in the run might race the daemon's `Scan()` call,
      // and on slow CI the index population can lag startup by tens
      // of milliseconds.
      const result = await pollUntil(
        async () => {
          const { status, body } = await apiGet<{ data: { kind: string; title: string; cwd: string } }>(
            `/v1/conversations/${fixture.kind}/${expectedSlug}`,
          )
          if (status !== 200) return null
          return body.data
        },
        {
          timeoutMs: 5_000,
          description: `fixture ${fixture.kind}/${expectedSlug} reachable via API`,
        },
      )

      expect(result.kind).toBe(fixture.kind)
      expect(result.title).toBe(fixture.title)
      expect(result.cwd).toBe(fixture.cwd)
    })
  }
})
