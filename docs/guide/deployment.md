# Deploying to Clusters

lok8s uses a two-step, **domain-based** pipeline: **build** the domain's
kustomization into ONE artifact, then **deploy** that artifact to a cluster.

> **Breaking change (domain-based build/deploy).** This replaced the old
> per-target model. `lo build`/`lo deploy` no longer loop over
> `targets/*/` on their own and no longer write
> `clusters/<domain>/artifacts/<target>/artifacts.yaml`. Instead **you** compose
> the domain's targets in `clusters/<domain>/kustomization.yaml`, and the
> framework renders that into a single `clusters/<domain>/artifacts.yaml`. There
> is no per-target `lo build <target>`. Domains with
> `spec.build.artifacts: split` (or a `spec.gitops.provider`) additionally
> emit committable
> per-resource files under `clusters/<domain>/artifacts/` — Secrets
> sops-encrypted — for GitOps consumers (see the
> [spec reference](../reference/specs.md)); `lo deploy` always applies the
> single `artifacts.yaml`.

The active domain is selected with `lo use <domain>` (or per-command with
`--domain <domain>`); the examples below assume it is set.

## Compose the domain

Author `clusters/<domain>/kustomization.yaml` — a normal kustomization whose
`resources:` list the targets you want in this domain. Targets may be **local**
(under the domain's own `targets/`) or **shared** (a repo-level tree such as
`.targets/`, referenced with a relative path):

```yaml
# clusters/example.com/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./targets/networking      # local to this domain
  - ./targets/monitoring      # local to this domain
  - ../../.targets/cert-manager  # shared across domains
```

Each entry is a directory with its own `kustomization.yaml` (a kustomize base),
so ordering and composition are entirely under your control. Cluster-infrastructure
ordering still lives in `spec.bootstrap` and runs during `lo up`/`lo provision`
before workloads — see the [Addons guide](./addons.md).

## Build

```bash
lo build            # active domain
lo build --domain example.com
```

Runs `kustomize build --enable-alpha-plugins` over
`clusters/<domain>/kustomization.yaml` (with the secrets/khelm plugins, per-domain
secret isolation, and the `LOK8S_*` envsubst pass) and writes the result to a
single file:

```
clusters/<domain>/artifacts.yaml
```

If the domain has no `kustomization.yaml`, the build fails with:

```
domain example.com has no kustomization.yaml — compose its targets there,
e.g. resources: [./targets/foo, ../../.targets/bar]
```

## Deploy

```bash
lo deploy            # active domain
lo deploy --domain example.com
```

Applies the single `clusters/<domain>/artifacts.yaml` to the cluster in two phases:

1. **CRDs first** — `CustomResourceDefinition` resources are extracted and applied
   server-side, then the deploy waits for them to become Established.
2. **Apply the rest + wait** — the whole artifact is applied server-side
   (re-applying the CRDs is a no-op), then the deploy waits for the artifact's own
   Deployments/DaemonSets/StatefulSets to become ready (best-effort, 120s).

Workload-plane ordering is intentionally not a framework primitive — kubectl
handles in-manifest order; Tilt handles runtime deps via `resource_deps`;
cluster-infra ordering lives in `spec.bootstrap`.

### Selective deploy (`-l` / `--label`)

Deploy only the resources carrying a given label with `-l key=value` (optional —
omit it to apply everything):

```bash
# Deploy only the objects labelled lok8s.dev/name=zitadel
lo deploy -l lok8s.dev/name=zitadel

# The key may be any label key, not just lok8s.dev/*
lo deploy -l app=frontend
```

Selective deploy is **opt-in**: it matches on labels you put on your resources, so
it only works if those resources actually carry the label. The idiomatic way is a
kustomize `labels:` block on the target you want to address:

```yaml
# clusters/example.com/targets/zitadel/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
labels:
  - pairs:
      lok8s.dev/name: zitadel
    includeSelectors: false
resources:
  - deployment.yaml
  - configmap.yaml
```

