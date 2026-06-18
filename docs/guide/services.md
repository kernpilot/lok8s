# Services

`services.yaml` is the heart of the lok8s development workflow. It declares
which services exist in your project, where their source lives, and how they
should be built and deployed in local dev — and it ties them to a CI-built
Docker registry so large multi-service projects don't have to rebuild
everything on every developer's laptop.

This guide covers:

- The two configuration files: `services.yaml` and per-service `lok8s.yaml`
- The small-project workflow ("just build everything locally")
- The large-project workflow ("build only what I'm working on, pull the rest from CI")
- How `services.local.yaml` lets each developer override behavior without committing
- How per-service registry overrides let you mix CI builds from different PRs
- How `image:` pinning bypasses the build pipeline entirely

## File overview

| File | Location | Committed? | Purpose |
|------|----------|-----------|---------|
| `services.yaml` | repo root | ✅ Yes | Project-wide service catalog and registry config |
| `services.<config>.yaml` | repo root | depends | **Workflow profiles** — named combinations of services/flags loaded via `LOK8S_SERVICE_CONFIG=<config>` |
| `services.local.yaml` | repo root | ❌ Gitignored | Personal per-developer overrides (one specific workflow profile) |
| `lok8s.yaml` | per service dir | ✅ Yes | How Tilt should build & live-reload **this** service |

`services.yaml` is the **what**: which services exist, what they're called,
where their source is. `lok8s.yaml` is the **how**: Dockerfile path,
live-update sync rules, port forwards, Tilt resource config.

