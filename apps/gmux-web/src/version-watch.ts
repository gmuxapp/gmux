// Reload the tab onto the new bundle when the daemon goes ahead of us.
//
// Background: the daemon serves the SPA bundle, so under normal
// operation `__GMUX_VERSION__` (baked in at build time) and
// `health.version` (live from the daemon) match. They drift when the
// daemon is upgraded while a tab stays open.
//
// Strategy: we don't yank the page out from under the user. Instead,
// we wait for the next in-app navigation event (sidebar click, back
// button, programmatic route change) and convert that soft route
// change into a full document load. The browser shows its normal
// navigation transition; the user never sees a "page reloaded"
// flicker, just a route change that happens to also pick up the new
// bundle.
//
// The hazard is a reload loop: if the server keeps serving the same
// stale bundle (CDN cache, browser cache, broken deploy), we'd flip
// back to "stale" immediately and reload on the next click forever.
// The guard is a sessionStorage marker that records *which bundle
// version triggered the reload*. On the next mount, if the marker
// still matches the current bundle, the reload didn't help, and we
// stay quiescent until a version match clears the marker.

import { signal, effect } from '@preact/signals'
import { health } from './store'

const RELOAD_MARKER = 'gmux:reload-from'

export type BundleDecision =
  /** Daemon version not yet known (first mount before SSE arrives).
   *  Distinct from `fresh` because we must NOT touch the loop-guard
   *  marker yet: if we just reloaded with a stale bundle and the
   *  daemon-version effect fires synchronously with `health = null`,
   *  clearing the marker would erase the only signal that the prior
   *  reload happened. */
  | { kind: 'unknown' }
  /** Daemon reported a version that matches ours. Clear the marker:
   *  this is the post-reload happy path or normal steady state. */
  | { kind: 'fresh' }
  /** Mismatch with no prior attempt for this bundle: next nav reloads. */
  | { kind: 'stale' }
  /** We already tried reloading from this exact bundle and the daemon
   *  still reports a different version: stay quiet to avoid looping. */
  | { kind: 'stuck' }

/**
 * Pure decision: given the inputs, what state is the bundle in?
 *
 * `stuck` takes precedence over `stale` because the loop guard is
 * the safety net we never want to bypass; a stuck bundle stays
 * quiescent regardless of what the daemon reports. `unknown` is its
 * own state precisely so the loop guard survives the first effect
 * tick before health arrives.
 */
export function decideBundleState(
  daemonVersion: string | undefined,
  bundleVersion: string,
  reloadMarker: string | null,
): BundleDecision {
  if (!daemonVersion) return { kind: 'unknown' }
  if (daemonVersion === bundleVersion) return { kind: 'fresh' }
  if (reloadMarker === bundleVersion) return { kind: 'stuck' }
  return { kind: 'stale' }
}

/**
 * True while the next in-app navigation should be a full reload.
 * Read by `store.navigate()` to decide between soft (pushState) and
 * hard (location.assign) navigation. Exported for tests; production
 * callers go through `installVersionWatch` + `navigate`.
 */
export const bundleStale = signal(false)

/**
 * If the bundle is stale, do a full document load to `url` and
 * return true (caller skips its own soft-nav). Otherwise return
 * false. Also responsible for setting the loop-guard marker at the
 * moment the reload is committed, so a future mount can detect a
 * stuck bundle.
 *
 * `replace` mirrors `history.replaceState` semantics: when true, the
 * reload uses `location.replace` so the current entry is overwritten
 * rather than a new one being pushed. This matters for callers like
 * `navigateToSession` that re-target the URL bar without intending
 * to grow the back stack (an auto-attach replacing `/foo` with
 * `/foo/shell/abc` shouldn't make Back send the user to `/foo`).
 */
export function navigateWithReload(
  url: string,
  replace: boolean = false,
  storage?: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>,
  bundleVersion: string = __GMUX_VERSION__,
  go: (u: string, replace: boolean) => void = (u, r) => {
    if (r) location.replace(u); else location.assign(u)
  },
): boolean {
  if (!bundleStale.value) return false
  // Resolve `sessionStorage` lazily so callers that never trip the
  // stale guard don't depend on a DOM Storage global being present.
  // Vitest's node environment doesn't ship one; jsdom and browsers do.
  const s = storage ?? sessionStorage
  s.setItem(RELOAD_MARKER, bundleVersion)
  go(url, replace)
  return true
}

let installed = false

export interface InstallOptions {
  /** Override the bundle version (tests; defaults to `__GMUX_VERSION__`). */
  bundleVersion?: string
  /** Override storage (tests; defaults to `sessionStorage`). */
  storage?: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>
}

/**
 * Subscribe the watcher to health changes. Flips `bundleStale` when
 * the daemon's version drifts from ours; clears it (and the loop-guard
 * marker) when the versions re-converge. Idempotent: a second call is
 * a noop until the first install's teardown runs.
 *
 * Returns a teardown function (mainly for tests).
 */
export function installVersionWatch(opts: InstallOptions = {}): () => void {
  if (installed) return () => {}
  installed = true

  const bundleVersion = opts.bundleVersion ?? __GMUX_VERSION__
  const storage = opts.storage ?? sessionStorage

  const dispose = effect(() => {
    const decision = decideBundleState(
      health.value?.version,
      bundleVersion,
      storage.getItem(RELOAD_MARKER),
    )
    switch (decision.kind) {
      case 'unknown':
        // First mount before the daemon health arrives. Crucially
        // we leave the marker alone: if a previous tab triggered a
        // reload and the bundle still hasn't caught up, the marker
        // is the only memory of that. Wait for a real verdict.
        bundleStale.value = false
        break
      case 'fresh':
        // Self-heal: a future mismatch gets one fresh reload attempt.
        // Clearing here covers the post-reload happy path where the
        // new bundle matches the daemon, and the normal steady state.
        storage.removeItem(RELOAD_MARKER)
        bundleStale.value = false
        break
      case 'stuck':
        // Loop guard latched: don't reload on the next nav.
        bundleStale.value = false
        break
      case 'stale':
        bundleStale.value = true
        break
    }
  })

  return () => {
    dispose()
    bundleStale.value = false
    installed = false
  }
}
