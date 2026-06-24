---
name: lok8s-service
description: >-
  Use when adding or editing a lok8s service — the per-service lok8s.yaml
  (build, live_update, multi-image components) and the root services.yaml, or
  when wiring a service's Tilt dev loop. Covers the build/live_update schema,
  the lok8s.Dockerfile→Dockerfile production swap, and the common shape mistakes.
---

# Authoring lok8s services

A lok8s project has a root `services.yaml` (the catalog) and one `lok8s.yaml` per
service (its build/dev-loop config). Scaffold both with `lo init service <name>`
— it writes a starter `lok8s.yaml`, registers the service in `services.yaml`, and
ensures the root `Tiltfile` is the canonical loader. Prefer that over writing
files by hand.

## The per-service `lok8s.yaml` (UNTYPED — no apiVersion/kind)

⚠️ Unlike every other lok8s spec, `lok8s.yaml` is a **bare object** — it has **no
`apiVersion`, `kind`, `metadata`, or `spec`.** Do not add them (a frequent
mistake — there is no `kind: Service`/`kind: App`). Top-level keys (validated by
`.lok8s/tilt/Tiltfile`): `build`, `components`, `ports`, `links`, `workloads`,
`tilt`. Exactly one of `build` (single image) **or** `components` (multi-image)
is required; setting both fails.

```yaml
# single-image service
build:
  context: .
  dockerfile: lok8s.Dockerfile          # dev Dockerfile (see production swap below)
  live_update:
    sync:
      - { local_path: ./src, remote_path: /app/src }   # ⚠️ remote_path, NOT container_path
    fall_back_on:
      files: [ ./package.json, ./go.mod ]              # changes here force a full rebuild
    run:
      - { cmd: "npm install", trigger: ./package.json }
ports:
  - { from: 3000, to: 3000 }            # both 'from' (host) and 'to' (container) required
tilt:
  labels: [ backend ]                   # Tilt UI grouping
  resource_deps: [ postgres ]
```

`build.*` other than the keys above is forwarded verbatim to Tilt's
`docker_build()` (`target`, `platform`, `secrets`, `ssh`, …). `build_args` is a
list of env-var **names** that must exist at build time.

## Multi-image services: `components` (mutually exclusive with `build`)

```yaml
components:
  - name: api                            # required, unique → image lok8s.local/api
    build: { context: ., dockerfile: api/lok8s.Dockerfile }
  - name: operator
    build: { context: ., dockerfile: operator/lok8s.Dockerfile }
```
Each component has its own `build` (+ optional `ports`/`links`/`workloads`);
manifests are matched by the label `lok8s.dev/name=<name>`.

## `services.yaml` (the catalog)

```yaml
registry:                                # endpoint/branch/tag envsubst-expanded
  endpoint: ${DOCKER_REGISTRY}           # remote pull source for build:false (cache mode)
  branch: ${DOCKER_PROJECT}              # path segment between endpoint and service name
  tag: ${DOCKER_TAG}
  prefix: lok8s.local                    # canonical local image name
defaults: { build: true, dockerfile: service }
services:
  api:
    path: ./api                          # default ./<name>
    namespace: api                       # inject ns into manifests
    dockerfile: production               # service (dev) | production
  docs: { image: ghcr.io/org/docs:latest }   # pin a ref (implies build:false; excludes registry:)
```
`parallel` is **top-level only** (under `registry:`), not per-service.

## The `lok8s.Dockerfile` → `Dockerfile` production swap

`lok8s.yaml` declares the **dev** Dockerfile (by convention `lok8s.Dockerfile` —
hot-reload dev server). Production is selected via `dockerfile: production` in
`services.yaml` (per-service or `defaults`): lok8s does a literal substring
replace `lok8s.Dockerfile` → `Dockerfile` on the **`build.dockerfile` field
only** (so `api/lok8s.Dockerfile` → `api/Dockerfile`). If the prod file is
missing it warns and falls back to the dev one. `lok8s.<name>` ↔ `<name>` (e.g.
`lok8s.entrypoint.sh` ↔ `entrypoint.sh`) is the documented file-pair convention,
but lok8s only auto-swaps the dockerfile path — pair any other dev/prod files
yourself inside the Dockerfile.

## ⚠️ Common mistakes
- Adding `apiVersion`/`kind`/`spec` to `lok8s.yaml` — it's an untyped object.
- `live_update.sync` using `container_path` — the key is **`remote_path`**.
- A `Tiltfile` that hardcodes `docker_build()`/`k8s_yaml()` instead of the
  canonical `load('./.lok8s/tilt/Tiltfile', 'lok8s'); lok8s()` — the service
  config is then ignored. `lo init service` writes the loader; `lo lint` warns.
- Missing the `lok8s.dev/name` label on a service's manifests → `lok8s()` can't
  match them (`lo lint` flags this).

## Verify
```bash
lo lint <domain>          # schema-checks services.yaml + every lok8s.yaml
lo env services           # prints the merged services catalog
lo up --ci                # headless build+deploy+wait (CI-safe), exits with real status
```
