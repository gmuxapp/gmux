import { describe, it, expect, beforeEach } from 'vitest'
import {
  addToast, removeToast, pushToast, pushError, dismissToast,
  toasts, MAX_TOASTS, type Toast,
} from './toasts'

function mk(id: string, message: string, kind: Toast['kind'] = 'error', count = 1): Toast {
  return { id, kind, message, at: 0, count }
}

describe('addToast (pure)', () => {
  it('appends newest last', () => {
    const out = addToast([mk('a', 'one')], mk('b', 'two'))
    expect(out.map(t => t.id)).toEqual(['a', 'b'])
  })

  it('caps the list at MAX_TOASTS, dropping oldest', () => {
    let list: Toast[] = []
    for (let i = 0; i < MAX_TOASTS + 3; i++) list = addToast(list, mk(`t${i}`, `m${i}`))
    expect(list).toHaveLength(MAX_TOASTS)
    expect(list[0].id).toBe('t3')
  })

  it('coalesces an identical kind+message: bumps count, takes new id, moves to end', () => {
    const start = [mk('a', 'same'), mk('b', 'other')]
    const out = addToast(start, mk('c', 'same'))
    expect(out).toHaveLength(2)
    // 'same' moved to the end with the new id and count bumped.
    expect(out.map(t => t.id)).toEqual(['b', 'c'])
    expect(out[1]).toMatchObject({ message: 'same', count: 2, id: 'c' })
  })

  it('keeps counting on repeated coalesce', () => {
    let list = addToast([], mk('a', 'dup'))
    list = addToast(list, mk('b', 'dup'))
    list = addToast(list, mk('c', 'dup'))
    expect(list).toHaveLength(1)
    expect(list[0]).toMatchObject({ count: 3, id: 'c' })
  })

  it('does not coalesce across kinds', () => {
    const out = addToast([mk('a', 'same', 'error')], mk('b', 'same', 'info'))
    expect(out).toHaveLength(2)
  })
})

describe('removeToast (pure)', () => {
  it('removes by id', () => {
    expect(removeToast([mk('a', 'x'), mk('b', 'y')], 'a').map(t => t.id)).toEqual(['b'])
  })
})

describe('signal-backed API', () => {
  beforeEach(() => { toasts.value = [] })

  it('pushToast adds to the signal and returns an id', () => {
    const id = pushToast('info', 'hello')
    expect(toasts.value).toHaveLength(1)
    expect(toasts.value[0]).toMatchObject({ id, kind: 'info', message: 'hello', count: 1 })
  })

  it('pushError coalesces a repeat into a single counted entry', () => {
    pushError('boom')
    pushError('boom')
    expect(toasts.value).toHaveLength(1)
    expect(toasts.value[0].count).toBe(2)
  })

  it('dismissToast removes by id and is idempotent', () => {
    const id = pushError('x')
    dismissToast(id)
    expect(toasts.value).toHaveLength(0)
    dismissToast(id)
    expect(toasts.value).toHaveLength(0)
  })

  it('dismissToast targets the current id after coalesce', () => {
    pushError('dup')
    const id2 = pushError('dup')
    // The coalesced entry carries the latest id; dismissing it clears.
    dismissToast(id2)
    expect(toasts.value).toHaveLength(0)
  })
})
