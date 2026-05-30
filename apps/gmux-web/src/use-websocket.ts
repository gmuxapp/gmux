/**
 * useWebSocket — manages the terminal's WebSocket connection lifecycle.
 *
 * Encapsulates: connect/reconnect loop, snapshot replay (createReplayBuffer),
 * dead-session prefetch, sync-diag tracking, terminal-size echo-gate sync,
 * and scroll-restore on reconnect.
 *
 * All mutable state is accessed via refs so the effect dep array is minimal:
 * [session.id, ghosttyReady].
 */
import { useEffect } from 'preact/hooks'
import type { WTerm } from '@wterm/dom'
import { createReplayBuffer } from './replay'
import { fetchScrollback } from './replay-fetch'
import { createTerminalIO, type TerminalSize } from './terminal-io'
import { interceptOsc52 } from './terminal-osc52'
import { prefetchCache } from './terminal-init'
import type { SyncDiag } from './terminal-types'
import type { Session } from './types'

// Use { current: T } rather than RefObject<T> for refs that are never null
// at the call sites (avoids spurious TS18047 null-checks throughout).
type Ref<T> = { current: T }

export interface UseWebSocketOptions {
  // Identity / gates
  session: Session
  ghosttyReady: boolean
  // Refs
  termRef:           Ref<WTerm | null>
  termIoRef:         Ref<ReturnType<typeof createTerminalIO> | null>
  wsRef:             Ref<WebSocket | null>
  reconnectTimer:    Ref<ReturnType<typeof setTimeout> | null>
  disposed:          Ref<boolean>
  currentSessionId:  Ref<string>
  sessionRef:        Ref<Session>
  termEpochRef:      Ref<number>
  reconnectCountRef: Ref<number>
  ptySizeRef:        Ref<TerminalSize | null>
  viewportSizeRef:   Ref<TerminalSize | null>
  // Stable callbacks
  queueData:              (data: Uint8Array, onWritten?: () => void) => void
  queueMany:              (chunks: Uint8Array[], onWritten?: () => void) => void
  queueResize:            (size: TerminalSize) => void
  resetResizeEchoGate:    () => void
  releaseResizeEchoGate:  (applied: TerminalSize) => void
  fitAndResize:           () => void
  emitSyncDiag:           (patch: Partial<SyncDiag>) => void
  // State setters
  setPtySize:     (size: TerminalSize | null) => void
  setViewportSize:(size: TerminalSize | null) => void
  setWsState:     (state: 'connecting' | 'open' | 'lost') => void
  setTermLoading: (loading: boolean) => void
  // Config
  scrollbackLimit: number
}