`services.<config>.yaml` files are **workflow profiles** — see the
[Workflow profiles](#workflow-profiles) section for the full pattern.

All of these are merged at runtime via yq's deep-merge strategy
(`. * $item`). An override only needs to specify the keys it wants to
change; everything else inherits from `services.yaml`. Resolution order:

1. `services.yaml` (base)
2. `services.${LOK8S_SERVICE_CONFIG}.yaml` if `LOK8S_SERVICE_CONFIG` is set
3. (later configs win on key conflicts)

## Quick start: small project (1–3 services)

If you have a handful of services and a beefy laptop, you don't need any of
the registry machinery. Just declare your services and build them all
locally:

```yaml
# services.yaml
services:
  api:
    path: ./api
  worker:
    path: ./worker
```

That's it. With `defaults.build` defaulting to `true`, every service builds
locally and gets Tilt live-update. Run `lo up` and start coding.

Each service directory needs a `lok8s.yaml` describing its build:

```yaml
# api/lok8s.yaml
build:
  context: .
  dockerfile: lok8s.Dockerfile
  live_update:
    sync:
      - local_path: .
        remote_path: /app
    fall_back_on:
      files:
        - package.json
        - bun.lock

ports:
  - from: 3000
    to: 3000
```

That's the entire surface for a small project. The next sections only matter
if you grow into a larger setup.

## The large-project workflow

In a 10-, 20-, or 50-service repo, building everything locally is no
longer realistic. A full cold build can take an hour. Instead, lok8s uses
a **two-tier model**:

1. **CI builds and pushes every service** to a Docker registry on every
   PR/branch/commit, tagged in a way that lets local devs pull a specific
   build.
2. **Local devs only build the services they're actively working on.**
   Everything else gets pulled from the registry as a pre-built image.

The flag that controls this is `build:` — per-service or per-defaults.

### Setting it up

In `services.yaml`, flip `defaults.build` to `false` and configure your
registry:

```yaml
# services.yaml
registry:
  endpoint: "${DOCKER_REGISTRY}"   # ghcr.io/myorg, etc.
  branch:   "${DOCKER_PROJECT}"    # the PR/branch slug your CI uses
  tag:      "${DOCKER_TAG}"        # commit SHA or build tag
  prefix:   lok8s.local            # canonical local image name (don't change)

defaults:
  build: false                     # NEW DEFAULT: don't build anything locally

services:
  api:        { path: ./api }
  worker:     { path: ./worker }
  dashboard:  { path: ./dashboard }
  scheduler:  { path: ./scheduler }
  notifier:   { path: ./notifier }
  # ...10 more...
```

`DOCKER_REGISTRY`, `DOCKER_PROJECT`, and `DOCKER_TAG` are typically exported
by `direnv` based on the current branch — for example, `DOCKER_PROJECT=$(git
branch --show-current)` and `DOCKER_TAG=$(git rev-parse HEAD)`. The exact
mechanism is up to your CI setup.

With this committed, **every developer who runs `lo up` pulls all services
from the registry by default.** No local builds. Fast cluster startup.

### Opting in to local development

When a developer wants to actively work on `api`, they create
`services.local.yaml` (gitignored):

```yaml
# services.local.yaml
services:
  api:
    build: true     # I'm developing this service — build locally
```

Then run `lo up` with `LOK8S_SERVICE_CONFIG=local` (or set it in your
direnv). The two files are merged: `api` builds locally with Tilt
live-update, the other 14 services pull from the registry.

This means:

- **Each developer has their own opt-in list.** No conflicts, no committed personal config.
- **Switching focus is one-line:** flip `build: true` for whatever you're working on, comment out the previous one.
- **CI doesn't care.** It builds everything regardless. Devs just consume.

## Workflow profiles

`services.local.yaml` is the simplest case of a more general pattern: any
file matching `services.<config>.yaml` is loaded when
`LOK8S_SERVICE_CONFIG=<config>` is set. You can use this to define **named
workflow profiles** — committed combinations of services that match how
people actually work.

Real-world combinations from a 10+ service project:

```yaml
# services.frontend.yaml — committed; use with LOK8S_SERVICE_CONFIG=frontend
services:
  api:        { build: true }
  dashboard:  { build: true }
  docs:       { build: true }
  # everything else stays on defaults.build: false
```

```yaml
# services.cluster.yaml — work on infra/cluster services only
services:
  ingress-controller:   { build: true }
  cert-manager-shim:    { build: true }
  observability-agent:  { build: true }
```

```yaml
# services.ci.yaml — committed; e2e test runs use LOK8S_SERVICE_CONFIG=ci
defaults:
  build: false             # CI never builds locally
services:
  api:
    image: ghcr.io/myorg/api:${E2E_TAG}     # exact image under test
  e2e-test-runner:
    enabled: true                            # CI-only service
    build: false
```

Pick a profile by exporting the env var (typically via direnv):

```bash
# In .envrc.local or shell
export LOK8S_SERVICE_CONFIG=frontend
lo up
```

Or one-shot:

```bash
LOK8S_SERVICE_CONFIG=cluster lo up
```

**What's the right thing to commit?**

| File | Commit? | Why |
|------|---------|-----|
| `services.yaml` | ✅ | The base catalog. Everyone needs the same view of "what services exist". |
| `services.local.yaml` | ❌ | Personal opt-ins. Goes in `.gitignore`. |
| `services.frontend.yaml`, `services.cluster.yaml`, etc. | ✅ | Shared workflow profiles. Onboarding new devs is `LOK8S_SERVICE_CONFIG=frontend lo up`. |
| `services.ci.yaml` | ✅ | CI-specific config. Set `LOK8S_SERVICE_CONFIG=ci` in your CI workflow. |

The merge is plain deep-merge, so a developer can layer
`services.local.yaml` on top of a committed profile to add their own
tweaks. Resolution order is: `services.yaml` → committed profile →
`services.local.yaml` last (if you have both, just be aware that the
machinery picks **one** `LOK8S_SERVICE_CONFIG` at a time — for
multi-layer overrides, see the upstream doc on the merge mechanics).

### Why default `build: true` then?

The framework default is `build: true` because **most projects start small**.
You shouldn't need to set up CI + a registry + per-service flags before you
can run your first service. The default favors greenfield projects.

When your project grows past the point where building everything is
practical, you flip `defaults.build` to `false` once and switch to opt-in
mode. The rest of the file doesn't change.

## Per-service registry overrides

Sometimes you need to mix builds from different PRs. For example: you're
working on `api` locally, and you want to test it against the version of
`worker` that lives in PR #1234.

Per-service `registry:` overrides let you do this without changing global
state:

```yaml
# services.local.yaml
services:
  api:
    build: true             # I'm developing api locally
  worker:
    registry:
      branch: pr-1234       # pull worker from PR #1234's CI build
      tag: "abc123def"      # specific commit SHA
```

The override only affects `worker`. Every other service continues to use
the global `registry:` config. You can override `endpoint`, `branch`, `tag`,
or `prefix` independently.

The resolution rule is: **per-service `registry.<key>` wins over global
`registry.<key>`, falling through key by key.** So if you only set
`registry.branch` per service, the global `endpoint` and `tag` are still
inherited.

## Pinning to a specific image

If you need to bypass the lok8s naming convention entirely — for example,
to use an image from a totally unrelated source — set `image:` on the
service:

```yaml
# services.local.yaml
services:
  worker:
    image: ghcr.io/external-org/worker:v2.3.1
  redis:
    image: redis@sha256:0123456789abcdef...
```

`image:` accepts:

- A simple `repo:tag` (e.g. `myimage:latest`)
- A digest pin (e.g. `myimage@sha256:abc...`)
- A bare name with no tag (rare; defaults to `:latest` per Docker)

`image:` is **mutually exclusive with `registry:`** — pinning a full ref
implies you've decided exactly what to use, no further substitution needed.
Setting both is a validation error.

When `image:` is set, the service is **never built locally** even if
`build: true`. You can't build a service whose image is externally pinned.

## How the image swap actually works

When you run `lo up` (or `lo build`), `lo env kustomization` generates a
`kustomization.yaml` at `clusters/<domain>/artifacts/kustomization.yaml`
containing an `images:` block. Nothing is written at the repo root — all
lok8s-generated files live inside the domain's artifacts directory next
to the rendered `.artifacts.yaml`:

```
.lok8s/
  lok8s.dev/
    targets/                # your kustomize sources (committed)
    artifacts/              # gitignored, generated on every lo build
      .artifacts.yaml       # rendered manifests (kustomize build targets/)
      kustomization.yaml    # generated wrapper with images: overrides
      .cache-queue          # TSV: services queued for cache pre-pull
    .containerd/certs.d/    # containerd hosts.toml (bind-mounted into nodes)
```

Tilt then runs `kustomize build clusters/lok8s.dev/artifacts/` to apply the
image overrides to the rendered manifests. The repo root stays clean.

The `images:` block tells Kustomize to rewrite every reference to
`${prefix}/<service>` in your manifests with whatever the resolution
rules say it should be:

| Service config | Result |
|---|---|
| `build: true` (or default) | **Local build.** No swap. Manifests use `${prefix}/<service>`, Tilt builds + tags exactly that and pushes to the build registry. Kind pulls via the `lok8s.local` containerd mirror. |
| `build: false` + registry endpoint resolves | **Cache mode.** Swap to `lok8s.cache/${branch}/<service>:${tag}`. `lo image cache` pre-pulls `${endpoint}/${branch}/<service>:${tag}` from the dev's docker host (using local credentials), retags, pushes to the cache registry. Kind pulls from the cache registry — no upstream credentials needed inside the cluster. |
| `build: false` + NO registry endpoint | **Warning.** No swap emitted. The service's manifests are still applied to the cluster but pods will fail to pull at runtime (which makes the misconfiguration loud). Fix: define `registry.endpoint`, set `image:`, or flip to `build: true`. |
| Any `image:` pin | **Direct pin.** Swap to the pinned `newName` (+ `newTag` or `digest`) regardless of build flag. No cache layer — kind pulls the pinned ref directly (works for public images via the io-* mirrors transparently; private images need kind credentials). |

## Cache mode (the `lok8s.cache` registry)

When a service has `build: false` AND a `registry.endpoint` resolves
(per-service or global), lok8s puts that service in **cache mode**:

1. **At `lo build` / `lo up` time**, `lo env kustomization` records the
   service in `clusters/<domain>/artifacts/.cache-queue` (a TSV file with
   one row per service: `<svc>\t<remote_ref>\t<branch>\t<tag>`).
2. The kustomize image swap rewrites the manifest reference from
   `${prefix}/<service>` to **`lok8s.cache/${branch}/<service>:${tag}`** —
   pointing at the **local cache registry**, NOT the remote endpoint.
3. **Cache pre-pull is opt-in** via `lo env kustomization --pull`.
   Tilt invokes this automatically (controlled by the `auto_cache_pull`
   kwarg on `lok8s()`, default `True`). For each queue entry the puller:
   - Checks if the cache registry already has a manifest matching the
     ref. **Skips if present** (idempotent — reload cycles are cheap).
   - Otherwise: `docker pull <remote>` (using the dev's docker
     credentials), `docker tag <remote> <cache_ip>/<branch>/<svc>:<tag>`,
     `docker push <cache_ip>/<branch>/<svc>:<tag>`.
4. Kind pulls from the cache registry via the `lok8s.cache` containerd
   mirror (configured by `lo::write_certs_d`). **No upstream credentials
   inside the cluster.**

### Driving the cache pull from CI

If you run a build pipeline in CI before standing up Tilt, pre-pull the
cache as a separate step so Tilt doesn't have to. Two equivalent ways:

```bash
# Option A: drain the queue in one go via the kustomization flag
lo env kustomization --pull

# Option B: write the queue first, drain it later
lo env kustomization
# ... other CI steps ...
lo image cache --all
```

Then in your `Tiltfile`, disable the in-Tilt pre-pull so it doesn't
duplicate the work:

```python
load('./.lok8s/tilt/Tiltfile', 'lok8s')
lok8s(auto_cache_pull = False)    # CI already pre-pulled
```

(`auto_cache_pull` defaults to `True`, so the only reason to set it
explicitly is to opt out.)

### Why opt-in instead of always-on?

Three reasons:

1. **Failure scoping.** Network failures during a cache pull are a
   different class of error than kustomize build failures. Keeping them
   in separate `local()` calls (one for kustomization, one for cache)
   makes Tilt's UI show the right error in the right place.
2. **CI flexibility.** A CI pipeline that pre-pulls images in a
   dedicated step shouldn't have Tilt re-attempt the pull a few seconds
   later. Disabling via `auto_cache_pull=False` skips the redundant
   work.
3. **Developer iteration speed.** When iterating on `services.yaml` /
   service overrides without changing remote tags, the pre-pull step is
   a no-op (idempotent skip), but the developer might want to check the
   generated kustomization without forcing a network round trip.
   `lo env kustomization` (without `--pull`) gives you that.

### Why a separate `cache` registry instead of reusing `build`?

The build registry holds **dev images with hot-reload tooling**. The
cache registry holds **production images pulled from upstream**. They
can have the same image name and tag but completely different content,
which is confusing during debugging. Keeping them in separate registries
prevents that ambiguity. Both are framework-private and live on the
project subnet — they never leak across kind clusters.

### Manual cache control

Most of the time the auto-pull on `lo build` is enough. For the cases
where it isn't:

```bash
# Pre-cache a single service (resolves the remote ref via services.yaml)
lo image cache api

# Force re-pull (skip the "already in cache" check)
lo image cache api --force

# Process the entire queue from the most recent `lo env kustomization`
lo image cache --all

# List what's currently in the cache registry
lo image list

# Drop everything from the cache (then run `lo provision` to recreate)
lo image clean
```

### Parallelism (`registry.parallel`)

By default, cache pre-pulls run **sequentially** (one image at a time).
Set `registry.parallel` in `services.yaml` to change this:

```yaml
registry:
  endpoint: ghcr.io/myorg
  branch: ${DOCKER_PROJECT}
  tag: ${DOCKER_TAG}
  parallel: 4   # up to 4 concurrent pulls
```

| Value | Meaning |
|---|---|
| `0` | **Unlimited.** All queued services pulled at once. |
| `1` | **Sequential** (default). One pull at a time. Easiest to debug. |
| `N ≥ 2` | **Bounded.** Up to N concurrent pulls. |

Bounded is the right choice for medium-sized service repos — it speeds
up cold cache fills (10s of services) without hammering the upstream
registry. Unlimited is fastest but may hit upstream rate limits or
saturate the dev's network.

`registry.parallel` only affects pre-pull throughput. The cache itself
is per-service: a failed pull on one service doesn't block another.

## Three registry hostnames (recap)

| Hostname | Purpose | When manifests reference it |
|---|---|---|
| **`lok8s.local`** | Build registry — Tilt push target for locally-built images. | Whenever `build: true` (the default). |
| **`lok8s.cache`** | Cache registry — pre-pull target for build:false services with a configured remote registry. | When the service is in cache mode. |
| _remote endpoint_ | The actual upstream (`ghcr.io/myorg`, etc.). Resolved per-service or globally via `registry.endpoint`. | Never directly — only used by `lo image cache` to fetch from. |

`lok8s.local` and `lok8s.cache` are wired into kind's containerd at
provision time (`lo::write_certs_d`) with their respective registry
container IPs. The KEP-1755 `local-registry-hosting` ConfigMap publishes
`lok8s.local` to Tilt so `docker_build('foo', ...)` auto-resolves to the
build registry — no per-user `default_registry()` setup.

Your kustomize manifests should always reference services as
`${prefix}/<service>` (e.g. `lok8s.local/api`). That's the canonical
name. The swap layer (covered above) rewrites it to either the cache
registry, an `image:` pin, or leaves it alone for local builds.

## File naming convention (`lok8s.<name>` for dev)

lok8s uses a single file-naming convention to distinguish development and
production variants of any per-service file: **prefix the development
version with `lok8s.`**, and the production version is the same file name
without the prefix.

| Dev file | Production file | Purpose |
|----------|-----------------|---------|
| `lok8s.Dockerfile` | `Dockerfile` | Dev image (live-reload, debug tools) vs production image |
| `lok8s.entrypoint.sh` | `entrypoint.sh` | Dev startup (e.g. dev server, file watcher) vs production startup |
| `lok8s.yaml` | _(none)_ | Tilt build/sync config — there is no production counterpart, the file is dev-only |

**How the swap works:** when a service sets `dockerfile: production` (or
`defaults.dockerfile: production`), Tilt does a literal string replace of
`lok8s.Dockerfile` → `Dockerfile` on the build's `dockerfile:` field. The
same convention is meant to apply to any other per-service files you
maintain in two variants — keep the dev version under `lok8s.<name>` and
your production file at `<name>`, and the relationship is obvious from
the filename alone.

**Why a prefix instead of a suffix** (`Dockerfile.dev`, etc.)?

- It's instantly greppable: `find . -name 'lok8s.*'` lists every dev artifact across the repo.
- The production file keeps its conventional name (`Dockerfile`, `entrypoint.sh`), so external tools (Docker BuildKit, IDE plugins, CI systems) work without configuration.
- The `lok8s.` prefix marks ownership: anything starting with `lok8s.` is "managed by lok8s tooling and only matters during development".

**Recommended for new repos:**

```
my-service/
├── Dockerfile              # production
├── lok8s.Dockerfile        # dev (extends or differs from production)
├── entrypoint.sh           # production
├── lok8s.entrypoint.sh     # dev
└── lok8s.yaml              # Tilt build/sync config
```

## Per-service `lok8s.yaml`

Each service directory contains a `lok8s.yaml` describing how Tilt should
treat it. This file is read **only when the service is being built locally**
(i.e. when the swap above is a no-op).

A service is one of two shapes: **single-image** (a top-level `build:`, the
common case, shown below) or **multi-image** (a top-level `components:` list
— see [Multi-image services](#multi-image-services-components)). The two are
mutually exclusive.

Full schema (single-image form):

```yaml
build:
  context: .                    # docker_build context, relative to service dir
  dockerfile: lok8s.Dockerfile  # dev dockerfile (swappable to Dockerfile in production mode)
  ignore:                       # dockerignore-style file patterns
    - node_modules/
    - .git/
  build_args:                   # list of env var names; values pulled from os.environ
    - API_KEY
    - DATABASE_URL
  live_update:
    sync:                       # files to copy on change without rebuild
      - local_path: ./src
        remote_path: /app/src
    fall_back_on:               # changes to these files trigger a full rebuild
      files:
        - package.json
        - lok8s.Dockerfile
    run:                        # commands to run inside container after sync
      cmd: 'npm install'
      trigger: ['package.json']
    restart_container: {}       # restart the container after live update

ports:                          # port forwards: localhost:from -> container:to
  - { from: 3000, to: 3000 }

links:                          # clickable links shown in the Tilt UI
  - "https://docs.example.com"

workloads:                      # k8s workload names if not the same as service name
  - api-deployment
  - api-worker

tilt:
  resource_deps:                # other Tilt resources this depends on
    - postgres
  labels:                       # Tilt UI grouping labels
    - backend
  extra_resources:              # additional Tilt resources tied to this service
    - name: api-migrations
      objects:
        - api-migration:job
      resource_deps:
        - postgres
```

All paths in `build` (`dockerfile`, `context`, `live_update.sync[].local_path`,
`live_update.fall_back_on.files[]`) are **resolved relative to the service
directory**, not the repo root. So `dockerfile: lok8s.Dockerfile` means
`<service path>/lok8s.Dockerfile`.

### The `build:` block is a Tilt pass-through

The `build:` block under a per-service `lok8s.yaml` is **forwarded
verbatim to Tilt's [`docker_build()`](https://docs.tilt.dev/api.html#api.docker_build)** —
any field that function accepts, you can put in `build:`. lok8s only
validates and interprets the subset it acts on directly:

- `dockerfile` — relative path, resolved against service dir, swapped
  to production variant when `dockerfile: production` is set
- `context` — relative path, resolved against service dir
- `build_args` — list of env var names, resolved at build time
- `live_update` — the four known step types (`sync`, `run`, `fall_back_on`,
  `restart_container`) are passed through to their Tilt counterparts,
  with paths inside them resolved the same way

**Everything else is handed to Tilt as-is.** If Tilt adds a new
`docker_build()` kwarg next release, it just works — no lok8s update
required. If you misspell a Tilt field, Tilt itself will report the
error (not lok8s), which is the correct layer.

The shape of validated fields is enforced at the boundary by
`_validate_service`, so the rest of the code path can trust them. Fields
lok8s doesn't interpret are not shape-checked — use them at Tilt's
discretion.

### Resolving paths in custom `build:` fields

If you want lok8s to resolve paths in a pass-through field it doesn't
know about, pass `extra_paths` to `lok8s()` in your root `Tiltfile`:

```python
load('./.lok8s/tilt/Tiltfile', 'lok8s')

lok8s(
  extra_paths = [
    # Any dotted JSON path under `build:` — use `#` to iterate a list
    'secrets.#.source',
    'cache_from.#.path',
  ],
)
```

If a path in the list can't be walked on a given service (e.g. the
service doesn't use that field), lok8s prints a warning and skips it.
To silence the warnings globally, set `warn_unresolved_paths = False` on
the `lok8s()` call.

### Multi-image services (`components`)

Some repos build **more than one image** from a single source tree — for
example, `kubehz-core` ships a Nuxt/Nitro API (`Dockerfile.api`) and a Go
operator (`Dockerfile.operator`) from the same checkout. Rather than split
them into two `services.yaml` entries with a `context: ..` subdirectory
hack, declare an optional top-level `components:` list in **one**
`lok8s.yaml`:

```yaml
# kubehz-core/lok8s.yaml — one repo, two images
components:
  - name: kubehz-api            # image built as lok8s.local/kubehz-api
    build:
      context: .
      dockerfile: Dockerfile.api
      live_update:
        sync:
          - { local_path: api/server, remote_path: /app/server }
    ports:
      - { from: 3000, to: 3000 }
    workloads: [kubehz-api]
  - name: kubehz-operator       # image built as lok8s.local/kubehz-operator
    build:
      context: .
      dockerfile: Dockerfile.operator
      only: [operator/]         # rebuild only when operator/ source changes
    ports:
      - { from: 8081, to: 8081 }
    workloads: [kubehz-operator]

tilt:
  labels:
    - kubehz
```

Each component is an independent build target:

- The image is `lok8s.local/<name>` (`lok8s.local/kubehz-api`,
  `lok8s.local/kubehz-operator`).
- Its `build:` block has the **same shape and validation** as a single-image
  `build:` — a pass-through to `docker_build()`, with `dockerfile`/`context`/
  `live_update` paths resolved against the **service `path:`** (the one
  source dir). Components differ only by Dockerfile/context/build kwargs.
- Its `ports`, `links`, and `workloads` wire to **its own** Tilt
  `k8s_resource`, matched by the `lok8s.dev/name=<name>` label on that
  component's manifests. So the API's deployment must carry
  `lok8s.dev/name: kubehz-api` and the operator's `lok8s.dev/name:
  kubehz-operator`, and each builds + reloads independently.

**`components:` is mutually exclusive with a top-level `build:`.** A
top-level `build:` is the single-image shorthand; `components:` is the
multi-image form. Setting both fails fast:

```
[ kubehz-core ] lok8s.yaml: 'components' and a top-level 'build' are mutually exclusive — put the build under each component, or drop 'components' for a single image
```

The service-level `tilt:` block (`resource_deps`, `labels`,
`extra_resources`) still applies at the **service** level, not per
component. See the [schema reference](/reference/schema#components) for the
exhaustive field-by-field table.

## `services.yaml` field reference

### Top level

```yaml
registry: {...}      # Global Docker registry config (optional)
defaults: {...}      # Project-wide defaults for service fields (optional)
services: {...}      # Service catalog (required for any Tilt activity)
```

### `registry`

```yaml
registry:
  endpoint: "ghcr.io/myorg"   # Registry hostname + namespace
  branch:   "${DOCKER_PROJECT}" # Path segment between endpoint and service name
  tag:      "${DOCKER_TAG}"     # Image tag
  prefix:   lok8s.local         # Canonical local-build prefix (rarely changed)
```

`endpoint`, `branch`, and `tag` go through `envsubst` so you can use
environment variables (typically exported by direnv). The full image ref for
a non-built service is `${endpoint}/${branch}/<service>:${tag}`.

### `defaults`

```yaml
defaults:
  build: true           # Default for services.<name>.build (default: true)
  dockerfile: service   # Default for services.<name>.dockerfile (default: service)
```

### `services.<name>`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | When `false`, the service is completely skipped (no manifests applied, no build) |
| `build` | bool | `defaults.build` | When `true`, build locally with Tilt + live-update. When `false`, pull from registry. |
| `path` | string | `./<name>` | Service source directory, relative to repo root |
| `namespace` | string | unset | Override the k8s namespace for this service's manifests |
| `dockerfile` | `service`\|`production` | `defaults.dockerfile` | Switches between dev and prod dockerfile (`lok8s.Dockerfile` ↔ `Dockerfile`) |
| `watch` | string[] | `[]` | Additional files to watch for Tiltfile reload |
| `registry` | object | unset | Per-service registry config override (`endpoint`, `branch`, `tag`, `prefix`) |
| `image` | string | unset | Pin to a specific image ref (mutually exclusive with `registry:`, implies `build: false`) |

## Common patterns

### "I want everything to build locally"

```yaml
defaults:
  build: true     # this is the default — you can omit this
services:
  api: {}
  worker: {}
```

### "I want everything to come from CI by default"

```yaml
defaults:
  build: false
services:
  api: {}
  worker: {}
```

### "Only `api` builds locally for me"

```yaml
# services.local.yaml (gitignored)
services:
  api:
    build: true
```

### "I want to test against PR #1234's worker build"

```yaml
# services.local.yaml
services:
  worker:
    registry:
      branch: pr-1234
```

### "Use this exact upstream image for redis"

```yaml
# services.local.yaml
services:
  redis:
    image: redis:7.2-alpine
```

### "Disable a service entirely for now"

```yaml
# services.local.yaml
services:
  notifier:
    enabled: false
```

### "I want a committed 'frontend dev' profile that builds api+dashboard+docs"

```yaml
# services.frontend.yaml (committed)
services:
  api:        { build: true }
  dashboard:  { build: true }
  docs:       { build: true }
```

Use it with `LOK8S_SERVICE_CONFIG=frontend lo up`.

## Validation and error messages

`lo` validates `services.yaml` and per-service `lok8s.yaml` files at the
boundary — when they're loaded, before any cluster operations happen.
Invalid files fail fast with a clear error pointing at the specific field.
Examples:

```
services.yaml: services.api.build must be a bool, got string
services.yaml: services.worker: 'image' and 'registry' are mutually exclusive
[ api ] lok8s.yaml: missing required 'build' block
[ api ] lok8s.yaml: live_update.fall_back_on: unknown keys ['ignore'] (allowed: ['files'])
[ api ] required build_arg env var not set: API_KEY
```

If you see one of these, fix the YAML — the framework won't paper over it
with silent fallbacks.

## Unlabeled resources

The Tilt extension routes manifests to services using two labels:

- `lok8s.dev/type=system` — applied as cluster infrastructure (CNI, ingress, CRDs, etc.)
- `lok8s.dev/name=<service>` — claimed by the matching service in `services.yaml`

A resource that has **neither** label after both filter passes is considered
unlabeled. By default, lok8s prints a per-resource breakdown and **drops**
the unlabeled resources:

```
!! [ unlabeled ] ConfigMap api/orphaned-config — no lok8s.dev/{type,name} label match
!! [ unlabeled ] dropped 1 resource(s) (set apply_unlabeled=True to apply, or strict_unlabeled=True to fail)
```

This is almost always a bug: missing label, typo, renamed service, or a
forgotten `system_types` entry. Two `lok8s()` kwargs let you change the
default:

| Kwarg | Default | Behavior |
|---|---|---|
| `strict_unlabeled` | `False` | When `True`, replace the warning with `fail()`. Use in CI. |
| `apply_unlabeled` | `False` | When `True`, apply the unlabeled resources via `k8s_yaml(...)` anyway, after printing the breakdown. Escape hatch. |

The two are **mutually exclusive** — combining them is rejected at the top
of `lok8s()`. Pick one, not both.

```python
# Tiltfile (CI)
load('./.lok8s/tilt/Tiltfile', 'lok8s')
lok8s(strict_unlabeled = True)   # any unlabeled resource fails the run
```

```python
# Tiltfile (legacy / migration)
load('./.lok8s/tilt/Tiltfile', 'lok8s')
lok8s(apply_unlabeled = True)    # apply everything, warn about unlabeled
```

**Note**: a service set to `enabled: false` in `services.yaml` ends up in
the unlabeled bucket too. Disabling a service drops its resources, and the
warning makes that drop visible. To fully suppress the resource, remove
the service from `services.yaml` entirely (the manifests get filtered out
upstream by kustomize because no kustomize target references them).

## See also

- [Local Dev with Tilt](/guide/local-dev) — the broader Tilt workflow
- [Concepts](/guide/concepts) — domains, targets, and the FQDN convention
- [CLI Reference](/reference/cli) — `lo env services`, `lo env kustomization`
- [Schema Reference](/reference/schema) — exhaustive field-by-field tables for `services.yaml` and `lok8s.yaml`
