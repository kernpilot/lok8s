# The Operator

The lok8s operator runs on a management cluster and watches lok8s custom resources. It uses [shell-operator](https://github.com/flant/shell-operator) with bash hooks that reuse the same library code as the CLI.

::: warning Alpha
The `Lo` lifecycle is complete — creation, drift detection, kubeconfig publication, and finalizer-guarded teardown. `Capi` covers creation and status sync only: **deleting a `Capi` resource does not tear down the cluster** ([#6](https://github.com/kernpilot/lok8s/issues/6)). Don't point the Capi path at production credentials yet.
:::

## Architecture

```
CLI mode (lo)                    Operator mode (shell-operator)
  synchronous                      event-driven
  runs from disk                   watches CRDs
  one-shot                         reconciliation loop
       \                          /
        \                        /
     Shared bash libraries
     .lok8s/libs/*
     .lok8s/drivers/*/main
```

The operator container bundles the same libraries and driver contracts as the CLI. Hooks source the libraries with an `import() { :; }` shim (since `import` is an argsh builtin that doesn't exist in plain bash).

## Custom Resource Definitions

The operator defines its CRDs in the `cluster.lok8s.dev` API group:

### Lo (cluster.lok8s.dev/v1beta1)

For local/CI clusters:

```yaml
# Minimal form — the framework derives everything from the domain
# (slot 125) and domain-independent defaults. See the Specs reference
# for the full defaulting table.
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
  namespace: default
spec:
  cluster:
    domain: lok8s.dev
  bootstrap:
    - cilium
    - metallb    # opt in; default is [cilium] only
```

```bash
kubectl get lo
# NAME    PHASE         READY   DOMAIN      AGE
# local   Provisioned   true    lok8s.dev   5m
```

### Capi (cluster.lok8s.dev/v1beta1)

For production clusters via Cluster API:

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: prod
spec:
  kubernetes:
    version: "v1.31.12"
  cluster:
    domain: prod.example.com
  managementCluster:
    domain: prod-mgmt.example.com
    local: true
  provider:
    name: hetzner
    config:
      region: fsn1
      sshKeyName: my-key
    credentials:
      envVars: [HCLOUD_TOKEN]
  controlPlane:
    replicas: 3
    type: cpx22
  workers:
    general:
      replicas: 3
      type: cpx22
  bootstrap:
    - cilium
    - ccm
```

```bash
kubectl get capi
# NAME   PHASE         READY   DOMAIN             PROVIDER   AGE
# prod   Provisioned   true    prod.example.com   hetzner    10m
```

### Deploy (cluster.lok8s.dev/v1beta1)

For deployment domains that target an existing cluster:

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: api
spec:
  clusterRef:
    domain: prod.example.com
```

> **Note:** Deploy CRD workload selection is being reworked — target
> selection will land alongside the `services.yaml` targets-map
> redesign. For now, a Deploy spec only carries `clusterRef`.

```bash
kubectl get deploys.cluster.lok8s.dev
# NAME   PHASE      CLUSTER            AGE
# api    Deployed   prod.example.com   3m
```

## Hooks

### lo-reconcile.sh

Full lifecycle for `Lo` resources — the same driver contract as the CLI:

1. **Create/Modify**: adds a `lok8s.dev/lo-teardown` finalizer, checks
   `driver::status` first (idempotent — a `Running` cluster is never
   re-provisioned), then `driver::provision` + framework bootstrap
   (`spec.bootstrap` addons), publishes the kubeconfig as Secret
   `<name>-kubeconfig`, and sets `status.kubeconfig.secretRef`.
2. **Delete**: the finalizer holds the object while `driver::destroy`
   tears the cluster down; on success the finalizer is removed and the
   kubeconfig Secret deleted. A failed teardown keeps the finalizer and
   is retried.
3. **Drift**: a `*/3` schedule re-lists every `Lo` and converges — a
   cluster deleted out-of-band is re-provisioned, a finished manual
   teardown is detected.

### capi-reconcile.sh

Watches `Capi` resources. On `Added`/`Modified` events:

1. Updates status to `Provisioning`
2. Detects the CAPI provider from the spec
3. Generates CAPI resources from templates
4. Applies resources to the cluster
5. Status sync is handled by `capi-status-sync.sh`

### capi-status-sync.sh

Watches `cluster.x-k8s.io/v1beta1 Cluster` resources with the `lok8s.dev/managed: "true"` label. When a CAPI Cluster's status changes:

1. Maps CAPI phase to lok8s phase
2. Updates the corresponding Capi CR status
3. On `Provisioned`: extracts kubeconfig, bootstraps GitOps or runs direct deploy

## Installation

```bash
# Apply CRDs
kubectl apply -f operator/crds/

# Deploy the operator (Capi reconciliation only)
kubectl apply -k operator/deploy/

# OR: deploy with the Lo driver enabled (kind clusters)
kubectl apply -k operator/deploy/lo/
```

The operator runs in the `lok8s-system` namespace with a dedicated service account and RBAC rules.

The `lo/` overlay mounts the node's Docker socket, enables host
networking (kind kubeconfigs point at host ports), and runs as root —
a deliberate trade-off for CI and single-node management hosts. Do not
apply it on multi-tenant clusters.

## Container Image

The operator image is built from the project root:

```bash
docker build -t ghcr.io/kernpilot/lok8s-operator:0.1.0 -f operator/Dockerfile .
```

The Dockerfile:
- Starts from `ghcr.io/flant/shell-operator:v1.14.0`
- Installs kubectl, kustomize, yq, jq, docker-cli, kind, clusterctl, flux, git, openssh-client, gettext (for `envsubst`), and the khelm kustomize plugin (bootstrap addon charts)
- Copies hooks, driver contracts, libraries, bootstrap addons, CAPI templates, and CRDs
- Strips exec bits from the library trees (shell-operator treats every executable under `/hooks` as a hook)
