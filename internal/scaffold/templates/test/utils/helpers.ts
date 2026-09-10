/**
 * Small, reusable test helpers shared across services. Page-object-free.
 */

import type { Page } from '@playwright/test'

/** Sleep — prefer Playwright auto-waiting; use only when polling an external system. */
export const sleep = (ms: number): Promise<void> => new Promise(r => setTimeout(r, ms))

/**
 * Poll an async predicate until it returns true or the timeout elapses.
 * Throws with `message` on timeout.
 */
export async function waitFor(
  fn: () => Promise<boolean> | boolean,
  { timeout = 30_000, interval = 1_000, message = 'Condition not met' } = {},
): Promise<void> {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    if (await fn()) return
    await sleep(interval)
  }
  throw new Error(`${message} (after ${timeout}ms)`)
}

/**
 * Probe an HTTP(S) endpoint until it responds OK (or a custom predicate
 * passes). Used by the global setup to wait for services to come up.
 */
export async function waitForHttp(
  url: string,
  {
    timeout = 60_000,
    interval = 5_000,
    accept = (res: Response) => res.ok,
    perRequestTimeout = 5_000,
  }: {
    timeout?: number
    interval?: number
    accept?: (res: Response) => boolean | Promise<boolean>
    perRequestTimeout?: number
  } = {},
): Promise<boolean> {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    try {
      const res = await fetch(url, { signal: AbortSignal.timeout(perRequestTimeout) })
      if (await accept(res)) return true
    }
    catch {
      // not up yet
    }
    await sleep(interval)
  }
  return false
}

/** Timestamped full-page screenshot under test-results/screenshots. */
export async function screenshot(page: Page, name: string): Promise<void> {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-')
  await page.screenshot({ path: `test-results/screenshots/${name}-${stamp}.png`, fullPage: true })
}

/** Extract the bare apex domain (last two labels) from a URL or hostname. */
export function apexOf(urlOrHost: string): string {
  let host = urlOrHost
  try {
    host = new URL(urlOrHost).hostname
  }
  catch {
    // already a hostname
  }
  const parts = host.split('.')
  return parts.length > 2 ? parts.slice(-2).join('.') : host
}
