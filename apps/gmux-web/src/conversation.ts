/**
 * ACP conversation store (ADR 0021 tracer #1: streaming assistant text).
 *
 * A headless, framework-agnostic model of one session's conversation, fed by
 * the runner's ACP WebSocket (`/acp/{sessionId}`). It consumes JSON-RPC 2.0
 * frames:
 *
 *   - `session/load`   — the history snapshot, sent unsolicited as the first
 *                        frame (mirrors the PTY renderScreen snapshot).
 *   - `session/update` — live `agent_message_chunk` (assistant text) and
 *                        `agent_thought_chunk` (reasoning) token deltas.
 *
 * Text and thinking deltas of one message accumulate into separate, ordered
 * content blocks (type `text` / `thinking`), so a renderer can present
 * reasoning distinctly while preserving interleaving order.
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
  // Tool-call fields (type === 'tool_call').
  toolCallId?: string
  toolName?: string
  /** Raw JSON arguments text. */
  args?: string
  /** in_progress | completed | failed */
  status?: string
  /** Textual tool result. */
  output?: string
}

export interface ConvMessage {
  role: Role
  content: ContentBlock[]
  /** Present for the in-flight assistant message being streamed. */
  messageId?: string
}

interface LoadParams {
  sessionId: string
  messages: (ConvMessage & { messageId?: string })[]
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
    // The snapshot is authoritative history: replace everything. The runner
    // tags the in-flight assistant tail (if any) with its streaming messageId,
    // so the live deltas that follow keep appending to that same message
    // instead of opening a duplicate bubble.
    // A session with no history sends `messages: null` (not []), so guard it —
    // otherwise .map throws and the (swallowed) snapshot is silently lost.
    messages = (p.messages ?? []).map((m) => ({
      role: m.role,
      messageId: m.messageId,
      content: [...m.content],
    }))
    notify()
  }

  // Tool calls append a distinct block (never coalesced) on tool_call, then
  // mutate it in place by toolCallId on tool_call_update. Both belong to the
  // in-flight assistant message identified by messageId.
  function applyToolCall(u: UpdateParams['update']) {
    const block = u.content
    const last = messages[messages.length - 1]
    const matches =
      last &&
      last.role === 'assistant' &&
      (u.messageId ? last.messageId === u.messageId : last.messageId === undefined)
    if (matches) {
      last.content.push({ ...block })
    } else {
      messages.push({
        role: 'assistant',
        messageId: u.messageId,
        content: [{ ...block }],
      })
    }
    notify()
  }

  function applyToolCallUpdate(u: UpdateParams['update']) {
    const id = u.content?.toolCallId
    if (!id) return
    // Find the matching tool-call block, scanning from the most recent message
    // (the in-flight one) backwards.
    for (let mi = messages.length - 1; mi >= 0; mi--) {
      const m = messages[mi]
      if (m.role !== 'assistant') continue
      for (let bi = m.content.length - 1; bi >= 0; bi--) {
        const b = m.content[bi]
        if (b.type === 'tool_call' && b.toolCallId === id) {
          b.status = u.content.status
          b.output = u.content.output
          notify()
          return
        }
      }
    }
  }

  function applyUpdate(p: UpdateParams) {
    const u = p.update
    if (u.sessionUpdate === 'tool_call') return applyToolCall(u)
    if (u.sessionUpdate === 'tool_call_update') return applyToolCallUpdate(u)
    // Assistant text and reasoning accumulate into blocks of the matching type.
    const blockType =
      u.sessionUpdate === 'agent_message_chunk'
        ? 'text'
        : u.sessionUpdate === 'agent_thought_chunk'
          ? 'thinking'
          : null
    if (!blockType) return // unknown update kinds ignored
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
      // Coalesce into the trailing block only if it's the same type, so
      // interleaved thinking/text keep their order as separate blocks.
      const block = last.content[last.content.length - 1]
      if (block && block.type === blockType) {
        block.text = (block.text ?? '') + delta
      } else {
        last.content.push({ type: blockType, text: delta })
      }
    } else {
      messages.push({
        role: 'assistant',
        messageId: u.messageId,
        content: [{ type: blockType, text: delta }],
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

/**
 * Send composed text to the session as keystrokes (ADR 0021 §6). POSTs raw
 * bytes to the daemon's /input/{id}, which proxies to the runner's POST /input
 * — the same actuator `gmux send` uses. Works whether or not a terminal is
 * mounted, so the conversation composer is independent of the PTY view.
 */
export async function sendSessionInput(sessionId: string, data: string): Promise<void> {
  const res = await fetch(`/input/${sessionId}`, {
    method: 'POST',
    body: data,
    headers: { 'Content-Type': 'application/octet-stream' },
  })
  if (!res.ok) throw new Error(`send input failed: ${res.status}`)
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
