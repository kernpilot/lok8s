# Schema reference

The canonical, validated schemas for the two YAML files lok8s consumes:

- **`services.yaml`** — project-wide service catalog and registry config
  (validated by `_validate_services_yaml` in `.lok8s/tilt/Tiltfile`)
- **`<service>/lok8s.yaml`** — per-service Tilt build/sync config
  (validated by `_validate_service` in `.lok8s/tilt/Tiltfile`)

> **Source of truth**: the validator functions are authoritative. This
> document is kept in sync with them but may lag a commit or two. If
> the docs and validator disagree, **the validator wins** — and please
> file an issue so the doc can catch up.

For workflow-level explanations of the fields, see
[Services Configuration](/guide/services). This page is the field
reference.

---

## `services.yaml`

```yaml
registry: {...}
defaults: {...}
services:
  <name>: {...}
```

Three top-level keys, all optional. Unknown top-level keys fail validation.

### `registry`

Project-wide Docker registry config. All fields are optional and
`envsubst`-expanded after read, so values like `${DOCKER_REGISTRY}`
work.

| Field | Type | Default | Description |
|---|---|---|---|
| `endpoint` | string | `${DOCKER_REGISTRY}` | Remote registry hostname + namespace (e.g. `ghcr.io/myorg`). Used as the pull source for `build: false` services in cache mode. |
| `branch` | string | `${DOCKER_PROJECT}` | Path segment between `endpoint` and the service name (typically a branch slug or PR number). |
| `tag` | string | `${DOCKER_TAG}` | Image tag. |
| `prefix` | string | `lok8s.local` | Canonical local image name — what manifests reference and what Tilt's `docker_build` produces. Resolves on-cluster via the `lok8s.local` containerd mirror. |
| `parallel` | int (≥0) | `1` | Concurrent cache pre-pull cap. `0` = unlimited, `1` = sequential, `N≥2` = bounded. |

**Validation**: unknown keys under `registry` fail. `parallel` must be a non-negative integer.

### `defaults`

Per-service defaults applied when a service omits the corresponding field.

| Field | Type | Default | Description |
|---|---|---|---|
| `build` | bool | `true` | Default for `services.<name>.build`. `true` = build locally + Tilt live-update; `false` = pull from registry (cache mode). |
| `dockerfile` | `service` \| `production` | `service` | Default for `services.<name>.dockerfile`. `service` uses `lok8s.Dockerfile`, `production` swaps to `Dockerfile`. |

**Validation**: unknown keys fail. `dockerfile` value is restricted. `build` must be a boolean.

### `services.<name>`

