import { describe, expect, test } from 'vitest'
import { launchersForPeer, type LaunchConfig } from './launcher'

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
