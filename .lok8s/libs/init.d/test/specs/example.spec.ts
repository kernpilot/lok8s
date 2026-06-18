import { test, expect } from '../fixtures/test'
import { ExamplePage } from '../pages/ExamplePage'

/**
 * Worked-example specs. DELETE these once you have your own — they only exist
 * to show the patterns the scaffold gives you:
 *
 *   - read URLs/flags from `config`/`caps`, never hardcode hosts;
 *   - gate environment-specific behaviour with `test.skip(!caps.x, 'reason')`;
 *   - drive services through page objects (ExamplePage) that extend BasePage.
 *
 * File-name convention used by playwright.config.ts projects:
 *   *.spec.ts        → runs unauthenticated (the `e2e` / `public` project)
 *   *.auth.spec.ts   → runs with the captured user storageState (`e2e-auth`)
 *   *.admin.spec.ts  → runs with the admin storageState (`admin`)
 */
test.describe('Example — public @smoke', () => {
  test('app origin responds', async ({ examplePage }) => {
    const res = await examplePage.goto(ExamplePage.routes.home)
    expect(res).not.toBeNull()
    expect(res!.status()).toBeLessThan(500)
  })

  test('app serves some content', async ({ examplePage }) => {
    await examplePage.gotoHome()
    const body = await examplePage.bodyText()
    expect(body.length).toBeGreaterThan(0)
  })

  test('API is reachable (gated example)', async ({ api, caps }) => {
    test.skip(!caps.canAuth && false, 'public health needs no auth — this is just a shape example')
    const res = await api.get('/api/health')
    // Many apps expose /api/health; if yours does not, point this at a real
    // public endpoint or delete the test.
    expect([200, 404]).toContain(res.status)
  })
})
