// Fetches persisted scrollback for a (typically dead) session from the
// gmuxd broker endpoint at GET /v1/sessions/<id>/scrollback.
//
// Response semantics (see services/gmuxd/cmd/gmuxd/scrollback.go):
//   200 + bytes  →  raw PTY scrollback (octet-stream)
//   200 + empty  →  session is known but no scrollback was captured
//                   (e.g. died before scrollback persistence shipped,
//                    or fast-exited before any output)
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
  const url = `/v1/sessions/${encodeURIComponent(sessionId)}/scrollback`

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
