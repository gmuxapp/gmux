import { test, expect } from '@playwright/test'
import { openApp } from '../helpers'

test.describe('no external network dependencies', () => {
  test('page loads without any external network requests', async ({ page }) => {
    const externalRequests: string[] = []

    // Block all external network traffic and record attempts
    await page.route(
      (url) => {
        const hostname = new URL(url.toString()).hostname
        return hostname !== '127.0.0.1' && hostname !== 'localhost'
      },
      (route) => {
        externalRequests.push(route.request().url())
        return route.abort('blockedbyclient')
      },
    )

    await openApp(page)
    // Give the page time to load resources (fonts, scripts, etc.)
    await page.waitForLoadState('domcontentloaded')
    await page.waitForTimeout(3000)

    // No requests should have been made to external hosts
    expect(externalRequests, `unexpected external requests: ${externalRequests.join(', ')}`).toEqual(
      [],
    )
  })
})