export function useWebSocket(opts: UseWebSocketOptions): void {
  const {
    session, ghosttyReady,
    termRef, termIoRef, wsRef, reconnectTimer, disposed, currentSessionId,
    sessionRef, termEpochRef, reconnectCountRef,
    ptySizeRef, viewportSizeRef,
    queueData, queueMany, queueResize,
    resetResizeEchoGate, releaseResizeEchoGate, fitAndResize, emitSyncDiag,
    setPtySize, setViewportSize, setWsState, setTermLoading,
    scrollbackLimit,
  } = opts

  useEffect(() => {
    if (!termRef.current || !termIoRef.current) return

    let isFirstConnect = true
    let attempt = 0
    let intentionalClose = false
    const epoch = termEpochRef.current + 1
    termEpochRef.current = epoch
    termIoRef.current.reset(epoch)



    resetResizeEchoGate()
    setPtySize(null);     ptySizeRef.current     = null
    setViewportSize(null); viewportSizeRef.current = null
    setWsState('connecting')
    reconnectCountRef.current = 0
    emitSyncDiag({
      syncPhase: 'idle', scrollbackBytes: 0, scrollbackMsgs: 0,
      syncStartedAt: null, syncEndedAt: null, pendingWrite: false,
      wsState: 'connecting', reconnects: 0, prefetchBytes: 0,
      scrollbackLines: 0, scrollbackLimit: scrollbackLimit,
    })
    setTermLoading(true)

    function connect() {
      if (disposed.current) return

      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }

      // wterm auto-scrolls to bottom; no forceNextScrollToBottom needed.
      emitSyncDiag({ syncPhase: 'waiting', wsState: 'connecting' })

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'

      // Strategy:
      //   Live sessions:  WS snapshot includes CSI_3J + renderScreen() — full scrollback.
      //   Dead sessions:  prefetch from on-disk file (ExtractBytes); WS will fail.
      //   Reconnects:     simple WS snapshot — scrollback already in host buffer.
      const openWs = (prefetchBarrier?: Promise<void>) => {
        if (disposed.current || currentSessionId.current !== session.id) return
        const url = `${wsProtocol}//${location.host}/ws/${session.id}`
        const ws = new WebSocket(url)
        wireWs(ws, prefetchBarrier)
      }

      if (!isFirstConnect) {
        openWs()
        return
      }

      // Clear the old session's buffer immediately (termLoading overlay hides the flash).
      queueData(new TextEncoder().encode('\x1b[3J\x1b[2J\x1b[H'))

      if (session.alive) {
        openWs()
        return
      }

      // Dead session: prefetch from on-disk scrollback file.
      // Cache is safe: the file doesn't change once the session has exited.
      let prefetchResolve!: () => void
      const prefetchBarrier = new Promise<void>(resolve => { prefetchResolve = resolve })
      const prefetchSessionId = session.id

      const injectPrefetch = (extracted: Uint8Array) => {
        emitSyncDiag({ prefetchBytes: extracted.length })
        if (extracted.length > 0) {
          queueData(extracted)
          const rows = termRef.current?.rows ?? 24
          queueData(new TextEncoder().encode('\r\n'.repeat(rows)))
        }
      }

      const cached = prefetchCache.get(prefetchSessionId)
      if (cached !== undefined) {
        if (cached !== null) injectPrefetch(cached)
        prefetchResolve()
      } else {
        fetchScrollback(prefetchSessionId).then(result => {
          if (disposed.current || currentSessionId.current !== prefetchSessionId) {
            prefetchResolve()
            return
          }
          if (result.kind === 'bytes') {
            const extracted = result.bytes
            emitSyncDiag({ prefetchBytes: extracted.length })
            prefetchCache.set(prefetchSessionId, extracted.length > 0 ? extracted : null)
            if (extracted.length > 0) {
              queueData(extracted)
              const rows = termRef.current?.rows ?? 24
              queueData(new TextEncoder().encode('\r\n'.repeat(rows)))
            }
          } else if (result.kind === 'empty' || result.kind === 'not-found') {
            prefetchCache.set(prefetchSessionId, null)
          }
          // error: don't cache so next visit retries
          prefetchResolve()
        }).catch(() => prefetchResolve())
      }

      openWs(prefetchBarrier)
    }

    // wireWs attaches message/error/close handlers to a freshly opened WebSocket.
    // Extracted so the prefetch path and reconnect path share the same wiring.
    function wireWs(ws: WebSocket, prefetchBarrier?: Promise<void>) {
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      let replaySyncBytes = 0
      let replaySyncMsgs  = 0

      // Gate the replay-block callback on the prefetch so the write order is always:
      // prefetch bytes → WS snapshot → live output.
      let prefetchSettled = !prefetchBarrier
      let pendingReplayWrite: (() => void) | null = null

      if (prefetchBarrier) {
        prefetchBarrier.then(() => {
          prefetchSettled = true
          if (pendingReplayWrite) { pendingReplayWrite(); pendingReplayWrite = null }
        })
      }

      const replay = createReplayBuffer(chunks => {
        const doWrite = () => {
          const filtered = chunks.map(interceptOsc52)
          queueMany(filtered, () => {
            setTermLoading(false)
            emitSyncDiag({
              pendingWrite: false,
              scrollbackLines: termRef.current?.bridge?.getScrollbackCount() ?? 0,
            })
          })
          emitSyncDiag({ syncEndedAt: Date.now(), pendingWrite: true })
          emitSyncDiag({ syncEndedAt: Date.now(), pendingWrite: true })
        }
        if (prefetchSettled) {
          doWrite()
        } else {
          pendingReplayWrite = doWrite
        }
      })

      ws.onopen = () => {
        attempt = 0
        setWsState('open')
        const rc = reconnectCountRef.current
        emitSyncDiag({ wsState: 'open', reconnects: rc })

        if (!isFirstConnect) {
          reconnectCountRef.current += 1
          emitSyncDiag({ reconnects: reconnectCountRef.current })
          resetResizeEchoGate()
          const sess = sessionRef.current
          if (sess.terminal_cols && sess.terminal_rows) {
            const cached = ptySizeRef.current
            if (!cached || cached.cols !== sess.terminal_cols || cached.rows !== sess.terminal_rows) {
              const size = { cols: sess.terminal_cols, rows: sess.terminal_rows }
              setPtySize(size); ptySizeRef.current = size
              queueResize(size)
            }
          }
          return
        }
        isFirstConnect = false
        fitAndResize()
      }

      ws.onmessage = ev => {
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data)
            if (msg.type === 'resize_state') {
              const cols = msg.cols as number | undefined
              const rows = msg.rows as number | undefined
              if (cols && rows) {
                const size = { cols, rows }
                setPtySize(size); ptySizeRef.current = size
                queueResize(size)
              }
              return
            }
            if (msg.type === 'terminal_resize' || msg.type === 'resize_applied') {
              const cols = msg.cols as number | undefined
              const rows = msg.rows as number | undefined
              if (cols && rows) {
                const size = { cols, rows }
                setPtySize(size); ptySizeRef.current = size
                queueResize(size)
                releaseResizeEchoGate(size)
              }
              return
            }
          } catch {
            // fall through to terminal write
          }

          const data = interceptOsc52(new TextEncoder().encode(ev.data))
          pushToReplay(data)
          return
        }

        const rawData = ev.data instanceof ArrayBuffer
          ? new Uint8Array(ev.data)
          : new TextEncoder().encode(ev.data)
        pushToReplay(interceptOsc52(rawData))
      }

      function pushToReplay(data: Uint8Array) {
        if (replay.state !== 'done') {
          replaySyncBytes += data.length
          replaySyncMsgs  += 1
          const wasWaiting = replay.state === 'waiting'
          replay.push(data)
          if (wasWaiting) {
            emitSyncDiag({
              syncPhase:      replay.wasSkipped ? 'skipped' : replay.state,
              syncStartedAt:  Date.now(),
              scrollbackBytes: replaySyncBytes,
              scrollbackMsgs:  replaySyncMsgs,
            })
          } else {
            emitSyncDiag({ syncPhase: replay.state, scrollbackBytes: replaySyncBytes, scrollbackMsgs: replaySyncMsgs })
          }
          return
        }
        if (prefetchSettled) {
          queueData(data, () => setTermLoading(false))
        }
      }

      ws.onclose = (ev: CloseEvent) => {
        console.debug(`[ws] closed: code=${ev.code} reason=${JSON.stringify(ev.reason)} wasClean=${ev.wasClean} session=${session.id}`)
        resetResizeEchoGate()
        setWsState('lost')
        emitSyncDiag({ wsState: 'lost' })
        if (disposed.current || intentionalClose) return
        if (currentSessionId.current !== session.id) return
        const delay = Math.min(500 * Math.pow(2, attempt), 8000)
        attempt++
        reconnectTimer.current = setTimeout(connect, delay)
      }

      ws.onerror = () => {}
    }

    connect()

    return () => {

      termEpochRef.current = epoch + 1
      termIoRef.current?.reset(termEpochRef.current)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      reconnectTimer.current = null
      resetResizeEchoGate()
      wsRef.current?.close()
      wsRef.current = null
    }
  // session.id and ghosttyReady are the only reactive deps;
  // everything else is a stable ref or callback.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session.id, ghosttyReady])
}
