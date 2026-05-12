import { useEffect, useRef, useState } from 'preact/hooks'
import { Terminal, FitAddon, UrlRegexProvider, OSC8LinkProvider, init as initGhostty } from 'ghostty-web'
import type { ITerminalOptions } from 'ghostty-web'
import type { Session } from './types'
import type { ResolvedTerminalOptions } from './settings-schema'
import { fetchScrollback, type ScrollbackResult } from './replay-fetch'
import { JumpToBottom } from './jump-to-bottom'

// gmuxd caps scrollback at 1 MiB × 2 files (~2 MiB max). The default
// scrollback line cap (1000) would silently truncate most of that for
// text-heavy sessions; bump it for replay so the user can scroll the full
// captured history.
const REPLAY_SCROLLBACK_LINES = 10000

function buildReplayOptions(opts: ResolvedTerminalOptions): ITerminalOptions {
  return {
    fontSize: opts.fontSize,
    fontFamily: opts.fontFamily,
    cursorBlink: false,
    cursorStyle: opts.cursorStyle,
    theme: opts.theme,
    scrollback: REPLAY_SCROLLBACK_LINES,
    disableStdin: true,
    smoothScrollDuration: opts.smoothScrollDuration,
  }
}

type ReplayState =
  | { kind: 'loading' }
  | ScrollbackResult

// Adapter kinds whose runners have an explicit resume protocol
// (--resume <id> or equivalent). Anything not in this set falls back
// to "Rerun" because there's no captured agent state to pick up from;
// re-launching just runs the original command again. Listed
// explicitly so adding a new agent adapter is a deliberate one-line
// change here, and unknown kinds default to the safe "Rerun" label.
const RESUMABLE_AGENT_KINDS = new Set(['claude', 'codex', 'pi'])

function resumeButtonLabel(kind: string, busy: boolean): string {
  const isAgent = RESUMABLE_AGENT_KINDS.has(kind)
  if (busy) return isAgent ? 'Resuming…' : 'Rerunning…'
  return isAgent ? 'Resume' : 'Rerun'
}

/**
 * Read-only xterm view that replays a dead session's persisted scrollback
 * from the gmuxd broker. No WebSocket, no input, no resize messages: this
 * is purely a viewer for bytes that already happened.
 *
 * The terminal is recreated when `session.id` changes (matching the
 * sidebar-click model). Live sessions go through TerminalView instead;
 * see main.tsx for the routing.
 *
 * The action bar at the bottom carries the lifecycle controls that
 * previously lived as auto-trigger-on-click in the sidebar: Resume /
 * Rerun (if the adapter is resumable). Promoting it out of an implicit
 * click means clicking a dead session navigates to its scrollback
 * first, and any state-changing action is a deliberate second click.
 *
 * The button label depends on the adapter kind: agent adapters
 * (claude/codex/pi) say "Resume" because they have explicit resume
 * semantics (`--resume <id>`), shells and one-off commands say "Rerun"
 * because there's no state to resume — re-launching just runs the
 * command again. Dismissal is intentionally not exposed here; the
 * sidebar's per-session close affordance is the single way to remove
 * a dead session.
 */
export function ReplayView({
  session,
  terminalOptions,
  onResume,
  resuming,
}: {
  session: Session
  terminalOptions: ResolvedTerminalOptions
  onResume?: (id: string) => void
  resuming?: boolean
}) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [term, setTerm] = useState<Terminal | null>(null)
  const [state, setState] = useState<ReplayState>({ kind: 'loading' })

  useEffect(() => {
    if (!containerRef.current) return
    let cancelled = false
    let cleanup: (() => void) | null = null

    initGhostty().then(() => {
      if (cancelled || !containerRef.current) return

      const t = new Terminal(buildReplayOptions(terminalOptions))
      const fit = new FitAddon()
      t.loadAddon(fit)
      t.open(containerRef.current)
      fit.fit()

      // Link detection
      t.registerLinkProvider(new UrlRegexProvider(t))
      t.registerLinkProvider(new OSC8LinkProvider(t))

      // Expose for e2e tests (matches TerminalView's window.__gmuxTerm).
      ;(window as any).__gmuxTerm = t

      setTerm(t)
      setState({ kind: 'loading' })

      fetchScrollback(session.id).then((result) => {
        if (cancelled) return
        setState(result)
        if (result.kind === 'bytes') {
          t.write(result.bytes, () => {
            if (cancelled) return
            t.scrollToBottom()
          })
        }
      })

      const onResize = () => fit.fit()
      window.addEventListener('resize', onResize)

      cleanup = () => {
        window.removeEventListener('resize', onResize)
        if ((window as any).__gmuxTerm === t) (window as any).__gmuxTerm = null
        setTerm(null)
        t.dispose()
      }

      if (cancelled) cleanup()
    })

    return () => {
      cancelled = true
      cleanup?.()
    }
  }, [session.id])

  const exitLabel = session.exit_code != null
    ? `Session ended (exit ${session.exit_code})`
    : 'Session ended'

  return (
    <div class="replay-root">
      <div class="terminal-shell">
        <div ref={containerRef} class="terminal-container" />
        <JumpToBottom term={term} />
        {state.kind === 'loading' && (
          <div class="terminal-loading">
            Loading scrollback…
          </div>
        )}
        {state.kind === 'empty' && (
          <div class="terminal-loading">
            No scrollback was captured for this session.
          </div>
        )}
        {state.kind === 'not-found' && (
          <div class="terminal-loading">
            This session is no longer known to gmuxd.
          </div>
        )}
        {state.kind === 'error' && (
          <div class="terminal-loading">
            Couldn't load scrollback (HTTP {state.status}: {state.message}).
          </div>
        )}
      </div>
      <div class="replay-actions">
        <span class="replay-status">{exitLabel}</span>
        <div class="replay-buttons">
          {session.resumable && onResume && (
            <button
              type="button"
              class="btn btn-primary"
              disabled={!!resuming}
              onClick={() => onResume(session.id)}
            >
              {resumeButtonLabel(session.kind, !!resuming)}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
