/** Unit + mutation-hardening tests for snapshot reconciliation.
 *
 * The equality fast path is the dangerous part of structural sharing: a
 * comparator that ignores a field would *reuse* a row that actually changed,
 * and the UI would silently stop updating that field. These tests flip every
 * `Session` field (and every nested `status`/`command`/`remotes` variant)
 * and assert the changed row is never reused — a mutant comparator that
 * drops any field check fails here deterministically.
 */
import { describe, expect, it } from 'vitest'
import {
  diffReplacedRows, factsUnchanged, reconcileSessions, SESSION_KEYS,
  sessionRowEquals, substituteRows,
} from './reconcile'
import type { Session } from './types'

/** Fully populated row: every optional field present, so a mutant that only
 * compares "fields that happen to be set" still gets caught per field. */
function fullRow(id: string): Session {
  return {
    id,
    peer: 'hostA',
    created_at: '2026-01-01T00:00:00Z',
    command: ['pi', '--flag'],
    cwd: '/work/p1',
    workspace_root: '/work',
    remotes: { origin: 'git@x:y.git' },
    adapter: 'pi',
    drive_mode: 'terminal',
    parent_session_id: 'parent-1',
    launched_from_session_id: 'launcher-1',
    semantic_agent: true,
    alive: true,
    pid: 42,
    exit_code: null,
    started_at: '2026-01-01T00:00:01Z',
    exited_at: null,
    title: 'a title',
    subtitle: 'a subtitle',
    status: { active: true, error: false, interrupted: false },
    unread: false,
    unread_token: 'tok-1',
    resumable: true,
    last_output_at: '2026-01-02T00:00:00Z',
    socket_path: '/tmp/s.sock',
    terminal_cols: 80,
    terminal_rows: 24,
    slug: 'a-title',
    conversation_file: '/conv/1.jsonl',
    runner_version: '1.2.3',
    binary_hash: 'abc',
    project_slug: 'p1',
    project_index: 3,
  }
}

/** Re-encode: what the server does every snapshot — same values, all-new
 * object identities (including nested ones). */
function reencode(s: Session): Session {
  return {
    ...s,
    command: [...s.command],
    remotes: s.remotes ? { ...s.remotes } : s.remotes,
    status: s.status ? { ...s.status } : s.status,
  }
}

/** One changed variant per field (plus extra nested variants below). */
function mutate(base: Session, key: keyof Session): Session {
  const next = reencode(base)
  const v = base[key]
  let changed: unknown
  switch (typeof v) {
    case 'string': changed = `${v}-changed`; break
    case 'boolean': changed = !v; break
    case 'number': changed = (v as number) + 1; break
    default:
      if (key === 'command') changed = [...base.command, 'extra']
      else if (key === 'remotes') changed = { ...base.remotes, upstream: 'new' }
      else if (key === 'status') changed = { ...base.status!, active: !base.status!.active }
      else if (v === null || v === undefined) {
        // Nullable scalar: give it a value of the field's type.
        changed = key === 'pid' || key === 'exit_code' ? 7 : '2026-02-02T00:00:00Z'
      } else changed = `${String(v)}-changed`
  }
  ;(next as unknown as Record<string, unknown>)[key as string] = changed
  return next
}

describe('sessionRowEquals', () => {
  it('re-encoded identical rows are equal', () => {
    const a = fullRow('s1')
    expect(sessionRowEquals(a, reencode(a))).toBe(true)
  })

  it('sparse rows (optionals absent) equal their re-encode', () => {
    const sparse: Session = {
      id: 's', created_at: 'c', command: [], cwd: '', adapter: 'shell',
      alive: false, pid: null, exit_code: 1, started_at: 'c', exited_at: 'e',
      title: 't', subtitle: '', status: null, unread: false, socket_path: '',
    }
    expect(sessionRowEquals(sparse, reencode(sparse))).toBe(true)
    expect(sessionRowEquals(sparse, { ...sparse, status: { active: false } })).toBe(false)
    expect(sessionRowEquals({ ...sparse, remotes: { a: 'b' } }, sparse)).toBe(false)
  })

  // Mutation hardening: every field, one at a time. A comparator mutant that
  // stops checking any field fails the corresponding case.
  for (const key of SESSION_KEYS) {
    it(`detects a change in '${String(key)}'`, () => {
      const a = fullRow('s1')
      const b = mutate(a, key)
      expect(b).not.toEqual(a) // the mutation itself must be real
      expect(sessionRowEquals(a, b)).toBe(false)
      expect(sessionRowEquals(b, a)).toBe(false)
    })
  }

  it('detects nested variants: command element, remotes value/removal, status flags', () => {
    const a = fullRow('s1')
    expect(sessionRowEquals(a, { ...reencode(a), command: ['pi', '--other'] })).toBe(false)
    expect(sessionRowEquals(a, { ...reencode(a), remotes: { origin: 'other' } })).toBe(false)
    expect(sessionRowEquals(a, { ...reencode(a), remotes: {} })).toBe(false)
    expect(sessionRowEquals(a, { ...reencode(a), remotes: undefined })).toBe(false)
    expect(sessionRowEquals(a, { ...reencode(a), status: { ...a.status!, error: true } })).toBe(false)
    expect(sessionRowEquals(a, { ...reencode(a), status: { ...a.status!, interrupted: true } })).toBe(false)
    expect(sessionRowEquals(a, { ...reencode(a), status: null })).toBe(false)
  })
})

