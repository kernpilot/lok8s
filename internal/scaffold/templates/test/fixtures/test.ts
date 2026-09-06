/**
 * Central test fixtures. Import `{ test, expect }` from here in every spec.
 *
 * Injects, per test:
 *   - `config` / `caps` — the resolved env config + capability flags
 *   - `api`            — a typed ApiClient bound to config.urls.api (+ token)
 *   - page objects     — add your POMs here as you create them
 *
 * Capability gating: specs call `test.skip(!caps.hasMailpit, …)` /
 * `test.skip(!caps.canAuth, …)` so the SAME spec runs green on a complete
 * cluster and skips-with-reason where a feature is gated today.
 */

import { test as base, expect } from '@playwright/test'
import { config as resolvedConfig, caps as resolvedCaps, type EnvironmentConfig, type Capabilities } from '../utils/config'
import { ApiClient, apiClient } from '../utils/api-client'
import { ExamplePage } from '../pages/ExamplePage'

export interface TestFixtures {
  config: EnvironmentConfig
  caps: Capabilities
  api: ApiClient
  /** Worked-example POM — replace/extend with your own service page objects. */
  examplePage: ExamplePage
}

export const test = base.extend<TestFixtures>({
  // Worker-agnostic constants — provided as fixtures so specs read them by DI
  // rather than importing the module directly (keeps specs uniform + mockable).
  config: async ({}, use) => {
    await use(resolvedConfig)
  },
  caps: async ({}, use) => {
    await use(resolvedCaps)
  },

  api: async ({}, use) => {
    await use(apiClient())
  },

  examplePage: async ({ page }, use) => {
    await use(new ExamplePage(page))
  },
})

export { expect }

// Re-export the most-used helpers so specs can do a single import if preferred.
export { config, caps } from '../utils/config'
export { ApiClient } from '../utils/api-client'
