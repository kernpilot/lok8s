# lok8s Operator

A Kubernetes operator that manages cluster lifecycle declaratively via CRDs.
Built on [shell-operator](https://github.com/flant/shell-operator); the
hooks are the `lo` binary itself (`lo operator <hook>`, package
`internal/operator`) behind two-line shims, sharing the same Go packages
as the `lo` CLI (drivers, bootstrap, gitops, deploy).

> **Status: alpha.** The `Lo` lifecycle is complete: creation, idempotent
> convergence with drift detection (3-minute schedule), kubeconfig
> publication as a Secret, and finalizer-guarded teardown on delete.
> `Capi` covers creation and status sync only — **deleting a `Capi`
> resource does not tear down the cluster**
> ([#6](https://github.com/kernpilot/lok8s/issues/6)). Don't point the
> Capi path at production credentials yet.

## How it works

The operator watches lok8s custom resources and reconciles them into real
infrastructure. It is the "controller mode" of lok8s — the same provisioning
logic that `lo provision` runs interactively, but event-driven inside a
management cluster.

```
CLI (lo provision)              Operator (shell-operator)
  you run a command               watches CRDs
  one-shot, synchronous           reconciliation loop
  runs from your machine          runs in lok8s-system namespace
       \                         /
        ── the same lo binary ──
        internal/driver/*  internal/bootstrap
        internal/operator (the hook bodies)
```

The typical flow:

1. `lo provision <domain>` bootstraps a management cluster from scratch
   (temporary kind cluster -> CAPI -> real VMs -> clusterctl move)
2. During bootstrap, the CLI **installs the operator** on the new management
   cluster (see `capi::bootstrap` in `.lok8s/drivers/capi/main`)
3. From that point on, you manage workload clusters by applying `Capi` CRDs
   to the management cluster — the operator handles reconciliation without
   needing the CLI

## Custom Resource Definitions

CRDs in the `cluster.lok8s.dev` API group:

### Capi (cluster.lok8s.dev/v1beta1) — primary use case

Provisions production clusters via [Cluster API](https://cluster-api.sigs.k8s.io/).
The operator translates the simplified lok8s spec into verbose CAPI manifests
(Cluster, KubeadmControlPlane, MachineDeployment, infrastructure resources)
using `envsubst` templates.

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: prod
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: prod.example.com
  managementCluster:
    domain: mgmt.example.com
  hcloud:
    region: fsn1
    sshKeyName: my-key
  controlPlane:
    replicas: 3
    type: cax21
  workers:
    general:
      replicas: 3
      type: cax21
  gitops:
    provider: flux
    repo: https://github.com/myorg/infra.git
```

What happens when you apply this:

1. **`capi-reconcile.sh`** detects the event
2. Detects provider from spec (`.hcloud` -> hetzner, `.aws` -> aws)
3. Renders CAPI resources from templates via `envsubst`
4. `kubectl apply` the generated manifests — CAPI takes over and provisions VMs
5. **`capi-status-sync.sh`** watches the CAPI `Cluster` object
   (labeled `lok8s.dev/managed: "true"`)
6. Maps CAPI status back to the `Capi` CR status fields
7. When CAPI reports `Provisioned`:
   - Extracts kubeconfig via `clusterctl` -> stores as a Secret
   - Bootstraps GitOps (flux/argo) if `spec.gitops` is set
   - Runs `deploy::apply` for direct deployment if no GitOps configured

```
kubectl get capi
NAME   PHASE         READY   DOMAIN             PROVIDER   AGE
prod   Provisioned   true    prod.example.com   hetzner    10m
```

Supported providers: **hetzner** (hcloud + hrobot), **aws** (scaffold).

### Lo (cluster.lok8s.dev/v1beta1) — local/CI clusters

Represents local development clusters using kind. Intended for scenarios where
an in-cluster controller manages dev environments (e.g., ephemeral CI clusters).

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  cluster:
    domain: lok8s.dev
  runtime: kind
  nodes:
    controlPlane: 1
    workers: 0
  network:
    name: lok8s
    cidr: "10.125.125.0/24"
  registries:
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
  loadBalancer:
    pool: "10.125.125.125-10.125.125.150"
  bootstrap:
    - cilium
```

For `*.lok8s.dev` domains, most of these fields are slot-derived
defaults — see [specs.md](../docs/reference/specs.md#default-resolution).

> **Note:** The Lo hook calls `driver::provision` from the kind provider, which
> requires Docker access. This only works if the operator pod has a Docker
> socket mounted. For normal local development, use `lo up` instead.

```
kubectl get lo
NAME    PHASE         READY   DOMAIN      AGE
local   Provisioned   true    lok8s.dev   5m
```

### Deploy (cluster.lok8s.dev/v1beta1) — deployment domains

References an existing cluster. No reconciliation hook is implemented yet —
this CRD currently serves as a declarative record of "deploy something to
that cluster." Workload selection is being reworked alongside the
`services.yaml` targets-map redesign.

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: api
spec:
  clusterRef:
    domain: prod.example.com
  namespace: api
```

## Hooks

All hooks live in `operator/hooks/` and follow the
[shell-operator hook contract](https://github.com/flant/shell-operator/blob/main/docs/src/HOOKS.md)
(`--config` returns the binding config, otherwise the hook runs against
`$BINDING_CONTEXT_PATH`).

| Hook | Implementation | Watches | Events | Purpose |
|------|----------------|---------|--------|---------|
| `lo-reconcile.sh` | `lo operator lo-reconcile` (`internal/operator/lo.go`) | `Lo` CRDs | Added, Modified, schedule `*/3` | Provisions local clusters via the lo (kind) driver, publishes the kubeconfig, finalizer-guarded teardown |
| `capi-reconcile.sh` | `lo operator capi-reconcile` (`internal/operator/capi.go`) | `Capi` CRDs | Added, Modified, schedule `*/3` | Generates and applies CAPI manifests, finalizer-guarded teardown |
| `capi-status-sync.sh` | `lo operator capi-status-sync` (`internal/operator/capistatus.go`) | CAPI `Cluster` objects | Modified | Syncs CAPI status -> lok8s Capi CR, triggers post-provision |

Each file under `operator/hooks/` is a shim — `exec lo operator <hook> "$@"`
— because shell-operator discovers hooks by executable path. The hook
bodies are Go: `lo operator <hook> --config` prints the binding
configuration (byte-identical to the bash heredocs it replaced, pinned by
`internal/operator/testdata/*.config.yaml`), otherwise the hook reads the
binding context and reconciles through the ported packages
(`internal/driver/lo`, `internal/bootstrap`, `internal/gitops`,
`internal/deploy`) with the same kubectl calls, status patches and log
lines the bash produced.

The bash hooks they replaced are frozen at
`.lok8s/legacy/operator/hooks/` (with their `runtime.sh`) — the reference
`hack/parity-operator.sh` measures the Go port against, and what the bats
suite still sources.

Runtime layout (`internal/operator.Env`, the port of `runtime.sh`): the
state volume `LOK8S_STATE_DIR` (default `/var/lib/lok8s`) is `PATH_BASE`,
laid out like a lok8s project (`clusters/<domain>/cluster.lok8s.yaml`,
`.kubeconfig/<cluster>.yaml`); the hook tree `PATH_LOK8S` (default
`/hooks`) carries the framework pieces (`addons/`, `drivers/`,
`capi-templates/`); `KUSTOMIZE_PLUGIN_HOME` defaults to
`/usr/local/kustomize-plugins`.

### capi-reconcile.sh flow

```
Capi CR created/modified
  |
  v
Detect provider from spec (.hcloud -> hetzner, .aws -> aws)
  |
  v
Export variables: CLUSTER_NAME, K8S_VERSION, CP_REPLICAS, HCLOUD_REGION, ...
  |
  v
Render templates: core/*.yaml + providers/<provider>/*.yaml via envsubst
  |
  v
kubectl apply -f <rendered manifests>
  |
  v
Patch Capi CR status -> Provisioning
```

### capi-status-sync.sh flow

```
CAPI Cluster status changes (label: lok8s.dev/managed=true)
  |
  v
Map CAPI phase -> lok8s phase (Provisioned, Provisioning, Failed)
  |
  v
Patch Capi CR status (phase, ready, controlPlaneEndpoint)
  |
  v
If Provisioned:
  ├── clusterctl get kubeconfig -> store as Secret
  ├── If spec.gitops: gitops::bootstrap (flux/argo)
  └── Else: deploy::apply (direct target deployment)
```

## Installation

### Prerequisites

- A Kubernetes cluster (the management cluster)
- [Cluster API](https://cluster-api.sigs.k8s.io/) installed with the
  appropriate infrastructure provider (e.g., `clusterctl init --infrastructure hetzner`)
- Provider credentials as Secrets on the management cluster

### Manual install

```bash
# Apply CRDs
kubectl apply -f operator/crds/

# Deploy the operator
kubectl apply -k operator/deploy/
```

### Via CLI bootstrap (recommended)

```bash
# Bootstrap a management cluster from scratch — the operator is installed
# automatically as part of the bootstrap flow
lo provision <management-cluster-domain>
```

The `capi::bootstrap` function in the CLI:
1. Creates a temporary kind cluster
2. Installs CAPI on it
3. Provisions the real management cluster via CAPI
4. Installs the lok8s operator on the new management cluster
5. Installs CAPI on the management cluster
6. `clusterctl move` migrates state from temp to real
7. Applies `spec.bootstrap` addons on the management cluster via the framework bootstrap (`.lok8s/libs/bootstrap`)
8. Deletes the temporary kind cluster

## Container image

Built from the project root:

```bash
docker build -t ghcr.io/kernpilot/lok8s-operator:0.1.0 -f operator/Dockerfile .
```

Base image: `ghcr.io/flant/shell-operator:v1.19.5`; a `golang:1.25-alpine`
build stage compiles the `lo` binary from the same tree.

Bundled tools: lo, kubectl, kustomize, yq, jq, clusterctl, flux, kind, docker-cli, git, openssh-client, envsubst, khelm

What gets copied into the container:

| Source | Destination | Purpose |
|--------|-------------|---------|
| build stage `/out/lo` | `/usr/local/bin/lo` | The hook bodies (`lo operator <hook>`) |
| `operator/hooks/` | `/hooks/` | shell-operator hook shims |
| `.lok8s/legacy/operator/hooks/` | `/hooks/legacy/` | Frozen bash hooks (reference / manual fallback; not executable) |
| `.lok8s/drivers/` | `/hooks/drivers/` | Bash driver contracts (used by the legacy hooks) |
| `.lok8s/providers/` | `/hooks/providers/` | Physical infra providers (hcloud, ...) |
| `.lok8s/utils/` | `/hooks/utils/` | IP arithmetic, shared utilities |
| `.lok8s/libs/` | `/hooks/lib/` | Shared bash libraries (used by the legacy hooks) |
| `.lok8s/addons/` | `/hooks/addons/` | Bootstrap addons (the Go bootstrap engine resolves `PATH_LOK8S/addons`) |
| `.lok8s/drivers/capi/cluster/` | `/hooks/capi-templates/` | CAPI templates rendered by `capi-reconcile` |
| `operator/crds/` | `/crds/` | CRD definitions (applied on startup) |

## Deployment details

- **Namespace:** `lok8s-system`
- **Service account:** `lok8s-operator` with ClusterRole/ClusterRoleBinding
- **Replicas:** 1
- **Resources:** 100m/500m CPU, 128Mi/256Mi memory
- **Security:** non-root (uid 65534), no privilege escalation, all capabilities dropped

### RBAC scope

The operator ClusterRole grants access to:

- lok8s CRDs (`cluster.lok8s.dev`) — full CRUD
- CAPI resources (`cluster.x-k8s.io`, `controlplane.cluster.x-k8s.io`,
  `bootstrap.cluster.x-k8s.io`, `infrastructure.cluster.x-k8s.io`) — full CRUD
- Core resources (secrets, configmaps, events, namespaces) — read + create
- Workload resources (deployments, statefulsets, daemonsets, services) — full CRUD for target deployment
- RBAC resources — full CRUD for target deployment
- CRDs (`apiextensions.k8s.io`) — for applying lok8s CRDs on startup
- Leases (`coordination.k8s.io`) — shell-operator leader election

## CI/CD

| Workflow | What it does |
|----------|-------------|
| `docker.yml` | Builds and pushes `ghcr.io/kernpilot/lok8s-operator` on main/tags (multi-arch: amd64, arm64) |
| `ci.yml` | ShellCheck on hooks, YAML lint on manifests, bats unit tests |
| `security.yml` | Trivy vulnerability scan on the operator image |
| `release.yml` | Includes `operator/crds/` and `operator/deploy/` in release tarball |

## Testing

```bash
# The hook bodies: hermetic Go tests (fake kubectl/clusterctl runner, fake
# lo driver) replicating every bats case + the --config goldens
go test ./internal/operator/

# The frozen bash reference + the shims (passthrough, --config parity
# through the built binary)
make build && bats tests/operator/hooks_test.bats

# Differential: Go hooks vs the frozen bash hooks against stubbed
# kubectl/clusterctl (call logs, patch bodies, exit codes)
make build && bash hack/parity-operator.sh
```

## File structure

```
operator/
├── Dockerfile                  # Container image (shell-operator + tools + hooks)
├── README.md                   # This file
├── crds/
│   ├── lo.yaml                 # Lo CRD (local/CI clusters)
│   ├── capi.yaml               # Capi CRD (production clusters)
│   └── deploy.yaml             # Deploy CRD (deployment domains)
├── deploy/
│   ├── kustomization.yaml      # Kustomize entry point
│   ├── namespace.yaml          # lok8s-system namespace
│   ├── service-account.yaml    # lok8s-operator SA
│   ├── rbac.yaml               # ClusterRole + ClusterRoleBinding
│   └── deployment.yaml         # Operator Deployment
└── hooks/
    ├── lo-reconcile.sh         # shim → lo operator lo-reconcile
    ├── capi-reconcile.sh       # shim → lo operator capi-reconcile
    ├── capi-status-sync.sh     # shim → lo operator capi-status-sync
    └── runtime.sh              # retired pointer (the bash runtime moved with the legacy hooks)

internal/operator/              # the hook bodies (Go)
├── operator.go                 # runtime layout (Env), binding context, Hook interface
├── lo.go / capi.go / capistatus.go
└── testdata/*.config.yaml      # --config goldens, generated once from the bash hooks

.lok8s/legacy/operator/hooks/   # the frozen bash hooks + runtime.sh (reference)
```

## Current limitations

- **Lo CRD:** Requires Docker socket access in the operator pod — not wired up
  in the default deployment. Use `lo up` for local development instead.
- **Deploy CRD:** No reconciliation hook implemented yet. The CRD exists as a
  spec but the operator doesn't act on it.
- **Provider support:** Only Hetzner is fully implemented (`.lok8s/drivers/capi/cluster/providers/hetzner/`). AWS is scaffolded in
  the hook but has no templates.
- **No leader election:** The deployment runs a single replica. The shell-operator
  supports leader election via leases (RBAC is in place) but it's not configured.
