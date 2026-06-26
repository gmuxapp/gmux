import { describe, expect, test } from 'vitest'
import { launchersForPeer, formatTarget, computeMenuPos } from './launcher'
import type { LauncherDef, PeerInfo } from './types'

const localLaunchers: LauncherDef[] = [
  { id: 'shell', label: 'Shell', command: ['bash'], available: true },
  { id: 'claude', label: 'Claude', command: ['claude'], available: true },
]
const localDefault = 'shell'

const peersWithLaunchers: PeerInfo[] = [
  {
    name: 'work-laptop', url: 'https://work-laptop', status: 'connected',
    session_count: 2, default_launcher: 'pi', launchers: [
      { id: 'shell', label: 'Shell', command: ['zsh'], available: true },
      { id: 'pi', label: 'pi', command: ['pi'], available: true },
    ],
  },
]

describe('launchersForPeer', () => {
  test('returns local config when peer is undefined', () => {
    const resolved = launchersForPeer(localLaunchers, localDefault, peersWithLaunchers, undefined)
    expect(resolved.default_launcher).toBe('shell')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'claude'])
  })

  test('returns peer config when peer matches', () => {
    const resolved = launchersForPeer(localLaunchers, localDefault, peersWithLaunchers, 'work-laptop')
    expect(resolved.default_launcher).toBe('pi')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'pi'])
  })

  test('falls back to local when peer is unknown', () => {
    const resolved = launchersForPeer(localLaunchers, localDefault, peersWithLaunchers, 'mystery-host')
    expect(resolved.default_launcher).toBe('shell')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'claude'])
  })

  test('falls back to local when peers list is empty', () => {
    const resolved = launchersForPeer(localLaunchers, localDefault, [], 'work-laptop')
    expect(resolved.default_launcher).toBe('shell')
  })
})

describe('computeMenuPos', () => {
  const viewport = { innerWidth: 1000, innerHeight: 800 }

  test('anchors left edge slightly left of the button', () => {
    const pos = computeMenuPos({ top: 100, left: 200 }, viewport, false)
    expect(pos.left).toBe(200 - 6)
  })

  test('clamps right edge inside the viewport for buttons near the right', () => {
    const pos = computeMenuPos({ top: 100, left: 980 }, viewport, false, 180)
    // maxLeft = 1000 - 8 - 180 = 812
    expect(pos.left).toBe(812)
  })

  test('clamps to left margin when menu is wider than viewport', () => {
    const narrow = { innerWidth: 120, innerHeight: 800 }
    const pos = computeMenuPos({ top: 100, left: 10 }, narrow, false, 180)
    expect(pos.left).toBe(8)
  })

  test('lifts the menu so the default item lands under the button (no target line)', () => {
    const pos = computeMenuPos({ top: 100, left: 200 }, viewport, false)
    expect(pos.top).toBe(100 - 4)
  })

  test('offsets for the target line + divider so the default stays under the button', () => {
    const pos = computeMenuPos({ top: 100, left: 200 }, viewport, true)
    expect(pos.top).toBe(100 - 4 - 32)
  })
})

describe('formatTarget', () => {
  test('shows short cwd for local target', () => {
    expect(formatTarget({ cwd: '/home/mg/dev/gmux' })).toBe('~/dev/gmux')
  })

  test('prefixes peer name for remote target', () => {
    expect(formatTarget({ peer: 'laptop', cwd: '/workspace' })).toBe('laptop: /workspace')
  })

  test('shortens home dir even with peer', () => {
    expect(formatTarget({ peer: 'server', cwd: '/home/mg/work' })).toBe('server: ~/work')
  })
})
