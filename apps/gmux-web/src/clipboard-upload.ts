/**
 * Clipboard binary upload helper.
 *
 * Materializes a non-text clipboard payload as a file on the gmuxd
 * that owns the session, returning the absolute path the caller should
 * type into the PTY. Owns: web-side size cap, MIME header, error
 * categorization. Does not own: clipboard inspection, blob extraction,
 * permission prompting, path injection — those belong to the call
 * sites in keyboard.ts.
 *
 * Mirrors the daemon-side cap in services/gmuxd/cmd/gmuxd/clipboard.go
 * (MaxClipboardBytes). Web-side enforcement avoids uploading bytes
 * that will be rejected; server-side enforcement is the safety floor.
 */

export const MAX_CLIPBOARD_BYTES = 10 * 1024 * 1024

export type UploadResult =
  | { ok: true; path: string }
  | { ok: false; error: UploadError }

/**
 * Categorical error codes for upload failures. The values are stable
 * identifiers callers can switch on for toast copy; they are deliberately
 * not user-facing strings.
 */
export type UploadError =
  | 'too_large'
  | 'network'
  | 'server_error'
  | 'empty_body'
  | string // server-supplied codes (e.g. 'write_failed')

/**
 * Returns the first MIME from `types` that is not `text/*`, or null if
 * the clipboard offers only text representations. Captures the
 * "binary intent wins over text fallback" rule from the design doc:
 * a clipboard with both an image and an alt-text representation is
 * treated as image paste.
 */
export function firstBinaryType(types: readonly string[]): string | null {
  for (const t of types) {
    if (!t.startsWith('text/')) return t
  }
  return null
}

/**
 * POST a clipboard binary payload to the session's clipboard endpoint
 * and return the materialized path.
 *
 * Errors are returned as UploadResult rather than thrown: every code
 * path produces a value the caller can inspect to decide between
 * "type the path" and "show a toast".
 */
export async function uploadClipboardBlob(
  blob: Blob,
  sessionId: string,
): Promise<UploadResult> {
  if (blob.size > MAX_CLIPBOARD_BYTES) {
    return { ok: false, error: 'too_large' }
  }

  const url = `/v1/sessions/${encodeURIComponent(sessionId)}/clipboard`

  let resp: Response
  try {
    resp = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': blob.type || 'application/octet-stream' },
      body: blob,
    })
  } catch {
    return { ok: false, error: 'network' }
  }

  // Try to parse the body regardless of status: success bodies carry
  // the path, error bodies carry the code. A non-JSON body from
  // either a 2xx or non-2xx response means the daemon broke its
  // contract; we collapse both to 'server_error'.
  let parsed: unknown = null
  try {
    parsed = await resp.json()
  } catch {
    return { ok: false, error: 'server_error' }
  }

  if (resp.ok && isOkPayload(parsed)) {
    return { ok: true, path: parsed.data.path }
  }

  if (isErrorPayload(parsed)) {
    return { ok: false, error: parsed.error.code || 'server_error' }
  }

  return { ok: false, error: 'server_error' }
}

interface OkPayload {
  ok: true
  data: { path: string }
}

interface ErrorPayload {
  ok: false
  error: { code: string; message?: string }
}

function isOkPayload(v: unknown): v is OkPayload {
  if (typeof v !== 'object' || v === null) return false
  const r = v as Record<string, unknown>
  if (r.ok !== true) return false
  const data = r.data
  if (typeof data !== 'object' || data === null) return false
  return typeof (data as Record<string, unknown>).path === 'string'
}

function isErrorPayload(v: unknown): v is ErrorPayload {
  if (typeof v !== 'object' || v === null) return false
  const r = v as Record<string, unknown>
  if (r.ok !== false) return false
  const err = r.error
  if (typeof err !== 'object' || err === null) return false
  return typeof (err as Record<string, unknown>).code === 'string'
}
