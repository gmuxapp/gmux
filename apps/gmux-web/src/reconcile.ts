/** Structural sharing for protocol-3 session snapshots (incremental frontend
 * reconciliation).
 *
 * The wire semantics are wholesale replacement: every committed transaction
 * delivers the *entire* session list, re-encoded server-side, so every row
 * arrives as a fresh object even when nothing about it changed. Before this
 * module, `_rawSessions` swallowed that array verbatim, which meant every
 * projection (folders, day partitions, family index, …) saw N brand-new row
 * identities per commit and rebuilt from scratch — a one-row flip cost
 * exactly a cold rebuild (93 ms @10k, see
 * report-combined-resource-gains.md).
 *
 * Equality strategy (explicit, because JSON identity cannot work — rows are
 * re-encoded per snapshot): a field-by-field structural comparison over the
 * closed UI `Session` shape produced by `toUISession`. There is no version /
 * updated_at field on the wire that proves a row unchanged, so the comparator
 * inspects every field, including the nested `status`, `command` and
 * `remotes` values. The field lists are checked against `keyof Session` at
 * compile time (`SessionKeyExhaustiveness` below), so adding a field to the
 * type without teaching the comparator is a build error — a comparator that
 * silently ignores a field would wrongly reuse a changed row.
 *
 * Cost: one O(row) comparison per row per snapshot — O(N) total with a small
 * constant (measured ~1 ms @10k rows, see the reconcile bench). Parsing
 * stays O(N) regardless; the win is that everything *downstream* of the raw
 * signal can now use object identity as a cheap "provably unchanged" test
 * and patch only the affected slices.
 *
 * This is purely an optimization layer over identical semantics: the
 * reconciled array is deep-equal to the incoming snapshot, atomic-commit
 * behavior and epoch handling are untouched.
 */

import type { Session, SessionStatus } from './types'

// ── Field inventory ──────────────────────────────────────────────────────────
//
// Scalar fields compared with `!==`. Nested fields (`command`, `remotes`,
// `status`) get bespoke comparisons below. The `SessionKeyExhaustiveness`
// alias fails to compile if `Session` grows a key not listed in either set.

const SESSION_SCALAR_KEYS = [
  'id', 'peer', 'created_at', 'cwd', 'workspace_root', 'adapter', 'drive_mode',
  'parent_session_id', 'launched_from_session_id', 'semantic_agent', 'alive',
  'pid', 'exit_code', 'started_at', 'exited_at', 'title', 'subtitle', 'unread',
  'unread_token', 'resumable', 'last_output_at', 'socket_path',
  'terminal_cols', 'terminal_rows', 'slug', 'conversation_file',
  'runner_version', 'binary_hash', 'project_slug', 'project_index',
] as const satisfies readonly (keyof Session)[]

const SESSION_NESTED_KEYS = ['command', 'remotes', 'status'] as const satisfies readonly (keyof Session)[]

type CoveredKey = typeof SESSION_SCALAR_KEYS[number] | typeof SESSION_NESTED_KEYS[number]
// Compile-time exhaustiveness: if `Session` gains a field the comparator
// doesn't know, this alias becomes non-`never` and the assignment errors.
type MissingSessionKey = Exclude<keyof Session, CoveredKey>
const _sessionKeysExhaustive: MissingSessionKey extends never ? true : never = true
void _sessionKeysExhaustive

const STATUS_KEYS = ['active', 'error', 'interrupted'] as const satisfies readonly (keyof SessionStatus)[]
type MissingStatusKey = Exclude<keyof SessionStatus, typeof STATUS_KEYS[number]>
const _statusKeysExhaustive: MissingStatusKey extends never ? true : never = true
void _statusKeysExhaustive

/** Exported for the mutation-hardening test, which flips every field and
 * asserts the fast path never reuses a changed row. */
export const SESSION_KEYS: readonly (keyof Session)[] = [...SESSION_SCALAR_KEYS, ...SESSION_NESTED_KEYS]

function statusEquals(a: SessionStatus | null, b: SessionStatus | null): boolean {
  if (a === b) return true
  if (!a || !b) return false
  for (const k of STATUS_KEYS) if (a[k] !== b[k]) return false
  return true
}

