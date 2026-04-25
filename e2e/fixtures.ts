/**
 * Adapter session-file fixtures used by both global-setup (pre-seeded
 * before gmuxd starts, so the bootstrap scan picks them up) and by
 * test specs (written at test time to exercise the watcher path).
 *
 * Each fixture is a minimally valid JSONL document for its adapter.
 * The shapes here mirror the contracts in `packages/adapter/adapters/*.go`:
 * if those parsers grow new required fields, the smoke spec is the
 * first place to fail and the failure points at fixture drift.
 *
 * Path encoding mirrors each adapter's `SessionDir(cwd)` exactly. We
 * intentionally don't import any Go logic; the fixtures stand on their
 * own and the smoke spec round-trips them through the real parsers via
 * the public API.
 */
import * as fs from 'node:fs'
import * as path from 'node:path'

export type AdapterKind = 'pi' | 'claude' | 'codex'

export interface FixtureSpec {
  kind: AdapterKind
  /** Synthetic cwd for path encoding; must be unique within a test run. */
  cwd: string
  /** Session ID / UUID, used as filename. */
  toolID: string
  /**
   * Human-readable title. Drives slug derivation. Use clean
   * lowercase-with-spaces text so the slug is predictable.
   */
  title: string
}

export interface FixtureResult {
  /** Absolute path the JSONL was written to. */
  filePath: string
  /** Slug `convIndex` will assign (matches Go `adapter.Slugify(title)`). */
  expectedSlug: string
}

/**
 * Mirror of Go's `adapter.Slugify`: lowercase, non-alphanumeric runs
 * become `-`, trim leading/trailing `-`, cap at 40 chars.
 */
export function slugify(s: string): string {
  let out = s.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '')
  if (out.length > 40) {
    out = out.slice(0, 40).replace(/-+$/, '')
  }
  return out
}

/** Pi: strip leading `/`, replace remaining `/` with `-`, wrap in `--`. */
function encodePiCwd(cwd: string): string {
  const stripped = cwd.replace(/^\/+/, '')
  return `--${stripped.replace(/\//g, '-')}--`
}

/** Claude: replace `/` and `.` with `-`. */
function encodeClaudeCwd(cwd: string): string {
  return cwd.replace(/[/.]/g, '-')
}

function piFile(home: string, spec: FixtureSpec): string {
  const root = path.join(home, '.pi', 'agent', 'sessions')
  return path.join(root, encodePiCwd(spec.cwd), `${spec.toolID}.jsonl`)
}

function claudeFile(home: string, spec: FixtureSpec): string {
  const root = path.join(home, '.claude', 'projects')
  return path.join(root, encodeClaudeCwd(spec.cwd), `${spec.toolID}.jsonl`)
}

function codexFile(home: string, spec: FixtureSpec): string {
  const root = path.join(home, '.codex', 'sessions')
  // Codex is date-nested by file creation time; use today's date so
  // ListSessionFiles finds it. Date format mirrors Go's time.Format.
  const now = new Date()
  const yyyy = String(now.getUTCFullYear())
  const mm = String(now.getUTCMonth() + 1).padStart(2, '0')
  const dd = String(now.getUTCDate()).padStart(2, '0')
  return path.join(root, yyyy, mm, dd, `${spec.toolID}.jsonl`)
}

function piContent(spec: FixtureSpec): string {
  const ts = new Date().toISOString()
  // Header line + session_info (sets title directly) + a user message
  // (so `MessageCount > 0` and the conversation passes `CanResume`).
  const header = JSON.stringify({
    type: 'session',
    id: spec.toolID,
    cwd: spec.cwd,
    timestamp: ts,
  })
  const sessionInfo = JSON.stringify({
    type: 'session_info',
    name: spec.title,
  })
  const userMsg = JSON.stringify({
    type: 'message',
    message: { role: 'user', content: [{ type: 'text', text: spec.title }] },
  })
  return header + '\n' + sessionInfo + '\n' + userMsg + '\n'
}

function claudeContent(spec: FixtureSpec): string {
  const ts = new Date().toISOString()
  // Claude's title priority is `custom-title` > first user message;
  // we use a user message so the title round-trip is observable.
  const userLine = JSON.stringify({
    type: 'user',
    sessionId: spec.toolID,
    cwd: spec.cwd,
    timestamp: ts,
    message: { role: 'user', content: spec.title },
  })
  return userLine + '\n'
}

function codexContent(spec: FixtureSpec): string {
  const ts = new Date().toISOString()
  // session_meta line + a user response_item so the title comes from
  // first-user-text (codex has no custom-title mechanism).
  const meta = JSON.stringify({
    type: 'session_meta',
    payload: { id: spec.toolID, timestamp: ts, cwd: spec.cwd },
  })
  const userMsg = JSON.stringify({
    type: 'response_item',
    payload: {
      type: 'message',
      role: 'user',
      content: [{ type: 'input_text', text: spec.title }],
    },
  })
  return meta + '\n' + userMsg + '\n'
}

/** Write a JSONL session file under `home` for the given fixture. */
export function writeFakeSession(home: string, spec: FixtureSpec): FixtureResult {
  let filePath: string
  let content: string
  switch (spec.kind) {
    case 'pi':
      filePath = piFile(home, spec)
      content = piContent(spec)
      break
    case 'claude':
      filePath = claudeFile(home, spec)
      content = claudeContent(spec)
      break
    case 'codex':
      filePath = codexFile(home, spec)
      content = codexContent(spec)
      break
  }
  fs.mkdirSync(path.dirname(filePath), { recursive: true })
  fs.writeFileSync(filePath, content)
  return {
    filePath,
    expectedSlug: slugify(spec.title),
  }
}

/**
 * Append a line to a JSONL session file. Used by tests that want to
 * exercise re-parse on Write events (e.g. claude `custom-title` after
 * initial index).
 */
export function appendToSession(filePath: string, jsonLine: object): void {
  fs.appendFileSync(filePath, JSON.stringify(jsonLine) + '\n')
}

/**
 * Pre-seeded smoke fixtures, written by global-setup before gmuxd
 * starts. Each tests the bootstrap scan path through a real parser.
 *
 * These slugs and toolIDs are referenced from the smoke spec, so the
 * spec asserts the same fixtures global-setup wrote without
 * re-deriving them.
 */
export const SMOKE_FIXTURES: FixtureSpec[] = [
  {
    kind: 'pi',
    cwd: '/var/gmux-e2e/smoke-pi',
    toolID: '00000000-0000-0000-0000-0000000000a1',
    title: 'pi smoke fixture',
  },
  {
    kind: 'claude',
    cwd: '/var/gmux-e2e/smoke-claude',
    toolID: '00000000-0000-0000-0000-0000000000a2',
    title: 'claude smoke fixture',
  },
  {
    kind: 'codex',
    cwd: '/var/gmux-e2e/smoke-codex',
    toolID: '00000000-0000-0000-0000-0000000000a3',
    title: 'codex smoke fixture',
  },
]
