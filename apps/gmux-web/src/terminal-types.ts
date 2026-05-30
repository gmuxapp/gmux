import type { ReplayState } from './replay'

export interface SyncDiag {
  /** Mirror of ReplayBuffer.state — updated as data flows in */
  syncPhase: ReplayState | 'skipped' | 'idle'
  /** Total bytes received in the scrollback (BSU…ESU) block */
  scrollbackBytes: number
  /** WebSocket messages that arrived before replay was done */
  scrollbackMsgs: number
  /** Wall-clock time when first BSU byte arrived (ms since epoch) */
  syncStartedAt: number | null
  /** Wall-clock time when ESU was detected */
  syncEndedAt: number | null
  /** True while terminalIO still has queued write work */
  pendingWrite: boolean
  /** WS connection state */
  wsState: 'connecting' | 'open' | 'lost'
  /** How many times the WS has reconnected (0 = first connect) */
  reconnects: number
  /** Bytes received from GET /v1/sessions/<id>/scrollback?extracted=1 */
  prefetchBytes: number
  /** Current lines in the scrollback buffer (live) */
  scrollbackLines: number
  /** Configured scrollback line limit */
  scrollbackLimit: number
}
