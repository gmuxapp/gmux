import { useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import type { ITerminalOptions } from '@xterm/xterm'
import type { Session } from './types'
import { fetchScrollback, type ScrollbackResult } from './replay-fetch'
import { JumpToBottom } from './jump-to-bottom'

// gmuxd caps scrollback at 1 MiB × 2 files (~2 MiB max). xterm's default
// scrollback line cap (1000) would silently truncate most of that for
// text-heavy sessions; bump it for replay so the user can scroll the full
// captured history.
const REPLAY_SCROLLBACK_LINES = 10000

function loadPreferredRenderer(term: Terminal) {
  try { term.loadAddon(new WebglAddon()) } catch { /* DOM fallback */ }
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
  terminalOptions: ITerminalOptions
  onResume?: (id: string) => void
  resuming?: boolean
}) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [term, setTerm] = useState<Terminal | null>(null)
  const [state, setState] = useState<ReplayState>({ kind: 'loading' })

  useEffect(() => {
    if (!containerRef.current) return

    const term = new Terminal({
      ...terminalOptions,
      scrollback: REPLAY_SCROLLBACK_LINES,
      disableStdin: true,
      cursorBlink: false,
      cursorInactiveStyle: 'none',
      linkHandler: {
        activate(_event, text) {
          window.open(text, '_blank', 'noopener')
        },
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.loadAddon(new ImageAddon())
    term.loadAddon(new WebLinksAddon())
    term.open(containerRef.current)
    loadPreferredRenderer(term)
    fit.fit()
    setTerm(term)

    // OSC 52 (set clipboard) suppression: the captured bytes may contain
    // OSC 52 sequences emitted by the *original* live session; replaying
    // them would silently overwrite the operator's clipboard. Swallow.
    term.parser.registerOscHandler(52, () => true)

    // Expose for e2e tests (matches TerminalView's window.__gmuxTerm).
    ;(window as any).__gmuxTerm = term

    setState({ kind: 'loading' })

    let cancelled = false
    fetchScrollback(session.id).then((result) => {
      if (cancelled) return
      setState(result)
      if (result.kind === 'bytes') {
        term.write(result.bytes, () => {
          // The write callback is async: between term.write and the
          // callback firing, the effect's cleanup may have run
          // (component unmount / session.id switch) and disposed
          // the terminal. Calling scrollToBottom on a disposed
          // terminal throws.
          if (cancelled) return
          // Wait for the write callback so xterm has actually parsed the
          // bytes before we ask it to scroll; otherwise scrollToBottom
          // pins us at row 0 because the buffer is still empty.
          term.scrollToBottom()
        })
      }
    })

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)

    return () => {
      cancelled = true
      window.removeEventListener('resize', onResize)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      setTerm(null)
      term.dispose()
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
