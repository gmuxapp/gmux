import { describe, it, expect } from 'vitest'
import { resolveReferences, remapReferenceItems, removeReferenceItems, removeHostReferenceItems, refKey } from './references'
import type { PeerInfo, ProjectItem } from './types'

function peer(name: string, node_id?: string): PeerInfo {
  return { name, url: `https://${name}`, status: 'connected', session_count: 0, node_id }
}
function ref(peer: string, slug: string, node_id?: string): ProjectItem {
  return { slug, peer, node_id }
}
function owned(slug: string): ProjectItem {
  return { slug, match: [{ path: `/home/${slug}` }] }
}

describe('resolveReferences', () => {
  it('resolves a legacy name-only reference and backfills its node_id', () => {
    const projects = [ref('gmux-hs', 'apps')]
    const roster = [peer('gmux-hs', 'node_hs')]
    const { resolution, unresolved, backfills } = resolveReferences(projects, roster)
    expect(resolution.get(refKey('gmux-hs', 'apps'))).toEqual({ effectivePeer: 'gmux-hs', resolved: true })
    expect(unresolved).toEqual([])
    expect(backfills).toEqual([{ slug: 'apps', fromPeer: 'gmux-hs', toPeer: 'gmux-hs', node_id: 'node_hs' }])
  })

  it('resolves by node_id and follows a renamed host (name drift)', () => {
    // Reference stored "unraid" + node, host now reports "gmux-hs".
    const projects = [ref('unraid', 'apps', 'node_hs')]
    const roster = [peer('gmux-hs', 'node_hs')]
    const { resolution, unresolved, backfills } = resolveReferences(projects, roster)
    expect(resolution.get(refKey('unraid', 'apps'))).toEqual({ effectivePeer: 'gmux-hs', resolved: true })
    expect(unresolved).toEqual([])
    // Backfill follows the rename: update the cached display name.
    expect(backfills).toEqual([{ slug: 'apps', fromPeer: 'unraid', toPeer: 'gmux-hs', node_id: 'node_hs' }])
  })

  it('node_id match needs no backfill when the name is unchanged', () => {
    const projects = [ref('gmux-hs', 'apps', 'node_hs')]
    const roster = [peer('gmux-hs', 'node_hs')]
    const { backfills } = resolveReferences(projects, roster)
    expect(backfills).toEqual([])
  })

  it('flags a reference whose host is in no roster bucket as unresolved', () => {
    // The classic broken case: reference points at "hs", live host is
    // "unraid"/"gmux-hs" — no roster peer named "hs".
    const projects = [ref('hs', 'apps'), ref('hs', 'home'), ref('hs', 'chezmoi')]
    const roster = [peer('unraid', 'node_hs'), peer('ft', 'node_ft')]
    const { resolution, unresolved, backfills } = resolveReferences(projects, roster)
    expect(resolution.get(refKey('hs', 'apps'))).toEqual({ effectivePeer: 'hs', resolved: false })
    expect(backfills).toEqual([])
    expect(unresolved).toEqual([{ name: 'hs', slugs: ['apps', 'home', 'chezmoi'] }])
  })

  it('treats an offline-but-known host (no node_id) as resolved, not unresolved', () => {
    // Offline tailnet peers are in the roster without a node_id.
    const projects = [ref('bespin', 'home')]
    const roster: PeerInfo[] = [{ name: 'bespin', url: 'https://bespin', status: 'offline', session_count: 0 }]
    const { resolution, unresolved, backfills } = resolveReferences(projects, roster)
    expect(resolution.get(refKey('bespin', 'home'))).toEqual({ effectivePeer: 'bespin', resolved: true })
    expect(unresolved).toEqual([])
    expect(backfills).toEqual([]) // nothing to stamp; offline peer has no node_id
  })

  it('falls back to name and refreshes a stale node_id', () => {
    // node_id was stamped, but that host is gone; another host now has
    // the referenced name. Resolve by name rather than orphaning, and
    // backfill the *current* node_id so the stale one doesn't linger
    // (otherwise every load repeats the failed-id-then-name dance).
    const projects = [ref('shared', 'proj', 'node_gone')]
    const roster = [peer('shared', 'node_new')]
    const { resolution, unresolved, backfills } = resolveReferences(projects, roster)
    expect(resolution.get(refKey('shared', 'proj'))).toEqual({ effectivePeer: 'shared', resolved: true })
    expect(unresolved).toEqual([])
    expect(backfills).toEqual([{ slug: 'proj', fromPeer: 'shared', toPeer: 'shared', node_id: 'node_new' }])
  })

  it('ignores owned projects entirely', () => {
    const projects = [owned('gmux'), ref('hs', 'apps')]
    const roster: PeerInfo[] = []
    const { resolution, unresolved } = resolveReferences(projects, roster)
    expect(resolution.has(refKey('', 'gmux'))).toBe(false)
    expect(unresolved).toEqual([{ name: 'hs', slugs: ['apps'] }])
  })

  it('groups multiple unresolved hosts independently', () => {
    const projects = [ref('hs', 'apps'), ref('old-laptop', 'dots'), ref('hs', 'home')]
    const { unresolved } = resolveReferences(projects, [])
    expect(unresolved).toContainEqual({ name: 'hs', slugs: ['apps', 'home'] })
    expect(unresolved).toContainEqual({ name: 'old-laptop', slugs: ['dots'] })
    expect(unresolved).toHaveLength(2)
  })
})

