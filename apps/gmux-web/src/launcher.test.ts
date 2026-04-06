import { describe, expect, test, beforeEach } from 'vitest'
import { launchersForPeer, getLastLauncher, setLastLauncher, type LaunchConfig } from './launcher'

const localConfig: LaunchConfig = {
  default_launcher: 'shell',
  launchers: [
    { id: 'shell', label: 'Shell', command: ['bash'], available: true },
    { id: 'claude', label: 'Claude', command: ['claude'], available: true },
  ],
  peers: {
    'work-laptop': {
      default_launcher: 'pi',
      launchers: [
        { id: 'shell', label: 'Shell', command: ['zsh'], available: true },
        { id: 'pi', label: 'pi', command: ['pi'], available: true },
      ],
    },
  },
}

describe('launchersForPeer', () => {
  test('returns local config when peer is undefined', () => {
    const resolved = launchersForPeer(localConfig, undefined)
    expect(resolved.default_launcher).toBe('shell')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'claude'])
  })

  test('returns peer config when peer matches', () => {
    const resolved = launchersForPeer(localConfig, 'work-laptop')
    expect(resolved.default_launcher).toBe('pi')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'pi'])
  })

  test('falls back to local when peer is unknown', () => {
    const resolved = launchersForPeer(localConfig, 'mystery-host')
    expect(resolved.default_launcher).toBe('shell')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'claude'])
  })

  test('falls back to local when peers map is absent', () => {
    const noPeers: LaunchConfig = { default_launcher: 'shell', launchers: localConfig.launchers }
    const resolved = launchersForPeer(noPeers, 'work-laptop')
    expect(resolved.default_launcher).toBe('shell')
  })
})

describe('quick-launch memory', () => {
  const store = new Map<string, string>()
  beforeEach(() => {
    store.clear()
    globalThis.localStorage = {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => { store.set(k, v) },
      removeItem: (k: string) => { store.delete(k) },
      clear: () => store.clear(),
      get length() { return store.size },
      key: () => null,
    }
  })

  test('returns null when no key is stored', () => {
    expect(getLastLauncher('gmux')).toBeNull()
  })

  test('returns null when storageKey is undefined', () => {
    expect(getLastLauncher(undefined)).toBeNull()
  })

  test('round-trips a launcher choice', () => {
    setLastLauncher('gmux', 'claude')
    expect(getLastLauncher('gmux')).toBe('claude')
  })

  test('setLastLauncher is a no-op when storageKey is undefined', () => {
    setLastLauncher(undefined, 'claude')
    // Nothing stored
    expect(getLastLauncher('gmux')).toBeNull()
  })

  test('different projects have independent memory', () => {
    setLastLauncher('gmux', 'claude')
    setLastLauncher('chezmoi', 'shell')
    expect(getLastLauncher('gmux')).toBe('claude')
    expect(getLastLauncher('chezmoi')).toBe('shell')
  })

  test('last launch overwrites previous', () => {
    setLastLauncher('gmux', 'shell')
    setLastLauncher('gmux', 'pi')
    expect(getLastLauncher('gmux')).toBe('pi')
  })
})
