# kubehz Platform

lok8s can optionally connect a cluster to [kubehz](https://kubehz.cloud), a
managed-Kubernetes platform for Hetzner. The integration gives you dashboard
visibility (node status, control-plane health, certificate expiry) for
clusters you run on your own Hetzner account.

::: tip No lock-in
The integration is strictly opt-in. lok8s works fully without kubehz: the
default is no platform contact at all, every kubehz call during provisioning
is non-fatal (a failed registration warns and continues), and
[deregistering](#status-and-deregistration) removes everything. Nothing in
the provisioning or deployment path depends on the platform being reachable.
:::

## `spec.kubehz`

Platform integration is configured per cluster in `cluster.lok8s.yaml`:

```yaml
spec:
  kubehz:
    hosting: self                        # self | hosted
    access: registered                   # none | registered | managed
    apiUrl: https://api.kubehz.cloud     # required when hosted or access != none
```

Two independent axes:

| Axis | Values | Meaning |
|------|--------|---------|
| `hosting` | `self` (default) | You provision and own the control plane — the normal lok8s flow. |
| | `hosted` | kubehz runs the control plane in its infrastructure; you run only workers. Requires `apiUrl`. With `kind: Lo` it additionally requires `spec.runner`. |
| `access` | `none` (default) | No platform contact whatsoever. |
| | `registered` | The in-cluster agent registers the cluster and sends authenticated heartbeats — read-only dashboard visibility. |
| | `managed` | `registered` plus the kubehz operator (a `kubehz-operator` Deployment with its own write RBAC, `kubehz-managed`) may act on the cluster. |

`apiUrl` is required when `hosting: hosted` **or** `access != none`, and it
must be **HTTPS** — the agent's bearer token travels on this URL, so `lo`
refuses plain-HTTP endpoints outright.

## Registration

When `access` is `registered` or `managed`, the cluster registers itself
**from inside the cluster** — the heartbeat agent
([below](#the-heartbeat-agent)) owns this. On its first run the agent generates
two 256-bit secrets locally and announces only their SHA-256 hashes to
`POST /api/clusters/agent-register`:

| Secret | Key in `kubehz-agent` | Role |
|--------|-----------------------|------|
| **agent-token** `A` | `agent-token` | The live credential every heartbeat authenticates with (`Authorization: Bearer A`). It **never** leaves the cluster and is **never** printed by any command. |
| **claim-code** `C`  | `claim-code`  | A one-time proof you paste into the dashboard to claim the cluster. |

The platform stores only `sha256(A)` and `sha256(C)` — it can never push a
credential into your cluster, and a database leak yields no usable secret. The
secrets are generated **once** and never rotated, so re-applying the agent is a
no-op. Registration is **provider-agnostic**: no SSH key, no hcloud token,
nothing Hetzner-specific.

::: details Legacy: `lo kubehz register`
A pre-agent announce still exists and runs automatically at the end of
`lo provision` (or standalone via `lo kubehz register`): it POSTs the cluster
domain plus an SSH-key MD5 fingerprint to `/api/clusters/register`, making the
cluster visible before the agent is deployed. It is **vestigial** for
self-hosted clusters — the agent's self-registration and the claim code below
are the real path — and is kept only as a best-effort announce and as the
anchor for the platform's hosted-control-plane auto-claim. It proves nothing
and needs no token; if the API is unreachable it warns and provisioning
continues.
:::

## Claiming

Claiming attaches a registered cluster to your account. It is
**provider-agnostic and needs no hcloud token** — you prove ownership by reading
a one-time code out of the cluster (which only its operator can do) and pasting
it into the dashboard while signed in.

1. **Deploy the heartbeat agent** ([below](#the-heartbeat-agent)). On its first
   run it registers the cluster and generates the claim code.

2. **Read the claim code** with your kubeconfig pointed at the cluster:

   ```bash
   lo kubehz claim-code
   ```

   It prints the code (`khzc_…`) plus a paste hint. The equivalent raw command
   is:

   ```bash
   kubectl -n kubehz-system get secret kubehz-agent \
     -o jsonpath='{.data.claim-code}' | base64 -d
   ```

3. **Paste it** into the dashboard **Claim** page while signed in. The cluster
   is attributed to your account and the code is consumed (single-use).

The claim code is the *only* secret that transits your browser, and only once.
The **agent-token** — the credential that authenticates heartbeats — never
leaves the cluster and is never printed by `lo kubehz claim-code` or any other
command.

## The heartbeat agent

Dashboard visibility comes from a small in-cluster agent: a CronJob
`kubehz-heartbeat` in the `kubehz-system` namespace that runs **every five
minutes** and POSTs a status snapshot to
`/api/clusters/<domain>/heartbeat`. Properties worth knowing:

- **Self-bootstrapping identity.** On each run the agent ensures one Secret,
  `kubehz-agent` in `kubehz-system`, holding a self-generated agent-token `A`
  (`khz_agt_…`) and claim-code `C` (`khzc_…`). It is created once and **never
  rotated**; the platform only ever receives `sha256(A)`/`sha256(C)`.
- **Authenticated.** Every heartbeat carries `Authorization: Bearer A`. Once a
  cluster has enrolled a token, the platform rejects anonymous heartbeats for
  it — the identity is proven from the first beat.
- **Outbound-only.** The agent pushes; the platform never connects into your
  cluster and holds no credentials for it (only verifier hashes).
- **Least-privilege RBAC.** The `kubehz-agent` ServiceAccount can read cluster
  state — `get`/`list` on nodes, namespaces, componentstatuses and CSRs, `list`
  pods in `kube-system` only (control-plane health), and `get` on the `/readyz`
  + `/version` API paths — plus `get`+`create` (never `update`/`patch`/`delete`)
  on Secrets in its own `kubehz-system` namespace to bootstrap its identity
  Secret once. It reads no Secrets elsewhere and can rotate nothing.
- **Domain-keyed.** The payload's `clusterId` is the cluster domain — the same
  identity it registers under.
- **Hardened.** Stock `registry.k8s.io/kubectl` image, non-root, read-only
  root filesystem, all capabilities dropped.

The snapshot contains the Kubernetes version, node list (name/readiness/
role/instance type, capped at 20 nodes), certificate expiry, and
control-plane component health:

```json
{
  "clusterId": "my-cluster.example.com",
  "timestamp": "2026-07-04T12:00:00Z",
  "kubernetes": { "version": "v1.31.8" },
  "nodes": [{ "name": "cp-1", "status": "True", "roles": "control-plane", "instanceType": "cx32" }],
  "components": [
    { "name": "apiserver", "status": "Healthy" },
    { "name": "etcd", "status": "Healthy" },
    { "name": "scheduler", "status": "Healthy" },
    { "name": "controller-manager", "status": "Healthy" }
  ],
  "certificates": { "expiresAt": "2027-06-15T00:00:00Z" }
}
```

Each node's `instanceType` mirrors the well-known
`node.kubernetes.io/instance-type` label (set by the cloud provider, e.g.
`cx32` on Hetzner) — this is what lets the dashboard show a price overview
for self-hosted clusters. Nodes without the label (bare metal, kind, no
CCM) report an empty string; like every other probe, it never breaks the
heartbeat.

Component health is honestly derived — `apiserver` and `etcd` from the
apiserver's `/readyz?verbose` checks, `scheduler` and `controller-manager`
from the `kube-system` static pods — and **fail-soft**: a probe that errors
omits its component instead of guessing, a control plane without static
pods (managed/external) simply reports fewer components, and the heartbeat
itself never fails because a probe did.

The agent manifests ship with lok8s as a kustomize directory at
`.lok8s/libs/kubehz/manifests/agent/`. Deploying it is templating two
values into the ConfigMap and applying:

```bash
agent=$(mktemp -d)
cp -r .lok8s/libs/kubehz/manifests/agent/* "$agent"/
sed -i "s|KUBEHZ_API_URL_PLACEHOLDER|https://api.kubehz.cloud|g" "$agent/configmap.yaml"
sed -i "s|CLUSTER_ID_PLACEHOLDER|my-cluster.example.com|g" "$agent/configmap.yaml"
kubectl apply -k "$agent"
```

On its first run the agent creates its identity Secret, self-registers, and
starts beating. Then read the claim code with `lo kubehz claim-code` and paste
it into the dashboard to [claim](#claiming) the cluster.

## Status and deregistration

```bash
lo kubehz status        # local spec + live platform status for the domain
lo kubehz deregister    # remove the cluster from the platform
```

`status` prints the configured hosting/access/apiUrl and queries the
platform for the cluster's live status; `deregister` deletes the platform
registration (and runs automatically during `lo destroy` when
`access != none`). Both send an `Authorization: Bearer $KUBEHZ_TOKEN`
header when that environment variable is set — claimed clusters may
require it.

Deregistering only removes the platform-side registration. To remove the
in-cluster agent as well:

```bash
kubectl delete namespace kubehz-system
```

## Troubleshooting

**No heartbeats arriving?** Check the CronJob is present, not suspended,
and actually firing, then read the last run's logs:

```bash
kubectl -n kubehz-system get cronjob kubehz-heartbeat   # SUSPEND + LAST SCHEDULE
kubectl -n kubehz-system logs -l app=kubehz-heartbeat
```

A `4xx` from the API in those logs usually means the agent could not register
or authenticate: a `401` is a missing or rejected agent-token, a `404` means the
cluster is not registered yet. The agent self-registers on its first run and
retries every tick, so a transient API outage clears itself.

**Registration failed during provisioning?** Provisioning no longer depends on
it — a self-hosted cluster registers from inside, once the agent is deployed.
Deploy the agent (above); it registers and starts beating on its own.

**No claim code yet?** `lo kubehz claim-code` reads it from the `kubehz-agent`
Secret. If the Secret or its `claim-code` key is missing, the agent has not run
yet — confirm the CronJob fired at least once, then retry. Make sure your
kubeconfig points at the cluster you mean to claim.

**`spec.kubehz.apiUrl is required` / HTTPS errors?** Set `apiUrl` whenever
`hosting: hosted` or `access != none`, and use an `https://` URL — plain
HTTP is rejected.
