import { useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import type { ITerminalOptions } from '@xterm/xterm'
import type { Session } from './types'
import { fetchScrollback, type ScrollbackResult } from './replay-fetch'

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
 * previously lived as auto-trigger-on-click in the sidebar: Resume
 * (if the adapter is resumable) and Dismiss. Promoting them out of an
 * implicit click means clicking a dead session navigates to its
 * scrollback first, and any state-changing action is a deliberate
 * second click.
 */
export function ReplayView({
  session,
  terminalOptions,
  onResume,
  onDismiss,
  resuming,
}: {
  session: Session
  terminalOptions: ITerminalOptions
  onResume?: (id: string) => void
  onDismiss?: (session: Session) => void
  resuming?: boolean
}) {
  const containerRef = useRef<HTMLDivElement>(null)
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
              {resuming ? 'Resuming…' : 'Resume'}
            </button>
          )}
          {onDismiss && (
            <button
              type="button"
              class="btn"
              onClick={() => onDismiss(session)}
            >
              Dismiss
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