The service catalog. Each entry describes one service.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | When `false`, the service is skipped completely. Its labeled resources show up in the unlabeled-leftover bucket — see [Unlabeled resources](/guide/services#unlabeled-resources). |
| `build` | bool | `defaults.build` | Per-service override. `true` = local build, `false` = cache mode (requires `registry.endpoint` to resolve). |
| `path` | string | `./<name>` | Service source directory, relative to repo root. |
| `namespace` | string | _(unset)_ | Inject this namespace into all of the service's k8s manifests. |
| `dockerfile` | `service` \| `production` | `defaults.dockerfile` | Per-service override. |
| `watch` | string[] | `[]` | Additional files to watch for Tiltfile reload. Paths relative to the service directory. |
| `registry` | object | _(unset)_ | Per-service registry override. Same fields as the top-level `registry` block. Per-service keys win over global, key-by-key. **Mutually exclusive with `image`.** |
| `image` | string | _(unset)_ | Pin to a specific image ref. Accepts `repo:tag`, `repo@sha256:digest`, or bare `repo`. **Mutually exclusive with `registry`.** Implies `build: false` (you can't build a service whose image is externally pinned). |

**Validation**:

- `services.<name>` must be a mapping
- Unknown keys fail
- `build` must be a boolean
- `dockerfile` value restricted to `service`/`production`
- `watch` must be a list
- `registry` must be a mapping with valid keys (`endpoint`, `branch`, `tag`, `prefix`)
- `image` must be a string
- `image` and `registry` cannot both be set

### Per-service `registry.parallel`

`registry.parallel` is **only valid at the top level** (`registry.parallel`),
not per-service. The cache pre-pull operation is project-wide, so a
per-service parallelism cap doesn't make sense.

### Examples

#### Minimal — single-service local build

```yaml
services:
  api: {}
```

Equivalent to: `enabled: true`, `build: true` (from default), `path: ./api`,
no namespace override, dev dockerfile (`lok8s.Dockerfile`), no extra
watches, no per-service registry.

#### Multi-service with global registry

```yaml
registry:
  endpoint: ghcr.io/myorg
  branch: ${DOCKER_PROJECT}
  tag: ${DOCKER_TAG}
  parallel: 4

defaults:
  build: false        # default everyone to "use the registry"

services:
  api:
    build: true       # but I'm developing api locally
  worker: {}
  dashboard: {}
```

#### Per-service registry override (PR mixing)

```yaml
registry:
  endpoint: ghcr.io/myorg
  branch: main
  tag: latest

services:
  api:
    build: true
  worker:
    build: false
    registry:
      branch: pr-1234       # pull worker from PR #1234, everything else from main
      tag: abc123def
```

#### Image pinning

```yaml
services:
  redis:
    image: redis:7.2-alpine
  external-tool:
    image: ghcr.io/external/tool@sha256:0123456789abcdef
```

`image:` always wins over `registry:` and disables local building.

---

## Per-service `lok8s.yaml`

Located at `<service-path>/lok8s.yaml`. Read by Tilt for **services that
build locally** (`build: true` or default). Skipped for cache-mode and
image-pinned services.

```yaml
build: {...}        # required UNLESS components is set
components: [...]   # optional; multi-image services. Mutually exclusive with build
ports: [...]        # optional
links: [...]        # optional
workloads: [...]    # optional
tilt: {...}         # optional
```

Top-level structure. Unknown keys fail. Exactly one of `build` (single
image) or `components` (multiple images) is required — see
[`components`](#components) below.

### `build`

The `build` block is a **pass-through to Tilt's
[`docker_build()`](https://docs.tilt.dev/api.html#api.docker_build)**.
Any field that function accepts can be put in `build:`. lok8s only
validates and interprets the subset it acts on directly.

#### Validated fields

| Field | Type | Description |
|---|---|---|
| `dockerfile` | string | Dockerfile path, relative to service directory. Resolved to absolute by `_update_paths`. Swapped via the `lok8s.Dockerfile` → `Dockerfile` rule when `dockerfile: production`. |
| `context` | string | Build context, relative to service directory. Resolved to absolute. |
| `build_args` | string[] | List of env var names to pass as Docker build args. Each name must exist in the host environment at build time, or `lok8s()` fails fast with the service name. |
| `live_update` | object | See below. |

#### Pass-through fields

Any other key under `build:` is forwarded verbatim to `docker_build()`.
Examples include `ignore`, `platform`, `target`, `cache_from`, `extra_tag`,
`network`, `pull`, `secrets`, `ssh`, `image_deps`, etc. — see Tilt's docs
for the full list. lok8s does not shape-check these; if Tilt rejects
them, the error surfaces from Tilt itself, not lok8s.

#### Resolving paths in custom fields

If you use a pass-through field with relative paths in it, register the
JSON path with the `extra_paths` kwarg on `lok8s()` so `_update_paths`
resolves it:

```python
load('./.lok8s/tilt/Tiltfile', 'lok8s')
lok8s(
  extra_paths = [
    'cache_from.#.path',     # # iterates a list
    'secrets.#.source',
  ],
)
```

#### `build.live_update`

Per-service live-reload steps. The four step types correspond directly
to Tilt's
[live_update API](https://docs.tilt.dev/api.html#api.live_update).

| Step | Type | Multiple? | Description |
|---|---|---|---|
| `fall_back_on` | dict OR list-of-dicts | yes (at start only) | Files whose change forces a full rebuild. Emits `fall_back_on(files=[...])`. |
| `sync` | dict OR list-of-dicts | yes | Files synced to the running container without rebuild. Each entry: `{local_path, remote_path}`. |
| `run` | dict OR list-of-dicts | yes | Commands to execute in the container after sync. Each entry: `{cmd, trigger?, echo_off?}`. |
| `restart_container` | dict (typically `{}`) | **no — singleton** | Restart the container after live update. Tilt requires this be the last and only step. |

The dispatcher emits steps in Tilt's required order:
`fall_back_on → sync → run → restart_container`.

`fall_back_on.files` (when `fall_back_on` is given as a single dict)
accepts a string OR list of strings. Paths are resolved relative to the
service directory.

**Validation**:

- `live_update` must be a mapping
- `sync`, `run`, `fall_back_on` accept dict OR list of dicts
- `restart_container` must be a dict (no list — Tilt singleton constraint)
- `fall_back_on.files` (single-dict form) accepts string or list of strings

### `ports`

Port forwards. Each entry is `{from, to}` where `from` is the host port
and `to` is the container port.

```yaml
ports:
  - { from: 3000, to: 3000 }
  - { from: 5432, to: 5432 }
```

**Validation**: must be a list; each entry must contain both `from` and `to`.

### `links`

Tilt UI clickable links shown next to the service.

```yaml
links:
  - "https://docs.example.com"
  - "https://app.example.com/healthz"
```

**Validation**: must be a list.

### `workloads`

Override which Kubernetes workload names this service controls. Defaults
to a single workload matching the service name.

```yaml
workloads:
  - api-deployment
  - api-worker
```

Use this when one service produces multiple deployments/statefulsets/etc.
that should all be managed under the same Tilt resource.

**Validation**: must be a list.

### `tilt`

Tilt-specific resource configuration.

| Field | Type | Description |
|---|---|---|
| `resource_deps` | string[] | Other Tilt resources this depends on. Tilt waits for them to be ready before starting this one. |
| `labels` | string[] | Tilt UI grouping labels. |
| `extra_resources` | object[] | Additional `k8s_resource(...)` calls tied to this service (e.g. migration jobs). Each entry: `{name, objects?, resource_deps?, labels?}`. |
| `hooks` | object[] | Dev-time lifecycle hooks — thin wrappers over Tilt `local_resource()`. See [tilt.hooks](#tilt-hooks) below. |

**Validation**: unknown keys under `tilt` fail. `extra_resources` must be a list, each entry must contain `name`. Each `hooks` entry must contain `name` and either a `do` verb (known to the hook map) or an explicit `cmd`.

#### tilt.hooks

A `hooks:` entry is a **thin YAML wrapper over Tilt's `local_resource()`** — the
same way `build:` wraps `docker_build()`. Every `local_resource` keyword
(`deps`, `resource_deps`, `trigger_mode`, `auto_init`, `ignore`, `env`, `dir`,
`allow_parallel`, `links`, `labels`, …) flows through verbatim, so you keep the
full Tilt API without dropping to Starlark. A hook **acts on already-rendered,
in-cluster objects by LABEL** when a watched file changes — nothing new is
deployed, so production is untouched. Hooks are **change-only** (`auto_init`
defaults to `false`): startup is the manifests' own job; a hook re-fires only on
edit.

| Field | Type | Description |
|---|---|---|
| `name` | string | _(required)_ Hook id → Tilt resource `hook:<name>`. |
| `do` | string | A verb in the hook map → fills `cmd` with the matching `lo hooks` action. Built-in: `recreate` (delete + apply the selected objects — immutable Jobs re-run), `restart` (rollout restart the selected workloads), `apply` (re-apply, no delete). Extend/override via `lok8s(hooks={'verb': 'lo …'})`. |
| `targets` | object | Label selector `{key: value, …}` → appended to the `cmd` as `--selector key=value,…`. |
| `cmd` | string | An explicit `local_resource` command. Skips the `do`/`targets` sugar. |
| `deps` | string[] | Watched files (the change trigger), resolved **service-relative** (like `watch`). |
| _(any `local_resource` kwarg)_ | — | `resource_deps`, `env`, `trigger_mode`, `ignore`, `dir`, … pass through. |

lok8s injects context as env (`LOK8S_SERVICE`, `LOK8S_HOOK`); the domain is `lo`'s
already-resolved global arg. The doing — filter the rendered artifacts by label,
then `kubectl delete`+apply / `rollout restart` — lives in the hidden `lo hooks`
command (bash, bats-tested), so the Tiltfile stays thin.

```yaml
tilt:
  hooks:
    # Re-run a seed Job when its script changes (replaces a manual re-deploy /
    # seed-revision bump). recreate = delete + apply the label-selected Job.
    - name: provision
      deps: [deploy/server/provision-clients.sh]   # service-relative
      do: recreate
      targets: { lok8s.dev/role: seed, lok8s.dev/name: zitadel }
    # Then restart a dependent workload to pick up what provision published.
    - name: login-pickup
      resource_deps: [hook:provision]
      deps: [deploy/server/provision-clients.sh]
      do: restart
      targets: { app: kubehz-auth }
```

> **Seed-Job targeting.** Give the Job a distinct label (e.g. `lok8s.dev/role: seed`)
> so the selector hits only it — sibling Jobs typically share `lok8s.dev/name`.
> Use a non-`type` label so the Job keeps its `lok8s.dev/type` (it still applies
> through the normal pass).

### `components`

For a repo that builds **more than one image** (e.g. an API and an
operator sharing one source tree), declare a `components:` list instead of
a top-level `build:`. Each entry is an independent build target whose image
is named `lok8s.local/<name>` and whose `ports`/`links`/`workloads` wire to
**its own** `k8s_resource` (matched via the `lok8s.dev/name=<name>` label on
that component's manifests) — not the service name.

`components` is **mutually exclusive with a top-level `build`**: a top-level
`build` is the single-image shorthand, while `components` declares N named
images. Setting both is a validation error.

```yaml
components:
  - name: api                 # REQUIRED, unique. Image built as lok8s.local/api
    build:                    # REQUIRED. Same shape + validation as the top-level build
      context: .
      dockerfile: Dockerfile.api
      live_update: { ... }
    ports:                    # optional — same shape as the top-level ports
      - { from: 3000, to: 3000 }
    workloads: [api]          # optional — defaults to [<name>]
    links: [ ... ]            # optional — same shape as the top-level links
  - name: operator
    build:
      context: .
      dockerfile: Dockerfile.operator
      only: [operator/]       # pass-through docker_build kwarg
    workloads: [operator]
```

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | _(required)_ | Component name. Must be non-empty and unique within the list. The built image is `${prefix}/<name>` (e.g. `lok8s.local/api`). |
| `build` | object | _(required)_ | A `build:` block with the **same shape and validation** as the top-level [`build`](#build) (pass-through to `docker_build()`; `dockerfile`/`context`/`live_update` paths resolve against the service `path:`, exactly as the single-image case). |
| `ports` | object[] | _(unset)_ | Per-component port forwards. Same `{from, to}` shape as the top-level [`ports`](#ports). |
| `workloads` | string[] | `[<name>]` | k8s workload name(s) this component controls. Same as the top-level [`workloads`](#workloads). |
| `links` | object[]/string[] | _(unset)_ | Per-component Tilt UI links. Same as the top-level [`links`](#links). |

Notes:

- All component build paths resolve against the **service `path:`** (the
  one source dir), so components differ only by `dockerfile`/`context`/
  build kwargs within a single repo — no per-component `path:` and no
  `context: ..` subdirectory hack.
- The service-level [`tilt`](#tilt) block (`resource_deps`, `labels`,
  `extra_resources`) applies at the **service** level, not per component.
  Per-component UI grouping is not yet supported — use `links`/`ports`
  per component and `tilt.labels` on the service.
- `dockerfile: production` (service- or defaults-level) applies the
  `lok8s.Dockerfile → Dockerfile` swap to **every** component's `build`.

**Validation**:

- `components` must be a **non-empty list** when present
- a sibling top-level `build` is **forbidden** when `components` is set
- each entry must be a mapping with a non-empty string `name`; names must be unique
- each entry requires a `build` block, validated by the same logic as the top-level `build`
- `ports`/`workloads`/`links` are optional, validated like their top-level counterparts
- unknown keys in a component entry fail (allowed: `name`, `build`, `ports`, `links`, `workloads`)

### Example

```yaml
build:
  context: .
  dockerfile: lok8s.Dockerfile
  ignore:
    - node_modules/
    - .git/
  build_args:
    - DATABASE_URL
    - SENTRY_DSN
  live_update:
    fall_back_on:
      files:
        - package.json
        - bun.lock
    sync:
      - { local_path: ./src, remote_path: /app/src }
      - { local_path: ./public, remote_path: /app/public }
    run:
      cmd: 'bun install'
      trigger: ['package.json']
    restart_container: {}

ports:
  - { from: 3000, to: 3000 }

links:
  - "http://localhost:3000"

tilt:
  resource_deps:
    - postgres
  labels:
    - backend
  extra_resources:
    - name: api-migrations
      objects:
        - api-migration:job
      resource_deps:
        - postgres
```

#### Multi-image (`components`)

A single repo that builds an API and an operator (the kubehz-core shape):

```yaml
# kubehz-core/lok8s.yaml — one source tree, two images
components:
  - name: kubehz-api
    build:
      context: .
      dockerfile: Dockerfile.api
      live_update:
        sync:
          - { local_path: api/server, remote_path: /app/server }
    ports:
      - { from: 3000, to: 3000 }
    workloads: [kubehz-api]
  - name: kubehz-operator
    build:
      context: .
      dockerfile: Dockerfile.operator
      only: [operator/]        # rebuild only when operator/ source changes
    ports:
      - { from: 8081, to: 8081 }
    workloads: [kubehz-operator]

tilt:
  labels:
    - kubehz
```

This produces two `docker_build` calls — `lok8s.local/kubehz-api` and
`lok8s.local/kubehz-operator` — both with `context` resolved against the
`kubehz-core` service directory. Each image's manifests are matched by its
own `lok8s.dev/name` label (`kubehz-api` / `kubehz-operator`).

---

## Validation error format

All errors emitted by both validators have a consistent shape:

**`services.yaml` errors** are prefixed with the file:

```
services.yaml: services.api.build must be a bool, got string
services.yaml: services.worker: 'image' and 'registry' are mutually exclusive
services.yaml: registry.parallel must be a non-negative integer
```

**`lok8s.yaml` errors** are prefixed with the service name in brackets:

```
[ api ] lok8s.yaml: missing required 'build' block
[ api ] lok8s.yaml: build.live_update must be a mapping
[ api ] lok8s.yaml: build.live_update.sync[2] must be a mapping
[ api ] lok8s.yaml: build.live_update.restart_container must be a mapping (Tilt API allows it only once, as the last step)
[ api ] lok8s.yaml: ports[0] requires 'from' and 'to'
[ api ] lok8s.yaml: tilt.extra_resources[1] missing 'name'
[ api ] required build_arg env var not set: API_KEY
```

For multi-image services, component errors carry the component index and name:

```
[ kubehz-core ] lok8s.yaml: 'components' and a top-level 'build' are mutually exclusive — put the build under each component, or drop 'components' for a single image
[ kubehz-core ] lok8s.yaml: 'components' must be a non-empty list
[ kubehz-core ] lok8s.yaml: components[1] (kubehz-operator) missing required 'build' block
[ kubehz-core ] lok8s.yaml: components[0] (kubehz-api) build.context must be a string
```

Both validators **fail fast** at the parse boundary — once they return,
the rest of the Tiltfile can trust the shape and doesn't add defensive
checks downstream. This is the framework's "validate at the boundary,
trust internally" principle in action.

---

## Schema evolution

When fields are added or removed, both happen in the same commit:

1. The validator allow-list is updated
2. This document is updated
3. Any `services.yaml` fixtures or example configs in the docs are updated
4. The CHANGELOG (if present) gets a note

The validator is the single source of truth — if you find a discrepancy
between the validator and this document, the validator is correct.
