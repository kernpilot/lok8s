# Deploying to Clusters

lok8s uses a two-step pipeline: **build** kustomize targets into per-target artifacts, then **deploy** artifacts to a cluster.

Workload targets have no framework-level ordering — `lo build` and `lo deploy` iterate alphabetically. Cluster-infrastructure ordering lives in `spec.bootstrap` and runs during `lo up` via the framework bootstrap (`.lok8s/libs/bootstrap`), before Tilt / workloads. See the [Addons guide](./addons.md) for the bootstrap model.

## Build

```bash
lo build [domain] [target...]
```

For each target directory under `clusters/<domain>/targets/`, runs `kustomize build --enable-alpha-plugins` and writes the output to `clusters/<domain>/artifacts/<target>/artifacts.yaml`.

```bash
# Build all targets for the active domain
lo build

# Build all targets for a specific domain
lo build example.com

# Build specific targets only
lo build example.com networking monitoring
```

### Split Output

Use `--split` to produce individual files per resource instead of a single `artifacts.yaml`:

```bash
lo build --split example.com
```

This creates files like `Deployment.default.my-app.yaml`, `Service.default.my-app.yaml`, etc. Useful for GitOps workflows where you want to review individual resources.

## Deploy

```bash
lo deploy [domain] [target...]
```

Applies per-target artifacts to the cluster. Targets are discovered from `clusters/<domain>/targets/<name>/` alphabetically, or supplied explicitly as positional arguments. Workload-plane ordering is intentionally not a framework primitive — kubectl handles in-manifest order; Tilt handles runtime deps via `resource_deps`; cluster-infra ordering lives in `spec.bootstrap`.

```bash
# Deploy all targets for the active domain
lo deploy

# Deploy a specific domain
lo deploy example.com

# Deploy specific targets
lo deploy example.com networking
```

### Deployment Phases

For each target, deployment follows three phases:

1. **CRDs first** — CustomResourceDefinition resources are extracted and applied separately, then the deploy waits for them to become Established
2. **Apply resources** — all resources in the target's `artifacts.yaml` are applied via `kubectl apply`
3. **Wait for health** — waits for all Deployments across all namespaces to become Available (default timeout: 120s)

### Label Filtering

Deploy specific resource types using the `--filter` flag:

```bash
# Deploy only system-type resources
lo deploy --filter type=system example.com
```

This filters resources by `lok8s.dev/<key>` labels. Resources are matched across all targets.

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
  namespace: api
```

> **Note:** Deploy CRD workload selection is being reworked post-refactor.
> Target selection for Deploy specs will land alongside the
> `services.yaml` targets-map design.

Build and deploy work the same way:

```bash
lo build api.example.com
lo deploy api.example.com
```

The deploy command uses the kubeconfig from the referenced cluster domain.

## Full Lifecycle: Provision

The `lo provision` command runs the full lifecycle for a cluster domain:

```bash
lo provision example.com
```

1. Creates the cluster (via driver contract)
2. Applies `spec.bootstrap` addons via the framework bootstrap
3. Registers with kubehz (if `spec.kubehz.access` is set)

Workload deployment is handled separately by `lo deploy` (headless/CI) or Tilt (local dev) — it is not part of `provision`.

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
prints its SSH-key **MD5 fingerprint** — both to the provision log and, in the
reusable workflow, to the GitHub job summary.

To attach the cluster to your account, open the dashboard **Claim** page and
provide two things:

1. that **MD5 fingerprint**, and
2. **your own Hetzner Cloud token** — used once to prove you control the account
   the cluster's SSH key lives in. It is never stored.

No platform/API token is needed in CI: ownership is proven interactively at claim
time, not at provision time. You can reproduce the fingerprint locally with:

```bash
ssh-keygen -E md5 -lf ~/.ssh/id_ed25519.pub
```

## Destroy

```bash
lo destroy example.com
```

Tears down the cluster via the driver contract's `driver::destroy` function.
