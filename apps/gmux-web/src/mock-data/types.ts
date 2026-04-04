import type { Session } from '../types'

/** A mock session: all runtime Session fields plus terminal content. */
export type MockSession = Session & {
  /** Cursor column (0-based). */
  cursorX?: number
  /** Cursor row (0-based, absolute in scrollback). */
  cursorY?: number
  /** Raw terminal content. Plain \n line endings (normalized to \r\n on render). */
  terminal: string
  /** Mock mode: simulate recent activity for this session (drives the active dot). */
  mockActive?: boolean
}

/** Helper: ISO timestamp for N minutes ago. */
export function ago(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString()
}
