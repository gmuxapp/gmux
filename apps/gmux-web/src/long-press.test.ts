import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { createLongPressRecognizer, LONG_PRESS_MS } from './long-press'

describe('createLongPressRecognizer', () => {
  beforeEach(() => { vi.useFakeTimers() })
  afterEach(() => { vi.useRealTimers() })

  test('fires after the hold duration with the start coordinates', () => {
    const onLongPress = vi.fn()
    const rec = createLongPressRecognizer(onLongPress)

    rec.start(10, 20)
    vi.advanceTimersByTime(LONG_PRESS_MS - 1)
    expect(onLongPress).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)
    expect(onLongPress).toHaveBeenCalledWith(10, 20)
    expect(rec.end()).toBe(true)
  })

  test('end() before the timer fires is a tap', () => {
    const onLongPress = vi.fn()
    const rec = createLongPressRecognizer(onLongPress)

    rec.start(0, 0)
    vi.advanceTimersByTime(100)
    expect(rec.end()).toBe(false)
    // Timer was disarmed; nothing fires later.
    vi.runAllTimers()
    expect(onLongPress).not.toHaveBeenCalled()
  })

  test('cancel() disarms a pending press', () => {
    const onLongPress = vi.fn()
    const rec = createLongPressRecognizer(onLongPress)

    rec.start(0, 0)
    rec.cancel()
    vi.runAllTimers()
    expect(onLongPress).not.toHaveBeenCalled()
    expect(rec.end()).toBe(false)
  })

  test('a fired press stays fired through cancel()', () => {
    const rec = createLongPressRecognizer(() => { /* noop */ })

    rec.start(0, 0)
    vi.runAllTimers()
    rec.cancel() // finger moved after the sheet opened
    expect(rec.end()).toBe(true)
  })

  test('end() resets state for the next press', () => {
    const rec = createLongPressRecognizer(() => { /* noop */ })

    rec.start(0, 0)
    vi.runAllTimers()
    expect(rec.end()).toBe(true)
    expect(rec.end()).toBe(false)
  })

  test('start() resets a prior fired press and re-arms', () => {
    const onLongPress = vi.fn()
    const rec = createLongPressRecognizer(onLongPress)

    rec.start(0, 0)
    vi.runAllTimers()
    rec.start(5, 5) // new touch before end() — e.g. touchcancel path
    expect(rec.end()).toBe(false)
    expect(onLongPress).toHaveBeenCalledTimes(1)
  })
})
