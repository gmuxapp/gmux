import { describe, it, expect } from 'vitest'
import { createConversationStore, messageText } from './conversation'

const loadFrame = (messages: unknown[]) => ({
  jsonrpc: '2.0',
  method: 'session/load',
  params: { sessionId: 's1', messages },
})

const chunkFrame = (delta: string, messageId?: string) => ({
  jsonrpc: '2.0',
  method: 'session/update',
  params: {
    sessionId: 's1',
    update: { sessionUpdate: 'agent_message_chunk', messageId, content: { type: 'text', text: delta } },
  },
})

// flush the coalesced microtask notification
const tick = () => new Promise((r) => queueMicrotask(() => r(undefined)))

describe('conversation store', () => {
  it('loads history snapshot as the initial messages', () => {
    const s = createConversationStore()
    s.applyFrame(
      loadFrame([
        { role: 'user', content: [{ type: 'text', text: 'hi' }] },
        { role: 'assistant', content: [{ type: 'text', text: 'hello' }] },
      ]),
    )
    const msgs = s.getMessages()
    expect(msgs).toHaveLength(2)
    expect(msgs[0].role).toBe('user')
    expect(messageText(msgs[1])).toBe('hello')
  })

  it('coalesces token deltas of the same messageId into one assistant message', () => {
    const s = createConversationStore()
    s.applyFrame(chunkFrame('Hel', 'm1'))
    s.applyFrame(chunkFrame('lo ', 'm1'))
    s.applyFrame(chunkFrame('world', 'm1'))
    const msgs = s.getMessages()
    expect(msgs).toHaveLength(1)
    expect(msgs[0].role).toBe('assistant')
    expect(messageText(msgs[0])).toBe('Hello world')
  })

  it('starts a new assistant message when the messageId changes', () => {
    const s = createConversationStore()
    s.applyFrame(chunkFrame('first', 'm1'))
    s.applyFrame(chunkFrame('second', 'm2'))
    const msgs = s.getMessages()
    expect(msgs).toHaveLength(2)
    expect(messageText(msgs[0])).toBe('first')
    expect(messageText(msgs[1])).toBe('second')
  })

  it('continues the same message when a mid-turn snapshot tail carries its messageId', () => {
    const s = createConversationStore()
    // snapshot: prior user turn + the in-flight assistant tail, tagged with its
    // streaming messageId (as the runner sends for a mid-turn joiner).
    s.applyFrame(
      loadFrame([
        { role: 'user', content: [{ type: 'text', text: 'q' }] },
        { role: 'assistant', messageId: 'm9', content: [{ type: 'text', text: 'par' }] },
      ]),
    )
    // subsequent deltas of the SAME message append rather than opening a new one
    s.applyFrame(chunkFrame('tial', 'm9'))
    const msgs = s.getMessages()
    expect(msgs).toHaveLength(2)
    expect(messageText(msgs[1])).toBe('partial')
  })

  it('notifies subscribers once per burst (coalesced)', async () => {
    const s = createConversationStore()
    let count = 0
    s.subscribe(() => count++)
    s.applyFrame(chunkFrame('a', 'm1'))
    s.applyFrame(chunkFrame('b', 'm1'))
    s.applyFrame(chunkFrame('c', 'm1'))
    await tick()
    expect(count).toBe(1)
  })

  it('ignores non-text updates and malformed frames', () => {
    const s = createConversationStore()
    s.applyFrame({ jsonrpc: '2.0', method: 'session/update', params: { update: { sessionUpdate: 'tool_call' } } })
    s.applyFrame(null)
    s.applyFrame({ foo: 'bar' })
    expect(s.getMessages()).toHaveLength(0)
  })
})
