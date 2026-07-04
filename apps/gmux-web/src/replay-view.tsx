import { useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { WebLinksAddon } from '@xterm/addon-web-links'
import type { ITerminalOptions } from '@xterm/xterm'
import { loadWebglRenderer } from './webgl-renderer'
import type { Session } from './types'
import { fetchScrollback, type ScrollbackResult } from './replay-fetch'
import { JumpToBottom } from './jump-to-bottom'

// gmuxd caps scrollback at 1 MiB × 2 files (~2 MiB max). xterm's default
// scrollback line cap (1000) would silently truncate most of that for
// text-heavy sessions; bump it for replay so the user can scroll the full
// captured history.
const REPLAY_SCROLLBACK_LINES = 10000

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
 * Lifecycle controls (Resume / Rerun / Restart) live in the header
 * SessionMenu, shared with alive sessions — this view is body-only,
 * matching TerminalView. Dismissal is intentionally not exposed here;
 * the sidebar's per-session close affordance is the single way to
 * remove a dead session.
 */
export function ReplayView({
  session,
  terminalOptions,
}: {
  session: Session
  terminalOptions: ITerminalOptions
}) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [term, setTerm] = useState<Terminal | null>(null)
  const [state, setState] = useState<ReplayState>({ kind: 'loading' })

  useEffect(() => {
    if (!containerRef.current) return

    // Scrollback was recorded at a specific column width. Re-flowing it
    // to a different width would corrupt cursor positioning, box-drawing
    // alignment, and any output that assumed the original geometry, so
    // we pin cols to the recorded value and rely on `.terminal-shell`'s
    // `overflow: auto` to allow horizontal scrolling when the viewport
    // is narrower than the recording. Rows still fit vertically so the
    // scrollback fills the available height without leaving an empty
    // strip below.
    const recordedCols = session.terminal_cols ?? 80
    const recordedRows = session.terminal_rows ?? 24

    const term = new Terminal({
      ...terminalOptions,
      cols: recordedCols,
      rows: recordedRows,
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
    loadWebglRenderer(term)
    // Vertical-only fit: use FitAddon's proposal for rows, but keep cols
    // pinned to the recording. FitAddon already knows the cell metrics
    // and shell-padding accounting; reusing it for the row dimension is
    // simpler than duplicating that logic here.
    const fitRows = () => {
      const proposed = fit.proposeDimensions()
      if (!proposed || !Number.isFinite(proposed.rows) || proposed.rows < 1) return
      if (term.cols !== recordedCols || term.rows !== proposed.rows) {
        term.resize(recordedCols, proposed.rows)
      }
    }
    fitRows()
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

    const onResize = () => fitRows()
    window.addEventListener('resize', onResize)

    return () => {
      cancelled = true
      window.removeEventListener('resize', onResize)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      setTerm(null)
      term.dispose()
    }
  }, [session.id])

  return (
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
  )
}
