import { test as setup, expect } from '@playwright/test'
import { config, caps } from '../utils/config'
import { waitForHttp } from '../utils/helpers'
import { isMailpitReachable } from '../utils/mailpit'

/**
 * Global setup project (runs before every other project as a dependency).
 *
 * 1. Waits for your primary service to report healthy — the gate for the whole
 *    run. Other services are probed best-effort and reported, not required, so
 *    one degraded service doesn't block the rest of the suite.
 * 2. Logs the resolved environment + capability matrix so CI output makes it
 *    obvious which env was hit and which gated areas were skipped.
 *
 * EDIT the hard gate below to your real health endpoint.
 */
setup('environment is ready', async () => {
  setup.setTimeout(120_000)

  /* eslint-disable no-console */
  console.log('\n──────────────── test environment ────────────────')
  console.log(`  env:        ${config.env}`)
  console.log(`  domain:     ${config.domain}`)
  for (const [role, url] of Object.entries(config.urls)) {
    console.log(`  ${role.padEnd(10)} ${url}`)
  }
  console.log(`  caps:       canAuth=${caps.canAuth} hasMailpit=${caps.hasMailpit} `
    + `autoCreateUsers=${caps.autoCreateUsers}`)
  console.log('───────────────────────────────────────────────────\n')

  // Hard gate: your API must be reachable. Adjust the URL + predicate.
  const apiUp = await waitForHttp(`${config.urls.api}/api/health`, {
    timeout: 90_000,
    interval: 5_000,
    accept: res => res.status < 500,
  })
  expect(apiUp, `API at ${config.urls.api} did not become reachable`).toBe(true)

  // Soft probes — report only.
  const appUp = await waitForHttp(config.urls.app, { timeout: 5_000, interval: 5_000, accept: r => r.status < 500 })
  const mailUp = caps.hasMailpit ? await isMailpitReachable() : false
  console.log(`  probes:     app=${appUp ? 'up' : 'down'} `
    + `mail=${caps.hasMailpit ? (mailUp ? 'up' : 'down') : 'n/a'}`)
  /* eslint-enable no-console */
})
