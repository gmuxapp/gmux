import { describe, it, expect } from 'vitest'
import { toThreadMessage, toThreadMessages } from './conversation-adapter'
import type { ConvMessage } from './conversation'

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
        args: { cmd: 'ls' },
        argsText: '{"cmd":"ls"}',
      },
    ])
  })

  it('maps a completed tool_call to a tool-call part with result', () => {
    const m: ConvMessage = {
      role: 'assistant',
      content: [
        {
          type: 'tool_call',
          toolCallId: 't1',
          toolName: 'bash',
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
        { type: 'tool_call', toolCallId: 't1', toolName: 'bash', args: '', status: 'failed', output: 'boom' },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([
      {
        type: 'tool-call',
        toolCallId: 't1',
        toolName: 'bash',
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
        { type: 'tool_call', toolCallId: 't1', toolName: 'bash', args: '{"cmd":', status: 'in_progress' },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([
      { type: 'tool-call', toolCallId: 't1', toolName: 'bash', args: {}, argsText: '{"cmd":' },
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
