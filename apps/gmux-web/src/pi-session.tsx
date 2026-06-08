// PiSessionView — terminal-style chat UI for pi-sdk and pi-sdk-sbx sessions.
// Connects to /ws/{session.id}, renders streaming events as a message list.
import { type Session } from './types'
import { useCallback, useEffect, useRef, useState } from 'preact/hooks'

// ---------------------------------------------------------------------------
// Content block types (mirroring SDK AgentMessage content)
// ---------------------------------------------------------------------------

export interface TextContent {
  type: 'text'
  text: string
}

export interface ThinkingContent {
  type: 'thinking'
  thinking: string
  redacted?: boolean
}

export interface ToolCallContent {
  type: 'toolCall'
  id: string
  name: string
  arguments: Record<string, unknown>
}

export type ContentBlock = TextContent | ThinkingContent | ToolCallContent

// ---------------------------------------------------------------------------
// Tool execution state
// ---------------------------------------------------------------------------

export interface ToolExec {
  toolCallId: string
  toolName: string
  args: Record<string, unknown>
  output: string
  done: boolean
  isError: boolean
}

export type ToolExecMap = Record<string, ToolExec>

// ---------------------------------------------------------------------------
// Render items
// ---------------------------------------------------------------------------

interface UserItem {
  kind: 'user'
  text: string
}

interface AssistantItem {
  kind: 'assistant'
  blocks: ContentBlock[]
  toolExecMap: ToolExecMap
  complete: boolean
}

type SystemSubtype = 'ready' | 'error' | 'warning' | 'info'

interface SystemItem {
  kind: 'system'
  subtype: SystemSubtype
  text: string
}

type RenderItem = UserItem | AssistantItem | SystemItem

// ---------------------------------------------------------------------------
// Pure helpers (exported for tests)
// ---------------------------------------------------------------------------

/** Extract typed ContentBlock[] from a raw message object (message_update payload). */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function extractBlocks(message: any): ContentBlock[] {
  const content = message?.content
  if (!Array.isArray(content)) return []
  return content as ContentBlock[]
}

