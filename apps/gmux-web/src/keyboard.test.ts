import { describe, it, expect } from 'vitest'
import { ctrlSequenceFor, formatPasteText } from './keyboard'

describe('formatPasteText', () => {
  // ── Bracketed paste mode ──────────────────────────────────────────────────

  it('wraps text in bracket sequences when bracketedPasteMode is on', () => {
    expect(formatPasteText('hello', true)).toBe('\x1b[200~hello\x1b[201~')
  })

  it('normalizes \\n to \\r inside brackets', () => {
    expect(formatPasteText('a\nb', true)).toBe('\x1b[200~a\rb\x1b[201~')
  })

  it('normalizes \\r\\n to \\r inside brackets', () => {
    expect(formatPasteText('a\r\nb', true)).toBe('\x1b[200~a\rb\x1b[201~')
  })

  it('sanitizes ESC inside brackets to prevent early termination', () => {
    // An attacker could embed \x1b[201~ to break out of bracketed paste.
    expect(formatPasteText('safe\x1b[201~injected', true))
      .toBe('\x1b[200~safe\u241b[201~injected\x1b[201~')
  })

  // ── Non-bracketed paste mode ──────────────────────────────────────────────
  // Newlines stay as \n so raw-mode apps can distinguish them from Enter (\r).

  it('keeps \\n as \\n in non-bracketed mode', () => {
    expect(formatPasteText('a\nb\nc', false)).toBe('a\nb\nc')
  })

  it('normalizes \\r\\n to \\n in non-bracketed mode', () => {
    expect(formatPasteText('a\r\nb', false)).toBe('a\nb')
  })

  it('normalizes bare \\r to \\n in non-bracketed mode', () => {
    expect(formatPasteText('a\rb', false)).toBe('a\nb')
  })

  it('handles mixed line endings in non-bracketed mode', () => {
    expect(formatPasteText('a\nb\r\nc\rd', false)).toBe('a\nb\nc\nd')
  })

  it('passes through text without newlines unchanged', () => {
    expect(formatPasteText('hello world', false)).toBe('hello world')
    expect(formatPasteText('hello world', true))
      .toBe('\x1b[200~hello world\x1b[201~')
  })
})

describe('ctrlSequenceFor', () => {
  it('produces traditional control codes for lowercase letters', () => {
    expect(ctrlSequenceFor('a')).toBe('\x01')
    expect(ctrlSequenceFor('c')).toBe('\x03')
    expect(ctrlSequenceFor('z')).toBe('\x1a')
  })

  it('produces CSI u sequences for uppercase letters (Ctrl+Shift)', () => {
    // Ctrl+Shift+A: ESC [ 97 ; 6 u
    expect(ctrlSequenceFor('A')).toBe('\x1b[97;6u')
    expect(ctrlSequenceFor('C')).toBe('\x1b[99;6u')
    expect(ctrlSequenceFor('Z')).toBe('\x1b[122;6u')
  })

  it('handles special characters', () => {
    expect(ctrlSequenceFor('[')).toBe('\x1b')  // ESC
    expect(ctrlSequenceFor(']')).toBe('\x1d')  // GS
    expect(ctrlSequenceFor('\\')).toBe('\x1c') // FS
  })

  it('returns null for multi-character strings and unknown chars', () => {
    expect(ctrlSequenceFor('ab')).toBeNull()
    expect(ctrlSequenceFor('1')).toBeNull()
    expect(ctrlSequenceFor('')).toBeNull()
  })
})
