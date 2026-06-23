import { describe, it, expect } from 'vitest'
import { referencePresence, unresolvedReferences, removeReferenceItems, removeHostReferenceItems } from './references'
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

describe('referencePresence', () => {
  it('matches a reference by node_id (name-reuse-proof)', () => {
    // node_id is the authority: a different host now holding the same
    // name must NOT count as present.
    const present = referencePresence([peer('gmux-hs', 'node_hs')])
    expect(present('gmux-hs', 'node_hs')).toBe(true)
    // node_id present under a different roster name: still present
    expect(present('whatever', 'node_hs')).toBe(true)
    // anchor gone, a confirmed different node_id holds the name: hijack,
    // so not present
    expect(present('gmux-hs', 'node_other')).toBe(false)
  })

  it('stays present when the named peer has no node_id yet (post-restart)', () => {
    // After a daemon restart a peer is listed but its node_id isn't
    // cached until it reconnects. A stamped reference to it must not
    // flash "host not found" — an unknown node_id is not a confirmed
    // mismatch.
    const present = referencePresence([
      { name: 'gmux-hs', url: 'https://gmux-hs', status: 'connecting', session_count: 0 },
    ])
    expect(present('gmux-hs', 'node_hs')).toBe(true)
  })

  it('matches a name-only reference by name', () => {
    // A reference with no node_id (hand-authored/legacy) falls back to
    // a plain name membership check.
    const present = referencePresence([peer('workstation', 'N1')])
    expect(present('workstation', undefined)).toBe(true)
    expect(present('elsewhere', undefined)).toBe(false)
  })

  it('treats an offline-but-known host (no node_id) as present by name', () => {
    // Offline peers sit in the roster without a cached node_id; a
    // reference to one is reachable-but-offline, not unresolved.
    const present = referencePresence([
      { name: 'bespin', url: 'https://bespin', status: 'offline', session_count: 0 },
    ])
    expect(present('bespin', undefined)).toBe(true)
  })

  it('keeps a stamped reference present while its host is offline', () => {
    // Once stamped, presence is by node_id. An offline peer still
    // carries its cached node_id in the roster (peer.go keeps
    // cachedHealth across disconnects), so a stamped reference to it
    // stays present (reachable-but-offline) rather than flipping to
    // unresolved. Guards against a future change that drops node_id
    // from offline roster entries.
    const present = referencePresence([
      { name: 'bespin', url: 'https://bespin', status: 'offline', session_count: 0, node_id: 'N_bespin' },
    ])
    expect(present('bespin', 'N_bespin')).toBe(true)
  })
})

describe('unresolvedReferences', () => {
  it('flags a reference whose anchor is in no roster bucket', () => {
    const projects = [ref('hs', 'apps', 'node_hs'), ref('hs', 'home', 'node_hs')]
    const roster = [peer('unraid', 'node_other'), peer('ft', 'node_ft')]
    expect(unresolvedReferences(projects, roster)).toEqual([{ name: 'hs', slugs: ['apps', 'home'] }])
  })

  it('does not flag a present (node_id-matched) reference', () => {
    const projects = [ref('gmux-hs', 'apps', 'node_hs')]
    const roster = [peer('gmux-hs', 'node_hs')]
    expect(unresolvedReferences(projects, roster)).toEqual([])
  })

  it('ignores owned projects entirely', () => {
    const projects = [owned('gmux'), ref('hs', 'apps', 'node_hs')]
    expect(unresolvedReferences(projects, [])).toEqual([{ name: 'hs', slugs: ['apps'] }])
  })

  it('groups multiple unresolved hosts independently', () => {
    const projects = [ref('hs', 'apps'), ref('old-laptop', 'dots'), ref('hs', 'home')]
    const unresolved = unresolvedReferences(projects, [])
    expect(unresolved).toContainEqual({ name: 'hs', slugs: ['apps', 'home'] })
    expect(unresolved).toContainEqual({ name: 'old-laptop', slugs: ['dots'] })
    expect(unresolved).toHaveLength(2)
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
