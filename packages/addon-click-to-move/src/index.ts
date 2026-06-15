import type { IDisposable, IMarker, ITerminalAddon, Terminal } from '@xterm/xterm'
import {
  type ClickToMoveCell,
  clickIsEditable,
  encodeSgrClick,
  parseOsc133,
  type PromptRegion,
} from './protocol.js'

export {
  type ClickToMoveCell,
  type Osc133,
  type PromptRegion,
  clickIsEditable,
  encodeSgrClick,
  parseOsc133,
} from './protocol.js'

export interface ClickToMoveOptions {
  /**
   * Produce the byte string sent to the application for a click at `cell`.
   * Defaults to {@link encodeSgrClick} (an xterm SGR left-button report).
   */
  encodeReport?: (cell: ClickToMoveCell) => string

  /**
   * Route the encoded report to the application. Defaults to
   * `terminal.input(data, true)`, which fires the terminal's `onData`
   * event like any other user input.
   *
   * Hosts that post-process `onData` (e.g. gmux applies armed ctrl/alt
   * modifiers) should pass a sink that writes to the PTY directly, so a
   * synthetic mouse report can't be mangled by modifier handling.
   */
  sendInput?: (data: string) => void

  /**
   * Force the addon armed regardless of the OSC 133 `click_events` opt-in.
   * Useful for tests or applications that advertise capability out of band.
   * Default false.
   */
  forceArmed?: boolean

  /**
   * Maximum pointer movement, in pixels, between mousedown and mouseup that
   * is still treated as a click (rather than a drag / text selection).
   * Default 3.
   */
  dragThresholdPx?: number

  /** Diagnostics hook called whenever a report is emitted. */
  onReport?: (cell: ClickToMoveCell, data: string) => void
}

/**
 * Click-to-move-cursor for xterm.js, driven by OSC 133 shell-integration
 * prompt marks.
 *
 * The addon never enables an xterm mouse mode, so native text selection and
 * scroll-wheel behaviour are preserved. Instead it:
 *
 *   1. tracks the current prompt's editable region from OSC 133 A/B/C/D
 *      marks (anchored to terminal markers so they follow scrollback);
 *   2. on a plain left-click inside that region, maps the pixel position to
 *      a grid cell and sends an SGR mouse report to the application, which
 *      moves its own cursor.
 *
 * The application must advertise support by emitting `OSC 133 ; A ;
 * click_events=1 ST` (matching kitty's convention); without the opt-in the
 * addon stays dormant and clicks behave normally.
 *
 * Note: set `altClickMovesCursor: false` on the Terminal to avoid xterm's
 * built-in alt-click arrow-key simulation fighting this addon.
 */
export class ClickToMoveAddon implements ITerminalAddon {
  private _term: Terminal | undefined
  private _opts: Required<Omit<ClickToMoveOptions, 'onReport'>> & Pick<ClickToMoveOptions, 'onReport'>
  private _disposables: IDisposable[] = []

  // Prompt state, updated by OSC 133 handling.
  private _armed = false
  private _commandRunning = false
  private _promptStart: IMarker | undefined
  private _inputStart: IMarker | undefined

  // Pointer gesture state.
  private _downX = 0
  private _downY = 0
  private _downButton = -1

  constructor(options: ClickToMoveOptions = {}) {
    this._opts = {
      encodeReport: options.encodeReport ?? encodeSgrClick,
      sendInput: options.sendInput ?? ((data) => this._term?.input(data, true)),
      forceArmed: options.forceArmed ?? false,
      dragThresholdPx: options.dragThresholdPx ?? 3,
      onReport: options.onReport,
    }
  }

  activate(terminal: Terminal): void {
    this._term = terminal

    this._disposables.push(
      terminal.parser.registerOscHandler(133, (data) => {
        this._handleOsc133(data)
        // Observer only: return false so any other OSC 133 handler (e.g. a
        // future prompt-jump feature) still runs.
        return false
      }),
    )

    const el = terminal.element
    if (el) {
      const onDown = (ev: MouseEvent) => this._onMouseDown(ev)
      const onUp = (ev: MouseEvent) => this._onMouseUp(ev)
      el.addEventListener('mousedown', onDown)
      el.addEventListener('mouseup', onUp)
      this._disposables.push({
        dispose: () => {
          el.removeEventListener('mousedown', onDown)
          el.removeEventListener('mouseup', onUp)
        },
      })
    }
  }

