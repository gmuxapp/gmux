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
 * markdown + code highlighting, reasoning (chain-of-thought) rendering, and the
 * composer. gmux supplies only:
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
  AttachmentPrimitive,
  type ThreadMessageLike,
  type AppendMessage,
  type ToolCallMessagePartProps,
} from '@assistant-ui/react'
import {
  attachmentPaths,
  composeMessageWithAttachments,
  makeAttachmentAdapter,
} from './composer-attachments'
import { MarkdownTextPrimitive } from '@assistant-ui/react-markdown'
// NB: we intentionally do NOT import '@assistant-ui/react-markdown/styles/
// dot.css'. That stylesheet's sole effect is a streaming "pulse cursor" (●)
// appended to the last element while a message generates — driven by the same
// isRunning (= gmux `working`) signal as our composer "Working…" pill, so it's
// a redundant second indicator. The pill is authoritative (whole-turn); the ●
// also reads like content mid-prose. Typographic markdown styling isn't shipped
// either (it's Tailwind `prose`); that stays in our .conversation-markdown CSS.
import { makePrismAsyncLightSyntaxHighlighter } from '@assistant-ui/react-syntax-highlighter'
import type { ConversationStore, ConvMessage } from './conversation'
import { toThreadMessage, toolCallKind } from './conversation-adapter'

export interface ConversationIslandProps {
  store: ConversationStore
  /** Send raw bytes to the session's PTY (the §6 keystroke actuator). */
  onSend?: (data: string) => void
  /** gmux session "working" status — drives the composer indicator + isRunning. */
  working?: boolean
  /** Session id — needed to upload composer attachments to the owning gmuxd. */
  sessionId?: string
}

// One attachment chip: a pill showing the file name with a remove button,
// styled to match the composer. assistant-ui drives add/remove; we only render.
const ComposerAttachment = () => (
  <AttachmentPrimitive.Root className="conversation-attachment">
    <AttachmentPrimitive.Name />
    <AttachmentPrimitive.Remove
      className="conversation-attachment-remove"
      aria-label="Remove attachment"
    >
      ×
    </AttachmentPrimitive.Remove>
  </AttachmentPrimitive.Root>
)

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
const Reasoning = ({ text, status }: { text?: string; status?: { type?: string } }) => {
  const running = status?.type === 'running'
  return (
    <details className="conversation-thinking" data-block="thinking">
      <summary className="conversation-disclosure">
        <span className={running ? 'conversation-shimmer' : undefined}>Thinking</span>
      </summary>
      <div className="conversation-thinking-body">{text}</div>
    </details>
  )
}

// Tool call (ACP tool_call/tool_call_update → assistant-ui tool-call part).
//
// One collapsed-by-default row per call — a status dot, a kind-derived label,
// and the single most useful argument inline (command / path / pattern), so you
// know what the tool did without expanding. Expanding reveals the full output
// (char-capped with a "show all") and the raw arguments. Rendering keys on the
// ACP `kind` (mirrored from the agent), NOT the free-form tool name — the ACP
// way (kinds "help Clients choose appropriate icons and optimize display").
// assistant-ui computes `status` from the message + whether a result is
// present, so we drive the running→done→error indicator off it.

// Verb per ACP ToolKind as [running, done] — the collapsed row reads like a
// quiet log line ("Executing cd .." → "Executed cd .."). Unknown kinds fall
// back to the tool name so a new/custom tool still reads sensibly.
const KIND_VERB: Record<string, [string, string]> = {
  read: ['Reading', 'Read'],
  edit: ['Editing', 'Edited'],
  delete: ['Deleting', 'Deleted'],
  move: ['Moving', 'Moved'],
  search: ['Searching', 'Searched'],
  execute: ['Executing', 'Executed'],
  fetch: ['Fetching', 'Fetched'],
  think: ['Thinking', 'Thought'],
}

// Output preview cap: expanding a call shows this many chars, then a "show all"
// reveals the rest (mirrors Open WebUI's second guardrail on top of collapse).
const OUTPUT_PREVIEW_LIMIT = 10000

const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined)

