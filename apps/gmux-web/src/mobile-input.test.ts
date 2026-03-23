import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { attachMobileInputHandler } from './mobile-input'

/** Minimal fake textarea for testing. */
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
    /** Simulate a beforeinput event. Returns whether the event was intercepted. */
    dispatchBeforeInput(inputType: string, data: string | null, dataTransfer?: DataTransfer | null) {
      let defaultPrevented = false
      let propagationStopped = false
      const event = {
        type: 'beforeinput',
        inputType,
        data,
        dataTransfer: dataTransfer ?? null,
        preventDefault() { defaultPrevented = true },
        stopImmediatePropagation() { propagationStopped = true },
      } as unknown as InputEvent
      for (const fn of listeners.get('beforeinput') ?? []) {
        fn(event)
      }
      return { defaultPrevented, propagationStopped }
    },
    _listeners: listeners,
  }
}

function createFakeTerminal(textarea: ReturnType<typeof createFakeTextarea>) {
  return { textarea } as any
}

describe('attachMobileInputHandler', () => {
  let textarea: ReturnType<typeof createFakeTextarea>
  let sent: string
  let send: (data: string) => void
  let dispose: () => void

  beforeEach(() => {
    textarea = createFakeTextarea()
    sent = ''
    send = (data) => { sent += data }
    dispose = attachMobileInputHandler(createFakeTerminal(textarea), send)
  })

  afterEach(() => {
    dispose()
  })

  it('ignores non-replacement input types', () => {
    textarea.value = 'hello'
    textarea.selectionStart = 5
    textarea.selectionEnd = 5
    const { defaultPrevented } = textarea.dispatchBeforeInput('insertText', 'x')
    expect(defaultPrevented).toBe(false)
    expect(sent).toBe('')
  })

  it('replaces word at start with suffix preserved', () => {
    // User typed "helo ", iOS selects "helo" for autocorrect
    textarea.value = 'helo '
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    textarea.dispatchBeforeInput('insertReplacementText', 'hello')

    // 5 backspaces (erase "helo ") + "hello" + " " (re-send suffix)
    expect(sent).toBe('\x7f'.repeat(5) + 'hello ')
    expect(textarea.value).toBe('hello ')
    expect(textarea.selectionStart).toBe(5)
  })

  it('replaces word at end of input (no suffix)', () => {
    textarea.value = 'wrld'
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    textarea.dispatchBeforeInput('insertReplacementText', 'world')

    expect(sent).toBe('\x7f'.repeat(4) + 'world')
    expect(textarea.value).toBe('world')
  })

  it('replaces word in the middle, preserving prefix and suffix', () => {
    // "the teh quick" -> replace "teh" (positions 4-7) with "the"
    textarea.value = 'the teh quick'
    textarea.selectionStart = 4
    textarea.selectionEnd = 7

    textarea.dispatchBeforeInput('insertReplacementText', 'the')

    // Erase from position 4 to end (9 chars: "teh quick"), then
    // re-send replacement + suffix: "the" + " quick"
    expect(sent).toBe('\x7f'.repeat(9) + 'the quick')
    expect(textarea.value).toBe('the the quick')
    expect(textarea.selectionStart).toBe(7) // cursor after replacement "the"
  })

  it('falls back to dataTransfer when ev.data is null', () => {
    textarea.value = 'tset'
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    const fakeTransfer = { getData: (type: string) => type === 'text/plain' ? 'test' : '' }
    textarea.dispatchBeforeInput('insertReplacementText', null, fakeTransfer as any)

    expect(sent).toBe('\x7f'.repeat(4) + 'test')
  })

  it('does nothing when selection is collapsed (not a replacement)', () => {
    textarea.value = 'hello'
    textarea.selectionStart = 5
    textarea.selectionEnd = 5

    const { defaultPrevented } = textarea.dispatchBeforeInput('insertReplacementText', 'world')
    expect(defaultPrevented).toBe(false)
    expect(sent).toBe('')
  })

  it('does nothing when replacement text is empty', () => {
    textarea.value = 'hello'
    textarea.selectionStart = 0
    textarea.selectionEnd = 5

    const { defaultPrevented } = textarea.dispatchBeforeInput('insertReplacementText', '')
    expect(defaultPrevented).toBe(false)
    expect(sent).toBe('')
  })

  it('cleanup removes the listener', () => {
    dispose()
    textarea.value = 'test'
    textarea.selectionStart = 0
    textarea.selectionEnd = 4

    const { defaultPrevented } = textarea.dispatchBeforeInput('insertReplacementText', 'fixed')
    expect(defaultPrevented).toBe(false)
    expect(sent).toBe('')
  })

  it('returns noop when terminal has no textarea', () => {
    const d = attachMobileInputHandler({ textarea: null } as any, send)
    d() // should not throw
  })
})
