import type { Terminal } from '@xterm/xterm'
import { WebglAddon } from '@xterm/addon-webgl'

/**
 * Attach the WebGL renderer to a terminal, recovering from context loss.
 *
 * Why this exists: the browser can drop a WebGL context at runtime — most
 * commonly when a laptop suspends/resumes, but also on GPU driver resets or
 * OOM. The WebGL addon recovers transparently if the browser fires
 * `webglcontextrestored` within ~3s (it reinitialises the glyph atlas). But
 * when the context stays lost past that window the addon fires `onContextLoss`
 * and gives up: the terminal is left rendering nothing into a dead context, so
 * any glyph not already in the GPU atlas shows up as a black rectangle until
 * the page is reloaded.
 *
 * A bare `term.loadAddon(new WebglAddon())` in a try/catch only guards the
 * *synchronous* activate() (context unavailable at load time). It does nothing
 * for a context that dies later, which is the common case.
 *
 * On loss we dispose the dead addon — which, importantly, makes xterm fall
 * back to its DOM renderer (WebglAddon.dispose() reinstalls a DomRenderer when
 * the terminal itself is still alive) — and then try to re-attach a fresh
 * WebglAddon on the next frame to regain GPU acceleration (e.g. after a
 * suspend/resume the new context is healthy again).
 *
 * If the context keeps dying in quick succession we stop re-attaching and stay
 * on the DOM renderer rather than thrash. The breaker is windowed, not a
 * lifetime count, so a terminal that's open for days survives the occasional
 * suspend/resume without permanently losing acceleration.
 */

// If the context is lost more than MAX_LOSSES_PER_WINDOW times within
// LOSS_WINDOW_MS, treat the GPU as chronically broken and stay on the DOM
// renderer. Sized generously: onContextLoss only fires ~3s after a loss, so
// genuine thrash means several losses inside a minute, while suspend/resume
// losses are minutes or hours apart and never trip the breaker.
const LOSS_WINDOW_MS = 60_000
const MAX_LOSSES_PER_WINDOW = 3

export function loadWebglRenderer(term: Terminal): void {
  // Timestamps of recent context losses, pruned to LOSS_WINDOW_MS.
  const recentLosses: number[] = []

  const attach = (): void => {
    let addon: WebglAddon
    try {
      addon = new WebglAddon()
    } catch {
      // Context unavailable right now (e.g. WebGL disabled/blocklisted).
      // xterm keeps using its DOM renderer.
      return
    }

    addon.onContextLoss(() => {
      // Tear down the dead addon. This reinstalls the DOM renderer, so the
      // terminal keeps rendering (just unaccelerated) even if we stop here.
      addon.dispose()

      const now = Date.now()
      recentLosses.push(now)
      while (recentLosses.length > 0 && now - recentLosses[0] > LOSS_WINDOW_MS) {
        recentLosses.shift()
      }
      if (recentLosses.length > MAX_LOSSES_PER_WINDOW) {
        // Chronically broken GPU/driver — stop fighting it and stay on DOM.
        return
      }

      // Recreate on the next frame, not synchronously inside the loss event:
      // the GPU is mid-reset and a fresh context isn't reliably available yet.
      requestAnimationFrame(() => {
        // Skip if the terminal was disposed while we waited. dispose() detaches
        // the element from the DOM but does not null term.element, so we test
        // isConnected rather than presence.
        if (!term.element?.isConnected) return
        attach()
      })
    })

    try {
      term.loadAddon(addon)
    } catch {
      // activate() failed synchronously (no context). Fall back to DOM.
      addon.dispose()
    }
  }

  attach()
}
