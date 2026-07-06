# Cluster Inventory

lok8s records **what it deployed** into the cluster itself: a cluster-scoped
singleton `ClusterInventory` named `cluster` (group `lok8s.dev/v1alpha1`),
written by `lo` at the end of `lo provision`, `lo up` and `lo bootstrap`.

Think of it as lok8s's answer to OpenShift's `ClusterVersion`: one well-known
object with a clean **spec/status split** — the CLI owns `spec`, an in-cluster
observer owns `status` (via the status subresource).

```bash
kubectl get clusterinventory cluster
# NAME      K8S        AGE
# cluster   v1.31.10   2d

kubectl get clusterinventory cluster -o yaml
```

## What is stored — and what never is

The inventory is **strictly metadata**:

| `spec` field | Meaning |
|---|---|
| `lok8sVersion` | The framework version that rendered the deploy (`.lok8s/VERSION`) |
| `kind` | Cluster driver (`lo`, `kubeone`, `capi`, `kkp`) |
| `provider` | Infrastructure provider (`spec.provider.name`, e.g. `hetzner`) |
| `kubernetesVersion` | `spec.kubernetes.version` (any `@sha256` digest stripped) |
| `specHash` | sha256 of the `cluster.lok8s.yaml` bytes the deploy was built from |
| `renderedAt` | When `lo` built the inventory (UTC) |
| `addons[]` | The resolved `spec.bootstrap` entries: `name`, `chartVersion`, `appVersion`, `category`, `source` (`addon` or `target`) |

**Never stored:** chart values, `env:` overrides, credentials, kubeconfigs or
rendered manifests. This is enforced *structurally*, not just by convention:
the CRD schema enumerates every field and carries no
`x-kubernetes-preserve-unknown-fields` marker anywhere, so the apiserver
prunes anything beyond the fields above. There is simply no place in the
schema where a values blob or a secret could live.

`specHash` lets you check drift without exposing the spec: if the hash in the
cluster differs from the sha256 of your local `cluster.lok8s.yaml`, the
cluster was last deployed from a different spec revision.

## Who writes what

| Field | Writer | How |
|---|---|---|
| `spec` | `lo` (field manager `lok8s`) | Server-side apply at the end of provision/bootstrap |
| `status` | An in-cluster agent (e.g. the kubehz heartbeat agent) | The status subresource only |

`lo` **defines** the `status` schema but never writes it:

- `status.observedAddons[]` — `{name, version, healthy}` as actually observed
  running in the cluster
- `status.availableUpdates[]` — `{name, current, latest}` computed against the
  published addon index (below)
- `status.lastReported` — when the agent last wrote status

## Fail-soft by design

Publishing the inventory can never break a deploy. If the cluster is
unreachable, the kubeconfig is missing, or the CRD conflicts with an existing
one, `lo` prints a warning and moves on — the provision/bootstrap result is
unaffected. Deploy domains (a `deploy.lok8s.yaml` with `clusterRef`) have no
inventory of their own; the referenced cluster's inventory is written when
that cluster provisions.

Both the CRD and the CR are applied idempotently (server-side apply), so
re-running `lo up` / `lo bootstrap` simply refreshes `spec` in place.

## Reading it

`lo status` shows the inventory when the cluster is reachable:

```
--- Inventory (ClusterInventory/cluster) ---
  lok8s:      0.1.0
  driver:     kubeone · hetzner
  kubernetes: v1.31.10
  specHash:   3fb0c5e02a41…
  renderedAt: 2026-07-06T10:12:03Z
  addons:     4
    cilium 1.19.2 (networking)
    cert-manager v1.20.1 (infrastructure)
    ...
```

Or straight via kubectl — the short name is `clinv`:

```bash
kubectl get clinv cluster -o jsonpath='{.spec.specHash}'
kubectl get clinv cluster -o jsonpath='{range .spec.addons[*]}{.name}{"\t"}{.chartVersion}{"\n"}{end}'
```

## The published addon index

The docs site serves a machine-readable index of every framework addon lok8s
ships, rebuilt on every docs deploy from the `.lok8s/addons/*` pins:

```
https://lok8s.io/addons-index.json
```

```json
{
  "generatedAt": "2026-07-06T10:00:00Z",
  "addons": [
    { "name": "cilium", "chartVersion": "1.19.2", "category": "networking" },
    { "name": "cert-manager", "chartVersion": "v1.20.1", "category": "infrastructure" }
  ]
}
```

Consumers (like the kubehz platform) poll this to compute
`status.availableUpdates` for a cluster's inventory — comparing each
`spec.addons[].chartVersion` against the index — without needing access to the
git repository. The index is deterministic per commit: entries are sorted by
name and `generatedAt` derives from the last commit touching the addons tree
(or `SOURCE_DATE_EPOCH` for reproducible builds).

## Relationship to `lo addons`

`lo addons --detail` is the *local* view of the same data (resolved through
the same `spec.bootstrap` parser); the `ClusterInventory` is that view made
durable *inside* the cluster, so dashboards and agents can read it without a
checkout. See [Addons](/guide/addons) for configuring the entries themselves.
