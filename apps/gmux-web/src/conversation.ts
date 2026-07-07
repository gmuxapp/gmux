/**
 * ACP conversation store (ADR 0021 tracer #1: streaming assistant text).
 *
 * A headless, framework-agnostic model of one session's conversation, fed by
 * the runner's ACP WebSocket (`/acp/{sessionId}`). It consumes JSON-RPC 2.0
 * frames:
 *
 *   - `session/load`   — the history snapshot, sent unsolicited as the first
 *                        frame (mirrors the PTY renderScreen snapshot).
 *   - `session/update` — live `agent_message_chunk` token deltas.
 *
 * Tokens are chatty by design (per-token on the wire); this store COALESCES
 * them: deltas with the same messageId append to one assistant message, and
 * subscribers are notified on a microtask so a burst of tokens triggers a
 * single render. The store is transport-agnostic so it can be unit-tested by
 * feeding frames directly via `applyFrame`.
 */

export type Role = 'user' | 'assistant'

export interface ContentBlock {
  type: string
  text?: string
}

export interface ConvMessage {
  role: Role
  content: ContentBlock[]
  /** Present for the in-flight assistant message being streamed. */
  messageId?: string
}

interface LoadParams {
  sessionId: string
  messages: ConvMessage[]
}

interface UpdateParams {
  sessionId: string
  update: {
    sessionUpdate: string
    messageId?: string
    content: ContentBlock
  }
}

interface JsonRpcFrame {
  jsonrpc: string
  method: string
  params: unknown
}

type Listener = () => void

export interface ConversationStore {
  applyFrame(frame: unknown): void
  getMessages(): ConvMessage[]
  subscribe(listener: Listener): () => void
}

function textOf(content: ContentBlock[]): string {
  return content
    .filter((b) => b.type === 'text')
    .map((b) => b.text ?? '')
    .join('')
}

export function createConversationStore(): ConversationStore {
  let messages: ConvMessage[] = []
  const listeners = new Set<Listener>()
  let notifyScheduled = false

  // Coalesce notifications: a burst of token deltas schedules one flush.
  function notify() {
    if (notifyScheduled) return
    notifyScheduled = true
    queueMicrotask(() => {
      notifyScheduled = false
      for (const l of listeners) l()
    })
  }

  function applyLoad(p: LoadParams) {
    // The snapshot is authoritative history: replace everything. Any partial
    // assistant tail the runner included arrives as a plain message here; the
    // live stream that follows continues appending to its messageId.
    messages = p.messages.map((m) => ({ role: m.role, content: [...m.content] }))
    notify()
  }

  function applyUpdate(p: UpdateParams) {
    const u = p.update
    if (u.sessionUpdate !== 'agent_message_chunk') return // only text this slice
    const delta = u.content?.text ?? ''
    if (!delta && !u.messageId) return

    // Append to the in-flight assistant message with the same id, else open a
    // new one. Without an id, fall back to the trailing assistant message.
    const last = messages[messages.length - 1]
    const matches =
      last &&
      last.role === 'assistant' &&
      (u.messageId ? last.messageId === u.messageId : last.messageId === undefined)

    if (matches) {
      const block = last.content[last.content.length - 1]
      if (block && block.type === 'text') {
        block.text = (block.text ?? '') + delta
      } else {
        last.content.push({ type: 'text', text: delta })
      }
    } else {
      messages.push({
        role: 'assistant',
        messageId: u.messageId,
        content: [{ type: 'text', text: delta }],
      })
    }
    notify()
  }

  return {
    applyFrame(frame: unknown) {
      if (!frame || typeof frame !== 'object') return
      const f = frame as JsonRpcFrame
      if (f.method === 'session/load') applyLoad(f.params as LoadParams)
      else if (f.method === 'session/update') applyUpdate(f.params as UpdateParams)
    },
    getMessages() {
      return messages
    },
    subscribe(listener: Listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}

/** Plain-text of one message's content, for rendering. */
export function messageText(m: ConvMessage): string {
  return textOf(m.content)
}

export interface ConversationConnection {
  close(): void
}

/**
 * Connect a store to a session's ACP WebSocket. Returns a handle to close it.
 * Reconnect/backoff hardening is deliberately out of scope for the tracer.
 */
export function connectConversation(
  sessionId: string,
  store: ConversationStore,
  opts?: { onStateChange?: (state: 'open' | 'closed') => void },
): ConversationConnection {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const ws = new WebSocket(`${proto}//${location.host}/acp/${sessionId}`)

  ws.onopen = () => opts?.onStateChange?.('open')
  ws.onclose = () => opts?.onStateChange?.('closed')
  ws.onmessage = (ev) => {
    try {
      store.applyFrame(JSON.parse(ev.data as string))
    } catch {
      // ignore malformed frames
    }
  }

  return {
    close() {
      ws.close()
    },
  }
}
