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
 * alias vs. a dedicated React island) to a follow-up. The store
 * (conversation.ts) is UI-framework-agnostic, so swapping the renderer later
 * touches only this file.
 */
import { useEffect, useState } from 'preact/hooks'
import {
  type ConversationStore,
  type ConvMessage,
  createConversationStore,
  connectConversation,
  messageText,
} from './conversation'

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
      <span class="conversation-text">{messageText(message)}</span>
    </div>
  )
}