// The one argument worth showing inline in the header, chosen per kind. Falls
// back to the common "key field" heuristic (opencode's GenericTool / assistant-
// ui ToolFallback) for `other`/unknown kinds.
function inlineArg(kind: string, args: Record<string, unknown>): string | undefined {
  switch (kind) {
    case 'execute':
      return str(args.command) ?? str(args.cmd)
    case 'read':
    case 'edit':
    case 'delete':
    case 'move':
      return str(args.file) ?? str(args.path) ?? str(args.filePath)
    case 'search':
      return str(args.pattern) ?? str(args.query) ?? str(args.regex)
    case 'fetch':
      return str(args.url)
    default:
      return (
        str(args.description) ??
        str(args.query) ??
        str(args.url) ??
        str(args.filePath) ??
        str(args.path) ??
        str(args.pattern) ??
        str(args.command) ??
        str(args.name)
      )
  }
}

const ToolCall = (props: ToolCallMessagePartProps) => {
  const { toolName, args, argsText, result, isError, status } = props
  // `kind` is smuggled onto the part by the adapter (assistant-ui's type doesn't
  // declare it); read it through the typed helper with a narrow cast.
  const kind = toolCallKind(props as { kind?: string })
  const running = status?.type === 'running'
  const argsObj = (args ?? {}) as Record<string, unknown>
  const isBash = kind === 'execute'
  const command = str(argsObj.command) ?? str(argsObj.cmd) ?? ''
  const [verbRunning, verbDone] = KIND_VERB[kind] ?? [toolName ?? 'Tool', toolName ?? 'Tool']
  const verb = running ? verbRunning : verbDone
  const arg = isBash ? command : inlineArg(kind, argsObj)
  const output = typeof result === 'string' ? result : result != null ? String(result) : ''

  const [showAll, setShowAll] = useState(false)
  const truncated = output.length > OUTPUT_PREVIEW_LIMIT && !showAll
  const shownOutput = truncated ? output.slice(0, OUTPUT_PREVIEW_LIMIT) : output
  const more = truncated ? (
    <button type="button" className="conversation-tool-more" onClick={() => setShowAll(true)}>
      Show all ({output.length.toLocaleString()} characters)
    </button>
  ) : null

  return (
    <details className="conversation-tool" data-block="tool" data-kind={kind}>
      <summary className="conversation-disclosure">
        <span className={running ? 'conversation-tool-verb conversation-shimmer' : 'conversation-tool-verb'}>
          {verb}
        </span>
        {arg ? (
          <span className={isBash ? 'conversation-tool-cmd' : 'conversation-tool-arg'}>{arg}</span>
        ) : null}
        {isError ? <span className="conversation-tool-fail" aria-label="failed" /> : null}
      </summary>
      <div className="conversation-tool-body">
        {isBash ? (
          <>
            {command ? (
              <div className="conversation-tool-cmdline">
                <span className="conversation-tool-prompt">❯ </span>
                {command}
              </div>
            ) : null}
            {output ? <pre className="conversation-tool-pre">{shownOutput}</pre> : null}
            {more}
          </>
        ) : output ? (
          <>
            <pre className="conversation-tool-pre">{shownOutput}</pre>
            {more}
          </>
        ) : running ? (
          <div className="conversation-tool-hint">Running…</div>
        ) : null}
        {!isBash && argsText && argsText !== '{}' ? (
          <details className="conversation-tool-args">
            <summary className="conversation-disclosure conversation-tool-args-label">
              Arguments
            </summary>
            <pre className="conversation-tool-pre">{argsText}</pre>
          </details>
        ) : null}
      </div>
    </details>
  )
}

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
    <MessagePrimitive.Parts
      components={{ Text: MarkdownText, Reasoning, tools: { Fallback: ToolCall } }}
    />
  </MessagePrimitive.Root>
)

