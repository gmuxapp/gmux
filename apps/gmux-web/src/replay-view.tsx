import { useEffect, useRef, useState } from 'preact/hooks'
import type { WTerm } from '@wterm/dom'
import type { Session } from './types'
import type { ResolvedTerminalOptions } from './settings-schema'
import { fetchScrollback, type ScrollbackResult } from './replay-fetch'
import { JumpToBottom } from './jump-to-bottom'
import { getGhosttyCore } from './terminal-init'
import { applyWtermTheme } from './terminal-theme'

// gmuxd caps scrollback at 1 MiB × 2 files (~2 MiB max). Bump scrollback
// for replay so the user can scroll the full captured history.
const REPLAY_SCROLLBACK_LINES = 10000

type ReplayState =
  | { kind: 'loading' }
  | ScrollbackResult

const RESUMABLE_AGENT_KINDS = new Set(['claude', 'codex', 'pi'])

function resumeButtonLabel(kind: string, busy: boolean): string {
  const isAgent = RESUMABLE_AGENT_KINDS.has(kind)
  if (busy) return isAgent ? 'Resuming…' : 'Rerunning…'
  return isAgent ? 'Resume' : 'Rerun'
}

/**
 * Read-only wterm view that replays a dead session's persisted scrollback.
 * No WebSocket, no input, no resize messages.
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
  const [term, setTerm] = useState<WTerm | null>(null)
  const [state, setState] = useState<ReplayState>({ kind: 'loading' })

  useEffect(() => {
    if (!containerRef.current) return
    let cancelled = false
    let cleanup: (() => void) | null = null

    const run = async () => {
      const { WTerm } = await import('@wterm/dom')
      const core = await getGhosttyCore()
      if (cancelled || !containerRef.current) return

      const t = new WTerm(containerRef.current, {
        core,
        autoResize: true,
        cursorBlink: false,
      })
      applyWtermTheme(t.element, {
        ...terminalOptions,
        scrollback: REPLAY_SCROLLBACK_LINES,
      })
      await t.init()
      if (cancelled) { t.destroy(); return }

      // Expose for e2e tests (matches TerminalView's window.__gmuxTerm).
      ;(window as any).__gmuxTerm = t

      setTerm(t)
      setState({ kind: 'loading' })

      fetchScrollback(session.id).then((result) => {
        if (cancelled) return
        setState(result)
        if (result.kind === 'bytes') {
          t.write(result.bytes)
          // Scroll to bottom after write
          requestAnimationFrame(() => {
            if (!cancelled) {
              const el = t.element
              el.scrollTop = el.scrollHeight
            }
          })
        }
      })

      cleanup = () => {
        if ((window as any).__gmuxTerm === t) (window as any).__gmuxTerm = null
        setTerm(null)
        t.destroy()
      }

      if (cancelled) cleanup()
    }

    run().catch(console.error)

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
