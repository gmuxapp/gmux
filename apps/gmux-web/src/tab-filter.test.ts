import { describe, it, expect } from 'vitest'
import { parseFilterParam, formatFilterParam, selectorLabel, sessionMatchesFilter, folderMatchesFilter } from './tab-filter'

describe('parseFilterParam', () => {
  it('parses the three selector shapes', () => {
    expect(parseFilterParam('gmux@*,*@server,api@server')).toEqual([
      { project: 'gmux', host: '*' },
      { project: '*', host: 'server' },
      { project: 'api', host: 'server' },
    ])
  })

  it('treats a bare project as project@*', () => {
    expect(parseFilterParam('gmux')).toEqual([{ project: 'gmux', host: '*' }])
  })

  it('drops empty and match-everything tokens', () => {
    expect(parseFilterParam('')).toEqual([])
    expect(parseFilterParam(null)).toEqual([])
    expect(parseFilterParam('*@*,*, ,@,gmux')).toEqual([{ project: 'gmux', host: '*' }])
  })

  it('trims whitespace around tokens and parts', () => {
    expect(parseFilterParam(' gmux @ server , api')).toEqual([
      { project: 'gmux', host: 'server' },
      { project: 'api', host: '*' },
    ])
  })

  it('dedupes repeated selectors (chips share a key)', () => {
    expect(parseFilterParam('gmux,gmux')).toEqual([{ project: 'gmux', host: '*' }])
    expect(parseFilterParam('gmux@server, gmux@server ,gmux')).toEqual([
      { project: 'gmux', host: 'server' },
      { project: 'gmux', host: '*' },
    ])
  })
})

describe('formatFilterParam', () => {
  it('round-trips and collapses p@* to the shorthand', () => {
    const raw = 'gmux,*@server,api@server'
    expect(formatFilterParam(parseFilterParam(raw))).toBe(raw)
  })
})

describe('selectorLabel', () => {
  it('labels each shape compactly', () => {
    expect(selectorLabel({ project: 'gmux', host: '*' })).toBe('gmux')
    expect(selectorLabel({ project: '*', host: 'server' })).toBe('@server')
    expect(selectorLabel({ project: 'gmux', host: 'server' })).toBe('gmux@server')
  })
})

describe('sessionMatchesFilter', () => {
  const local = { project_slug: 'gmux', peer: undefined }
  const onServer = { project_slug: 'api', peer: 'server' }
  const container = { project_slug: 'gmux', peer: 'devcontainer' }
  const unstamped = { project_slug: undefined, peer: 'server' }

  it('matches everything when no selectors', () => {
    expect(sessionMatchesFilter(local, [], 'ws')).toBe(true)
  })

  it('project@* matches across hosts', () => {
    const sel = parseFilterParam('gmux')
    expect(sessionMatchesFilter(local, sel, 'ws')).toBe(true)
    expect(sessionMatchesFilter(container, sel, 'ws')).toBe(true)
    expect(sessionMatchesFilter(onServer, sel, 'ws')).toBe(false)
  })

  it('*@host matches all projects on one host', () => {
    const sel = parseFilterParam('*@server')
    expect(sessionMatchesFilter(onServer, sel, 'ws')).toBe(true)
    expect(sessionMatchesFilter(unstamped, sel, 'ws')).toBe(true)
    expect(sessionMatchesFilter(local, sel, 'ws')).toBe(false)
  })

  it('local host matches by hostname and the local alias', () => {
    for (const token of ['*@ws', '*@local']) {
      const sel = parseFilterParam(token)
      expect(sessionMatchesFilter(local, sel, 'ws')).toBe(true)
      expect(sessionMatchesFilter(onServer, sel, 'ws')).toBe(false)
      // Devcontainer sessions are their own host, not local.
      expect(sessionMatchesFilter(container, sel, 'ws')).toBe(false)
    }
  })

  it('exact project@host requires both', () => {
    const sel = parseFilterParam('gmux@devcontainer')
    expect(sessionMatchesFilter(container, sel, 'ws')).toBe(true)
    expect(sessionMatchesFilter(local, sel, 'ws')).toBe(false)
  })

  it('union across selectors', () => {
    const sel = parseFilterParam('gmux@local,*@server')
    expect(sessionMatchesFilter(local, sel, 'ws')).toBe(true)
    expect(sessionMatchesFilter(onServer, sel, 'ws')).toBe(true)
    expect(sessionMatchesFilter(container, sel, 'ws')).toBe(false)
  })

  it('unstamped sessions only survive wildcard-project selectors', () => {
    expect(sessionMatchesFilter(unstamped, parseFilterParam('api@server'), 'ws')).toBe(false)
    expect(sessionMatchesFilter(unstamped, parseFilterParam('*@server'), 'ws')).toBe(true)
  })
})

describe('folderMatchesFilter', () => {
  const localFolder = { slug: 'gmux', peer: undefined }
  const refFolder = { slug: 'api', peer: 'server' }

  it('keeps in-scope folders visible even when empty (launch targets)', () => {
    expect(folderMatchesFilter(localFolder, parseFilterParam('gmux'), 'ws')).toBe(true)
    expect(folderMatchesFilter(refFolder, parseFilterParam('*@server'), 'ws')).toBe(true)
    expect(folderMatchesFilter(refFolder, parseFilterParam('api@server'), 'ws')).toBe(true)
  })

  it('hides folders outside the scope', () => {
    expect(folderMatchesFilter(refFolder, parseFilterParam('gmux'), 'ws')).toBe(false)
    expect(folderMatchesFilter(localFolder, parseFilterParam('*@server'), 'ws')).toBe(false)
    expect(folderMatchesFilter(localFolder, parseFilterParam('gmux@server'), 'ws')).toBe(false)
  })

  it('local folders match the hostname and the local alias', () => {
    expect(folderMatchesFilter(localFolder, parseFilterParam('*@ws'), 'ws')).toBe(true)
    expect(folderMatchesFilter(localFolder, parseFilterParam('*@local'), 'ws')).toBe(true)
  })

  it('matches everything when no selectors', () => {
    expect(folderMatchesFilter(localFolder, [], 'ws')).toBe(true)
  })
})
