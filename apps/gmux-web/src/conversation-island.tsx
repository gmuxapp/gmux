/** @jsxImportSource react */
/**
 * assistant-ui React island for the ACP conversation surface (ADR 0021).
 *
 * The pragma above forces React's JSX runtime for THIS file only (the rest of
 * the app is Preact). Paired with vite.config.ts `reactAliasesEnabled: false`,
 * it makes the island a genuine React 18 subtree.
 *
 * This is a *real React 18* subtree (see vite.config.ts `reactAliasesEnabled:
 * false`) mounted from the Preact app by conversation-view.tsx. Everything the
 * dependency provides is used from the dependency: message list + streaming,
 * markdown + code highlighting, reasoning (chain-of-thought) rendering, the
 * composer, and Web Speech dictation. gmux supplies only:
 *
 *   1. the conversation data — via the framework-agnostic conversation.ts store
 *      adapted to ThreadMessageLike (conversation-adapter.ts), and
 *   2. the write path — `onNew` bridges to the existing keystroke input channel
 *      (ADR 0021 §6), the same PTY actuator the mobile key bar uses. No ACP
 *      `session/prompt` duplex is introduced.
 *
 * SPIKE TODOs (tracked in the PR): the ACP live stream carries only assistant
 * chunks, so a just-sent user turn is not echoed live (only on reconnect via
 * session/load). We optimistically render sent messages locally; de-duping
 * against a future live user echo (or a `user_message_chunk` variant) is open.
 */
import { useEffect, useMemo, useState } from 'react'
import {
  AssistantRuntimeProvider,
  useExternalStoreRuntime,
  ThreadPrimitive,
  MessagePrimitive,
  ComposerPrimitive,
  WebSpeechDictationAdapter,
  type ThreadMessageLike,
  type AppendMessage,
} from '@assistant-ui/react'
import { MarkdownTextPrimitive } from '@assistant-ui/react-markdown'
// The only styling the packages ship: a streaming "pulse cursor" appended to
// the last element while a message is generating (targets .aui-md[data-status
// =running]). Bundled into the lazy island chunk. Typographic markdown styling
// is NOT shipped (it's Tailwind `prose` in assistant-ui's copied components,
// which we skip) — that stays in our minimal .conversation-markdown rules.
import '@assistant-ui/react-markdown/styles/dot.css'
import { makePrismAsyncLightSyntaxHighlighter } from '@assistant-ui/react-syntax-highlighter'
import type { ConversationStore, ConvMessage } from './conversation'
import { toThreadMessage } from './conversation-adapter'

export interface ConversationIslandProps {
  store: ConversationStore
  /** Send raw bytes to the session's PTY (the §6 keystroke actuator). */
  onSend?: (data: string) => void
}

// Code highlighting comes from the dependency (replaces the removed
// highlight.js). Async-light registers languages on demand, keeping the
// island's initial payload small. useInlineStyles:false suppresses the
// bundled (light) Prism theme so our own dark `.token` CSS applies and code
// blocks match the terminal.
const SyntaxHighlighter = makePrismAsyncLightSyntaxHighlighter({ useInlineStyles: false })

// Assistant text → markdown. `smooth` animates streamed tokens; the primitive
// already tolerates the incomplete markdown that arrives mid-stream. Reuses the
// app's existing `.conversation-markdown` styles verbatim.
const MarkdownText = () => (
  <MarkdownTextPrimitive
    className="conversation-markdown aui-md"
    smooth
    components={{ SyntaxHighlighter }}
  />
)

// Reasoning (ACP agent_thought_chunk → assistant-ui reasoning part), rendered
// as a distinct collapsible block. Renders ONLY this part's reasoning text
// (via the part props) — NOT MessagePrimitive.Parts, which would re-render the
// whole message's text inside every thinking block (the duplication/empty-block
// bug). Reasoning is prose; plain text with preserved newlines is enough.
const Reasoning = ({ text }: { text?: string }) => (
  <details className="conversation-thinking" data-block="thinking">
    <summary className="conversation-thinking-label">Thinking</summary>
    <div className="conversation-thinking-body">{text}</div>
  </details>
)

const UserMessage = () => (
  <MessagePrimitive.Root
    className="conversation-message conversation-message--user"
    data-role="user"
  >
    <MessagePrimitive.Parts />
  </MessagePrimitive.Root>
)

const AssistantMessage = () => (
  <MessagePrimitive.Root
    className="conversation-message conversation-message--assistant"
    data-role="assistant"
  >
    <MessagePrimitive.Parts components={{ Text: MarkdownText, Reasoning }} />
  </MessagePrimitive.Root>
)

const dictation = WebSpeechDictationAdapter.isSupported()
  ? new WebSpeechDictationAdapter()
  : undefined

export function ConversationIsland({ store, onSend }: ConversationIslandProps) {
  const [tick, setTick] = useState(0)
  // Optimistic local echo of sent user turns (see SPIKE TODO above).
  const [echoes, setEchoes] = useState<ConvMessage[]>([])

  useEffect(() => store.subscribe(() => setTick((n) => n + 1)), [store])

  const messages: ThreadMessageLike[] = useMemo(
    () => [...store.getMessages(), ...echoes].map(toThreadMessage),
    // Re-derive on every store notification (tick) and when a new echo lands.
    [store, echoes, tick],
  )

  const runtime = useExternalStoreRuntime({
    messages,
    // messages are already ThreadMessageLike, so conversion is identity — this
    // also sidesteps assistant-ui not passing an index to convertMessage.
    convertMessage: (m: ThreadMessageLike) => m,
    onNew: async (message: AppendMessage) => {
      const text = message.content
        .filter((p): p is { type: 'text'; text: string } => p.type === 'text')
        .map((p) => p.text)
        .join('')
      if (!text) return
      setEchoes((prev) => [
        ...prev,
        { role: 'user', content: [{ type: 'text', text }] },
      ])
      // §6: composed text reaches pi as keystrokes + Enter.
      onSend?.(text + '\r')
    },
    adapters: dictation ? { dictation } : undefined,
  })

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <ThreadPrimitive.Root className="conversation-thread">
        <ThreadPrimitive.Viewport className="conversation-viewport">
          <ThreadPrimitive.Empty>
            <div className="conversation-empty">No conversation yet.</div>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages
            components={{ UserMessage, AssistantMessage }}
          />
        </ThreadPrimitive.Viewport>
        <ComposerPrimitive.Root className="conversation-composer">
          <ComposerPrimitive.Input
            className="conversation-composer-input"
            placeholder="Message…"
          />
          {dictation && (
            <>
              <ComposerPrimitive.Dictate className="conversation-composer-mic">
                🎤
              </ComposerPrimitive.Dictate>
            </>
          )}
          <ComposerPrimitive.Send className="conversation-composer-send">
            Send
          </ComposerPrimitive.Send>
        </ComposerPrimitive.Root>
      </ThreadPrimitive.Root>
    </AssistantRuntimeProvider>
  )
}
