import { describe, it, expect } from 'vitest'
import { extractBlocks, getSystemText } from './pi-session'

// ---------------------------------------------------------------------------
// extractBlocks
// ---------------------------------------------------------------------------

describe('extractBlocks', () => {
  it('returns empty array for message with no content', () => {
    expect(extractBlocks({})).toEqual([])
    expect(extractBlocks({ content: [] })).toEqual([])
    expect(extractBlocks({ content: null })).toEqual([])
  })

  it('extracts text blocks', () => {
    const msg = { content: [{ type: 'text', text: 'hello' }] }
    expect(extractBlocks(msg)).toEqual([{ type: 'text', text: 'hello' }])
  })

  it('extracts thinking blocks', () => {
    const msg = { content: [{ type: 'thinking', thinking: 'hmm', redacted: false }] }
    expect(extractBlocks(msg)).toEqual([{ type: 'thinking', thinking: 'hmm', redacted: false }])
  })

  it('extracts toolCall blocks', () => {
    const msg = {
      content: [{ type: 'toolCall', id: 'tc_1', name: 'bash', arguments: { command: 'ls' } }],
    }
    expect(extractBlocks(msg)).toEqual([
      { type: 'toolCall', id: 'tc_1', name: 'bash', arguments: { command: 'ls' } },
    ])
  })

  it('preserves block order for mixed content', () => {
    const msg = {
      content: [
        { type: 'thinking', thinking: 'plan', redacted: false },
        { type: 'text', text: 'result' },
        { type: 'toolCall', id: 'tc_2', name: 'read', arguments: { path: '/a' } },
      ],
    }
    const blocks = extractBlocks(msg)
    expect(blocks).toHaveLength(3)
    expect(blocks[0].type).toBe('thinking')
    expect(blocks[1].type).toBe('text')
    expect(blocks[2].type).toBe('toolCall')
  })

  it('skips unknown block types gracefully', () => {
    const msg = {
      content: [
        { type: 'text', text: 'ok' },
        { type: 'future_unknown_type', data: 123 },
      ],
    }
    // unknown types pass through (we don't strip them — renderer ignores them)
    expect(extractBlocks(msg)).toHaveLength(2)
  })
})

// ---------------------------------------------------------------------------
// getSystemText
// ---------------------------------------------------------------------------

describe('getSystemText', () => {
  it('formats session_ready with model', () => {
    const text = getSystemText({ type: 'session_ready', model: 'claude-opus-4', thinkingLevel: 'medium' })
    expect(text).toContain('claude-opus-4')
    expect(text).toContain('connected')
  })

  it('formats error event', () => {
    const text = getSystemText({ type: 'error', message: 'subprocess crashed' })
    expect(text).toBe('subprocess crashed')
  })

  it('formats warning event', () => {
    const text = getSystemText({ type: 'warning', message: 'rate limit approaching' })
    expect(text).toBe('rate limit approaching')
  })

  it('formats compaction_start', () => {
    const text = getSystemText({ type: 'compaction_start', reason: 'threshold' })
    expect(text.toLowerCase()).toContain('compact')
  })

  it('formats compaction_end success', () => {
    const text = getSystemText({ type: 'compaction_end', reason: 'threshold', aborted: false, willRetry: false })
    expect(text.toLowerCase()).toContain('compact')
  })

  it('formats auto_retry_start', () => {
    const text = getSystemText({ type: 'auto_retry_start', attempt: 1, maxAttempts: 3, delayMs: 1000, errorMessage: 'overload' })
    expect(text).toContain('1')
    expect(text).toContain('3')
  })

  it('formats auto_retry_end success', () => {
    const text = getSystemText({ type: 'auto_retry_end', success: true, attempt: 1 })
    expect(text.toLowerCase()).toContain('success')
  })

  it('formats auto_retry_end failure', () => {
    const text = getSystemText({ type: 'auto_retry_end', success: false, attempt: 2, finalError: 'gave up' })
    expect(text.toLowerCase()).toMatch(/fail|gave up/)
  })

  it('returns a non-empty string for unrecognised event types', () => {
    const text = getSystemText({ type: 'some_future_event' })
    expect(typeof text).toBe('string')
  })
})
