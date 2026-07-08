/**
 * Pure mapping from gmux's framework-agnostic conversation store
 * (conversation.ts) to assistant-ui's ThreadMessageLike shape.
 *
 * This is the whole "shape mismatch" surface between our ACP-derived store and
 * @assistant-ui/react, isolated here so it's unit-testable without React:
 *
 *   - role            → role (both use 'user' | 'assistant')
 *   - {type:'text'}   → { type: 'text', text }         (rendered as markdown)
 *   - {type:'thinking'} → { type: 'reasoning', text }  (assistant-ui's CoT part)
 *   - {type:'tool_call'} → { type: 'tool-call', ... }  (assistant-ui's tool part)
 *
 * Block order is preserved so interleaved reasoning/text/tool-call keeps its
 * sequence. Empty text/thinking blocks are dropped (mid-stream a block can
 * momentarily be ''); tool-call blocks are always kept.
 */
import type { ConvMessage, ContentBlock } from './conversation'
import type { ThreadMessageLike } from '@assistant-ui/react'

type Part = Extract<ThreadMessageLike['content'], readonly unknown[]>[number]
type ToolCallPart = Extract<Part, { type: 'tool-call' }>

// Map a gmux tool-call block to assistant-ui's tool-call content part. The
// raw JSON `args` text is parsed to an object (assistant-ui also keeps the raw
// text via argsText for streaming/partial display); the textual output becomes
// the result, and a `failed` status maps to isError. assistant-ui derives the
// running/complete status from the message + whether a result is present.
function toToolCallPart(block: ContentBlock): ToolCallPart {
  type Args = ToolCallPart['args']
  let args: Args = {} as Args
  const argsText = block.args ?? ''
  if (argsText) {
    try {
      const parsed = JSON.parse(argsText)
      if (parsed && typeof parsed === 'object') args = parsed as Args
    } catch {
      // Partial/invalid JSON mid-stream: leave args empty, keep argsText.
    }
  }
  const done = block.status === 'completed' || block.status === 'failed'
  return {
    type: 'tool-call',
    toolCallId: block.toolCallId ?? '',
    toolName: block.toolName ?? '',
    args,
    argsText,
    ...(done ? { result: block.output ?? '' } : {}),
    ...(block.status === 'failed' ? { isError: true } : {}),
  }
}

export function toThreadMessage(m: ConvMessage, index: number): ThreadMessageLike {
  const content: Part[] = []
  for (const block of m.content) {
    if (block.type === 'tool_call') {
      content.push(toToolCallPart(block))
      continue
    }
    const text = block.text ?? ''
    if (!text) continue
    if (block.type === 'thinking') {
      content.push({ type: 'reasoning', text })
    } else if (block.type === 'text') {
      content.push({ type: 'text', text })
    }
    // Unknown block types are ignored rather than mis-rendered.
  }
  // assistant-ui requires non-empty content; a just-opened assistant message
  // with no delta yet gets an empty text part so the bubble can render.
  if (content.length === 0) content.push({ type: 'text', text: '' })

  return {
    // Stable id: the streaming messageId when present, else positional. Ids
    // must be stable across renders so assistant-ui doesn't remount bubbles.
    id: m.messageId ?? `msg-${index}`,
    role: m.role,
    content,
  }
}

export function toThreadMessages(messages: ConvMessage[]): ThreadMessageLike[] {
  return messages.map(toThreadMessage)
}