  dispose(): void {
    for (const d of this._disposables) d.dispose()
    this._disposables = []
    this._disposeMarks()
    this._term = undefined
  }

  // --- OSC 133 prompt tracking ------------------------------------------

  private _handleOsc133(data: string): void {
    const term = this._term
    if (!term) return
    const { kind, params } = parseOsc133(data)
    switch (kind) {
      case 'A': // fresh prompt begins
        this._disposeMarks()
        this._armed = this._opts.forceArmed || params.get('click_events') === '1'
        this._commandRunning = false
        this._promptStart = term.registerMarker(0)
        break
      case 'B': // end of prompt, start of editable input
        this._inputStart = term.registerMarker(0)
        break
      case 'C': // command started executing — region no longer editable
      case 'D': // command finished — still not editable until next prompt
        this._commandRunning = true
        break
    }
  }

  private _disposeMarks(): void {
    this._promptStart?.dispose()
    this._inputStart?.dispose()
    this._promptStart = undefined
    this._inputStart = undefined
  }

  private _region(): PromptRegion {
    const term = this._term
    const startMark = this._inputStart ?? this._promptStart
    const startLine = startMark && startMark.line >= 0 ? startMark.line : undefined
    let endLine: number | undefined
    if (term && startLine !== undefined) {
      const buf = term.buffer.active
      endLine = buf.baseY + buf.cursorY
    }
    return {
      armed: this._armed,
      commandRunning: this._commandRunning,
      startLine,
      endLine,
    }
  }

  // --- Pointer gesture handling -----------------------------------------

  private _onMouseDown(ev: MouseEvent): void {
    // Only plain left-clicks are candidates. Any modifier means the user is
    // selecting (shift), doing rectangular selection (alt), etc. — leave
    // those to xterm.
    if (ev.button !== 0 || ev.shiftKey || ev.altKey || ev.ctrlKey || ev.metaKey) {
      this._downButton = -1
      return
    }
    this._downButton = 0
    this._downX = ev.clientX
    this._downY = ev.clientY
  }

  private _onMouseUp(ev: MouseEvent): void {
    if (this._downButton !== 0 || ev.button !== 0) return
    this._downButton = -1

    // Reject drags (text selection): only a near-stationary press→release
    // counts as a click.
    const moved = Math.hypot(ev.clientX - this._downX, ev.clientY - this._downY)
    if (moved > this._opts.dragThresholdPx) return

    const cell = this._cellFromEvent(ev)
    if (!cell) return
    if (!clickIsEditable(this._region(), cell.line)) return

    const data = this._opts.encodeReport(cell)
    this._opts.sendInput(data)
    this._opts.onReport?.(cell, data)
  }

  /** Map a mouse event to a grid cell, or undefined if outside the screen. */
  private _cellFromEvent(ev: MouseEvent): ClickToMoveCell | undefined {
    const term = this._term
    if (!term) return undefined
    const screen = this._screenEl()
    if (!screen) return undefined
    const rect = screen.getBoundingClientRect()
    if (rect.width <= 0 || rect.height <= 0) return undefined

    const cols = term.cols
    const rows = term.rows
    const cw = rect.width / cols
    const ch = rect.height / rows
    const col = Math.floor((ev.clientX - rect.left) / cw)
    const row = Math.floor((ev.clientY - rect.top) / ch)
    if (col < 0 || row < 0 || col >= cols || row >= rows) return undefined

    const line = term.buffer.active.viewportY + row
    return { col, row, line }
  }

  private _screenEl(): HTMLElement | undefined {
    const el = this._term?.element
    if (!el) return undefined
    return (el.querySelector('.xterm-screen') as HTMLElement | null) ?? el
  }
}