With that, `lo deploy -l lok8s.dev/name=zitadel` applies exactly that target's
objects out of the composed artifact. lok8s does **not** require or lint these
labels — add them only for the subsets you want to address individually.

## Deployment Domains

Deployment domains let you deploy content to another domain's cluster. They have a `deploy.lok8s.yaml` with a `clusterRef`:

```yaml
# clusters/api.example.com/deploy.lok8s.yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: api
spec:
  clusterRef:
    domain: example.com    # deploys to this cluster
```

A deploy domain composes its own targets in its own
`clusters/api.example.com/kustomization.yaml`, exactly like a cluster domain.
Build and deploy work the same way — they just apply to the referenced cluster's
kubeconfig:

```bash
lo build --domain api.example.com
lo deploy --domain api.example.com
# selective deploy works here too:
lo deploy --domain api.example.com -l lok8s.dev/name=api
```

## Full Lifecycle: Provision

The `lo provision` command runs the full lifecycle for a cluster domain:

```bash
lo provision example.com
```

1. Creates the cluster (via driver contract)
2. Applies `spec.bootstrap` addons via the framework bootstrap
3. Registers with kubehz (if `spec.kubehz.access` is set)

Workload deployment is handled separately by `lo deploy` (headless/CI) or Tilt (local dev) — it is not part of `provision`.

### Re-applying bootstrap only (`-b`)

`spec.bootstrap` addons (Cilium, cert-manager, Rook, …) are applied — in
`dependsOn` order with `wait:` health gates — as the last step of `lo provision`.
To **re-apply that bootstrap graph on an already-provisioned cluster** without the
infrastructure reconcile (no `hcloud`/`kubeone` calls, no Hetzner API check), pass
`-b` / `--bootstrap`:

```bash
lo provision my-cluster.example.com --bootstrap
```

It skips the provider reconcile and `driver::provision`, then runs the **same
bootstrap DAG** against the existing kubeconfig. Because the `dependsOn` / `wait:`
gates still fire, this is the gated way to **roll a bootstrap-addon upgrade**: bump
the addon's pinned version (the Rook operator chart, a `cephVersion`, …), then
`lo provision -b`. That's safer than `lo build <target>` + `kubectl apply`, which
applies the target directly and **bypasses** the dependency graph and its health
gates.

Requires a cluster that has already been provisioned (an existing
`.kubeconfig/<cluster-name>.yaml`); otherwise it errors and changes nothing.

## Provisioning from CI

You can run `lo provision` from GitHub Actions to spin up a committed cluster on
every push (or on demand). A reusable workflow ships with lok8s:

```yaml
# .github/workflows/spinup.yml in your repo
jobs:
  spinup:
    uses: kernpilot/lok8s/.github/workflows/spinup.yml@main
    with:
      domain: my-cluster.example.com
    secrets:
      HCLOUD_TOKEN: ${{ secrets.HCLOUD_TOKEN }}
```

It needs **only** `HCLOUD_TOKEN` (your Hetzner Cloud API token, used to create the
infrastructure). The `cluster.lok8s.yaml` for the domain must already be committed
under `clusters/<domain>/`.

### Claiming a registered cluster

When a cluster opts into kubehz dashboard visibility (`spec.kubehz.access` is set
to `registered` or `managed`), `lo provision` registers it as **pending** and
prints its SSH-key **MD5 fingerprint**. You then claim the cluster in the
dashboard with that fingerprint plus your own Hetzner Cloud token (used once as
ownership proof, never stored) — no platform token is needed in CI.

See the [kubehz Platform guide](./kubehz.md#claiming) for the full flow:
registration, claiming, the heartbeat agent, and troubleshooting.

## Destroy

```bash
lo destroy example.com
```

Tears down the cluster via the driver contract's `driver::destroy` function.
