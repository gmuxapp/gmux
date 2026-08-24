import type { IDisposable, IEvent, ITerminalAddon, Terminal } from '@xterm/xterm'

export type ScrollAnchorMode = 'following' | 'anchored'

export interface ScrollAnchorSnapshot {
  line: string | null
  distanceFromBottom: number
}

export interface ScrollAnchorOptions {
  /** Filter for anchor-worthy lines. Default: trimmed length >= 4. */
  isAnchorLine?: (text: string) => boolean
}

export interface ScrollAnchorBuffer {
  baseY: number
  rows: number
  getLine(y: number): string | null
}

type CsiParams = Array<number | number[]>

/** Resolve a pre-wipe snapshot against a post-wipe buffer. */
export function resolveScrollAnchor(snapshot: ScrollAnchorSnapshot, buffer: ScrollAnchorBuffer): number {
  if (snapshot.line !== null) {
    let best: number | null = null
    let bestDiff = Number.POSITIVE_INFINITY
    for (let y = 0; y < buffer.baseY + buffer.rows; y++) {
      if (buffer.getLine(y) !== snapshot.line) continue
      const target = Math.min(y, buffer.baseY)
      const diff = Math.abs(buffer.baseY - target - snapshot.distanceFromBottom)
      if (diff < bestDiff) {
        best = target
        bestDiff = diff
      }
    }
    if (best !== null) return best
  }

  return snapshot.distanceFromBottom <= buffer.baseY
    ? buffer.baseY - snapshot.distanceFromBottom
    : buffer.baseY
}

class EventEmitter<T> implements IDisposable {
  private listeners = new Set<(value: T) => void>()
  readonly event: IEvent<T> = (listener) => {
    this.listeners.add(listener)
    return { dispose: () => this.listeners.delete(listener) }
  }

  fire(value: T): void {
    for (const listener of this.listeners) listener(value)
  }

  dispose(): void {
    this.listeners.clear()
  }
}

function containsParam(params: CsiParams, expected: number): boolean {
  return params.some(value => Array.isArray(value) ? value.includes(expected) : value === expected)
}

function firstParam(params: CsiParams): number {
  const first = params[0] ?? 0
  return Array.isArray(first) ? (first[0] ?? 0) : first
}

/** Keeps an xterm viewport following output until wheel/touch intent anchors it. */
export class ScrollAnchorAddon implements ITerminalAddon {
  private terminal: Terminal | null = null
  private disposables: IDisposable[] = []
  private readonly modeEmitter = new EventEmitter<ScrollAnchorMode>()
  private readonly busyEmitter = new EventEmitter<boolean>()
  private currentMode: ScrollAnchorMode = 'following'
  private snapshot: ScrollAnchorSnapshot = { line: null, distanceFromBottom: 0 }
  private userIntent = false
  private userIntentExpiryRAF: number | null = null
  private pendingProgrammaticScrolls = 0
  private wipePending = false
  private wipeSyncRAF: number | null = null
  private syncMode = false
  private readonly isAnchorLine: (text: string) => boolean

  readonly onModeChange: IEvent<ScrollAnchorMode> = this.modeEmitter.event
  /**
   * Fires for the fence a host must respect before resizing the terminal.
   * Deliberately the only fence on the public API: it already covers DEC 2026
   * and post-ED3 re-resolution, and exposing the narrower synchronized-output
   * flag alongside it only invites hosts to pick the one that races.
   */
  readonly onBusyChange: IEvent<boolean> = this.busyEmitter.event

  constructor(options: ScrollAnchorOptions = {}) {
    this.isAnchorLine = options.isAnchorLine ?? (text => text.trim().length >= 4)
  }

  get mode(): ScrollAnchorMode { return this.currentMode }
  get busy(): boolean { return this.syncMode || this.wipePending || this.wipeSyncRAF !== null }

