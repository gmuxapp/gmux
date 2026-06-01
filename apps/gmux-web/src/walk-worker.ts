/**
 * walk-worker.ts — Web Worker for streaming the full project walk.
 *
 * Fetches GET /v1/fs/{slug}/walk?full=true which returns NDJSON (one path per
 * line). Parses incrementally so the main thread is never blocked by a large
 * JSON.parse(). Posts batches of ~1000 paths back to the main thread, then a
 * final "done" message with the snapshot version parsed from the trailer line.
 *
 * Messages TO main thread:
 *   { type: 'batch', paths: string[] }
 *   { type: 'done',  version: number }
 *   { type: 'error', message: string }
 */

const BATCH_SIZE = 1000

self.onmessage = async (evt: MessageEvent<{ slug: string; includeHidden: boolean }>) => {
  const { slug, includeHidden } = evt.data
  const params = new URLSearchParams({ full: 'true' })
  if (includeHidden) params.set('include_hidden', 'true')

  let response: Response
  try {
    response = await fetch(`/v1/fs/${encodeURIComponent(slug)}/walk?${params}`)
  } catch (e) {
    self.postMessage({ type: 'error', message: String(e) })
    return
  }

  if (!response.ok || !response.body) {
    self.postMessage({ type: 'error', message: `walk stream failed: ${response.status}` })
    return
  }

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let batch: string[] = []
  let version = -1

  const flushBatch = () => {
    if (batch.length > 0) {
      self.postMessage({ type: 'batch', paths: batch })
      batch = []
    }
  }

  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break

      buffer += decoder.decode(value, { stream: true })
      const lines = buffer.split('\n')
      // Keep the last (potentially incomplete) line in the buffer
      buffer = lines.pop() ?? ''

      for (const line of lines) {
        const trimmed = line.trim()
        if (!trimmed) continue
        // Trailer line: {"version":N}
        if (trimmed.startsWith('{')) {
          try {
            const trailer = JSON.parse(trimmed) as { version?: number }
            if (typeof trailer.version === 'number') version = trailer.version
          } catch {
            // ignore malformed trailer
          }
          continue
        }
        batch.push(trimmed)
        if (batch.length >= BATCH_SIZE) flushBatch()
      }
    }
    // Flush remaining buffer content
    if (buffer.trim() && !buffer.trim().startsWith('{')) {
      batch.push(buffer.trim())
    }
    flushBatch()
  } catch (e) {
    self.postMessage({ type: 'error', message: String(e) })
    return
  }

  self.postMessage({ type: 'done', version })
}
