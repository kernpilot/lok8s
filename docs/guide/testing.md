# Testing

lok8s ships a reusable, domain-parameterized [Playwright](https://playwright.dev)
integration-test scaffold. Run `lo init test` and you get a `tests/` suite that
exercises your stack end-to-end (public pages, API, browser login, email) and
runs the **same specs** against your dev cluster, staging, and production by
changing one environment variable.

## Scaffold a suite

```bash
lo init test            # scaffolds ./tests (use --path to change)
cd tests
npm install
npx playwright install chromium      # or set LOK8S_TEST_CHROMIUM_PATH=/usr/bin/chromium
LOK8S_TEST_DOMAIN=example.dev npm test
```

`lo init test` is idempotent: it refuses to overwrite a non-empty `tests/`
unless you pass `--force` (and even then copies file-by-file, so your own files
survive).

## What you get

A small, self-contained suite whose **generic layer** is the scaffold and whose
**specs + page objects** are yours to write:

```
tests/
  utils/
    config.ts        # single source of truth: env detection + URL map + caps
    tls.ts           # trust a local CA (mkcert) for native fetch (dev only)
    resolver.ts      # map *.<domain> -> a local LB IP for browser + node (dev)
    mailpit.ts       # poll/read/clear a Mailpit dev mailbox
    api-client.ts    # typed HTTP client skeleton
    oidc.ts          # OIDC discovery + token resolution
    helpers.ts
  fixtures/test.ts   # import { test, expect } from here
  pages/BasePage.ts  # base page object (extend per service)
  pages/ExamplePage.ts
  config/dev.ts | staging.ts | production.ts   # per-env presets
  setup/global.setup.ts    # health gate + capability banner
  setup/auth.setup.ts      # log in once -> storageState (+ captured bearer)
  specs/example.spec.ts    # worked example (delete once you have your own)
  playwright.config.ts
```

## Domain parameterization

The whole point: **never hardcode a hostname**. Read `config.urls.<role>` and
capability flags instead.

```ts
import { test, expect } from '../fixtures/test'

test('homepage serves', async ({ page, config }) => {
  await page.goto(config.urls.website)        // not "https://example.dev"
  await expect(page).toHaveTitle(/my app/i)
})
```

`utils/config.ts` resolves everything from `LOK8S_TEST_DOMAIN`:

1. **Service URLs** are derived from the **`SERVICES`** map (`app` → `app.<domain>`,
   `''` → the apex). Edit that map for your stack; override any single URL with
   `LOK8S_TEST_<ROLE>_URL`.
2. **The environment** (`dev` / `staging` / `production`) is detected from the
   domain (`.dev` ⇒ dev) and selects the matching `config/<env>.ts` preset.

Point the suite anywhere with no code change:

```bash
LOK8S_TEST_DOMAIN=example.dev            npm test   # local kind cluster
LOK8S_TEST_DOMAIN=staging.example.com    npm test   # staging
LOK8S_TEST_DOMAIN=example.com LOK8S_TEST_ENV=production npm test
```

## Capability flags

A capability flag lets the **same spec** pass on a complete cluster and
`test.skip` with a reason where a feature isn't available yet:

```ts
test('login journey', async ({ page, caps, config }) => {
  test.skip(!caps.canAuth, 'auth not wired up in this environment yet')
  // ...
})
```

Flags (`config/<env>.ts`, overridable by env):

| Flag | Env override | Meaning |
|---|---|---|
| `canAuth` | `LOK8S_TEST_CAN_AUTH` | login + bearer-protected calls are exercisable |
| `hasMailpit` | `LOK8S_TEST_HAS_MAILPIT` | a Mailpit dev mailbox is reachable |
| `autoCreateUsers` | `LOK8S_TEST_AUTO_CREATE_USERS` | the suite may register throwaway users |

## Spec naming → project

`playwright.config.ts` routes specs to projects by file name:

| Pattern | Project | Session |
|---|---|---|
| `specs/*.spec.ts` | `e2e` | none (public) |
| `*.auth.spec.ts` | `e2e-auth` | user `storageState` from `auth.setup.ts` |
| `*.admin.spec.ts` | `admin` | admin `storageState` |
| `*.api.spec.ts` | `api` | bearer captured by `auth.setup.ts` |

`setup/auth.setup.ts` logs in **once** and saves the session as a `storageState`
file; the authed projects reuse it so each authed spec starts already logged in.
It's gated on `caps.canAuth` — when login isn't usable it writes an empty state
and the authed specs skip cleanly.

## Local-cluster networking

On a kind/dev cluster the gateway often answers on a private LB IP and
`*.<domain>` may not resolve, behind a self-signed (mkcert) cert. The scaffold
handles both, for native `fetch` and the browser:

```bash
LOK8S_TEST_LB_IP=10.0.0.5 \
LOK8S_TEST_CA="$(mkcert -CAROOT)/rootCA.pem" \
LOK8S_TEST_DOMAIN=example.dev npm test
```

- `utils/resolver.ts` feeds Chromium `--host-resolver-rules` so the browser maps
  `*.<domain>` → `LOK8S_TEST_LB_IP` (and forces IPv4, dodging stale AAAA records).
- `utils/tls.ts` points `NODE_EXTRA_CA_CERTS` at your local CA (mkcert by default)
  so native `fetch` trusts the dev certs.

Both are no-ops outside `dev`, so production runs against real DNS + public certs
unchanged.

## CI

`playwright.config.ts` switches reporters when `CI` is set (JUnit + HTML + JSON +
GitHub annotations) and enables retries. Run it in a job that can reach the
target cluster:

```yaml
- run: cd tests && npm ci && npx playwright install --with-deps chromium
- run: cd tests && LOK8S_TEST_DOMAIN=${{ vars.TEST_DOMAIN }} npm test
  env:
    CI: 'true'
```