function commandEquals(a: readonly string[], b: readonly string[]): boolean {
  if (a === b) return true
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

function remotesEquals(a: Record<string, string> | undefined, b: Record<string, string> | undefined): boolean {
  if (a === b) return true
  if (!a || !b) return false
  const ak = Object.keys(a)
  if (ak.length !== Object.keys(b).length) return false
  for (const k of ak) if (a[k] !== b[k]) return false
  return true
}

/** Deep structural equality over the closed UI Session shape. */
export function sessionRowEquals(a: Session, b: Session): boolean {
  if (a === b) return true
  for (const k of SESSION_SCALAR_KEYS) if (a[k] !== b[k]) return false
  return commandEquals(a.command, b.command)
    && remotesEquals(a.remotes, b.remotes)
    && statusEquals(a.status, b.status)
}

// ── Snapshot reconciliation ──────────────────────────────────────────────────

/**
 * Reconcile an incoming wholesale snapshot against the previous one:
 *  - a row deep-equal to the previous row with the same id keeps the
 *    *previous* object identity;
 *  - if every row is reused and positions match, the previous *array*
 *    identity is returned, so a byte-identical re-encode is a signal no-op.
 *
 * The returned array is always deep-equal to `next` (order and content come
 * from `next`; only object identities are borrowed from `prev`), so
 * full-replace semantics are preserved exactly.
 */
export function reconcileSessions(prev: readonly Session[], next: readonly Session[]): Session[] {
  if (prev === next) return next as Session[]
  const prevById = new Map<string, Session>()
  for (const p of prev) prevById.set(p.id, p)
  const out = new Array<Session>(next.length)
  let identical = prev.length === next.length
  for (let i = 0; i < next.length; i++) {
    const n = next[i]
    const p = prevById.get(n.id)
    if (p && sessionRowEquals(p, n)) {
      out[i] = p
      if (identical && prev[i] !== p) identical = false
    } else {
      out[i] = n
      identical = false
    }
  }
  return identical ? (prev as Session[]) : out
}

// ── Identity diffing for projection fast paths ───────────────────────────────

const EMPTY_REPLACED: ReadonlyMap<Session, Session> = new Map()

/**
 * Identity diff of two session lists. Returns the old→new object map when
 * `next` differs from `prev` only by *replaced rows* (same length, same ids
 * in the same positions, some object identities swapped); returns `null` for
 * any structural change (insert, removal, reorder, id change) — callers must
 * then take their full non-incremental path.
 */
export function diffReplacedRows(
  prev: readonly Session[],
  next: readonly Session[],
): ReadonlyMap<Session, Session> | null {
  if (prev === next) return EMPTY_REPLACED
  if (prev.length !== next.length) return null
  let replaced: Map<Session, Session> | null = null
  for (let i = 0; i < next.length; i++) {
    const p = prev[i]
    const n = next[i]
    if (p === n) continue
    if (p.id !== n.id) return null
    replaced ??= new Map()
    replaced.set(p, n)
  }
  return replaced ?? EMPTY_REPLACED
}

// ── Fact subsets: which fields a projection's *structure* reads ─────────────
//
// A projection fast path may substitute new row objects into its previous
// output only when every replaced row kept the facts that projection's
// shape (membership, bucketing, ordering) is derived from. Fields not
// listed here may change freely — they only alter row *content*, which the
// substitution carries through. Each list is pinned by the differential
// property test (`reconcile-differential.test.ts`): dropping a needed fact
// makes randomized mutation sequences diverge from the uncached reference.

/** Family topology + child ordering: `createFamilyIndex` reads parentage and
 * agent-ness; `byRecency` orders children by output/creation stamps. */
export const FAMILY_FACT_KEYS = [
  'parent_session_id', 'semantic_agent', 'last_output_at', 'created_at',
] as const satisfies readonly (keyof Session)[]

/** Sidebar membership facts (`sidebarSessions`): liveness + resumability
 * decide list membership; family facts decide which roots get pulled in. */
export const SIDEBAR_FACT_KEYS = [
  'alive', 'resumable', ...FAMILY_FACT_KEYS,
] as const satisfies readonly (keyof Session)[]

/** Folder structure facts (`foldersFrom`/`buildProjectFolders`): bucketing
 * (project_slug, peer), visibility (alive/resumable), ordering
 * (project_index, created_at), family placement (parent/semantic edges,
 * temporary placements from stamped children). */
export const PLACEMENT_FACT_KEYS = [
  'project_slug', 'project_index', 'peer', 'alive', 'resumable', ...FAMILY_FACT_KEYS,
] as const satisfies readonly (keyof Session)[]

/** Day-partition facts (`partitionByDay`): bucket key and in-bucket order
 * derive from the activity timestamp (last_output_at ?? created_at). */
export const ACTIVITY_FACT_KEYS = [
  'last_output_at', 'created_at',
] as const satisfies readonly (keyof Session)[]

/** True when every replaced (old→new) pair kept all the given facts. */
export function factsUnchanged(
  replaced: ReadonlyMap<Session, Session>,
  keys: readonly (keyof Session)[],
): boolean {
  for (const [oldRow, newRow] of replaced) {
    for (const k of keys) if (oldRow[k] !== newRow[k]) return false
  }
  return true
}

/** Substitute replaced row objects into a previous output list. Returns the
 * previous array identity untouched when it contains none of the replaced
 * rows (so unaffected slices keep identity and memoized consumers skip). */
export function substituteRows(
  rows: readonly Session[],
  replaced: ReadonlyMap<Session, Session>,
): readonly Session[] {
  if (replaced.size === 0) return rows
  let touched = false
  for (const s of rows) {
    if (replaced.has(s)) { touched = true; break }
  }
  if (!touched) return rows
  return rows.map(s => replaced.get(s) ?? s)
}
