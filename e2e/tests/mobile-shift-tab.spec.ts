import { test, expect } from '@playwright/test'
import { gotoTestSession, openApp } from '../helpers'

test.describe('mobile Shift+Tab keyboard', () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true })

  test.beforeEach(async ({ page }) => {
    await page.addInitScript(() => {
      const terminalInputs: number[][] = []
      const originalSend = WebSocket.prototype.send
      WebSocket.prototype.send = function (data: string | ArrayBufferLike | Blob | ArrayBufferView) {
        // Terminal input is the binary WebSocket path; resize/control
        // messages are JSON strings and are intentionally excluded here.
        let input: number[] | undefined
        if (data instanceof ArrayBuffer) {
          input = [...new Uint8Array(data)]
        } else if (ArrayBuffer.isView(data)) {
          input = [...new Uint8Array(data.buffer, data.byteOffset, data.byteLength)]
        }
        if (input) terminalInputs.push(input)
        // Keep the shared harness shell pristine: observe the real browser
        // input send call but do not inject BackTab into the fixture shell.
        if (!(input?.length === 3 && input[0] === 0x1b && input[1] === 0x5b && input[2] === 0x5a)) {
          originalSend.call(this, data)
        }
      }
      ;(window as unknown as { __mobileTerminalInputs: number[][] }).__mobileTerminalInputs = terminalInputs
    })
    await openApp(page)
    await gotoTestSession(page)
  })

  test('clicks BackTab once with exact raw bytes and no side effects', async ({ page }) => {
    const backTab = page.getByRole('button', { name: 'Shift+Tab (BackTab)' })
    await expect(backTab).toBeVisible()

    await page.evaluate(() => {
      const textarea = document.querySelector('.xterm-helper-textarea') as HTMLTextAreaElement | null
      textarea?.focus()
      ;(window as unknown as { __mobileTerminalInputs: number[][] }).__mobileTerminalInputs.length = 0
    })
    const before = await page.evaluate(() => ({
      active: document.activeElement?.className ?? '',
      ctrl: document.querySelector('.mk-ctrl')?.getAttribute('aria-pressed'),
      alt: document.querySelector('.mk-alt')?.getAttribute('aria-pressed'),
    }))

    const box = await backTab.boundingBox()
    expect(box).not.toBeNull()
    await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2)
    await page.mouse.down()
    await page.waitForTimeout(700) // longer than every toolbar repeat delay
    await page.mouse.up()
    await page.waitForTimeout(100)

    const result = await page.evaluate(() => ({
      terminalInputs: (window as unknown as { __mobileTerminalInputs: number[][] }).__mobileTerminalInputs,
      active: document.activeElement?.className ?? '',
      ctrl: document.querySelector('.mk-ctrl')?.getAttribute('aria-pressed'),
      alt: document.querySelector('.mk-alt')?.getAttribute('aria-pressed'),
      menuOpen: document.querySelector('.sidebar-overlay.visible') !== null,
    }))
    expect(result.terminalInputs).toEqual([[0x1b, 0x5b, 0x5a]])
    expect(result.active).toBe(before.active)
    expect(result.ctrl).toBe(before.ctrl)
    expect(result.alt).toBe(before.alt)
    expect(result.menuOpen).toBe(false)
  })

  test('keeps responsive ordering, visibility, safe areas, and badge clear', async ({ page }) => {
    for (const [width, height, branch] of [
      [390, 844, 'portrait'],
      [700, 390, 'medium'],
      [844, 390, 'wide'],
      [320, 568, 'small'],
    ] as const) {
      await page.setViewportSize({ width, height })
      await page.waitForTimeout(100)
      const layout = await page.evaluate(() => {
        // The live shell session is not unread by default; force the existing
        // status state so the badge-vs-Esc regression is exercised too.
        document.querySelector('.mk-menu')?.classList.add('bg-waiting')
        const bar = document.querySelector('.mobile-bottom-bar') as HTMLElement
        const get = (selector: string) => {
          const el = document.querySelector(selector) as HTMLElement
          const r = el.getBoundingClientRect()
          return { x: r.x, y: r.y, right: r.right, bottom: r.bottom, width: r.width, display: getComputedStyle(el).display }
        }
        const barRect = bar.getBoundingClientRect()
        const menu = get('.mk-menu')
        const esc = get('.mk-esc')
        const escFace = get('.mk-esc .mkey-face')
        const stab = get('.mk-shift-tab')
        const tab = get('.mk-tab')
        const dotStyle = getComputedStyle(document.querySelector('.mk-menu')!, '::after')
        const dot = { x: menu.x + menu.width - Number.parseFloat(dotStyle.right) - Number.parseFloat(dotStyle.width), y: menu.y + Number.parseFloat(dotStyle.top), right: menu.x + menu.width - Number.parseFloat(dotStyle.right), bottom: menu.y + Number.parseFloat(dotStyle.top) + Number.parseFloat(dotStyle.height) }
        return { bar: { x: barRect.x, right: barRect.right }, menu, esc, escFace, stab, tab, dot, words: [get('.mk-wl'), get('.mk-wr')], columns: getComputedStyle(bar).gridTemplateColumns.split(' ').length }
      })
      expect(layout.bar.right).toBeLessThanOrEqual(width)
      expect(layout.bar.x).toBeGreaterThanOrEqual(0)
      if (branch === 'portrait' || branch === 'small') {
        expect(layout.esc.x).toBe(layout.menu.x)
        expect(layout.esc.y).toBeLessThan(layout.menu.y)
      } else {
        expect(layout.esc.x).toBeGreaterThan(layout.menu.x)
        expect(layout.esc.y).toBe(layout.menu.y)
      }
      expect(layout.stab.x).toBeGreaterThan(layout.esc.x)
      expect(layout.tab.x).toBeGreaterThan(layout.stab.x)
      expect(layout.dot.x).toBeGreaterThanOrEqual(layout.menu.x)
      expect(layout.dot.bottom).toBeLessThanOrEqual(layout.menu.bottom)
      expect(
        layout.dot.right <= layout.escFace.x || layout.dot.x >= layout.escFace.right ||
        layout.dot.bottom <= layout.escFace.y || layout.dot.y >= layout.escFace.bottom,
      ).toBe(true)
      if (branch === 'portrait' || branch === 'small') {
        expect(layout.columns).toBe(7)
      } else if (branch === 'medium') {
        expect(layout.columns).toBe(11)
        expect(layout.words[0].display).toBe('none')
        expect(layout.words[1].display).toBe('none')
      } else {
        expect(layout.columns).toBe(13)
        expect(layout.words[0].display).not.toBe('none')
        expect(layout.words[1].display).not.toBe('none')
      }
    }

    const safeAreaContract = await page.evaluate(() => {
      const rules: CSSRule[] = []
      const collect = (items: CSSRuleList) => {
        for (const rule of items) {
          rules.push(rule)
          if ('cssRules' in rule && rule.cssRules) collect(rule.cssRules)
        }
      }
      for (const sheet of document.styleSheets) {
        try { collect(sheet.cssRules) } catch { /* cross-origin sheets are irrelevant */ }
      }
      const rule = rules.find(candidate => candidate instanceof CSSStyleRule && candidate.selectorText === '.mobile-bottom-bar' && candidate.style.getPropertyValue('--mobile-safe-area-left')) as CSSStyleRule | undefined
      return {
        left: rule?.style.getPropertyValue('--mobile-safe-area-left'),
        right: rule?.style.getPropertyValue('--mobile-safe-area-right'),
      }
    })
    // Chromium cannot synthesize env(safe-area-inset-*) values, so pin the
    // production env-to-custom-property contract separately from the
    // synthetic geometry test below. Removing either env declaration fails.
    expect(safeAreaContract.left).toContain('env(safe-area-inset-left')
    expect(safeAreaContract.right).toContain('env(safe-area-inset-right')

    await page.addStyleTag({ content: '.mobile-bottom-bar { --mobile-safe-area-left: 24px; --mobile-safe-area-right: 28px; }' })
    const safe = await page.evaluate(() => {
      const bar = document.querySelector('.mobile-bottom-bar')!.getBoundingClientRect()
      const menu = document.querySelector('.mk-menu')!.getBoundingClientRect()
      const send = document.querySelector('.mk-send')!.getBoundingClientRect()
      return { bar: { x: bar.x, right: bar.right }, menu: { x: menu.x }, send: { right: send.right }, width: innerWidth }
    })
    expect(safe.menu.x).toBeGreaterThanOrEqual(24)
    expect(safe.send.right).toBeLessThanOrEqual(safe.width - 28)
  })
})
