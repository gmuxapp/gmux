// Fetches persisted scrollback for a session from the gmuxd broker at
// GET /v1/sessions/<id>/scrollback?extracted=1.
//
// The server runs ExtractBytes (Go) on the raw PTY bytes and returns only
// the compact, human-readable result — typically ~1–2% of the raw file
// size for long pi sessions (e.g. 400–900 KB instead of 20 MB). The
// returned bytes can be written directly into the terminal emulator with
// no further processing on the client.
//
// Response semantics (see services/gmuxd/cmd/gmuxd/scrollback.go):
//   200 + bytes  →  extracted scrollback ready to inject
//   200 + empty  →  session known, no scrollback captured yet
//   404          →  session unknown to this gmuxd
//   5xx          →  server-side I/O error
//
// Peer-owned sessions are forwarded transparently by the hub, so callers
// don't need to special-case `id@peer`.

export type ScrollbackResult =
  | { kind: 'bytes'; bytes: Uint8Array }
  | { kind: 'empty' }
  | { kind: 'not-found' }
  | { kind: 'error'; status: number; message: string }

/** Inject-friendly fetcher; defaults to global fetch. */
export type FetchFn = (input: RequestInfo, init?: RequestInit) => Promise<Response>

export async function fetchScrollback(
  sessionId: string,
  fetchImpl: FetchFn = fetch,
): Promise<ScrollbackResult> {
  const url = `/v1/sessions/${encodeURIComponent(sessionId)}/scrollback?extracted=1`

  let resp: Response
  try {
    resp = await fetchImpl(url)
  } catch (e) {
    return { kind: 'error', status: 0, message: e instanceof Error ? e.message : String(e) }
  }

  if (resp.status === 404) {
    return { kind: 'not-found' }
  }
  if (!resp.ok) {
    return { kind: 'error', status: resp.status, message: resp.statusText || 'request failed' }
  }

  // Body read can still reject after headers arrive (connection
  // aborted mid-stream). Without this catch the caller would see
  // an unhandled rejection and the UI would stall on 'Loading…'
  // forever.
  let buf: ArrayBuffer
  try {
    buf = await resp.arrayBuffer()
  } catch (e) {
    return { kind: 'error', status: resp.status, message: e instanceof Error ? e.message : String(e) }
  }
  if (buf.byteLength === 0) {
    return { kind: 'empty' }
  }
  return { kind: 'bytes', bytes: new Uint8Array(buf) }
}
