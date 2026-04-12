import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { attachMobileInputHandler } from './mobile-input'

// ── Test helpers ──

/** Minimal fake textarea. */
function createFakeTextarea() {
  let value = ''
  let selectionStart = 0
  let selectionEnd = 0
  const listeners = new Map<string, Set<EventListener>>()

  return {
    get value() { return value },
    set value(v: string) { value = v },
    get selectionStart() { return selectionStart },
    set selectionStart(v: number) { selectionStart = v },
    get selectionEnd() { return selectionEnd },
    set selectionEnd(v: number) { selectionEnd = v },
    addEventListener(type: string, fn: EventListener, _opts?: any) {
      if (!listeners.has(type)) listeners.set(type, new Set())
      listeners.get(type)!.add(fn)
    },
    removeEventListener(type: string, fn: EventListener, _opts?: any) {
      listeners.get(type)?.delete(fn)
    },
    dispatch(type: string, props: Record<string, any> = {}) {
      let defaultPrevented = false
      let immediateStopped = false
      const event = {
        type,
        ...props,
        preventDefault() { defaultPrevented = true },
        stopImmediatePropagation() { immediateStopped = true },
      }
      for (const fn of listeners.get(type) ?? []) {
        if (immediateStopped) break
        fn(event as any)
      }
      return { defaultPrevented, immediateStopped }
    },
  }
}

function createFakeContainer() {
  const listeners = new Map<string, Set<EventListener>>()
  return {
    addEventListener(type: string, fn: EventListener, _opts?: any) {
      if (!listeners.has(type)) listeners.set(type, new Set())
      listeners.get(type)!.add(fn)
    },
    removeEventListener(type: string, fn: EventListener, _opts?: any) {
      listeners.get(type)?.delete(fn)
    },
    dispatch(type: string, props: Record<string, any> = {}) {
      let immediateStopped = false
      const event = {
        type,
        ...props,
        stopImmediatePropagation() { immediateStopped = true },
      }
      for (const fn of listeners.get(type) ?? []) {
        if (immediateStopped) break
        fn(event as any)
      }
      return { immediateStopped }
    },
  }
}

/**
 * Simulate the browser event flow for an input event:
 * 1. beforeinput fires on textarea
 * 2. browser applies the change to textarea.value
 * 3. input fires on container (capture, parent-first) then textarea
 *
 * Returns whether the container stopped propagation (meaning xterm's
 * handler on the textarea would NOT have fired).
 */
function simulateInput(
  textarea: ReturnType<typeof createFakeTextarea>,
  container: ReturnType<typeof createFakeContainer>,
  inputType: string,
  data: string,
  dataTransfer?: any,
): { stoppedBeforeXterm: boolean } {
  // Phase 1: beforeinput on textarea
  textarea.dispatch('beforeinput', { inputType, data, dataTransfer: dataTransfer ?? null })

  // Browser applies the change
  const start = textarea.selectionStart
  const end = textarea.selectionEnd
  if (data) {
    textarea.value = textarea.value.substring(0, start) + data + textarea.value.substring(end)
    textarea.selectionStart = textarea.selectionEnd = start + data.length
  }

  // Phase 2: input propagates container (capture) → textarea
  const { immediateStopped } = container.dispatch('input', { inputType, data })
  if (!immediateStopped) {
    textarea.dispatch('input', { inputType, data })
  }

  return { stoppedBeforeXterm: immediateStopped }
}

/**
 * Simulate Android autocorrect: deleteContentBackward with non-collapsed
 * selection, immediately followed by insertText with collapsed selection.
 *
 * Returns whether the insertText's input event was stopped before xterm.
 */
