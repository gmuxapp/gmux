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
 *
 * Block order is preserved so interleaved reasoning/text keeps its sequence.
 * Empty blocks are dropped (mid-stream a block can momentarily be '').
 */
import type { ConvMessage } from './conversation'
import type { ThreadMessageLike } from '@assistant-ui/react'

type Part = Extract<ThreadMessageLike['content'], readonly unknown[]>[number]

export function toThreadMessage(m: ConvMessage, index: number): ThreadMessageLike {
  const content: Part[] = []
  for (const block of m.content) {
    const text = block.text ?? ''
    if (!text) continue
    if (block.type === 'thinking') {
      content.push({ type: 'reasoning', text })
    } else if (block.type === 'text') {
      content.push({ type: 'text', text })
    }
    // Unknown block types (future tool_call etc.) are handled by dedicated
    // slices; ignored here rather than mis-rendered.
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