  activate(terminal: Terminal): void {
    if (this.terminal) throw new Error('ScrollAnchorAddon is already active')
    this.terminal = terminal

    const armUserIntent = () => {
      if (this.isAlternate()) return
      // Fresh input supersedes any addon scroll that produced no onScroll.
      this.pendingProgrammaticScrolls = 0
      this.userIntent = true
      this.scheduleIntentExpiry()
    }
    terminal.element?.addEventListener('wheel', armUserIntent, { capture: true, passive: true })
    terminal.element?.addEventListener('touchmove', armUserIntent, { capture: true, passive: true })
    this.disposables.push({ dispose: () => {
      terminal.element?.removeEventListener('wheel', armUserIntent, true)
      terminal.element?.removeEventListener('touchmove', armUserIntent, true)
    } })

    this.disposables.push(terminal.onScroll(() => this.handleScroll()))

    const observeSync = (active: boolean) => (params: CsiParams): boolean => {
      if (containsParam(params, 2026)) this.setSyncActive(active)
      return false
    }
    this.disposables.push(terminal.parser.registerCsiHandler({ prefix: '?', final: 'h' }, observeSync(true)))
    this.disposables.push(terminal.parser.registerCsiHandler({ prefix: '?', final: 'l' }, observeSync(false)))
    this.disposables.push(terminal.parser.registerCsiHandler({ final: 'J' }, (params: CsiParams) => {
      if (firstParam(params) !== 3 || this.isAlternate() || this.currentMode !== 'anchored') return false
      const wasBusy = this.busy
      this.cancelWipeRAF()
      // ED3 destroys line identity after parser hooks, so refresh beforehand.
      this.snapshot = this.captureSnapshot()
      this.wipePending = true
      this.fireBusyChange(wasBusy)
      return false
    }))

    this.disposables.push(terminal.onWriteParsed(() => this.afterWriteParsed()))
  }

  follow(): void {
    this.clearUserIntent()
    this.cancelWipeResolution()
    this.setMode('following')
    if (!this.terminal || this.isAlternate()) return
    this.runProgrammaticScroll(() => this.scrollToBottomWithoutAnimation(this.terminal!))
  }

  /** Clear parser/transient state at an epoch boundary without changing mode. */
  reset(): void {
    const wasBusy = this.busy
    this.cancelWipeRAF()
    this.syncMode = false
    this.wipePending = false
    this.snapshot = { line: null, distanceFromBottom: 0 }
    this.clearUserIntent()
    this.pendingProgrammaticScrolls = 0
    this.fireBusyChange(wasBusy)
  }

  dispose(): void {
    this.reset()
    for (const disposable of this.disposables.splice(0)) disposable.dispose()
    this.terminal = null
    this.modeEmitter.dispose()
    this.busyEmitter.dispose()
  }

  private handleScroll(): void {
    const terminal = this.terminal
    if (!terminal || this.isAlternate()) return
    const buffer = terminal.buffer.active

    if (this.pendingProgrammaticScrolls > 0) {
      this.pendingProgrammaticScrolls--
      this.clearUserIntent()
      return
    }

    if (this.userIntent) {
      this.clearUserIntent()
      // User intent always supersedes a pending post-wipe rendering catch-up.
      this.cancelWipeResolution()
      if (buffer.viewportY >= buffer.baseY) {
        this.setMode('following')
      } else {
        // Anchor identity is captured immediately before ED3; ordinary
        // streaming relies on xterm's native eviction-adjusted viewport.
        this.setMode('anchored')
      }
      return
    }

    // Bottom is structural follow intent for keyboard input, scrollbar drags,
    // and application settings such as scrollOnUserInput. During ED3 the
    // buffer transiently reports 0/0, so ignore that output-driven event while
    // wipe re-resolution is fenced. Other destructive resets (RIS or a
    // scrollback-size change) intentionally have no identity snapshot and may
    // therefore drop an anchored viewport into following mode.
    if (!this.busy && buffer.viewportY >= buffer.baseY) this.setMode('following')
  }

