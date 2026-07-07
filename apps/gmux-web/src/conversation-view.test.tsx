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

  it('renders an empty state with no messages', () => {
    const store = createConversationStore()
    const html = renderToString(<ConversationView sessionId="s1" store={store} connect={false} />)
    expect(html).toContain('No conversation yet.')
  })
})
