# Integration tests (Playwright)

Scaffolded by `lo init test`. A domain-parameterized, multi-service Playwright
suite that runs the SAME specs against your dev cluster, staging, and production
by changing one env var.

## Quick start

```bash
cd tests
npm install
npx playwright install chromium     # or set LOK8S_TEST_CHROMIUM_PATH=/usr/bin/chromium
LOK8S_TEST_DOMAIN=example.dev npm test
```

## How it's wired

- **`utils/config.ts`** — the single source of truth. Resolves the environment
  from `LOK8S_TEST_DOMAIN` (or `LOK8S_TEST_BASE_URL`), merges the matching
  `config/<env>.ts` preset, and exposes:
  - `config.urls.<role>` — a URL per service, derived as `<label>.<domain>` from
    the **`SERVICES`** map at the top of the file. **Edit `SERVICES`** to match
    your stack. Override any single URL with `LOK8S_TEST_<ROLE>_URL`.
  - `caps` — capability flags (`canAuth`, `hasMailpit`, `autoCreateUsers`) so the
    same spec passes on a full cluster and `test.skip`s with a reason where a
    feature isn't available yet. Override with `LOK8S_TEST_CAN_AUTH=true` etc.
- **`config/{dev,staging,production}.ts`** — per-env presets (capabilities, test
  users, OIDC client id). No secrets checked in — supply staging/prod creds via
  env.
- **`utils/tls.ts`** — trusts a local CA (mkcert by default) for native `fetch`
  on a dev cluster. No-op in non-dev.
- **`utils/resolver.ts`** — maps `*.<domain>` to a local gateway LB IP for the
  browser (and, opt-in, for Node) when DNS doesn't. Set `LOK8S_TEST_LB_IP`.
- **`utils/mailpit.ts`** — poll/read/clear a Mailpit dev mailbox for email
  assertions (gated by `caps.hasMailpit`).
- **`utils/api-client.ts`** — a typed HTTP client skeleton; add a method per
  endpoint you test.
- **`fixtures/test.ts`** — `import { test, expect }` from here; injects
  `config` / `caps` / `api` / your page objects.
- **`pages/BasePage.ts`** — base POM (absolute, per-service navigation). Extend
  it per service (see `pages/ExamplePage.ts`).
- **`setup/global.setup.ts`** — health gate + env/capability banner.
- **`setup/auth.setup.ts`** — logs in once and writes the `storageState` the
  authed projects reuse; captures a bearer for API specs. Gated on `caps.canAuth`.

## Spec naming → project

`playwright.config.ts` selects specs by file name:

| Pattern | Project | Session |
|---|---|---|
| `specs/*.spec.ts` | `e2e` | none (public) |
| `*.auth.spec.ts` | `e2e-auth` | user `storageState` |
| `*.admin.spec.ts` | `admin` | admin `storageState` |
| `*.api.spec.ts` | `api` | bearer from auth-setup |

## Make it yours

1. Edit the **`SERVICES`** map in `utils/config.ts` for your services.
2. Adjust the health gate in `setup/global.setup.ts`.
3. Replace `setup/auth.setup.ts`'s `loginViaUi()` with your real login (ideally a
   page object).
4. Replace `pages/ExamplePage.ts` + `specs/example.spec.ts` with your own page
   objects and specs. Delete the examples.
5. Flip capability flags in `config/dev.ts` as features come online.

Point the suite at another environment with no code change:
`LOK8S_TEST_DOMAIN=staging.example.com npm test`.