/** Return a human-readable string for a system-level AgentSessionEvent. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function getSystemText(event: any): string {
  switch (event.type) {
    case 'session_ready':
      return `connected · ${event.model}`
    case 'error':
      return String(event.message ?? 'unknown error')
    case 'warning':
      return String(event.message ?? 'warning')
    case 'compaction_start':
      return `compacting context (${event.reason ?? ''})…`
    case 'compaction_end':
      if (event.aborted) return `compaction aborted`
      return `compaction done`
    case 'auto_retry_start':
      return `retrying (attempt ${event.attempt}/${event.maxAttempts})… ${event.errorMessage ?? ''}`
    case 'auto_retry_end':
      if (event.success) return `retry success`
      return `retry failed: ${event.finalError ?? 'unknown'}`
    default:
      return String(event.type ?? 'event')
  }
}

// ---------------------------------------------------------------------------
// WebSocket reconnect logic
// ---------------------------------------------------------------------------

const WS_BASE_MS = 500
const WS_CAP_MS = 8000

function wsUrl(sessionId: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${location.host}/ws/${sessionId}`
}

// ---------------------------------------------------------------------------
// Sub-components
// ---------------------------------------------------------------------------

function ToolBlock({ block, exec }: { block: ToolCallContent; exec: ToolExec | undefined }) {
  const name = block.name
  const argsStr = (() => {
    try { return JSON.stringify(block.arguments) } catch { return '' }
  })()

  const statusLine = !exec
    ? '⋯ waiting'
    : exec.done
      ? (exec.isError ? '✗ error' : '✓ done')
      : '⋯ running'

  return (
    <div class="pi-session-tool">
      <div class="pi-session-tool-header">┌─ {name} {'─'.repeat(Math.max(0, 42 - name.length))}</div>
      {argsStr && <div class="pi-session-tool-args">│ {argsStr}</div>}
      {exec && exec.output && exec.output.split('\n').map((line, i) => (
        <div key={i} class="pi-session-tool-output">│ {line}</div>
      ))}
      <div class={`pi-session-tool-footer ${exec?.isError ? 'pi-session-tool-error' : exec?.done ? 'pi-session-tool-done' : ''}`}>
        └─ {statusLine} {'─'.repeat(Math.max(0, 40 - statusLine.length))}
      </div>
    </div>
  )
}

function ThinkingBlock({ block }: { block: ThinkingContent }) {
  const [open, setOpen] = useState(false)
  return (
    <div class="pi-session-thinking">
      <button
        class="pi-session-thinking-toggle"
        onClick={() => setOpen(o => !o)}
        type="button"
      >
        {open ? '▾' : '▸'} ⟨thinking⟩
      </button>
      {open && (
        <div class="pi-session-thinking-body">
          {block.redacted ? '(redacted)' : block.thinking}
        </div>
      )}
    </div>
  )
}

function AssistantItemView({ item }: { item: AssistantItem }) {
  return (
    <div class="pi-session-item pi-session-item-assistant">
      {item.blocks.map((block, i) => {
        if (block.type === 'text') {
          return <div key={i} class="pi-session-text">{block.text}</div>
        }
        if (block.type === 'thinking') {
          return <ThinkingBlock key={i} block={block} />
        }
        if (block.type === 'toolCall') {
          return <ToolBlock key={i} block={block} exec={item.toolExecMap[block.id]} />
        }
        return null
      })}
      {!item.complete && <span class="pi-session-cursor">▌</span>}
    </div>
  )
}

function RenderItemView({ item }: { item: RenderItem }) {
  if (item.kind === 'user') {
    return (
      <div class="pi-session-item pi-session-item-user">
        <span class="pi-session-prompt-prefix">&gt; </span>{item.text}
      </div>
    )
  }
  if (item.kind === 'assistant') {
    return <AssistantItemView item={item} />
  }
  // system
  return (
    <div class={`pi-session-item pi-session-item-system pi-session-item-system-${item.subtype}`}>
      · {item.text}
    </div>
  )
}

// ---------------------------------------------------------------------------
// PiSessionView
// ---------------------------------------------------------------------------

interface PiSessionViewProps {
  session: Session
  isActive: boolean
}

export function PiSessionView({ session, isActive }: PiSessionViewProps) {
  const [items, setItems] = useState<RenderItem[]>([])
  const [streaming, setStreaming] = useState(false)
  const [wsState, setWsState] = useState<'connecting' | 'open' | 'lost'>('connecting')
  const [inputText, setInputText] = useState('')

  const wsRef = useRef<WebSocket | null>(null)
  const retryCountRef = useRef(0)
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const messagesEndRef = useRef<HTMLDivElement>(null)

  // Auto-scroll on new items
  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [items])

  // Stable event dispatcher — mutates items state
  const dispatchEvent = useCallback((ev: Record<string, unknown>) => {
    switch (ev.type) {
      case 'session_ready': {
        setItems(prev => [...prev, {
          kind: 'system',
          subtype: 'ready',
          text: getSystemText(ev),
        }])
        break
      }
      case 'agent_start': {
        setStreaming(true)
        setItems(prev => [...prev, {
          kind: 'assistant',
          blocks: [],
          toolExecMap: {},
          complete: false,
        }])
        break
      }
      case 'message_update': {
        const blocks = extractBlocks(ev.message)
        setItems(prev => {
          const next = [...prev]
          // find last assistant item
          for (let i = next.length - 1; i >= 0; i--) {
            if (next[i].kind === 'assistant') {
              const cur = next[i] as AssistantItem
              next[i] = { ...cur, blocks }
              return next
            }
          }
          // no existing assistant item — create one
          return [...prev, { kind: 'assistant', blocks, toolExecMap: {}, complete: false }]
        })
        break
      }
      case 'message_end': {
        setItems(prev => {
          const next = [...prev]
          for (let i = next.length - 1; i >= 0; i--) {
            if (next[i].kind === 'assistant') {
              const cur = next[i] as AssistantItem
              next[i] = { ...cur, complete: true }
              return next
            }
          }
          return prev
        })
        break
      }
      case 'agent_end': {
        if (!ev.willRetry) setStreaming(false)
        break
      }
      case 'tool_execution_start': {
        const exec: ToolExec = {
          toolCallId: String(ev.toolCallId),
          toolName: String(ev.toolName),
          args: (ev.args as Record<string, unknown>) ?? {},
          output: '',
          done: false,
          isError: false,
        }
        setItems(prev => updateLastAssistantToolExec(prev, exec.toolCallId, exec))
        break
      }
      case 'tool_execution_update': {
        const toolCallId = String(ev.toolCallId)
        const partial = String(ev.partialResult ?? '')
        setItems(prev => updateLastAssistantToolExec(prev, toolCallId, existing => ({
          ...(existing ?? { toolCallId, toolName: '', args: {}, output: '', done: false, isError: false }),
          output: (existing?.output ?? '') + partial,
        })))
        break
      }
      case 'tool_execution_end': {
        const toolCallId = String(ev.toolCallId)
        const resultStr = (() => {
          const r = ev.result
          if (!r) return ''
          if (typeof r === 'string') return r
          try { return JSON.stringify(r) } catch { return '' }
        })()
        setItems(prev => updateLastAssistantToolExec(prev, toolCallId, existing => ({
          ...(existing ?? { toolCallId, toolName: '', args: {}, output: '', done: false, isError: false }),
          output: resultStr,
          done: true,
          isError: Boolean(ev.isError),
        })))
        break
      }
      case 'error': {
        setStreaming(false)
        setItems(prev => [...prev, {
          kind: 'system',
          subtype: 'error',
          text: getSystemText(ev),
        }])
        break
      }
      case 'warning':
      case 'compaction_start':
      case 'compaction_end':
      case 'auto_retry_start':
      case 'auto_retry_end': {
        setItems(prev => [...prev, {
          kind: 'system',
          subtype: ev.type === 'warning' ? 'warning' : 'info',
          text: getSystemText(ev),
        }])
        break
      }
      // ignored: queue_update, session_info_changed, thinking_level_changed, turn_start, turn_end, message_start
      default:
        break
    }
  }, [])

  const connect = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.onopen = null
      wsRef.current.onmessage = null
      wsRef.current.onclose = null
      wsRef.current.onerror = null
      wsRef.current.close()
      wsRef.current = null
    }
    setWsState('connecting')
    const ws = new WebSocket(wsUrl(session.id))
    wsRef.current = ws

    ws.onopen = () => {
      retryCountRef.current = 0
      setWsState('open')
    }

    ws.onmessage = (ev) => {
      const line = String(ev.data).trim()
      if (!line) return
      try {
        const parsed = JSON.parse(line) as Record<string, unknown>
        dispatchEvent(parsed)
      } catch {
        // ignore malformed lines
      }
    }

    ws.onclose = () => {
      if (wsRef.current !== ws) return // superseded
      setWsState('lost')
      setStreaming(false)
      const delay = Math.min(WS_BASE_MS * Math.pow(2, retryCountRef.current), WS_CAP_MS)
      retryCountRef.current++
      retryTimerRef.current = setTimeout(connect, delay)
    }

    ws.onerror = () => {
      ws.close()
    }
  }, [session.id, dispatchEvent])

  useEffect(() => {
    connect()
    return () => {
      if (retryTimerRef.current) clearTimeout(retryTimerRef.current)
      if (wsRef.current) {
        wsRef.current.onopen = null
        wsRef.current.onmessage = null
        wsRef.current.onclose = null
        wsRef.current.onerror = null
        wsRef.current.close()
        wsRef.current = null
      }
    }
  }, [connect])

  const sendMessage = useCallback((text: string) => {
    const trimmed = text.trim()
    if (!trimmed) return
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    ws.send(JSON.stringify({ type: 'prompt', text: trimmed }))
    setItems(prev => [...prev, { kind: 'user', text: trimmed }])
    setInputText('')
  }, [])

  const sendAbort = useCallback(() => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    ws.send(JSON.stringify({ type: 'abort' }))
  }, [])

  const handleKeyDown = useCallback((e: KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      sendMessage(inputText)
    }
  }, [inputText, sendMessage])

  return (
    <div
      class="pi-session"
      style={{ display: isActive ? 'flex' : 'none' }}
    >
      <div class="pi-session-messages">
        {items.map((item, i) => <RenderItemView key={i} item={item} />)}
        <div ref={messagesEndRef} />
      </div>

      <div class="pi-session-input-bar">
        {wsState !== 'open' && (
          <span class={`pi-session-ws-state pi-session-ws-state-${wsState}`}>
            {wsState === 'connecting' ? '⋯ connecting' : '✗ lost'}
          </span>
        )}

        {streaming && (
          <button
            class="pi-session-abort-btn"
            type="button"
            onClick={sendAbort}
          >
            ■ abort
          </button>
        )}

        <input
          class="pi-session-input"
          type="text"
          value={inputText}
          onInput={(e) => setInputText((e.target as HTMLInputElement).value)}
          onKeyDown={handleKeyDown}
          placeholder="message…"
          disabled={wsState !== 'open'}
        />
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Helper: update toolExecMap on the last AssistantItem
// ---------------------------------------------------------------------------

function updateLastAssistantToolExec(
  prev: RenderItem[],
  toolCallId: string,
  update: ToolExec | ((existing: ToolExec | undefined) => ToolExec),
): RenderItem[] {
  const next = [...prev]
  for (let i = next.length - 1; i >= 0; i--) {
    if (next[i].kind === 'assistant') {
      const cur = next[i] as AssistantItem
      const existing = cur.toolExecMap[toolCallId]
      const resolved = typeof update === 'function' ? update(existing) : update
      next[i] = {
        ...cur,
        toolExecMap: { ...cur.toolExecMap, [toolCallId]: resolved },
      }
      return next
    }
  }
  return prev
}

// ---------------------------------------------------------------------------
// Export guard used by main.tsx
// ---------------------------------------------------------------------------

/** Returns true for sessions driven by the pi-sdk subprocess adapter. */
export function isPiSDKSession(session: { kind: string }): boolean {
  return session.kind === 'pi-sdk' || session.kind === 'pi-sdk-sbx'
}
