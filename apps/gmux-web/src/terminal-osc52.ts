/**
 * Scans a Uint8Array for complete OSC 52 sequences, writes their payload
 * to the clipboard, and returns the data with those sequences stripped.
 *
 * ghostty-web has no parser-hook API, so we handle OSC 52 (terminal
 * clipboard writes) before passing data to the terminal.
 *
 * Handles both BEL-terminated (0x07) and ST-terminated (ESC \) sequences.
 * Cross-chunk sequences are NOT preserved — practically, pi always emits
 * them in a single chunk.
 */
export function interceptOsc52(data: Uint8Array): Uint8Array {
  const ESC = 0x1b
  const BRACKET = 0x5d // ]
  const BEL = 0x07
  const BACKSLASH = 0x5c

  // Fast path: no ESC in data
  let hasEsc = false
  for (let i = 0; i < data.length; i++) {
    if (data[i] === ESC) { hasEsc = true; break }
  }
  if (!hasEsc) return data

  const output: number[] = []
  let i = 0
  while (i < data.length) {
    if (data[i] === ESC && i + 1 < data.length && data[i + 1] === BRACKET) {
      const oscStart = i
      i += 2
      let body = ''
      while (i < data.length) {
        if (data[i] === BEL) { i++; break }
        if (data[i] === ESC && i + 1 < data.length && data[i + 1] === BACKSLASH) { i += 2; break }
        body += String.fromCharCode(data[i])
        i++
      }
      if (body.startsWith('52;')) {
        const rest = body.slice(3) // strip "52;"
        const semiIdx = rest.indexOf(';')
        if (semiIdx >= 0) {
          const payload = rest.slice(semiIdx + 1)
          if (payload !== '?') {
            try {
              const bytes = Uint8Array.from(atob(payload), c => c.charCodeAt(0))
              const text = new TextDecoder().decode(bytes)
              navigator.clipboard.writeText(text).catch(() => {})
            } catch { /* invalid base64; ignore */ }
          }
          // Consumed — do NOT push to output
          continue
        }
      }
      // Not OSC 52: push original bytes
      for (let j = oscStart; j < i; j++) output.push(data[j])
      continue
    }
    output.push(data[i])
    i++
  }
  // If nothing was stripped, return the original buffer
  if (output.length === data.length) return data
  return new Uint8Array(output)
}
