/**
 * Debug tool: copies the currently selected session as a complete MockSession
 * file to clipboard, ready to paste into this folder.
 *
 * Usage: open devtools console, run __gmuxCopySession()
 *
 * - Waits for document focus before clipboard write
 * - Serializes xterm buffer cell-by-cell preserving all SGR escapes
 * - Deduplicates: spaces with default bg skip SGR transitions
 * - Extracts repeated escape codes into named variables
 * - Outputs a valid TypeScript file matching the MockSession format
 */

import type { Session } from '../types'
import type { Terminal } from '@xterm/xterm'

// ── SGR serializer ──

/** Build the full SGR param list for a single cell. */
function cellSGR(cell: any): number[] {
  const sgr: number[] = []
  if (cell.isBold()) sgr.push(1)
  if (cell.isDim()) sgr.push(2)
  if (cell.isItalic()) sgr.push(3)
  if (cell.isUnderline()) sgr.push(4)
  if (cell.isBlink()) sgr.push(5)
  if (cell.isInverse()) sgr.push(7)
  if (cell.isInvisible()) sgr.push(8)
  if (cell.isStrikethrough()) sgr.push(9)
  if (cell.isOverline()) sgr.push(53)
  if (cell.isFgPalette()) {
    const c = cell.getFgColor()
    if (c < 8) sgr.push(30 + c)
    else if (c < 16) sgr.push(90 + c - 8)
    else sgr.push(38, 5, c)
  } else if (cell.isFgRGB()) {
    const c = cell.getFgColor()
    sgr.push(38, 2, (c >> 16) & 0xff, (c >> 8) & 0xff, c & 0xff)
  }
  if (cell.isBgPalette()) {
    const c = cell.getBgColor()
    if (c < 8) sgr.push(40 + c)
    else if (c < 16) sgr.push(100 + c - 8)
    else sgr.push(48, 5, c)
  } else if (cell.isBgRGB()) {
    const c = cell.getBgColor()
    sgr.push(48, 2, (c >> 16) & 0xff, (c >> 8) & 0xff, c & 0xff)
  }
  return sgr
}

/** Serialize one buffer line with minimal SGR escape changes. */
function serializeLine(line: any, cols: number): string {
  const ESC = '\x1b'
  const cells: { ch: string; key: string }[] = []
  const cell = line.getCell(0)!
  for (let x = 0; x < cols; x++) {
    line.getCell(x, cell)
    if (cell.getWidth() === 0) continue
    const ch = cell.getChars() || ' '
    const sgr = cellSGR(cell)
    cells.push({ ch, key: sgr.join(';') })
  }

  // Trim trailing whitespace (spaces with no bg)
  const hasBg = (key: string) => key.split(';').some(n => { const v = +n; return v >= 40 && v <= 107 })
  while (cells.length && cells[cells.length - 1].ch === ' ' && !hasBg(cells[cells.length - 1].key))
    cells.pop()
  if (!cells.length) return ''

  let out = ''
  let curKey = ''
  let i = 0
  while (i < cells.length) {
    const c = cells[i]
    const isPlainSpace = c.ch === ' ' && !hasBg(c.key)
    if (isPlainSpace) {
      let spaces = ''
      while (i < cells.length && cells[i].ch === ' ' && !hasBg(cells[i].key)) {
        spaces += ' '
        i++
      }
      out += spaces
    } else {
      if (c.key !== curKey) {
        out += c.key ? `${ESC}[0;${c.key}m` : `${ESC}[0m`
        curKey = c.key
      }
      out += c.ch
      i++
    }
  }
  if (curKey) out += `${ESC}[0m`
  return out
}

/** Serialize the full terminal buffer + cursor position. */
function serializeTerminal(term: Terminal): { content: string; cursorX: number; cursorY: number } {
  const buf = term.buffer.active
  const lines: string[] = []
  for (let i = 0; i < buf.length; i++) {
    const line = buf.getLine(i)
    if (!line) continue
    lines.push(serializeLine(line, term.cols))
  }
  while (lines.length > 0 && lines[lines.length - 1] === '') lines.pop()
  return { content: lines.join('\n'), cursorX: buf.cursorX, cursorY: buf.cursorY + buf.baseY }
}

// ── Variable extraction ──

/** Simple SGR sequences that get a readable name instead of C1, C2, etc. */
const WELL_KNOWN: Record<string, string> = {
  '\x1b[0m': 'RST',
  '\x1b[0;1m': 'BOLD',
  '\x1b[0;2m': 'DIM',
  '\x1b[0;3m': 'ITALIC',
  '\x1b[0;4m': 'UNDER',
  '\x1b[0;7m': 'INVERSE',
  '\x1b[0;9m': 'STRIKE',
  '\x1b[0;31m': 'RED',
  '\x1b[0;32m': 'GREEN',
  '\x1b[0;33m': 'YELLOW',
  '\x1b[0;34m': 'BLUE',
  '\x1b[0;35m': 'MAGENTA',
  '\x1b[0;36m': 'CYAN',
  '\x1b[0;37m': 'WHITE',
  '\x1b[0;90m': 'GRAY',
  '\x1b[0;41m': 'BG_RED',
  '\x1b[0;42m': 'BG_GREEN',
  '\x1b[0;43m': 'BG_YELLOW',
  '\x1b[0;44m': 'BG_BLUE',
  '\x1b[0;91m': 'HI_RED',
  '\x1b[0;92m': 'HI_GREEN',
  '\x1b[0;93m': 'HI_YELLOW',
  '\x1b[0;94m': 'HI_BLUE',
  '\x1b[0;95m': 'HI_MAGENTA',
  '\x1b[0;96m': 'HI_CYAN',
  '\x1b[0;97m': 'HI_WHITE',
  '\x1b[0;1;37m': 'BOLD_WHITE',
  '\x1b[0;1;31m': 'BOLD_RED',
  '\x1b[0;1;32m': 'BOLD_GREEN',
  '\x1b[0;1;36m': 'BOLD_CYAN',
}

