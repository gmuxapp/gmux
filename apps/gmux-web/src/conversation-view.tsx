/**
 * Conversation view (ADR 0021 tracer #1): a live, structured render of one
 * session's assistant text, separate from the xterm PTY surface. It attaches
 * to the runner's ACP stream via connectConversation and re-renders as token
 * deltas coalesce into the store.
 *
 * NOTE (convention flagged for review): ADR 0021 and the tracer brief suggest
 * `@assistant-ui/react`. This app is Preact, not React; assistant-ui is
 * React-only and pulling react/react-dom in behind preact/compat is a
 * consequential dependency+build decision that shouldn't be baked in silently
 * by the foundational tracer. So this slice ships a minimal native Preact
 * renderer over the same store, and defers the assistant-ui decision (compat
 * alias vs. a dedicated React island) to when rich tool-call UIs land. The
 * store (conversation.ts) is UI-framework-agnostic, so swapping the renderer
 * later touches only this file.
 *
 * Slice #2 adds markdown rendering (assistant text + thinking) and a distinct,
 * collapsible presentation for reasoning (`thinking` blocks). Markdown → HTML
 * happens in markdown.ts, which escapes raw HTML and tolerates the incomplete
 * markdown that arrives mid-stream.
 */
import { useEffect, useState } from 'preact/hooks'
import {
  type ConversationStore,
  type ConvMessage,
  type ContentBlock,
  createConversationStore,
  connectConversation,
} from './conversation'
import { renderMarkdown } from './markdown'

interface Props {
  sessionId: string
  /** Injectable for tests; defaults to a fresh store wired to the live WS. */
  store?: ConversationStore
  /** When false, skip opening the WebSocket (tests pass a pre-fed store). */
  connect?: boolean
}

export function ConversationView({ sessionId, store: injected, connect = true }: Props) {
  const [store] = useState(() => injected ?? createConversationStore())
  const [, forceRender] = useState(0)
  const [connState, setConnState] = useState<'open' | 'closed'>('closed')

  useEffect(() => {
    const unsub = store.subscribe(() => forceRender((n) => n + 1))
    let conn: { close(): void } | undefined
    if (connect) {
      conn = connectConversation(sessionId, store, { onStateChange: setConnState })
    }
    return () => {
      unsub()
      conn?.close()
    }
  }, [sessionId, store, connect])

  const messages = store.getMessages()

  return (
    <div class="conversation-view" data-conn={connState}>
      {messages.length === 0 ? (
        <div class="conversation-empty">No conversation yet.</div>
      ) : (
        messages.map((m, i) => <MessageRow key={m.messageId ?? i} message={m} />)
      )}
    </div>
  )
}

function MessageRow({ message }: { message: ConvMessage }) {
  return (
    <div class={`conversation-message conversation-message--${message.role}`} data-role={message.role}>
      <span class="conversation-role">{message.role}</span>
      <div class="conversation-content">
        {message.content.map((block, i) => (
          <ContentBlockView key={i} block={block} />
        ))}
      </div>
    </div>
  )
}

// A single content block. Text renders as markdown; thinking renders as a
// dimmed, collapsible markdown block so reasoning is visually distinct from
// the answer. Both go through the same defensive markdown renderer.
function ContentBlockView({ block }: { block: ContentBlock }) {
  const html = renderMarkdown(block.text ?? '')
  if (block.type === 'thinking') {
    return (
      <details class="conversation-thinking" data-block="thinking">
        <summary class="conversation-thinking-label">Thinking</summary>
        <div class="conversation-markdown" dangerouslySetInnerHTML={{ __html: html }} />
      </details>
    )
  }
  return (
    <div
      class="conversation-markdown conversation-text"
      data-block="text"
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
}
