import { describe, it, expect } from 'vitest'
import { toThreadMessage, toThreadMessages, interleaveEchoes } from './conversation-adapter'
import type { EchoTurn } from './conversation-adapter'
import type { ConvMessage } from './conversation'

const user = (text: string, messageId?: string): ConvMessage => ({
  role: 'user',
  ...(messageId ? { messageId } : {}),
  content: [{ type: 'text', text }],
})
const assistant = (text: string, messageId?: string): ConvMessage => ({
  role: 'assistant',
  ...(messageId ? { messageId } : {}),
  content: [{ type: 'text', text }],
})

describe('conversation-adapter: ConvMessage → ThreadMessageLike', () => {
  it('maps a user text message', () => {
    const m: ConvMessage = { role: 'user', content: [{ type: 'text', text: 'hi' }] }
    expect(toThreadMessage(m, 0)).toEqual({
      id: 'msg-0',
      role: 'user',
      content: [{ type: 'text', text: 'hi' }],
    })
  })

  it('maps thinking blocks to assistant-ui reasoning parts, preserving order', () => {
    const m: ConvMessage = {
      role: 'assistant',
      messageId: 'm1',
      content: [
        { type: 'thinking', text: 'hmm' },
        { type: 'text', text: 'answer' },
      ],
    }
    expect(toThreadMessage(m, 3)).toEqual({
      id: 'm1', // streaming messageId wins over the positional fallback
      role: 'assistant',
      content: [
        { type: 'reasoning', text: 'hmm' },
        { type: 'text', text: 'answer' },
      ],
    })
  })

  it('drops empty blocks but keeps a placeholder so a just-opened bubble renders', () => {
    const m: ConvMessage = {
      role: 'assistant',
      messageId: 'm2',
      content: [{ type: 'text', text: '' }],
    }
    expect(toThreadMessage(m, 0).content).toEqual([{ type: 'text', text: '' }])
  })

  it('ignores unknown block types', () => {
    const m: ConvMessage = {
      role: 'assistant',
      content: [
        { type: 'text', text: 'ok' },
        { type: 'mystery', text: 'ignored' },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([{ type: 'text', text: 'ok' }])
  })

  it('maps an in-progress tool_call block to a tool-call part (no result yet)', () => {
    const m: ConvMessage = {
      role: 'assistant',
      content: [
        {
          type: 'tool_call',
          toolCallId: 't1',
          toolName: 'bash',
          kind: 'execute',
          args: '{"cmd":"ls"}',
          status: 'in_progress',
        },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([
      {
        type: 'tool-call',
        toolCallId: 't1',
        toolName: 'bash',
        kind: 'execute',
        args: { cmd: 'ls' },
        argsText: '{"cmd":"ls"}',
      },
    ])
  })

  it('carries the ACP kind onto the tool-call part; defaults to "other" when absent', () => {
    const withKind: ConvMessage = {
      role: 'assistant',
      content: [{ type: 'tool_call', toolCallId: 't1', toolName: 'grep', kind: 'search', args: '' }],
    }
    expect(toThreadMessage(withKind, 0).content[0]).toMatchObject({ kind: 'search' })

    const noKind: ConvMessage = {
      role: 'assistant',
      content: [{ type: 'tool_call', toolCallId: 't2', toolName: 'weird', args: '' }],
    }
    expect(toThreadMessage(noKind, 0).content[0]).toMatchObject({ kind: 'other' })
  })

  it('maps a completed tool_call to a tool-call part with result', () => {
    const m: ConvMessage = {
      role: 'assistant',
      content: [
        {
          type: 'tool_call',
          toolCallId: 't1',
          toolName: 'bash',
          kind: 'execute',
          args: '{"cmd":"ls"}',
          status: 'completed',
          output: 'file.txt',
        },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([
      {
        type: 'tool-call',
        toolCallId: 't1',
        toolName: 'bash',
        kind: 'execute',
        args: { cmd: 'ls' },
        argsText: '{"cmd":"ls"}',
        result: 'file.txt',
      },
    ])
  })

  it('marks a failed tool_call as an error', () => {
    const m: ConvMessage = {
      role: 'assistant',
      content: [
        { type: 'tool_call', toolCallId: 't1', toolName: 'bash', kind: 'execute', args: '', status: 'failed', output: 'boom' },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([
      {
        type: 'tool-call',
        toolCallId: 't1',
        toolName: 'bash',
        kind: 'execute',
        args: {},
        argsText: '',
        result: 'boom',
        isError: true,
      },
    ])
  })

  it('tolerates partial/invalid args JSON mid-stream (empty args, raw text kept)', () => {
    const m: ConvMessage = {
      role: 'assistant',
      content: [
        { type: 'tool_call', toolCallId: 't1', toolName: 'bash', kind: 'execute', args: '{"cmd":', status: 'in_progress' },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([
      { type: 'tool-call', toolCallId: 't1', toolName: 'bash', kind: 'execute', args: {}, argsText: '{"cmd":' },
    ])
  })

  it('assigns stable positional ids when no messageId is present', () => {
    const msgs: ConvMessage[] = [
      { role: 'user', content: [{ type: 'text', text: 'a' }] },
      { role: 'user', content: [{ type: 'text', text: 'b' }] },
    ]
    expect(toThreadMessages(msgs).map((m) => m.id)).toEqual(['msg-0', 'msg-1'])
  })
})

describe('interleaveEchoes (optimistic user echo ordering)', () => {
  it('renders the echoed user turn ABOVE the assistant reply that lands after it (regression: P1-1)', () => {
    // At send time the store held one prior assistant message, so the echo is
    // anchored at index 1. The reply then streams in as a NEW store message.
    const stored = [assistant('prev reply', 'm0'), assistant('new reply', 'm1')]
    const echoes: EchoTurn[] = [{ after: 1, message: user('my question', 'echo-0') }]
    expect(interleaveEchoes(stored, echoes).map((m) => m.content[0].text)).toEqual([
      'prev reply',
      'my question',
      'new reply',
    ])
  })

  it('no-ops when there are no echoes (returns a copy)', () => {
    const stored = [assistant('a')]
    const out = interleaveEchoes(stored, [])
    expect(out).toEqual(stored)
    expect(out).not.toBe(stored)
  })

  it('appends an echo sent against an empty store', () => {
    expect(interleaveEchoes([], [{ after: 0, message: user('hi') }]).map((m) => m.content[0].text)).toEqual([
      'hi',
    ])
  })

  it('keeps multiple echoes at the same anchor in send order', () => {
    const echoes: EchoTurn[] = [
      { after: 0, message: user('first', 'e0') },
      { after: 0, message: user('second', 'e1') },
    ]
    expect(interleaveEchoes([assistant('reply')], echoes).map((m) => m.content[0].text)).toEqual([
      'first',
      'second',
      'reply',
    ])
  })

  it('clamps an out-of-range anchor to the store length', () => {
    // Store shrank (e.g. after a reload) below the echo's original anchor.
    expect(
      interleaveEchoes([assistant('only')], [{ after: 5, message: user('late') }]).map(
        (m) => m.content[0].text,
      ),
    ).toEqual(['only', 'late'])
  })
})