  private afterWriteParsed(): void {
    // A write-driven scroll cannot belong to an earlier wheel/touch event or
    // an addon scroll from a previous parse cycle.
    this.clearUserIntent()
    this.pendingProgrammaticScrolls = 0
    const terminal = this.terminal
    if (!terminal || this.isAlternate()) return

    if (this.wipePending && !this.syncMode) {
      if (this.currentMode !== 'anchored') {
        const wasBusy = this.busy
        this.wipePending = false
        this.fireBusyChange(wasBusy)
        return
      }

      const buffer = terminal.buffer.active
      const target = resolveScrollAnchor(this.snapshot, {
        baseY: buffer.baseY,
        rows: terminal.rows,
        getLine: y => buffer.getLine(y)?.translateToString(true) ?? null,
      })
      const wasBusy = this.busy
      this.cancelWipeRAF()
      this.wipePending = false
      this.runProgrammaticScroll(() => terminal.scrollToLine(target))

      // The gmux xterm fork applies synchronized-output viewport state after
      // onWriteParsed. Keep the resize fence through one rendering catch-up.
      if (typeof requestAnimationFrame !== 'undefined') {
        this.wipeSyncRAF = requestAnimationFrame(() => {
          const wasRAFBusy = this.busy
          this.runProgrammaticScroll(() => terminal.scrollToLine(target))
          this.wipeSyncRAF = null
          this.fireBusyChange(wasRAFBusy)
        })
      }
      this.fireBusyChange(wasBusy)
      return
    }

    if (this.currentMode === 'following') {
      const buffer = terminal.buffer.active
      if (buffer.viewportY < buffer.baseY) {
        this.runProgrammaticScroll(() => this.scrollToBottomWithoutAnimation(terminal))
      }
    }
  }

  private setSyncActive(active: boolean): void {
    // A boolean, never a depth counter: xterm's mode set is idempotent, so a
    // stream that opens DEC 2026 twice and closes it once has finished
    // synchronized output. Counting depth leaves the fence latched and
    // starves every later resize for the lifetime of the session.
    const wasBusy = this.busy
    this.syncMode = active
    this.fireBusyChange(wasBusy)
  }

  private cancelWipeResolution(): void {
    const wasBusy = this.busy
    this.cancelWipeRAF()
    this.wipePending = false
    this.fireBusyChange(wasBusy)
  }

  private cancelWipeRAF(): void {
    if (this.wipeSyncRAF === null) return
    if (typeof cancelAnimationFrame !== 'undefined') cancelAnimationFrame(this.wipeSyncRAF)
    this.wipeSyncRAF = null
  }

  private fireBusyChange(previous: boolean): void {
    if (previous !== this.busy) this.busyEmitter.fire(this.busy)
  }

  private runProgrammaticScroll(action: () => void): void {
    // The fork can deliver the resulting onScroll on a later animation frame.
    // Keep one suppression token per call until those events arrive.
    this.pendingProgrammaticScrolls++
    try {
      action()
    } catch (error) {
      this.pendingProgrammaticScrolls--
      throw error
    }
  }

  private clearUserIntent(): void {
    this.userIntent = false
    if (this.userIntentExpiryRAF !== null && typeof cancelAnimationFrame !== 'undefined') {
      cancelAnimationFrame(this.userIntentExpiryRAF)
    }
    this.userIntentExpiryRAF = null
  }

  private scheduleIntentExpiry(): void {
    if (this.userIntentExpiryRAF !== null || typeof requestAnimationFrame === 'undefined') return
    // Two frames cover xterm's first smooth-scroll tick without allowing a
    // no-op wheel to arm unrelated SearchAddon/output scrolling indefinitely.
    this.userIntentExpiryRAF = requestAnimationFrame(() => {
      this.userIntentExpiryRAF = requestAnimationFrame(() => {
        this.userIntentExpiryRAF = null
        this.userIntent = false
      })
    })
  }

  private scrollToBottomWithoutAnimation(terminal: Terminal): void {
    // gmux's xterm fork accepts this optional public behavior flag even though
    // its generated declaration still exposes the upstream zero-arg shape.
    ;(terminal.scrollToBottom as (disableSmoothScroll?: boolean) => void)(true)
  }

  private captureSnapshot(): ScrollAnchorSnapshot {
    const terminal = this.terminal
    if (!terminal) return { line: null, distanceFromBottom: 0 }
    const buffer = terminal.buffer.active
    const text = buffer.getLine(buffer.viewportY)?.translateToString(true) ?? null
    return {
      line: text !== null && this.isAnchorLine(text) ? text : null,
      distanceFromBottom: Math.max(0, buffer.baseY - buffer.viewportY),
    }
  }

  private isAlternate(): boolean {
    return this.terminal?.buffer.active.type === 'alternate'
  }

  private setMode(mode: ScrollAnchorMode): void {
    if (this.currentMode === mode) return
    this.currentMode = mode
    this.modeEmitter.fire(mode)
  }
}
