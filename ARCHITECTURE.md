# lok8s — architecture & folder structure

Authoritative reference for the on-disk layout of a lok8s project. For
concepts (atoms/molecules, bootstrap vs workloads, build/deploy
pipeline) see [`docs/guide/concepts.md`](docs/guide/concepts.md). For
spec field semantics see [`docs/reference/specs.md`](docs/reference/specs.md).
For writing addons see [`docs/guide/addons.md`](docs/guide/addons.md).

---

## Principles

- **Production first.** Production is the reference setup; local dev is
  a nerfed overlay of the same structure.
- **Same tree everywhere.** Lo (kind), Capi, and future drivers all use
  the same `.lok8s/` layout.
- **Domain keyed.** Every cluster and every deployment is keyed by an
  FQDN. One cluster = one folder under `clusters/`.
- **Kustomize native.** Targets are kustomize builds. Helm charts are
  inflated via [khelm](https://github.com/mgoltzsche/khelm) at
  `kustomize build` time — no helm CLI dependency.
- **Artifacts gitignored.** Rendered output under `artifacts/` is
  ephemeral and rebuilt on demand. If you want GitOps-committed
  artifacts, opt in per-project.
- **Framework-owned `.lok8s/`.** Everything under `.lok8s/` is
  shipped/managed by the framework (`b sync` owns it) and stays a flat,
  override-free tree. User content lives under `clusters/` and at the
  repo root.
- **No framework-level workload ordering.** The only ordering primitive
  lok8s exposes is `spec.bootstrap` for cluster-infrastructure addons
  applied at provision time. Workload ordering (if needed) is expressed
  by the workload layer itself — kubectl's in-manifest order, Tilt's
  `resource_deps`, or GitOps engine primitives.

---

## Layer model

| Layer | What                                                 | lok8s scope                             |
| ----- | ---------------------------------------------------- | --------------------------------------- |
| 0     | Hardware provider (hcloud, AWS, GCP, bare metal)     | Abstracted by CAPI                      |
| 1     | System / kernel                                      | Abstracted by CAPI                      |
| 2     | Kubernetes (control plane, nodes)                    | Created by driver (Lo / Capi)           |
| 3     | Cluster infrastructure (CNI, CSI, MetalLB, CRDs)     | `spec.bootstrap` → framework addons     |
| 4     | Third-party software (monitoring, operators)         | `targets/<name>/` (referencing addons)  |
| 5     | User applications                                    | `targets/<name>/` + `services.yaml`     |

Layers 3–5 are the cluster content. Layer 3 runs at provision time
before anything else lands. Layers 4–5 run after the cluster is
healthy, via Tilt (dev) or `lo deploy` (headless/CI).

---

## Two concerns, two mechanisms

### 1. Cluster creation

How the cluster comes into existence. Handled by the **driver**
(`.lok8s/drivers/<kind>/main`), not by kustomize.

| Kind | Driver              | Runtime                                   |
| ---- | ------------------- | ----------------------------------------- |
| `Lo` | `drivers/lo/main`  | kind (Docker-in-Docker)                   |
| `Capi` | `drivers/capi/main` | Cluster API (any provider — Hetzner, AWS, ...)|
| `KubeOne` | `drivers/kubeone/main` | KubeOne (imperative worker management)  |
| `Kkp` | `drivers/kkp/main` | Kubermatic Kubernetes Platform              |

The driver consumes `cluster.lok8s.yaml` and provisions the cluster.
When run locally, the `lo` script invokes the driver directly. In the
operator mode (future), the operator reads the same CRD from
Kubernetes.

### 2. Cluster content

What runs on the cluster. Split into two planes:

- **Plane A — cluster infrastructure** (`spec.bootstrap`). Ordered list
  of framework addons applied during provisioning (by
  `.lok8s/libs/bootstrap`, identically for every driver), with health
  waits between stages. CNI → MetalLB → cert-manager → ... The cluster
  is not considered ready until this phase completes.
- **Plane B — workloads** (`targets/`). User-named kustomize
  directories, each built independently into
  `artifacts/<target>/artifacts.yaml`. Applied by Tilt (dev) or
  `lo deploy` (headless). No framework-level ordering.

See [concepts.md](docs/guide/concepts.md) for the full model.

---

## Top-level project layout

```
my-project/
├── services.yaml                 # service definitions + registry config (committed)
├── services.<config>.yaml        # personal overrides (gitignored)
├── Tiltfile                      # loads .lok8s/tilt/Tiltfile
├── .bin/                         # b-managed tool binaries (argsh, kustomize, ...)
├── .kustomize/                   # b-managed kustomize plugin binaries
│   ├── khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer
│   └── secrets.lok8s.dev/v1/secret/Secret
├── clusters/                     # user cluster definitions, one dir per FQDN
└── .lok8s/                       # framework tree (synced, don't edit)
```

`cluster.lok8s.yaml` never lives at the repo root. It lives inside its
domain directory, under `clusters/<domain>/`.

---

## The `.lok8s/` tree (framework) and `clusters/` tree (yours)

```
.lok8s/                           # framework — flat, synced, override-free
├── lo                            # CLI entrypoint
├── libs/                         # shared bash libraries
│   ├── bootstrap                 # applies spec.bootstrap (framework-level)
│   ├── addons                    # lo addons command
│   ├── build, deploy, env
│   ├── lint, status
│   ├── gitops, kubehz/
│   └── ...
├── utils/                        # shared helpers (verbose, ip, types, http, ...)
├── addons/                       # framework-shipped bootstrap addons
│   ├── cilium/                   # Cilium CNI (khelm ChartRenderer + layered values)
│   └── metallb/                  # MetalLB L2 LB (khelm ChartRenderer)
├── drivers/
│   ├── lo/
│   │   ├── main                  # Lo driver (kind)
│   │   ├── cluster/              # kind runtime templates
│   │   │   ├── config.yaml       # base kind config
│   │   │   ├── registry/         # registry container templates
│   │   │   └── coredns/          # CoreDNS overlay
│   │   └── utils/                # driver helpers
│   ├── capi/                     # CAPI driver + envsubst templates
│   ├── kubeone/                  # KubeOne driver
│   └── kkp/                      # KKP driver
├── providers/                    # physical infra providers
│   └── hetzner/                  # hcloud + Robot (cloud-init, installimage)
└── tilt/
    └── Tiltfile                  # the lok8s() extension function

clusters/                         # user content — one dir per FQDN
├── .active                       # runtime state: current domain (gitignored)
├── lok8s.dev/                    # default cluster domain (ships with lok8s)
│   ├── cluster.lok8s.yaml
│   ├── targets/                  # workload plane (Plane B)
│   │   └── <name>/
│   │       └── kustomization.yaml
│   ├── artifacts/                # built output (gitignored)
│   │   ├── kustomization.yaml    # auto-generated by lo env kustomization
│   │   └── <target>/
│   │       └── artifacts.yaml
│   └── .secrets/                 # secret cache (gitignored; encrypted opt-in)
└── example.com/                  # additional cluster domain
    └── cluster.lok8s.yaml
```

### `.lok8s/addons/`

Framework-shipped **bootstrap addons**. Each subdirectory is a
kustomize-buildable addon — typically a khelm `ChartRenderer` + layered
values files, but any kustomization works. These are the **atoms** of
the lok8s addon model, shared across every driver.

Referenced by name from `cluster.lok8s.yaml`:

```yaml
spec:
  bootstrap:
    - cilium        # → .lok8s/addons/cilium/
    - metallb       # → .lok8s/addons/metallb/
```

Or as kustomize bases from workload targets:

```yaml
# clusters/<domain>/targets/platform/kustomization.yaml
resources:
  - ../../../addons/cert-manager/
  - ./ingress-routes.yaml
```

Values files stack as `base < driver < provider < inline` — see
[`docs/guide/addons.md`](docs/guide/addons.md) for the full addon
authoring guide and precedence rationale.

### `clusters/<domain>/targets/`

The workload plane. Each subdirectory is an independent kustomize
target — the **molecules** of the model. Targets compose addons and
in-repo services into a single kustomization.

```
targets/
├── platform/
│   └── kustomization.yaml        # e.g. cert-manager + ingress routes
├── monitoring/
│   └── kustomization.yaml        # e.g. prometheus stack
└── apps/
    └── kustomization.yaml        # your applications
```

Targets are **independently built** by `lo build`, producing one
`artifacts/<target>/artifacts.yaml` per target. There is no top-level
composition and no inter-target ordering.

### `clusters/<domain>/artifacts/`

Generated by `lo build` and `lo env kustomization`. Gitignored.

```
artifacts/
├── kustomization.yaml            # auto-generated: resources: per-target + images swap
├── .cache-queue                  # Tilt's build:false pre-pull queue (TSV)
├── platform/
│   └── artifacts.yaml            # per-target rendered output
├── monitoring/
│   └── artifacts.yaml
└── apps/
    └── artifacts.yaml
```

The top-level `kustomization.yaml` is what Tilt reads: one
`kustomize build` over this directory yields a unified pool that Tilt
then partitions via `filter_yaml()` (by `lok8s.dev/type: system` for
infrastructure, by service-name labels for per-service workloads, rest
as uncategorized).

---

## `cluster.lok8s.yaml` — the one place for cluster config

Everything the driver needs to create and prepare a cluster lives in a
single file under `clusters/<domain>/cluster.lok8s.yaml`. The full field
reference is in
[`docs/reference/specs.md`](docs/reference/specs.md); a minimal Lo
(kind) example:

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  cluster:
    domain: lok8s.dev
  bootstrap:
    - cilium
    - metallb
```

Every other field is defaulted — slot-derived from the `*.lok8s.dev`
domain or applied as a domain-independent default. See the
[Specs reference](docs/reference/specs.md#default-resolution) for the
full defaulting table.

### Folder-name convention

The folder name under `clusters/` MUST match `spec.cluster.domain`.
`spec.cluster.domain` should be the **k8s API endpoint hostname**, not
the user-facing brand domain. This lets you have multiple clusters
serving the same brand from different environments without folder
collisions:

- `clusters/example.com/` — local kind cluster claiming `example.com` via `/etc/hosts`
- `clusters/cluster.example.in.net/` — production, API at `cluster.example.in.net`,
  serving public traffic on `app.example.com` via Envoy routes

### `deploy.lok8s.yaml`

Deployment domains use `deploy.lok8s.yaml` instead of `cluster.lok8s.yaml`.
They reference an existing cluster via `spec.clusterRef.domain` to
deploy content to someone else's cluster. Workload selection for
Deploy specs is being reworked alongside the `services.yaml` targets-map
redesign — currently a Deploy spec carries only `clusterRef` + `namespace`.

---

## Services and Tilt integration

### `services.yaml`

One committed file at the repo root. Defines which services exist,
their build config, and a shared registry config. Personal overrides
land in `services.<config>.yaml` (gitignored).

```yaml
# services.yaml
registry:
  endpoint: "${DOCKER_REGISTRY}"
  branch: "${DOCKER_PROJECT}"
  tag: "${DOCKER_TAG}"
  prefix: lok8s.local

defaults:
  build: true

services:
  my-api:
    build: true
  my-frontend:
    build: true
  redis:
    build: false                  # use prebuilt image
```

```yaml
# services.local.yaml — personal override, gitignored
services:
  my-frontend:
    enabled: false                # not working on this today
```

See [`docs/guide/services.md`](docs/guide/services.md) for the full
schema and image-swap pipeline.

### Per-service `lok8s.yaml`

Services with `build: true` can carry a `lok8s.yaml` alongside their
source describing how Tilt should build and run them:

```yaml
# my-api/lok8s.yaml
build:
  dockerfile: service
  context: .
ports:
  - "3000:3000"
live_update:
  sync:
    - src/
  run:
    - npm run build
tilt:
  resource_deps: [redis]
```

The Tiltfile iterates active services, reads each `lok8s.yaml`, and
wires `docker_build` + `k8s_resource` accordingly.

### Label convention

Three standard labels on kustomize output, applied via `labels:` blocks
in target/addon kustomizations:

| Label             | Values                             | Purpose                           |
| ----------------- | ---------------------------------- | --------------------------------- |
| `lok8s.dev/name`  | addon or service name              | Identity — Tilt groups by this    |
| `lok8s.dev/type`  | `system`, `service`, `job`         | Partition (system → infra pool)   |
| `lok8s.dev/node`  | `all`, `core`, `database`, `none`  | Scheduling hints (node affinity)  |

The Tilt extension reads the compiled pool and partitions by
`lok8s.dev/type: system` first (applied as uncategorized
infrastructure), then by `lok8s.dev/name` per active service, and
whatever's left goes in as uncategorized.

---

## Build, deploy, and `lo up`

```
lo up <domain>
 ├─ provision              driver creates the cluster (kind/CAPI/...)
 ├─ bootstrap              framework applies spec.bootstrap addons
 │                         (.lok8s/libs/bootstrap) in order, waits
 │                         healthy between stages
 └─ tilt up                Tilt reads services.yaml, builds targets,
                           applies with service-enable filters, wires
                           docker_build + live_update
```

The headless primitives:

- **`lo build [target...]`** — per-target kustomize build. No args →
  build every target alphabetically. Args → build only those.
- **`lo deploy [target...]`** — per-target apply loop. For each target,
  extract CRDs and apply first (waits for establishment), then apply
  the rest, then wait for Deployments to become Available before
  moving on. Not an ordering primitive — per-target health waits are
  just good UX.
- **`lo env kustomization`** — builds all targets and writes the
  top-level `artifacts/kustomization.yaml` that references each
  `<target>/artifacts.yaml` as a resource, with image swaps generated
  from `services.yaml`.
- **`lo env services`** — prints the merged `services.yaml` +
  `services.<config>.yaml` as YAML (used by the Tiltfile).
- **`lo addons`** — lists framework bootstrap addons. See
  [`docs/guide/addons.md`](docs/guide/addons.md).
- **`lo lint`** — validates specs, bootstrap entries, target
  kustomization references, labels, secrets.
- **`lo status`** — cluster health + per-target build state.

---

## Deferred components

The following are intentionally incomplete while their designs settle:

- **CAPI bootstrap path** — `drivers/capi/main` provisions the work
  cluster and hands off to framework bootstrap, but the addon set for
  CAPI clusters (CCM, CSI, cert-manager) is still being defined.
  Currently only Cilium is applied by default.
- **`lo gitops flux|argo`** — stubbed with a deferred-error message.
  Will be rebuilt from the post-`services.yaml`-targets-map model to
  emit per-target Flux `Kustomization` / Argo `Application` resources
  with native ordering primitives (`dependsOn`, `sync-wave` annotations).
- **Deploy CRD workload selection** — `deploy.lok8s.yaml` currently
  carries only `clusterRef` + `namespace`. Target selection will land
  with the `services.yaml` targets-map design.
- **Provision lifecycle hooks** — pluggable PreProvision/PostProvision
  events so integrations can attach registration/teardown steps without
  patching the core provision path. Needs a design for the hook
  mechanism first.

---

## Metadata

| Key           | Value                                    |
| ------------- | ---------------------------------------- |
| Last rewrite  | 2026-06-11 (consolidated from STRUCTURE.md / STRUCTURE-PLAN.md) |
| Related docs  | [concepts.md](docs/guide/concepts.md), [specs.md](docs/reference/specs.md), [addons.md](docs/guide/addons.md) |