export function ConversationIsland({ store, onSend, working, sessionId }: ConversationIslandProps) {
  const [tick, setTick] = useState(0)
  // Optimistic local echo of sent user turns (see SPIKE TODO above).
  const [echoes, setEchoes] = useState<ConvMessage[]>([])
  // Visible surface for an attachment-upload failure (chip-level error copy).
  const [attachError, setAttachError] = useState<string | null>(null)

  useEffect(() => store.subscribe(() => setTick((n) => n + 1)), [store])

  // Upload-on-add attachment adapter, rebuilt when the session changes so the
  // upload always targets the gmuxd that owns the current PTY. Omitted when we
  // have no session id (attachments simply stay disabled then).
  const attachments = useMemo(
    () => (sessionId ? makeAttachmentAdapter(sessionId) : undefined),
    [sessionId],
  )

  const messages: ThreadMessageLike[] = useMemo(
    () => [...store.getMessages(), ...echoes].map(toThreadMessage),
    // Re-derive on every store notification (tick) and when a new echo lands.
    [store, echoes, tick],
  )

  const runtime = useExternalStoreRuntime({
    messages,
    // Feed the gmux session's "working" status in as assistant-ui's isRunning,
    // so the thread reflects agent activity through the dependency's own state
    // (rather than a parallel signal we'd have to thread everywhere).
    isRunning: !!working,
    // messages are already ThreadMessageLike, so conversion is identity — this
    // also sidesteps assistant-ui not passing an index to convertMessage.
    convertMessage: (m: ThreadMessageLike) => m,
    adapters: attachments ? { attachments } : undefined,
    onNew: async (message: AppendMessage) => {
      const text = message.content
        .filter((p): p is { type: 'text'; text: string } => p.type === 'text')
        .map((p) => p.text)
        .join('')
      // Uploaded /tmp path(s) carried by completed attachments (upload happened
      // on add). Splice them into the outgoing text before the submit \r.
      const paths = attachmentPaths(message.attachments)
      const composed = composeMessageWithAttachments(text, paths)
      if (!composed) return
      // Echo exactly what we send (text + spliced paths) so the local echo
      // matches the turn pi receives.
      setEchoes((prev) => [
        ...prev,
        { role: 'user', content: [{ type: 'text', text: composed.trimEnd() }] },
      ])
      // §6: composed text reaches pi as keystrokes + Enter.
      onSend?.(composed + '\r')
    },
  })

  // Surface attachment-upload failures (adapter throws → runtime emits
  // attachmentAddError) as an inline error under the composer.
  useEffect(
    () =>
      runtime.thread.composer.unstable_on('attachmentAddError', (e) => {
        setAttachError(e.message || 'Attachment failed')
      }),
    [runtime],
  )

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
        {working ? (
          <div className="conversation-working" aria-live="polite">
            <span className="conversation-working-dot" aria-hidden="true" />
            Working…
          </div>
        ) : null}
        {attachError ? (
          <div className="conversation-attach-error" role="alert">
            {attachError}
            <button
              type="button"
              className="conversation-attach-error-dismiss"
              aria-label="Dismiss"
              onClick={() => setAttachError(null)}
            >
              ×
            </button>
          </div>
        ) : null}
        <ComposerPrimitive.Root className="conversation-composer">
          {attachments ? (
            <ComposerPrimitive.AttachmentDropzone className="conversation-composer-dropzone">
              <div className="conversation-composer-main">
                <div className="conversation-attachments">
                  <ComposerPrimitive.Attachments
                    components={{ Attachment: ComposerAttachment }}
                  />
                </div>
                <div className="conversation-composer-row">
                  <ComposerPrimitive.AddAttachment
                    className="conversation-composer-add"
                    aria-label="Attach file"
                    multiple
                  >
                    +
                  </ComposerPrimitive.AddAttachment>
                  <ComposerPrimitive.Input
                    className="conversation-composer-input"
                    placeholder="Message…"
                  />
                  <ComposerPrimitive.Send className="conversation-composer-send">
                    Send
                  </ComposerPrimitive.Send>
                </div>
              </div>
            </ComposerPrimitive.AttachmentDropzone>
          ) : (
            <>
              <ComposerPrimitive.Input
                className="conversation-composer-input"
                placeholder="Message…"
              />
              <ComposerPrimitive.Send className="conversation-composer-send">
                Send
              </ComposerPrimitive.Send>
            </>
          )}
        </ComposerPrimitive.Root>
      </ThreadPrimitive.Root>
    </AssistantRuntimeProvider>
  )
}