describe('reconcileSessions', () => {
  const rows = [fullRow('a'), { ...fullRow('b'), peer: undefined }, fullRow('c')]

  it('returns the previous array identity for an identical re-encode', () => {
    expect(reconcileSessions(rows, rows.map(reencode))).toBe(rows)
  })

  it('reuses unchanged row objects, keeps the changed row fresh', () => {
    const incoming = rows.map(reencode)
    incoming[1] = { ...incoming[1], unread: true, unread_token: 'flip' }
    const out = reconcileSessions(rows, incoming)
    expect(out).not.toBe(rows)
    expect(out[0]).toBe(rows[0])
    expect(out[2]).toBe(rows[2])
    // Mutation guard: the changed row must NOT be the previous object.
    expect(out[1]).not.toBe(rows[1])
    expect(out[1]).toBe(incoming[1])
    expect(out).toEqual(incoming) // full-replace semantics preserved exactly
  })

  it('every single-field mutant yields a fresh row object (never reused)', () => {
    for (const key of SESSION_KEYS) {
      const incoming = rows.map(reencode)
      incoming[0] = mutate(rows[0], key)
      const out = reconcileSessions(rows, incoming)
      expect(out[0], `field ${String(key)}`).not.toBe(rows[0])
      expect(out[0], `field ${String(key)}`).toEqual(incoming[0])
      expect(out).toEqual(incoming)
    }
  })

  it('reorder keeps row identities but returns a new array in the new order', () => {
    const incoming = [reencode(rows[2]), reencode(rows[0]), reencode(rows[1])]
    const out = reconcileSessions(rows, incoming)
    expect(out).not.toBe(rows)
    expect(out.map(s => s.id)).toEqual(['c', 'a', 'b'])
    expect(out[0]).toBe(rows[2])
    expect(out[1]).toBe(rows[0])
  })

  it('removal / addition produce a new array; survivors keep identity', () => {
    const removed = reconcileSessions(rows, [reencode(rows[0]), reencode(rows[2])])
    expect(removed.map(s => s.id)).toEqual(['a', 'c'])
    expect(removed[0]).toBe(rows[0])
    const added = reconcileSessions(rows, [...rows.map(reencode), fullRow('d')])
    expect(added.length).toBe(4)
    expect(added[1]).toBe(rows[1])
    expect(added[3].id).toBe('d')
  })

  it('empty previous snapshot passes the incoming list through', () => {
    const incoming = rows.map(reencode)
    expect(reconcileSessions([], incoming)).toEqual(incoming)
  })
})

describe('diffReplacedRows / substituteRows', () => {
  const rows = [fullRow('a'), fullRow('b'), fullRow('c')]

  it('identical arrays diff to an empty replacement map', () => {
    expect(diffReplacedRows(rows, rows)!.size).toBe(0)
    expect(diffReplacedRows(rows, [...rows])!.size).toBe(0)
  })

  it('replaced-only successor yields the old→new map', () => {
    const next = [...rows]
    next[1] = { ...rows[1], unread: true }
    const d = diffReplacedRows(rows, next)!
    expect(d.size).toBe(1)
    expect(d.get(rows[1])).toBe(next[1])
  })

  it('structural changes (reorder, removal, addition, id swap) return null', () => {
    expect(diffReplacedRows(rows, [rows[1], rows[0], rows[2]])).toBeNull()
    expect(diffReplacedRows(rows, rows.slice(0, 2))).toBeNull()
    expect(diffReplacedRows(rows, [...rows, fullRow('d')])).toBeNull()
    expect(diffReplacedRows(rows, [rows[0], fullRow('x'), rows[2]])).toBeNull()
  })

  it('factsUnchanged rejects a replaced pair that moved a listed fact', () => {
    const next = [...rows]
    next[0] = { ...rows[0], alive: !rows[0].alive }
    const d = diffReplacedRows(rows, next)!
    expect(factsUnchanged(d, ['alive'])).toBe(false)
    expect(factsUnchanged(d, ['unread'])).toBe(true)
  })

  it('substituteRows preserves array identity when nothing in it changed', () => {
    const replaced = new Map([[rows[1], { ...rows[1], unread: true }]])
    const untouched = [rows[0], rows[2]]
    expect(substituteRows(untouched, replaced)).toBe(untouched)
    const touched = substituteRows(rows, replaced)
    expect(touched).not.toBe(rows)
    expect(touched[1]).toBe(replaced.get(rows[1]))
    expect(touched[0]).toBe(rows[0])
  })
})
