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

  it('continues streaming after a snapshot that includes a partial tail', () => {
    const s = createConversationStore()
    // snapshot: prior user turn + a partial assistant tail (no id in snapshot)
    s.applyFrame(
      loadFrame([
        { role: 'user', content: [{ type: 'text', text: 'q' }] },
        { role: 'assistant', content: [{ type: 'text', text: 'par' }] },
      ]),
    )
    // a fresh assistant message with an id streams next
    s.applyFrame(chunkFrame('tial', 'm9'))
    const msgs = s.getMessages()
    expect(msgs).toHaveLength(3)
    expect(messageText(msgs[2])).toBe('tial')
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