describe('remapReferenceItems', () => {
  it('repoints the named references from one host to another and stamps node_id', () => {
    const items = [owned('gmux'), ref('hs', 'apps'), ref('hs', 'home'), ref('ft', 'dots')]
    const next = remapReferenceItems(items, 'hs', ['apps', 'home'], 'gmux-hs', 'node_hs')
    expect(next).toEqual([
      owned('gmux'),
      { slug: 'apps', peer: 'gmux-hs', node_id: 'node_hs' },
      { slug: 'home', peer: 'gmux-hs', node_id: 'node_hs' },
      ref('ft', 'dots'), // untouched
    ])
  })

  it('does not touch a same-named reference outside the slug scope', () => {
    // Data-loss guard: during a rename transition a stored name can be
    // shared by an unresolved reference (in scope) and one already
    // resolving via node_id (out of scope). Only the scoped slug moves.
    const items = [
      ref('hs', 'apps', 'node_hs'), // resolves via node_id; NOT in scope
      ref('hs', 'legacy'),          // unresolved; in scope
    ]
    const next = remapReferenceItems(items, 'hs', ['legacy'], 'gmux-hs', 'node_hs')
    expect(next).toEqual([
      ref('hs', 'apps', 'node_hs'),
      { slug: 'legacy', peer: 'gmux-hs', node_id: 'node_hs' },
    ])
  })

  it('is a no-op when fromPeer equals toPeer (never wipes references)', () => {
    const items = [ref('hs', 'apps', 'node_hs'), ref('hs', 'home', 'node_hs'), owned('gmux')]
    expect(remapReferenceItems(items, 'hs', ['apps', 'home'], 'hs', 'node_hs')).toEqual(items)
  })

  it('clears a stale node_id when the remap target has no node_id', () => {
    const items = [ref('hs', 'apps', 'stale_node'), ref('hs', 'home', 'stale_node')]
    const next = remapReferenceItems(items, 'hs', ['apps', 'home'], 'gmux-hs', undefined)
    expect(next).toEqual([
      { slug: 'apps', peer: 'gmux-hs', node_id: undefined },
      { slug: 'home', peer: 'gmux-hs', node_id: undefined },
    ])
  })

  it('drops a remapped reference whose slug the target already has', () => {
    const items = [ref('gmux-hs', 'apps', 'node_hs'), ref('hs', 'apps'), ref('hs', 'home')]
    const next = remapReferenceItems(items, 'hs', ['apps', 'home'], 'gmux-hs', 'node_hs')
    expect(next).toEqual([
      ref('gmux-hs', 'apps', 'node_hs'),
      { slug: 'home', peer: 'gmux-hs', node_id: 'node_hs' },
    ])
  })
})

describe('removeReferenceItems', () => {
  it('removes only the named references for the host', () => {
    const items = [owned('gmux'), ref('hs', 'apps'), ref('hs', 'home'), ref('ft', 'dots')]
    expect(removeReferenceItems(items, 'hs', ['apps', 'home'])).toEqual([owned('gmux'), ref('ft', 'dots')])
  })

  it('keeps a same-named reference that resolves via node_id (outside scope)', () => {
    // The data-loss path Greptile caught: removing the unresolved
    // "legacy" slug must not delete the working node_id-anchored "apps".
    const items = [ref('hs', 'apps', 'node_hs'), ref('hs', 'legacy')]
    expect(removeReferenceItems(items, 'hs', ['legacy'])).toEqual([ref('hs', 'apps', 'node_hs')])
  })
})

describe('removeHostReferenceItems', () => {
  it('drops every reference to the host by name, keeping owned and other hosts', () => {
    const items = [owned('gmux'), ref('hs', 'apps'), ref('hs', 'home'), ref('ft', 'dots')]
    expect(removeHostReferenceItems(items, 'hs')).toEqual([owned('gmux'), ref('ft', 'dots')])
  })

  it('matches by node_id even when the cached name differs (post-rename)', () => {
    // The reference was stamped under the old name 'hs' but the host is
    // now removed under its current name 'gmux-hs'; node_id catches it.
    const items = [ref('hs', 'apps', 'node_hs'), ref('ft', 'dots', 'node_ft')]
    expect(removeHostReferenceItems(items, 'gmux-hs', 'node_hs')).toEqual([ref('ft', 'dots', 'node_ft')])
  })

  it('leaves everything untouched when nothing references the host', () => {
    const items = [owned('gmux'), ref('ft', 'dots')]
    expect(removeHostReferenceItems(items, 'hs', 'node_hs')).toEqual(items)
  })
})
