---
name: lok8s-dev
description: >-
  Use when working the lok8s development loop or its tests — running a cluster
  with `lo up` + Tilt hot-reload, the dev-vs-prod Dockerfile split, headless/CI
  bring-up, and the Playwright integration suite scaffolded by `lo init test`
  (config/SERVICES, capability flags, spec→project naming). Also the framework's
  own bats tests.
---

# lok8s dev loop & testing

## 1. The inner loop

```bash
lo use <domain>     # set clusters/.active (or pass --domain on each call)
lo up               # kind cluster + registries + spec.bootstrap addons, then Tilt (backgrounded)
lo up --open-tilt   # ...and open the Tilt UI (port is hashed 10351-10499; or TILT_PORT / spec.tilt.port)
lo down             # tilt down + kind delete
lo clean [--all]    # down + remove cluster volumes (+ docker prune with --all)
```

`lo up` reconciles infra **every time**, then starts Tilt. Once up, Tilt watches
your sources and applies `live_update`:
- **`sync`** copies changed files into the running container (`{local_path, remote_path}`).
- **`fall_back_on.files`** forces a full image rebuild when those files change.

**Principle: don't manually restart/trigger a Tilt resource to pick up changes.**
If a change isn't hot-reloading, the fix is the service's `live_update` (add the
`sync` path, or list the right `fall_back_on.files`), not a manual restart — see
the `lok8s-service` skill. A correct live_update makes Tilt reload or fall back to
a rebuild on its own.

## 2. dev vs prod images

`lok8s.yaml` declares the **dev** Dockerfile (`lok8s.Dockerfile` by convention — a
hot-reload dev server). Production builds set `dockerfile: production` in
`services.yaml`, which swaps `lok8s.Dockerfile` → `Dockerfile` (literal substring
replace). So `lo up` runs the dev server with live_update; a production build uses
the sibling `Dockerfile`. (Details in `lok8s-service`.)

## 3. Headless / CI

`lo up` backgrounds interactive `tilt up` (returns immediately) — it does **not**
give a pass/fail. For CI or any non-TTY run:

```bash
lo up --ci [--timeout 600s]    # foreground `tilt ci`: build + deploy + wait-ready,
                               # exits with the REAL status. No browser, no TTY.
```

Debug the loop: `lo status <domain>` (cluster/nodes/builds/Tilt), `lo tilt status`
(`tilt doctor` — must report `Env: kind`). For failures, use the `lok8s-doctor`
skill.

## 4. Integration tests — `lo init test` (Playwright)

```bash
lo init test                      # scaffolds ./tests from the lok8s template
cd tests
npm install
npx playwright install chromium   # or set LOK8S_TEST_CHROMIUM_PATH
LOK8S_TEST_DOMAIN=<your-domain> npm test
```

A **domain-parameterized** suite: the same specs run against dev / staging / prod
by changing one env var. Layout & how it's wired:

- **`utils/config.ts` — the single source of truth.** Resolves the env from
  `LOK8S_TEST_DOMAIN` (or `LOK8S_TEST_BASE_URL`), merges `config/<env>.ts`, and
  exposes:
  - `config.urls.<role>` — one URL per service, derived `<label>.<domain>` from the
    **`SERVICES`** map at the top of the file. **Edit `SERVICES` for your stack.**
    Override one URL with `LOK8S_TEST_<ROLE>_URL`.
  - `caps` — capability flags (`canAuth`, `hasMailpit`, `autoCreateUsers`) so a spec
    passes on a full cluster and `test.skip`s (with a reason) where a feature isn't
    available. Override with `LOK8S_TEST_CAN_AUTH=true`, etc.
- **`config/{dev,staging,production}.ts`** — per-env presets (caps, test users, OIDC
  client id). No secrets committed — supply staging/prod creds via env.
- Helpers: **`tls.ts`** (trust the local mkcert CA for `fetch` on dev),
  **`resolver.ts`** (map `*.<domain>` → the gateway LB IP when DNS doesn't; set
  `LOK8S_TEST_LB_IP`), **`mailpit.ts`** (email assertions, gated on
  `caps.hasMailpit`), **`api-client.ts`** (typed HTTP client skeleton).
- **`fixtures/test.ts`** — `import { test, expect } from '../fixtures/test'`;
  injects `config` / `caps` / `api` / page objects.
- **`pages/BasePage.ts`** — base page-object (absolute, per-service nav); extend per
  service (`ExamplePage.ts`).
- **`setup/global.setup.ts`** — health gate + env/capability banner.
- **`setup/auth.setup.ts`** — logs in once → `storageState` reused by authed
  projects; captures a bearer for API specs. Gated on `caps.canAuth`.

**Spec name → Playwright project (session):**

| File pattern | Project | Session |
|--------------|---------|---------|
| `specs/*.spec.ts` | `e2e` | none (public) |
| `*.auth.spec.ts` | `e2e-auth` | user `storageState` |
| `*.admin.spec.ts` | `admin` | admin `storageState` |
| `*.api.spec.ts` | `api` | bearer from auth-setup |

Scripts: `npm test`, `test:ui`, `test:debug`, `test:headed`, `test:dev` /
`test:staging` / `test:prod` (prod runs only `@smoke`).

**Make it yours:** (1) edit the `SERVICES` map in `utils/config.ts`; (2) adjust the
health gate in `setup/global.setup.ts`. Re-running `lo init test` is safe — it
copies file-by-file and never deletes your additions (use `--force` into a
non-empty dir).

## 5. Testing the framework itself (contributors)

The lok8s codebase uses bats (not Playwright):
```bash
./.bin/argsh test tests/unit/ tests/operator/   # the lo CLI / operator
./.bin/argsh lint '.lok8s/**/*.sh'              # shellcheck + argsh-lint
```
