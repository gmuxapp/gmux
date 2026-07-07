/**
 * Markdown → HTML rendering for the ACP conversation view (ADR 0021 slice #2).
 *
 * Assistant text (and thinking) is rendered as markdown with fenced-code
 * syntax highlighting. Two properties matter for a *streaming* renderer:
 *
 *   1. Safety. This is model output rendered as HTML. markdown-it is configured
 *      with `html: false`, so any raw HTML in the source is escaped, not passed
 *      through — no DOM sanitizer needed (and none would run in the node test
 *      env). markdown-it also validates link/image URLs by default, blocking
 *      `javascript:` and other dangerous schemes.
 *
 *   2. Streaming tolerance. During token streaming the input is almost always
 *      *incomplete* markdown: an unterminated ``` fence, a half-written `[link`,
 *      a dangling `*`. markdown-it is designed to degrade gracefully here — an
 *      unclosed fence renders as a (growing) code block, unmatched emphasis
 *      renders literally — so we can re-render on every delta without guarding
 *      against a parse throw. `render()` is wrapped defensively regardless.
 *
 * Highlighting uses highlight.js. Unknown/unspecified languages fall back to
 * plain (escaped) code — never a throw.
 */
import MarkdownIt from 'markdown-it'
import hljs from 'highlight.js/lib/common'

const md = new MarkdownIt({
  html: false, // escape raw HTML in model output — the safety boundary
  linkify: true, // autolink bare URLs
  breaks: false,
  highlight(code: string, lang: string): string {
    if (lang && hljs.getLanguage(lang)) {
      try {
        return hljs.highlight(code, { language: lang, ignoreIllegals: true }).value
      } catch {
        /* fall through to escaped plain text */
      }
    }
    // No language (or unknown): escape and return; markdown-it wraps it.
    return md.utils.escapeHtml(code)
  },
})

/** Render a markdown string to sanitized HTML. Never throws. */
export function renderMarkdown(src: string): string {
  if (!src) return ''
  try {
    return md.render(src)
  } catch {
    // Defensive: partial/streaming markdown shouldn't ever throw, but if it
    // does, degrade to escaped plain text rather than losing the message.
    return md.utils.escapeHtml(src)
  }
}