function simulateAndroidAutocorrect(
  textarea: ReturnType<typeof createFakeTextarea>,
  container: ReturnType<typeof createFakeContainer>,
  data: string,
): { stoppedBeforeXterm: boolean } {
  // Phase 1a: beforeinput deleteContentBackward
  textarea.dispatch('beforeinput', {
    inputType: 'deleteContentBackward',
    data: null,
    dataTransfer: null,
  })

  // Browser applies the deletion
  const delStart = textarea.selectionStart
  const delEnd = textarea.selectionEnd
  textarea.value = textarea.value.substring(0, delStart) + textarea.value.substring(delEnd)
  textarea.selectionStart = textarea.selectionEnd = delStart

  // Phase 1b: input deleteContentBackward (container capture → textarea)
  const delResult = container.dispatch('input', { inputType: 'deleteContentBackward', data: null })
  if (!delResult.immediateStopped) {
    textarea.dispatch('input', { inputType: 'deleteContentBackward', data: null })
  }

  // Phase 2: the insertText half
  return simulateInput(textarea, container, 'insertText', data)
}

// ── Tests ──

describe('attachMobileInputHandler', () => {
  let textarea: ReturnType<typeof createFakeTextarea>
  let container: ReturnType<typeof createFakeContainer>
  let sent: string
  let send: (data: string) => void
  let dispose: () => void

  beforeEach(() => {
    textarea = createFakeTextarea()
    container = createFakeContainer()
    sent = ''
    send = (data) => { sent += data }
    dispose = attachMobileInputHandler(
      { textarea } as any,
      container as any,
      send,
    )
  })

  afterEach(() => {
    dispose()
  })

  // ── Normal typing (must not interfere) ──

  it('lets normal character appends propagate to xterm', () => {
    textarea.value = 'hel'
    textarea.selectionStart = 3
    textarea.selectionEnd = 3

    const { stoppedBeforeXterm } = simulateInput(textarea, container, 'insertText', 'l')

    expect(sent).toBe('')
    expect(stoppedBeforeXterm).toBe(false) // xterm's handler must fire
  })

  // ── iOS dictation (insertText with selection) ──

  it('replays the exact iOS Safari dictation trace', () => {
    // Trace from real iPhone (iOS 18.6, Safari 604.1):
    //   beforeinput insertText data="t"              selStart=0 selEnd=0   textarea=""
    //   beforeinput insertText data="test"           selStart=0 selEnd=1   textarea="t"
    //   beforeinput insertText data="testing test"   selStart=0 selEnd=4   textarea="test"
    //   beforeinput insertText data="testing testing" selStart=0 selEnd=12 textarea="testing test"

    // Step 1: "t" — plain append
    textarea.value = ''
    textarea.selectionStart = 0
    textarea.selectionEnd = 0
    let r = simulateInput(textarea, container, 'insertText', 't')
    expect(r.stoppedBeforeXterm).toBe(false)
    expect(sent).toBe('')

    // Step 2: replace "t" with "test"
    textarea.selectionStart = 0
    textarea.selectionEnd = 1
    r = simulateInput(textarea, container, 'insertText', 'test')
    expect(r.stoppedBeforeXterm).toBe(true)
    expect(sent).toBe('\x7f' + 'test')
    expect(textarea.value).toBe('test')

    sent = ''

    // Step 3: replace "test" with "testing test"
    textarea.selectionStart = 0
    textarea.selectionEnd = 4
    r = simulateInput(textarea, container, 'insertText', 'testing test')
    expect(r.stoppedBeforeXterm).toBe(true)
    expect(sent).toBe('\x7f'.repeat(4) + 'testing test')
    expect(textarea.value).toBe('testing test')

    sent = ''

    // Step 4: replace "testing test" with "testing testing"
    textarea.selectionStart = 0
    textarea.selectionEnd = 12
    r = simulateInput(textarea, container, 'insertText', 'testing testing')
    expect(r.stoppedBeforeXterm).toBe(true)
    expect(sent).toBe('\x7f'.repeat(12) + 'testing testing')
    expect(textarea.value).toBe('testing testing')
  })

  // ── Autocorrect (insertReplacementText) ──

  it('handles autocorrect with suffix after selection', () => {
    // "helo " → replace "helo" with "hello", space preserved
    textarea.value = 'helo '
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    simulateInput(textarea, container, 'insertReplacementText', 'hello')

    // 5 backspaces (erase "helo ") + "hello" + " " (suffix)
    expect(sent).toBe('\x7f'.repeat(5) + 'hello ')
  })

  it('handles autocorrect in the middle of a line', () => {
    // "the teh quick" → replace "teh" (positions 4-7) with "the"
    textarea.value = 'the teh quick'
    textarea.selectionStart = 4
    textarea.selectionEnd = 7

    simulateInput(textarea, container, 'insertReplacementText', 'the')

    // 9 backspaces (erase "teh quick") + "the" + " quick"
    expect(sent).toBe('\x7f'.repeat(9) + 'the quick')
    expect(textarea.value).toBe('the the quick')
  })

  it('handles autocorrect at end of input', () => {
    textarea.value = 'wrld'
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    simulateInput(textarea, container, 'insertReplacementText', 'world')

    expect(sent).toBe('\x7f'.repeat(4) + 'world')
  })

  // ── dataTransfer fallback (Safari spell-check) ──

  it('reads replacement text from dataTransfer when data is null', () => {
    textarea.value = 'tset'
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    const transfer = { getData: (t: string) => t === 'text/plain' ? 'test' : '' }
    // Pass null as data to exercise the fallback path
    textarea.dispatch('beforeinput', {
      inputType: 'insertReplacementText',
      data: null,
      dataTransfer: transfer,
    })
    // Manually apply the change (browser would do this)
    textarea.value = 'test'
    textarea.selectionStart = textarea.selectionEnd = 4
    container.dispatch('input', { inputType: 'insertReplacementText', data: null })

    expect(sent).toBe('\x7f'.repeat(4) + 'test')
  })

  // ── Android autocorrect (deleteContentBackward + insertText) ──

  it('handles Android autocorrect at end of line', () => {
    // Trace from real Android device (Chrome 146, GBoard):
    // User typed "lets", keyboard corrects to "let's "
    //   deleteContentBackward selStart=36 selEnd=37 (deletes "s")
    //   insertText data="'s " selStart=36 selEnd=36
    textarea.value = 'hello , let\'s autocorrect thists lets'
    textarea.selectionStart = 36
    textarea.selectionEnd = 37

    const { stoppedBeforeXterm } = simulateAndroidAutocorrect(textarea, container, "'s ")

    expect(stoppedBeforeXterm).toBe(true)
    // 1 backspace (erase from deleteStart=36 to end=37) + replacement + no suffix
    expect(sent).toBe('\x7f' + "'s ")
    // Textarea reset to pre-autocorrect value to neutralize _handleAnyTextareaChanges
    expect(textarea.value).toBe('hello , let\'s autocorrect thists lets')
  })

  it('handles Android autocorrect in the middle of text', () => {
    // "helo world" → correct "helo" to "hello"
    // delete "lo" (positions 2-4), insert "llo"
    textarea.value = 'helo world'
    textarea.selectionStart = 2
    textarea.selectionEnd = 4

    const { stoppedBeforeXterm } = simulateAndroidAutocorrect(textarea, container, 'llo')

    expect(stoppedBeforeXterm).toBe(true)
    // 8 backspaces (erase from deleteStart=2 to end=10: "lo world")
    // then replacement "llo" + suffix " world"
    expect(sent).toBe('\x7f'.repeat(8) + 'llo world')
    // Textarea reset to pre-autocorrect value
    expect(textarea.value).toBe('helo world')
  })

  it('does not treat collapsed backspace + typing as autocorrect', () => {
    // Non-collapsed delete sets tracking
    textarea.value = 'hello world'
    textarea.selectionStart = 5
    textarea.selectionEnd = 8
    textarea.dispatch('beforeinput', {
      inputType: 'deleteContentBackward',
      data: null,
      dataTransfer: null,
    })

    // Collapsed delete (normal backspace) should clear the stale tracking
    textarea.value = 'helloorld'
    textarea.selectionStart = 5
    textarea.selectionEnd = 5
    textarea.dispatch('beforeinput', {
      inputType: 'deleteContentBackward',
      data: null,
      dataTransfer: null,
    })

    // This insertText should pass through as a normal append, not autocorrect
    textarea.value = 'hellorld'
    textarea.selectionStart = 4
    textarea.selectionEnd = 4

    const { stoppedBeforeXterm } = simulateInput(textarea, container, 'insertText', 'o')

    expect(sent).toBe('')
    expect(stoppedBeforeXterm).toBe(false)
  })

  it('clears tracked deletion when a non-text event intervenes', () => {
    textarea.value = 'hello'
    textarea.selectionStart = 3
    textarea.selectionEnd = 5

    // deleteContentBackward with non-collapsed selection
    textarea.dispatch('beforeinput', {
      inputType: 'deleteContentBackward',
      data: null,
      dataTransfer: null,
    })

    // An unrelated event type intervenes, clearing the tracked deletion
    textarea.dispatch('beforeinput', {
      inputType: 'insertCompositionText',
      data: null,
      dataTransfer: null,
    })

    // The following insertText should be treated as a normal append
    textarea.value = 'hel'
    textarea.selectionStart = 3
    textarea.selectionEnd = 3

    const { stoppedBeforeXterm } = simulateInput(textarea, container, 'insertText', 'p')

    expect(sent).toBe('')
    expect(stoppedBeforeXterm).toBe(false)
  })

  it('handles successive Android autocorrects independently', () => {
    // First autocorrect: "teh" → "the"
    textarea.value = 'teh wrld'
    textarea.selectionStart = 0
    textarea.selectionEnd = 3

    let r = simulateAndroidAutocorrect(textarea, container, 'the')
    expect(r.stoppedBeforeXterm).toBe(true)
    expect(sent).toBe('\x7f'.repeat(8) + 'the wrld')
    expect(textarea.value).toBe('teh wrld') // reset

    sent = ''

    // Second autocorrect: "wrld" → "world" (on the reset textarea)
    textarea.value = 'the wrld'
    textarea.selectionStart = 4
    textarea.selectionEnd = 8

    r = simulateAndroidAutocorrect(textarea, container, 'world')
    expect(r.stoppedBeforeXterm).toBe(true)
    expect(sent).toBe('\x7f'.repeat(4) + 'world')
    expect(textarea.value).toBe('the wrld') // reset
  })

  // ── Edge cases ──

  it('ignores replacement with empty text', () => {
    textarea.value = 'hello'
    textarea.selectionStart = 0
    textarea.selectionEnd = 5

    const { stoppedBeforeXterm } = simulateInput(textarea, container, 'insertText', '')

    expect(sent).toBe('')
    expect(stoppedBeforeXterm).toBe(false)
  })

  it('ignores unhandled input types', () => {
    textarea.value = 'hello'
    textarea.selectionStart = 0
    textarea.selectionEnd = 5

    const { stoppedBeforeXterm } = simulateInput(textarea, container, 'insertLineBreak', '')

    expect(sent).toBe('')
    expect(stoppedBeforeXterm).toBe(false)
  })

  // ── Lifecycle ──

  it('cleanup removes both listeners', () => {
    dispose()

    textarea.value = 'test'
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    const { stoppedBeforeXterm } = simulateInput(textarea, container, 'insertText', 'fixed')

    expect(sent).toBe('')
    expect(stoppedBeforeXterm).toBe(false)
  })

  it('returns noop when terminal has no textarea', () => {
    const d = attachMobileInputHandler({ textarea: null } as any, container as any, send)
    d() // should not throw
  })
})
