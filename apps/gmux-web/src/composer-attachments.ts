/**
 * Composer attachments: reuse the clipboard-upload endpoint to materialize a
 * dropped/attached file as a `/tmp` path on the gmuxd that owns the session,
 * then splice that path into the outgoing composed message.
 *
 * Owns: the assistant-ui AttachmentAdapter (upload-on-add), the send-time text
 * splice, and error-code → human copy for the attachment chip. Does NOT own:
 * the HTTP upload itself (clipboard-upload.ts) or the PTY keystroke send
 * (conversation-view.tsx / island onSend).
 *
 * Framing decision (per findings-composer-features.md "Task 1 — Attachments"):
 * a composed message is NOT a paste into a TUI line editor, so we splice the
 * BARE absolute path — no bracketed-paste escapes. Paths are appended after the
 * user's text, space-separated, and the message keeps a trailing space before
 * the caller appends the submit `\r` (mirrors how paste leaves the cursor after
 * the inserted path).
 */
import type { AttachmentAdapter, CompleteAttachment, PendingAttachment } from '@assistant-ui/react'
import { uploadClipboardBlob } from './clipboard-upload'

/**
 * Marker prefix stored in a completed attachment's content text part. It lets
 * onNew recover the uploaded `/tmp` path from `message.attachments` without a
 * side-channel. Not user-visible (attachments render Name, not content).
 */
const PATH_CONTENT_TYPE = 'text' as const

/**
 * Splice uploaded attachment paths into the composed message text.
 *
 * - No attachments → text unchanged.
 * - Attachments only (empty text) → just the path(s), trailing space.
 * - Text + attachments → text, a space, the path(s), trailing space.
 *
 * Bare paths, no bracketed-paste escapes (see module header). The trailing
 * space keeps the path a distinct token before the caller's submit `\r`.
 */
export function composeMessageWithAttachments(
  text: string,
  paths: readonly string[],
): string {
  if (paths.length === 0) return text
  const joined = paths.join(' ')
  const trimmed = text.trimEnd()
  const body = trimmed ? `${trimmed} ${joined}` : joined
  return `${body} `
}

/**
 * Extract the uploaded paths carried by a message's completed attachments, in
 * order. Each adapter-completed attachment stores its `/tmp` path as a single
 * text content part (see makeAttachmentAdapter.send).
 */
export function attachmentPaths(
  attachments: readonly CompleteAttachment[] | undefined,
): string[] {
  if (!attachments) return []
  const paths: string[] = []
  for (const att of attachments) {
    for (const part of att.content ?? []) {
      if (part.type === 'text' && part.text) paths.push(part.text)
    }
  }
  return paths
}

/**
 * Human-facing copy for an upload error code shown on the attachment chip.
 * Mirrors keyboard.ts `pasteErrorMessage` (kept in sync deliberately — the
 * underlying endpoint and codes are identical), reworded for "attach".
 */
export function attachmentErrorMessage(code: string): string {
  switch (code) {
    case 'too_large':
      return 'File too large (limit 10MB)'
    case 'network':
      return 'Attach failed: gmuxd unreachable'
    case 'empty_body':
      return 'File is empty'
    case 'not_found':
      return 'Attach failed: session not found'
    case 'write_failed':
      return 'Attach failed: could not write file'
    case 'server_error':
      return 'Attach failed: server error'
    default:
      return `Attach failed: ${code}`
  }
}

/**
 * Build an assistant-ui AttachmentAdapter that uploads on add (so the `/tmp`
 * path exists by the time the chip appears) and, on send, hands the path back
 * as the attachment's text content for onNew to splice.
 *
 * On upload failure the adapter throws with human copy; the composer runtime
 * surfaces it via its `attachmentAddError` event (wired in the island).
 */
export function makeAttachmentAdapter(sessionId: string): AttachmentAdapter {
  // id → uploaded path, populated in add(), read in send().
  const paths = new Map<string, string>()

  return {
    accept: '*',
    async add({ file }): Promise<PendingAttachment> {
      const result = await uploadClipboardBlob(file, sessionId)
      if (!result.ok) {
        throw new Error(attachmentErrorMessage(result.error))
      }
      const id = `${file.name}:${result.path}`
      paths.set(id, result.path)
      return {
        id,
        type: 'file',
        name: file.name || result.path,
        contentType: file.type || undefined,
        file,
        status: { type: 'requires-action', reason: 'composer-send' },
      }
    },
    async remove(attachment): Promise<void> {
      paths.delete(attachment.id)
    },
    async send(attachment): Promise<CompleteAttachment> {
      const path = paths.get(attachment.id) ?? attachment.name
      return {
        ...attachment,
        status: { type: 'complete' },
        content: [{ type: PATH_CONTENT_TYPE, text: path }],
      }
    },
  }
}
