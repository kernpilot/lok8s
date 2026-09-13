import { defineConfig, devices } from '@playwright/test'
import os from 'node:os'

// Best-effort .env loading (dotenv is optional). Shell env always wins.
try {
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const dotenv = require('dotenv')
  dotenv.config()
  dotenv.config({ path: '.env.local', override: true })
}
catch { /* dotenv not installed — rely on the ambient environment */ }

// Importing the config resolves the env from LOK8S_TEST_DOMAIN / LOK8S_TEST_*_URL
// and (via utils/tls, pulled in transitively) trusts the local CA for native
// fetch. We read it here only to seed the project baseURL.
import { config } from './utils/config'
// Browser host resolution for a local dev cluster. The native-fetch side is
// handled by utils/resolver.ts; the browser needs Chromium's
// --host-resolver-rules. Empty unless LOK8S_TEST_LB_IP is set in dev.
import { hostResolverRule } from './utils/resolver'

const CI = !!process.env.CI

const _resolverRule = hostResolverRule()
const _chromiumArgs = _resolverRule ? [`--host-resolver-rules=${_resolverRule}`] : []

/**
 * Browser executable escape hatch. Playwright's bundled Chromium can't always be
 * fetched (offline / throttled CDN). Set LOK8S_TEST_CHROMIUM_PATH to a system
 * chromium (e.g. /usr/bin/chromium) to run without the bundled download.
 */
const _chromiumPath = process.env.LOK8S_TEST_CHROMIUM_PATH || undefined

/**
 * Playwright config for a lok8s project's integration suite.
 *
 * Domain parameterization: change LOK8S_TEST_DOMAIN (or per-service
 * LOK8S_TEST_*_URL) to point the SAME suite at dev / staging / prod. See
 * utils/config.ts.
 *
 * File-name → project convention:
 *   specs/*.spec.ts        → public (no session)        → `e2e`
 *   specs/*.auth.spec.ts   → user storageState          → `e2e-auth`
 *   specs/*.admin.spec.ts  → admin storageState         → `admin`
 *   <dir>/*.api.spec.ts    → API (depends on auth-setup) → `api`
 */
export default defineConfig({
  testDir: '.',
  testIgnore: ['**/node_modules/**', '**/dist/**', '**/test-results/**', '**/playwright-report/**'],
  fullyParallel: true,
  forbidOnly: CI,
  retries: CI ? 2 : 0,
  workers: CI ? Math.max(1, Math.floor(os.cpus().length / 2)) : undefined,
  timeout: 30_000,

  reporter: CI
    ? [
        ['list'],
        ['html', { outputFolder: 'test-results/html-report', open: 'never' }],
        ['junit', { outputFile: 'test-results/junit.xml' }],
        ['json', { outputFile: 'test-results/results.json' }],
        ['github'],
      ]
    : [
        ['list'],
        ['html', { outputFolder: 'test-results/html-report', open: 'on-failure' }],
      ],

  use: {
    baseURL: config.urls.app,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    ignoreHTTPSErrors: true, // self-signed certs on a local dev plane
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    // NB: do NOT set a global `extraHTTPHeaders: { Accept: 'application/json' }`
    // here — it is inherited by BROWSER navigations too and can break apps that
    // content-negotiate their HTML (e.g. an OIDC/PKCE login page returning JSON).
    // The API client / Playwright `request` fixture set their own JSON Accept.
    launchOptions: { args: _chromiumArgs, executablePath: _chromiumPath },
  },

  expect: { timeout: 10_000 },
  outputDir: 'test-results/artifacts',

  metadata: {
    env: config.env,
    domain: config.domain,
    urls: config.urls,
    capabilities: config.capabilities,
  },

  projects: [
    { name: 'global-setup', testMatch: /setup\/global\.setup\.ts/, timeout: 120_000 },
    {
      name: 'auth-setup',
      testMatch: /setup\/auth\.setup\.ts/,
      timeout: 120_000,
      dependencies: ['global-setup'],
      use: { ...devices['Desktop Chrome'] },
    },

    // API specs (public ones need no browser; authed ones read the captured
    // token from auth-setup, which is a no-op when canAuth is false).
    {
      name: 'api',
      testMatch: /.*\.api\.spec\.ts/,
      dependencies: ['auth-setup'],
      use: { ...devices['Desktop Chrome'] },
    },

    // Public browser specs (no storageState).
    {
      name: 'e2e',
      testMatch: /specs\/.*\.spec\.ts/,
      testIgnore: [/.*\.api\.spec\.ts/, /.*\.auth\.spec\.ts/, /.*\.admin\.spec\.ts/],
      dependencies: ['global-setup'],
      use: { ...devices['Desktop Chrome'] },
    },

    // Authenticated browser specs (user storageState from auth-setup).
    {
      name: 'e2e-auth',
      testMatch: /.*\.auth\.spec\.ts/,
      testIgnore: /.*\.admin\.spec\.ts/,
      dependencies: ['auth-setup'],
      use: { ...devices['Desktop Chrome'], storageState: 'test-results/.auth/user.json' },
    },

    // Admin-only browser specs (admin storageState).
    {
      name: 'admin',
      testMatch: /.*\.admin\.spec\.ts/,
      dependencies: ['auth-setup'],
      use: { ...devices['Desktop Chrome'], storageState: 'test-results/.auth/admin.json' },
    },
  ],
})
