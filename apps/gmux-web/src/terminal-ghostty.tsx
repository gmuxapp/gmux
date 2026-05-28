/**
 * Ghostty-based parallel TerminalView.
 *
 * Feature-flagged alternative to `terminal.tsx` (xterm.js). Selected at
 * mount time by `?ghostty=1` URL param or `localStorage.gmux_ghostty`.
 *
 * This is a **prototype**: minimum-viable WS connect + render + input
 * + resize. Many features of the xterm.js implementation are
 * deliberately deferred to keep the prototype small and evaluable.
 * See ../../GHOSTTY_PROTOTYPE.md for the full feature parity matrix.
 *
 * Stubbed / deferred:
 *   - BSU/ESU scroll preservation (terminal-io.ts) \u2014 ghostty has a
 *     different buffer-row/viewport model and porting the anchor logic
 *     is non-trivial. Behavior on TUI redraw: viewport may scroll
 *     unexpectedly when a TUI wipes and repaints.
 *   - Resize echo gate ("Sized for another device" pill). ghostty
 *     terminal sends size we want; ptySize tracking dropped for the
 *     prototype.
 *   - Image addon (sixel / iTerm inline images). ghostty-web has no
 *     equivalent; image escape sequences will be ignored by the parser.
 *   - OSC52 clipboard set. Needs to be intercepted in the byte stream
 *     before `term.write()`. Marked as TODO; a small `extractOsc52`
 *     helper would unblock this.
 *   - Custom keybinds via `keyboard.ts`. ghostty has
 *     `attachCustomKeyEventHandler`; we wire a *minimal* shim so the
 *     ctrl/alt-armed modifier from the mobile bar still works, but the
 *     full `attachKeyboardHandler` flow isn't ported.
 *   - mobile-input.ts (iOS / Android autocorrect interception). Not
 *     wired \u2014 ghostty has its own `beforeinput` handling. Worth
 *     testing whether autocorrect cascades happen without it; if so,
 *     we'd need to port the interceptor.
 *   - Replay buffer (`replay.ts`) is still used; it pushes bytes
 *     through `term.write` exactly the same way.
 *   - `__gmuxTerm`, `__gmuxInject` test hooks: not exposed yet.
 *
 * Working:
 *   - Theme + font from `terminalOptions` (mapped to ghostty's
 *     `ITerminalOptions`).
 *   - WS reconnect with exponential backoff.
 *   - WS \u2192 term.write byte path.
 *   - term.onData \u2192 WS send.
 *   - Resize via FitAddon + ResizeObserver. onResize fires WS
 *     `{ type: 'resize', cols, rows }` matching the existing protocol.
 *   - Bracketed paste via `term.paste()` (auto-detects mode 2004).
 *   - Mobile keyboard via ghostty's built-in textarea (no `mobile-input.ts`).
 *   - Focus / blur hooks for mobile bottom bar.
 *   - OSC8 + URL-regex hyperlinks via ghostty's built-in providers.
 */

import { FitAddon, Ghostty, Terminal } from 'ghostty-web'
// Vite's `?url` suffix asks the bundler to emit the WASM as an asset
// and return the hashed URL. Without this the lib's default URL
// resolution (`new URL('../ghostty-vt.wasm', import.meta.url)`) is
// pre-baked at lib build time and points nowhere useful in our app.
import ghosttyWasmUrl from 'ghostty-web/ghostty-vt.wasm?url'
import { useEffect, useRef, useState } from 'preact/hooks'
import type { ResolvedKeybind } from './keybinds'
import { ctrlSequenceFor } from './keyboard'
import { createReplayBuffer } from './replay'
import type { ResolvedTerminalOptions } from './settings-schema'
import type { Session } from './types'

// Singleton load: one WASM instance shared across all Terminal mounts.
// We pass the `ghostty` option to `new Terminal({ ghostty })` rather
// than relying on the library's internal singleton (which uses
// `init()` + `Ghostty.load()` without a path arg).
let ghosttyLoadPromise: Promise<Ghostty> | null = null
function ensureGhostty(): Promise<Ghostty> {
  if (!ghosttyLoadPromise) {
    ghosttyLoadPromise = Ghostty.load(ghosttyWasmUrl)
  }
  return ghosttyLoadPromise
}

