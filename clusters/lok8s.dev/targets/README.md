# `targets/` — the workload plane

This directory is where **your cluster's workloads** live. Each
subdirectory is an independent kustomize target. Everything under
`targets/` gets built into `artifacts/<target>/artifacts.yaml` and
eventually applied to the cluster.

> Cluster **infrastructure** (CNI, CSI, MetalLB, cert-manager CRDs, ...)
> does **not** live here. That lives in the cluster spec's
> `spec.bootstrap: []` and is applied by `driver::bootstrap` at
> provision time, before anything under `targets/` runs.
> See [Concepts — Two Deployment Planes](https://kernpilot.github.io/lok8s/guide/concepts#two-deployment-planes).

## What is a target?

A target is a user-named kustomize directory with its own
`kustomization.yaml`. There are **no hardcoded names**. You pick the
names that make sense for your project (`platform/`, `ingress/`,
`monitoring/`, `apps/`, `api/`, ...). Each target is a *molecule* that
composes one or more addons, services, and in-repo manifests into one
unit of deployable content.

```text
targets/
├── platform/
│   └── kustomization.yaml       # e.g. cert-manager + ingress overlays
├── monitoring/
│   └── kustomization.yaml       # e.g. prometheus stack
└── apps/
    ├── kustomization.yaml
    ├── my-api.yaml
    └── my-worker.yaml
```

A minimal target:

```yaml
# targets/apps/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
labels:
  - includeSelectors: true
    pairs:
      lok8s.dev/name: my-api          # Tilt groups by this label
      lok8s.dev/type: service
resources:
  - my-api.yaml
```

## How to reference framework addons from a target

Framework-shipped addons live at `.lok8s/addons/<name>/` (shared across
drivers). A target can pull them in as kustomize bases:

```yaml
# targets/platform/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../../.lok8s/addons/cert-manager/
  - ./ingress-routes.yaml
```

(The relative path is ugly but deliberate — kustomize does not resolve
symlinks reliably.) See the [Addons Guide](https://kernpilot.github.io/lok8s/guide/addons)
for addon authoring and the full vendor matrix.

## How targets get built

`lo build` iterates every subdirectory of `targets/` alphabetically,
runs `kustomize build --enable-alpha-plugins` per target, pipes the
output through `envsubst` with the `LOK8S_SPEC_*` / `LOK8S_USER_*`
whitelist, and writes each target's rendered manifest to:

```text
clusters/<domain>/artifacts/<target>/artifacts.yaml
```

Then `lo env kustomization` writes a top-level
`artifacts/kustomization.yaml` that references every
`<target>/artifacts.yaml` as a resource, plus image swaps generated
from `services.yaml`. That top-level file is what Tilt reads.

`lo build` can also be scoped to specific targets:

```bash
lo build                           # every target under targets/
lo build platform apps             # just these two
```

There is **no ordering** between targets. `lo build` and `lo deploy`
both iterate alphabetically — that order is not semantic. If you need
runtime ordering, express it inside Tilt (via `resource_deps`) or in
the manifests themselves. Cluster-infra ordering lives in
`spec.bootstrap`, not here.

## How targets get deployed

### Via Tilt (`lo up` — the dev flow)

`lo up` runs:

```text
provision  → driver creates the cluster
bootstrap  → spec.bootstrap addons applied with health waits
tilt up    → Tilt reads services.yaml + targets/, builds, applies
```

The Tilt extension (`.lok8s/tilt/Tiltfile`) does a single
`kustomize build` over `clusters/<domain>/artifacts/` (which contains
every target's rendered output), then partitions the unified pool:

1. Resources labeled `lok8s.dev/type: system` are pulled out and
   applied first as infrastructure (rare in practice — most infra is
   in `spec.bootstrap` now).
2. Each active service from `services.yaml` claims its matching
   resources by the `lok8s.dev/name: <service>` label; Tilt wires
   `docker_build` + `k8s_resource` for services with a `build:` block.
3. Anything unclaimed becomes "uncategorized" and Tilt auto-applies it.

So a target's resources flow to Tilt automatically. To make a target
live-reloadable, label its resources with `lok8s.dev/name: <name>` and
add a matching entry to `services.yaml`.

### Via `lo deploy` (headless / CI)

`lo deploy` is the same pipeline without Tilt's live-update layer:

```bash
lo deploy                          # apply every target alphabetically
lo deploy platform                 # apply just one target
lo deploy --filter type=system     # label filter across all targets
```

Each target's `artifacts/<target>/artifacts.yaml` is applied with a
two-phase sweep (CRDs first, then the rest), and `lo deploy` waits for
Deployments to become Available before moving to the next target.
Per-target health waits are good UX, not a semantic ordering primitive.

## Labels convention

Three standard labels on resources (applied via kustomize `labels:`):

| Label             | Values                             | Purpose                          |
| ----------------- | ---------------------------------- | -------------------------------- |
| `lok8s.dev/name`  | service/target name                | Identity — Tilt groups by this   |
| `lok8s.dev/type`  | `system`, `service`, `job`         | Partition (system → infra pool)  |
| `lok8s.dev/node`  | `all`, `core`, `database`, `none`  | Node affinity hints              |

## Getting started

This directory is **empty by default** — lok8s ships no example
workload targets because every project's platform layer looks
different. Create one when you're ready:

```bash
mkdir -p targets/apps
cat > targets/apps/kustomization.yaml <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
labels:
  - includeSelectors: true
    pairs:
      lok8s.dev/name: hello
      lok8s.dev/type: service
resources:
  - deployment.yaml
YAML

cat > targets/apps/deployment.yaml <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hello
spec:
  replicas: 1
  selector:
    matchLabels: { app: hello }
  template:
    metadata:
      labels: { app: hello }
    spec:
      containers:
        - name: hello
          image: nginx:alpine
YAML

lo build                            # builds targets/apps/
lo deploy apps                      # applies it to the cluster
# ... or `lo up` to get the full bootstrap + Tilt flow
```

## See also

- [Concepts](https://kernpilot.github.io/lok8s/guide/concepts) — two planes, build/deploy pipeline
- [Addons](https://kernpilot.github.io/lok8s/guide/addons) — how to write and reference addons
- [Specs Reference](https://kernpilot.github.io/lok8s/reference/specs) — `spec.bootstrap` and related fields
- [Services Configuration](https://kernpilot.github.io/lok8s/guide/services) — `services.yaml` and Tilt wiring
