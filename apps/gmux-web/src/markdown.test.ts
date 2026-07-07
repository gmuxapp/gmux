import { describe, it, expect } from 'vitest'
import { renderMarkdown } from './markdown'

describe('renderMarkdown', () => {
  it('renders basic markdown', () => {
    expect(renderMarkdown('**b**')).toContain('<strong>b</strong>')
    expect(renderMarkdown('- a\n- b')).toContain('<li>a</li>')
  })

  it('highlights fenced code with a known language', () => {
    const html = renderMarkdown('```js\nconst x = 1\n```')
    expect(html).toContain('<pre>')
    expect(html).toContain('hljs-keyword') // `const`
  })

  it('escapes code without a language rather than throwing', () => {
    const html = renderMarkdown('```\n<not html>\n```')
    expect(html).toContain('&lt;not html&gt;')
  })

  it('escapes raw HTML in the source (safety boundary)', () => {
    const html = renderMarkdown('<script>alert(1)</script>')
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('blocks dangerous link schemes', () => {
    // markdown-it's default validateLink rejects javascript:, so no href is emitted.
    const html = renderMarkdown('[click](javascript:alert)')
    expect(html).not.toContain('href="javascript:')
    // a normal link still renders
    expect(renderMarkdown('[ok](https://example.com)')).toContain('href="https://example.com"')
  })

  it('does not throw on incomplete streaming markdown', () => {
    // The shapes a token stream produces mid-flight.
    for (const partial of ['```js\nconst x =', 'a [link', '**bold', '> quote\n> mo', '| a | b\n| -']) {
      expect(() => renderMarkdown(partial)).not.toThrow()
    }
  })

  it('returns empty string for empty input', () => {
    expect(renderMarkdown('')).toBe('')
  })
})
