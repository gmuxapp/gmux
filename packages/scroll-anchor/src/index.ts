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

function hasParam(params: readonly (number | readonly number[])[], expected: number): boolean {
  return params.some(value => Array.isArray(value) ? value.includes(expected) : value === expected)
}

/** Keeps an xterm viewport following output until wheel/touch intent anchors it. */
export class ScrollAnchorAddon implements ITerminalAddon {
  private terminal: Terminal | null = null
  private disposables: IDisposable[] = []
  private readonly modeEmitter = new EventEmitter<ScrollAnchorMode>()
  private readonly syncEmitter = new EventEmitter<boolean>()
  private currentMode: ScrollAnchorMode = 'following'
  private snapshot: ScrollAnchorSnapshot = { line: null, distanceFromBottom: 0 }
  private userIntent = false
  private userIntentTimer: ReturnType<typeof setTimeout> | null = null
  private wipePending = false
  private wipeSyncRAF: number | null = null
  private userScrollVersion = 0
  private syncDepth = 0
  private wasAlternate = false
  private readonly isAnchorLine: (text: string) => boolean

  readonly onModeChange: IEvent<ScrollAnchorMode> = this.modeEmitter.event
  readonly onSyncActiveChange: IEvent<boolean> = this.syncEmitter.event

  constructor(options: ScrollAnchorOptions = {}) {
    this.isAnchorLine = options.isAnchorLine ?? (text => text.trim().length >= 4)
  }

  get mode(): ScrollAnchorMode { return this.currentMode }
  get syncActive(): boolean { return this.syncDepth > 0 }

  activate(terminal: Terminal): void {
    if (this.terminal) throw new Error('ScrollAnchorAddon is already active')
    this.terminal = terminal
    this.wasAlternate = terminal.buffer.active.type === 'alternate'

    const armUserIntent = () => {
      if (this.isAlternate()) return
      this.userIntent = true
      if (this.userIntentTimer !== null) clearTimeout(this.userIntentTimer)
      this.userIntentTimer = setTimeout(() => {
        this.userIntent = false
        this.userIntentTimer = null
      }, 0)
    }
    terminal.element?.addEventListener('wheel', armUserIntent, { capture: true, passive: true })
    terminal.element?.addEventListener('touchmove', armUserIntent, { capture: true, passive: true })
    this.disposables.push({ dispose: () => {
      terminal.element?.removeEventListener('wheel', armUserIntent, true)
      terminal.element?.removeEventListener('touchmove', armUserIntent, true)
    } })

    this.disposables.push(terminal.onScroll(() => {
      if (!this.userIntent || this.isAlternate()) return
      this.userIntent = false
      this.userScrollVersion++
      if (this.userIntentTimer !== null) {
        clearTimeout(this.userIntentTimer)
        this.userIntentTimer = null
      }
      const buffer = terminal.buffer.active
      if (buffer.viewportY < buffer.baseY) {
        this.snapshot = this.captureSnapshot()
        this.setMode('anchored')
      } else {
        this.setMode('following')
      }
    }))

    const observeSync = (opening: boolean) => (params: any): boolean => {
      if (!hasParam(params, 2026)) return false
      const before = this.syncActive
      this.syncDepth = opening ? this.syncDepth + 1 : Math.max(0, this.syncDepth - 1)
      if (before !== this.syncActive) this.syncEmitter.fire(this.syncActive)
      return false
    }
    this.disposables.push(terminal.parser.registerCsiHandler({ prefix: '?', final: 'h' }, observeSync(true)))
    this.disposables.push(terminal.parser.registerCsiHandler({ prefix: '?', final: 'l' }, observeSync(false)))
    this.disposables.push(terminal.parser.registerCsiHandler({ final: 'J' }, (params: any) => {
      if (!hasParam(params, 3) || this.isAlternate() || this.currentMode !== 'anchored') return false
      // ED3 runs after parser hooks and destroys line identity, so refresh now.
      this.snapshot = this.captureSnapshot()
      this.wipePending = true
      return false
    }))

    this.disposables.push(terminal.onWriteParsed(() => this.afterWriteParsed()))
  }

  follow(): void {
    this.setMode('following')
    if (!this.terminal || this.isAlternate()) return
    this.scrollToBottomWithoutAnimation(this.terminal)
  }

  dispose(): void {
    for (const disposable of this.disposables.splice(0)) disposable.dispose()
    if (this.userIntentTimer !== null) clearTimeout(this.userIntentTimer)
    if (this.wipeSyncRAF !== null && typeof cancelAnimationFrame !== 'undefined') cancelAnimationFrame(this.wipeSyncRAF)
    this.userIntentTimer = null
    this.wipeSyncRAF = null
    this.userIntent = false
    this.terminal = null
    this.modeEmitter.dispose()
    this.syncEmitter.dispose()
  }

  private afterWriteParsed(): void {
    const terminal = this.terminal
    if (!terminal) return
    const alternate = this.isAlternate()
    if (alternate) {
      this.wasAlternate = true
      return
    }
    if (this.wasAlternate) this.wasAlternate = false

    if (this.wipePending && !this.syncActive && this.currentMode === 'anchored') {
      this.wipePending = false
      const buffer = terminal.buffer.active
      const target = resolveScrollAnchor(this.snapshot, {
        baseY: buffer.baseY,
        rows: terminal.rows,
        getLine: y => buffer.getLine(y)?.translateToString(true) ?? null,
      })
      terminal.scrollToLine(target)
      // The gmux xterm fork applies synchronized-output viewport DOM state
      // after onWriteParsed. Repeat only if no newer user scroll occurred;
      // this is a rendering catch-up, never permission to override intent.
      if (typeof requestAnimationFrame !== 'undefined') {
        const version = this.userScrollVersion
        this.wipeSyncRAF = requestAnimationFrame(() => {
          this.wipeSyncRAF = null
          if (this.terminal === terminal && this.currentMode === 'anchored'
            && this.userScrollVersion === version && !this.isAlternate()) {
            terminal.scrollToLine(target)
          }
        })
      }
      return
    }

    if (this.currentMode === 'following') {
      const buffer = terminal.buffer.active
      if (buffer.viewportY < buffer.baseY) this.scrollToBottomWithoutAnimation(terminal)
    }
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
