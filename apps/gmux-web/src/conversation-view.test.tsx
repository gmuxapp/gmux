import { describe, it, expect } from 'vitest'
import { renderToString } from 'preact-render-to-string'
import { ConversationView } from './conversation-view'
import { createConversationStore } from './conversation'

// Render test: a pre-fed store rendered without opening a WebSocket
// (connect=false) must produce the streamed assistant text in the DOM output.
describe('ConversationView', () => {
  it('renders user and assistant messages from the store', () => {
    const store = createConversationStore()
    store.applyFrame({
      jsonrpc: '2.0',
      method: 'session/load',
      params: {
        sessionId: 's1',
        messages: [{ role: 'user', content: [{ type: 'text', text: 'ping' }] }],
      },
    })
    store.applyFrame({
      jsonrpc: '2.0',
      method: 'session/update',
      params: {
        sessionId: 's1',
        update: { sessionUpdate: 'agent_message_chunk', messageId: 'm1', content: { type: 'text', text: 'pong' } },
      },
    })

    const html = renderToString(<ConversationView sessionId="s1" store={store} connect={false} />)
    expect(html).toContain('ping')
    expect(html).toContain('pong')
    expect(html).toContain('conversation-message--assistant')
  })

  it('renders assistant text as markdown with highlighted code', () => {
    const store = createConversationStore()
    store.applyFrame({
      jsonrpc: '2.0',
      method: 'session/update',
      params: {
        sessionId: 's1',
        update: {
          sessionUpdate: 'agent_message_chunk',
          messageId: 'm1',
          content: { type: 'text', text: '**bold** and `code`\n\n```js\nconst x = 1\n```' },
        },
      },
    })
    const html = renderToString(<ConversationView sessionId="s1" store={store} connect={false} />)
    expect(html).toContain('<strong>bold</strong>')
    expect(html).toContain('<code>code</code>')
    expect(html).toContain('conversation-markdown')
    // highlight.js wraps tokens in hljs spans for the fenced js block
    expect(html).toContain('hljs-keyword')
  })

  it('escapes raw HTML in model output (no injection)', () => {
    const store = createConversationStore()
    store.applyFrame({
      jsonrpc: '2.0',
      method: 'session/update',
      params: {
        sessionId: 's1',
        update: {
          sessionUpdate: 'agent_message_chunk',
          messageId: 'm1',
          content: { type: 'text', text: 'hi <img src=x onerror=alert(1)>' },
        },
      },
    })
    const html = renderToString(<ConversationView sessionId="s1" store={store} connect={false} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('renders thinking as a distinct collapsible block', () => {
    const store = createConversationStore()
    store.applyFrame({
      jsonrpc: '2.0',
      method: 'session/update',
      params: {
        sessionId: 's1',
        update: {
          sessionUpdate: 'agent_thought_chunk',
          messageId: 'm1',
          content: { type: 'thinking', text: 'let me _reason_' },
        },
      },
    })
    const html = renderToString(<ConversationView sessionId="s1" store={store} connect={false} />)
    expect(html).toContain('conversation-thinking')
    expect(html).toContain('data-block="thinking"')
    // thinking is markdown too
    expect(html).toContain('<em>reason</em>')
  })

  it('does not throw on incomplete streaming markdown (unterminated fence)', () => {
    const store = createConversationStore()
    store.applyFrame({
      jsonrpc: '2.0',
      method: 'session/update',
      params: {
        sessionId: 's1',
        update: {
          sessionUpdate: 'agent_message_chunk',
          messageId: 'm1',
          content: { type: 'text', text: 'here:\n\n```js\nconst x =' },
        },
      },
    })
    expect(() =>
      renderToString(<ConversationView sessionId="s1" store={store} connect={false} />),
    ).not.toThrow()
  })

  it('renders an empty state with no messages', () => {
    const store = createConversationStore()
    const html = renderToString(<ConversationView sessionId="s1" store={store} connect={false} />)
    expect(html).toContain('No conversation yet.')
  })
})
