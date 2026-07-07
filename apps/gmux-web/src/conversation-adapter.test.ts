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

  it('ignores unknown block types (future tool_call etc.)', () => {
    const m: ConvMessage = {
      role: 'assistant',
      content: [
        { type: 'text', text: 'ok' },
        { type: 'tool_call', text: 'ignored' },
      ],
    }
    expect(toThreadMessage(m, 0).content).toEqual([{ type: 'text', text: 'ok' }])
  })

  it('assigns stable positional ids when no messageId is present', () => {
    const msgs: ConvMessage[] = [
      { role: 'user', content: [{ type: 'text', text: 'a' }] },
      { role: 'user', content: [{ type: 'text', text: 'b' }] },
    ]
    expect(toThreadMessages(msgs).map((m) => m.id)).toEqual(['msg-0', 'msg-1'])
  })
})