/**
 * Extract all escape sequences from terminal content into variables.
 * Well-known simple codes get readable names (RST, BOLD, GREEN, etc).
 * Everything else gets short sequential names (C1, C2, C3, ...).
 */
function extractVariables(terminal: string): { vars: string; body: string } {
  // Collect unique escape sequences in order of first appearance
  const seen = new Set<string>()
  const ordered: string[] = []
  for (const m of terminal.matchAll(/\x1b\[[0-9;]*m/g)) {
    if (!seen.has(m[0])) {
      seen.add(m[0])
      ordered.push(m[0])
    }
  }

  if (!ordered.length) {
    return { vars: '', body: quoteTerminal(terminal) }
  }

  // Assign names: well-known get readable names, rest get C1, C2, ...
  const usedNames = new Set<string>()
  const nameMap = new Map<string, string>()
  let counter = 1
  for (const esc of ordered) {
    let name = WELL_KNOWN[esc]
    if (name && !usedNames.has(name)) {
      usedNames.add(name)
    } else {
      name = `C${counter++}`
      usedNames.add(name)
    }
    nameMap.set(esc, name)
  }

  // Build variable declarations
  const vars = [...nameMap.entries()]
    .map(([esc, name]) => {
      const escaped = esc.replace(/\x1b/g, '\\x1b')
      return `const ${name} = '${escaped}'`
    })
    .join('\n')

  // Replace escapes in terminal string with interpolations
  // Sort by length descending so longer sequences are replaced first
  let replaced = terminal
  const sorted = [...nameMap.entries()].sort((a, b) => b[0].length - a[0].length)
  for (const [esc, name] of sorted) {
    replaced = replaced.split(esc).join(`\${${name}}`)
  }

  return { vars, body: quoteTerminal(replaced) }
}

/** Quote a terminal string as a single backtick template literal. */
function quoteTerminal(s: string): string {
  const escaped = s.replace(/\\/g, '\\\\').replace(/`/g, '\\`')
  return '`' + escaped + '`'
}

// ── File generator ──

function generateFile(session: Session, terminal: string, cursorX: number, cursorY: number): string {
  const { vars, body } = extractVariables(terminal)

  const lines: string[] = []
  lines.push("import { type MockSession, ago } from '../types'")
  lines.push('')
  if (vars) {
    lines.push(vars)
    lines.push('')
  }
  lines.push('export default {')
  lines.push(`  id: '${session.id}',`)
  lines.push(`  created_at: ago(0),`)
  lines.push(`  command: ${JSON.stringify(session.command)},`)
  lines.push(`  cwd: '${session.cwd}',`)
  if (session.workspace_root) {
    lines.push(`  workspace_root: '${session.workspace_root}',`)
  }
  lines.push(`  kind: '${session.kind}',`)
  lines.push(`  alive: ${session.alive},`)
  lines.push(`  pid: ${session.pid},`)
  lines.push(`  exit_code: ${session.exit_code},`)
  lines.push(`  started_at: ago(0),`)
  lines.push(`  exited_at: ${session.exited_at ? 'ago(0)' : 'null'},`)
  lines.push(`  title: ${JSON.stringify(session.title)},`)
  lines.push(`  subtitle: ${JSON.stringify(session.subtitle)},`)
  if (session.status) {
    lines.push(`  status: { label: ${JSON.stringify(session.status.label)}, working: ${session.status.working} },`)
  } else {
    lines.push(`  status: null,`)
  }
  lines.push(`  unread: ${session.unread},`)
  lines.push(`  socket_path: '/tmp/gmux-sessions/${session.id}.sock',`)
  if (session.stale) lines.push(`  stale: true,`)
  lines.push(`  cursorX: ${cursorX},`)
  lines.push(`  cursorY: ${cursorY},`)
  lines.push(`  terminal: ${body},`)
  lines.push('} satisfies MockSession')
  lines.push('')
  return lines.join('\n')
}

// ── Attach to window ──

export function installCopySession(): void {
  ;(window as any).__gmuxCopySession = () => {
    const doCopy = () => {
      const session = (window as any).__gmuxSession as Session | null
      const term = (window as any).__gmuxTerm as Terminal | null

      if (!session) {
        console.warn('No session selected')
        return
      }
      if (!term) {
        console.warn('No active terminal')
        return
      }

      const { content, cursorX, cursorY } = serializeTerminal(term)
      const file = generateFile(session, content, cursorX, cursorY)

      navigator.clipboard.writeText(file).then(
        () => console.log(`Copied MockSession file (${file.length} chars) to clipboard`),
        (err) => console.error('Copy failed:', err),
      )
    }

    if (document.hasFocus()) {
      doCopy()
    } else {
      console.log('Click the page to focus, then copy will proceed...')
      window.addEventListener('focus', doCopy, { once: true })
    }
  }
}