function buildTheme(opts: ResolvedTerminalOptions) {
  // ResolvedTerminalOptions has a theme field with xterm-shaped color
  // keys; ghostty's ITheme uses the same names so we can pass it
  // through. The fields are optional on both sides.
  return opts.theme ?? undefined
}

interface GhosttyTerminalViewProps {
  session: Session
  terminalOptions: ResolvedTerminalOptions
  // eslint-disable-next-line @typescript-eslint/no-unused-vars -- prototype: not yet wired
  keybinds: ResolvedKeybind[]
  // eslint-disable-next-line @typescript-eslint/no-unused-vars -- prototype
  macCommandIsCtrl: boolean
  ctrlArmed: boolean
  onCtrlConsumed: () => void
  altArmed: boolean
  onAltConsumed: () => void
  onInputReady?: (send: ((data: string) => void) | null) => void
  onPasteReady?: (paste: (() => void) | null) => void
  onFocusReady?: (focus: (() => void) | null) => void
}

export function TerminalViewGhostty({
  session,
  terminalOptions,
  ctrlArmed,
  onCtrlConsumed,
  altArmed,
  onAltConsumed,
  onInputReady,
  onPasteReady,
  onFocusReady,
}: GhosttyTerminalViewProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposedRef = useRef(false)
  const currentSessionId = useRef(session.id)
  const ctrlArmedRef = useRef(ctrlArmed)
  const altArmedRef = useRef(altArmed)
  const [wsState, setWsState] = useState<'connecting' | 'open' | 'lost'>('connecting')
  const [termLoading, setTermLoading] = useState(true)
  const ghosttyRef = useRef<Ghostty | null>(null)
  const [ready, setReady] = useState(false)

  currentSessionId.current = session.id
  ctrlArmedRef.current = ctrlArmed
  altArmedRef.current = altArmed

  // Boot: load the WASM module before mounting the terminal.
  useEffect(() => {
    let cancelled = false
    ensureGhostty().then((g) => {
      if (cancelled) return
      ghosttyRef.current = g
      setReady(true)
    }).catch((err) => {
      console.error('[gmux/ghostty] WASM load failed:', err)
    })
    return () => { cancelled = true }
  }, [])

  // Terminal lifecycle (stable across session changes).
  useEffect(() => {
    if (!ready || !containerRef.current) return
    disposedRef.current = false

    const term = new Terminal({
      ghostty: ghosttyRef.current ?? undefined,
      cursorBlink: terminalOptions.cursorBlink,
      cursorStyle: terminalOptions.cursorStyle,
      fontSize: terminalOptions.fontSize,
      fontFamily: terminalOptions.fontFamily,
      theme: buildTheme(terminalOptions),
      scrollback: terminalOptions.scrollback,
    })
    termRef.current = term
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    fit.fit()

    // Send user input over the WebSocket. Honors the mobile bar's
    // armed Ctrl/Alt modifiers \u2014 same wire format as
    // `sendInput` in terminal.tsx.
    const sendRawInput = (data: string) => {
      const ws = wsRef.current
      if (!ws || ws.readyState !== WebSocket.OPEN) return
      ws.send(new TextEncoder().encode(data))
    }
    const sendInput = (data: string) => {
      if (ctrlArmedRef.current) {
        const ctrlData = ctrlSequenceFor(data)
        if (ctrlData) {
          ctrlArmedRef.current = false
          onCtrlConsumed()
          sendRawInput(ctrlData)
          return
        }
      }
      if (altArmedRef.current) {
        altArmedRef.current = false
        onAltConsumed()
        sendRawInput('\x1b' + data)
        return
      }
      sendRawInput(data)
    }

    const dataDisp = term.onData((data) => sendInput(data))

    // Forward terminal resize \u2192 backend. ghostty's `onResize` fires
    // post-resize with the new cols/rows.
    const resizeDisp = term.onResize(({ cols, rows }) => {
      const ws = wsRef.current
      if (!ws || ws.readyState !== WebSocket.OPEN) return
      ws.send(JSON.stringify({ type: 'resize', cols, rows }))
    })

    // Resize on container layout changes (sidebar toggle, window
    // resize, keyboard slide, etc.). We use FitAddon directly here
    // since ghostty's FitAddon computes cols/rows from the parent
    // element and then calls `term.resize()`.
    const ro = new ResizeObserver(() => {
      // rAF to let layout settle (same reasoning as xterm path).
      requestAnimationFrame(() => {
        if (disposedRef.current) return
        try { fit.fit() } catch (err) { console.warn('[gmux/ghostty] fit error', err) }
      })
    })
    ro.observe(containerRef.current)

    // Expose hooks to the parent (mobile bar wiring).
    onInputReady?.(sendRawInput)
    onPasteReady?.(() => {
      navigator.clipboard.readText().then((text) => {
        if (text) term.paste(text)
      }).catch(() => {})
    })
    onFocusReady?.(() => term.focus())

    // ── WebSocket connection ────────────────────────────────────────
    let attempt = 0
    let isFirstConnect = true
    let intentionalClose = false

    function connect() {
      if (disposedRef.current) return
      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }

      const replay = createReplayBuffer((chunks) => {
        for (const c of chunks) term.write(c)
        term.scrollToBottom()
        setTermLoading(false)
      })

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(`${wsProtocol}//${location.host}/ws/${session.id}`)
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      ws.onopen = () => {
        attempt = 0
        setWsState('open')

        if (isFirstConnect) {
          isFirstConnect = false
          // Claim ownership: send our current size.
          ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
        }
      }

      ws.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          // Control frames (resize echoes, etc.)
          try {
            const msg = JSON.parse(ev.data)
            if (msg.type === 'terminal_resize' || msg.type === 'resize_applied' || msg.type === 'resize_state') {
              // Prototype: trust the backend size, but don't yet show
              // the "sized for another device" pill. If the size
              // differs from ours, just resize the local terminal.
              const cols = msg.cols as number | undefined
              const rows = msg.rows as number | undefined
              if (cols && rows && (cols !== term.cols || rows !== term.rows)) {
                term.resize(cols, rows)
              }
              return
            }
          } catch {
            // not JSON, treat as terminal output
          }
          const bytes = new TextEncoder().encode(ev.data)
          if (replay.state !== 'done') { replay.push(bytes); return }
          term.write(bytes, () => setTermLoading(false))
          return
        }

        const data = ev.data instanceof ArrayBuffer
          ? new Uint8Array(ev.data)
          : new TextEncoder().encode(ev.data as string)

        if (replay.state !== 'done') { replay.push(data); return }
        term.write(data, () => setTermLoading(false))
      }

      ws.onclose = () => {
        setWsState(prev => prev === 'open' ? 'lost' : prev)
        if (disposedRef.current || intentionalClose) return
        if (currentSessionId.current !== session.id) return
        const delay = Math.min(500 * Math.pow(2, attempt), 8000)
        attempt++
        reconnectTimer.current = setTimeout(connect, delay)
      }

      ws.onerror = () => {}
    }

    connect()

    return () => {
      disposedRef.current = true
      intentionalClose = true
      ro.disconnect()
      dataDisp.dispose()
      resizeDisp.dispose()
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      onInputReady?.(null)
      onPasteReady?.(null)
      onFocusReady?.(null)
      // ghostty Terminal doesn't have a documented dispose() in the
      // public d.ts; we drop the ref and let GC handle it.
      termRef.current = null
    }
    // We intentionally only depend on `ready` and `session.id`. Theme /
    // option changes should trigger a separate effect; for the prototype
    // they're picked up on next mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ready, session.id])

  return (
    <div class={`terminal-shell terminal-ghostty${termLoading ? ' terminal-loading' : ''}`}>
      <div class="terminal-container" ref={containerRef} style={{ width: '100%', height: '100%' }} />
      {wsState === 'lost' && (
        <div class="terminal-disconnected-pill" role="status">
          Reconnecting…
        </div>
      )}
      {!ready && (
        <div class="terminal-loading" role="status">
          Loading Ghostty…
        </div>
      )}
    </div>
  )
}
