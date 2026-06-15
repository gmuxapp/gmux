// Pure, DOM-free logic for the click-to-move addon.
//
// Everything here is independent of xterm.js and the browser so it can be
// unit-tested directly. The addon (index.ts) wires these helpers to xterm's
// OSC 133 parser hook, terminal markers, and DOM mouse events.

/** A click resolved to terminal grid coordinates. */
export interface ClickToMoveCell {
  /** 0-based column within the viewport row. */
  col: number
  /** 0-based row within the viewport (0 = top visible row). */
  row: number
  /** Absolute buffer line index (baseY-relative) that was clicked. */
  line: number
}

/** Parsed OSC 133 sequence. */
export interface Osc133 {
  /** The mark kind: "A" (prompt start), "B" (input start), "C" (command
   *  executing), "D" (command finished), or any other token verbatim. */
  kind: string
  /** Semicolon-separated key=value parameters following the kind. Bare
   *  tokens (no "=") map to an empty string. */
  params: Map<string, string>
}

/**
 * Parse the payload of an OSC 133 sequence (the bytes after "133;").
 *
 * Examples:
 *   "A"                    -> { kind: "A", params: {} }
 *   "A;click_events=1"     -> { kind: "A", params: { click_events: "1" } }
 *   "D;0"                  -> { kind: "D", params: { "0": "" } }
 */
export function parseOsc133(data: string): Osc133 {
  const parts = data.split(';')
  const kind = parts[0] ?? ''
  const params = new Map<string, string>()
  for (let i = 1; i < parts.length; i++) {
    const tok = parts[i]
    const eq = tok.indexOf('=')
    if (eq >= 0) params.set(tok.slice(0, eq), tok.slice(eq + 1))
    else params.set(tok, '')
  }
  return { kind, params }
}

/**
 * Encode a click as the default report sent to the application: an xterm
 * SGR mouse press+release for the left button (button code 0), with 1-based
 * column and row matching the terminal grid the application renders into.
 *
 *   ESC [ < 0 ; col ; row M   (press)
 *   ESC [ < 0 ; col ; row m   (release)
 *
 * This is the same encoding kitty uses for its shell-integration click
 * events, so a cooperating application (e.g. pi) can decode it with a
 * standard SGR mouse parser.
 */
export function encodeSgrClick(cell: ClickToMoveCell): string {
  const c = cell.col + 1
  const r = cell.row + 1
  return `\x1b[<0;${c};${r}M\x1b[<0;${c};${r}m`
}

/** Snapshot of the prompt region used to decide whether a click is editable. */
export interface PromptRegion {
  /** True once the app advertised click support (OSC 133 click_events=1). */
  armed: boolean
  /** True while a command is running (between OSC 133 C and the next A): the
   *  prompt is no longer editable, so clicks must be ignored. */
  commandRunning: boolean
  /** First absolute buffer line of the editable region (the B mark, or the A
   *  mark when no B was seen). Undefined when there is no active prompt. */
  startLine: number | undefined
  /** Last absolute buffer line of the editable region — normally the line the
   *  cursor is currently on. Undefined when there is no active prompt. */
  endLine: number | undefined
}

/**
 * Decide whether a click at `clickedLine` should produce a move report.
 *
 * A click qualifies only when the application has opted in, no command is
 * running, and the click lands within the current prompt's editable line
 * range. Everything else (scrollback, output region, clicks before opt-in)
 * is left to xterm's native selection behaviour.
 */
export function clickIsEditable(region: PromptRegion, clickedLine: number): boolean {
  if (!region.armed || region.commandRunning) return false
  if (region.startLine === undefined || region.endLine === undefined) return false
  const lo = Math.min(region.startLine, region.endLine)
  const hi = Math.max(region.startLine, region.endLine)
  return clickedLine >= lo && clickedLine <= hi
}
