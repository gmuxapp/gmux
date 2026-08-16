/** Copy text to the clipboard, honestly: resolves false when it didn't
 * happen. The async API needs a secure context, and gmux is served
 * over plain http from peer hosts, so fall back to the textarea +
 * execCommand path there (same trick the terminal's OSC 52 handler
 * can't use — it has no user gesture to spend). */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      document.body.removeChild(ta)
      return ok
    } catch {
      return false
    }
  }
}
