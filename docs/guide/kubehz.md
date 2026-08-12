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
    hosting: self                        # self | hosted | shared
    access: registered                   # none | registered | managed
    apiUrl: https://api.kubehz.cloud     # required when hosted/shared, or access != none
    connectHcloudToken: false            # opt-in: hand kubehz your HCLOUD_TOKEN (see below)
```

Two independent axes:

| Axis | Values | Meaning |
|------|--------|---------|
| `hosting` | `self` (default) | You provision and own the control plane — the normal lok8s flow. |
| | `hosted` | kubehz runs the control plane in its infrastructure; you run only workers. Requires `apiUrl`. With `kind: Lo` it additionally requires `spec.runner`. |
| | `shared` | kubehz runs a control plane shared by many customers and you get namespaces on it, with nodes you register yourself. Requires `apiUrl` and `kind: Kubehz` — see [Spaces](#spaces). |
| `access` | `none` (default) | No platform contact whatsoever. |
| | `registered` | The in-cluster agent registers the cluster and sends authenticated heartbeats — read-only dashboard visibility. |
| | `managed` | Everything `registered` does, plus kubehz's management features (healing policies, capacity watches, desired-state management) driven from the dashboard. **Subscription-gated (Supporter+)** — the platform enforces the tier once the cluster is claimed. Acting is pull-based: the in-cluster agent fetches desired state and applies it locally with the cluster's own credentials; the platform never pushes into your cluster, and per-feature execution switches let you keep acting off. |

Plus one opt-in flag:

| Key | Default | Meaning |
|-----|---------|---------|
| `connectHcloudToken` | `false` | When `true` **and** a `KUBEHZ_TOKEN` is set, `lo provision` also stores your `HCLOUD_TOKEN` with the platform (encrypted at rest, used only to manage your clusters) so the dashboard can drive provisioning — worker pools, SSH keys. Without it the token is used only locally by `lo` and never sent. A read-only token is stored for account-exact pricing but leaves provisioning locked. |

`apiUrl` is required when `hosting` is `hosted` or `shared`, **or** when
`access != none`, and it
must be **HTTPS** — the agent's bearer token travels on this URL, so `lo`
refuses plain-HTTP endpoints outright.

## Spaces

`hosting: shared` is the third shape: kubehz runs a control plane shared by
many customers, you get namespaces on it, and **you register your own
machines** as the nodes your workloads run on. There is no control plane to
provision, so the driver is thin — `kind: Kubehz`.

```yaml
# clusters/acme.example.org/cluster.lok8s.yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Kubehz
metadata:
  name: acme
spec:
  cluster:
    domain: acme.example.org
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.cloud
    space:
      slug: acme          # optional — defaults to the domain's first label
      name: Acme Prod     # optional — defaults to the slug
      nodes:              # optional — one join ticket minted per name
        - worker-1
        - worker-2
```

`lo provision` creates the space (or adopts it, if it already exists) and
mints a single-use join ticket for every node listed under
`space.nodes`. `lo destroy` removes the space.

```bash
lo provision                  # create/adopt the space + mint join tickets
lo kubehz join worker-3       # mint another ticket, any time
lo kubehz status              # space phase, plan, and the registered nodes
lo kubehz deregister          # remove the space
```

A few things worth knowing before you reach for it:

- **`KUBEHZ_TOKEN` is required**, not optional as it is elsewhere: a space
  belongs to your account from the moment it exists, so there is no anonymous
  path and no claim step.
- **A join ticket is shown once**, is bound to one node name, and expires
  quickly. Nothing stores the plaintext — a lost ticket is re-minted, never
  recovered.
- **There is no kubeconfig to download.** The control plane is
  platform-operated and is not handed out; you reach your namespaces with
  your kubehz account (OIDC). `lo kubeconfig` on a space says exactly that
  rather than producing a file that could not work.
- **`spec.bootstrap` does not apply.** The platform baseline (CNI, ingress,
  certificates, DNS) is already running on the shared plane, and a space
  cannot install cluster-scoped things anyway.
- **Machines are yours.** lok8s does not provision them and kubehz does not
  bill for them. Deregistering a node withdraws its credentials; shutting the
  machine down is still your call.

### Limits, and whose problem each one is

A space runs inside two independent budgets, and the error codes keep them
apart deliberately:

- **Your space's plan quota** — nodes and namespaces per space. Exceeding it
  answers `403 QUOTA_EXCEEDED` and names the limit. Fix: remove something or
  move to a bigger plan.
- **The platform's capacity** — how much a shared control plane can carry.
  When the platform is the limit you get `409 SHARD_AT_CAPACITY` (a node
  join the current plane genuinely cannot take) or `409 NO_SHARD_AVAILABLE`
  (a space create with nowhere to land). Both say so explicitly: *"this is a
  platform capacity limit, not an account limit"* — upgrading your plan will
  not change them, and nothing you delete will either.

The platform manages its own headroom (shared planes are added as they fill),
so the 409s are expected to be rare and transient — retry later, and if one
persists, that is a support ticket, not a configuration hunt.

Joining a node — requirements, the recommended host firewall, the bare-metal
lane — is documented on the platform side:
[kubehz docs → Spaces](https://kubehz.io/docs/spaces/).

::: tip Still no lock-in
A space is the one shape where the control plane is not yours, so it is worth
being explicit: everything else in lok8s keeps working without kubehz, and
moving to a cluster of your own (`hosting: hosted` or `self`) is a supported
path rather than an export-and-rebuild.
:::

## Bootstrap addons on hosted clusters

Your spec's `bootstrap` section applies to a **hosted** cluster too: after
`lo provision` has the control plane running and the kubeconfig downloaded,
the same bootstrap DAG a self-hosted cluster gets is applied onto it — your
gateway, sso-gate, cert-manager, whatever you declared. A few
hosted-specific rules:

- **`cilium` and `ccm` are platform-owned** and always skipped (matched on
  the addon directory, so a `name:`-renamed entry skips too): the hosting
  platform manages the CNI as a system application and runs the cloud
  integration on its side (kubehz, for one, ships its hosted CNI with
  WireGuard encryption on by default — a platform property, not something
  this skip configures). Declaring them is harmless; bootstrapping your
  own copy would fight the platform for the datapath, so `lo` refuses on
  your behalf.
- **You need working kubectl access**: the default hosted kubeconfig is
  OIDC (kubelogin + a browser sign-in). Headless/CI runs without it get a
  clear notice and the bootstrap is skipped — install the kubelogin
  plugin and re-run `lo provision` or `lo bootstrap` interactively.
- **Workers first**: a control plane with no Ready workers can't run addon
  workloads, and `wait:`-gated entries would hang — `lo` skips the
  bootstrap with a notice and you re-run once workers have joined. The
  apply is idempotent.

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
  Secret once. It reads no Secrets elsewhere and can rotate nothing. The
  [assessment](#assessment-and-handover) adds read-only probe grants
  (storageclasses, persistentvolumes, services `list`, webhook configuration
  `list`, `get` on the one CAPI CRD, kube-system daemonset `list` + the
  `kubeadm-config` ConfigMap) and `get`/`patch` scoped to the single
  `kubehz-agent-config` ConfigMap for its send-gate marker.
- **Domain-keyed.** The payload's `clusterId` is the cluster domain — the same
  identity it registers under.
- **Hardened.** Stock `alpine/k8s` image (pinned by digest), non-root, read-only
  root filesystem, all capabilities dropped.

The snapshot contains the Kubernetes version, node list (name/readiness/
role/instance type, capped at 20 nodes), certificate expiry, and
control-plane component health:

```json
{
  "clusterId": "my-cluster.example.com",
  "timestamp": "2026-07-04T12:00:00Z",
  "kubernetes": { "version": "v1.31.8" },
  "nodes": [{ "name": "cp-1", "status": "Ready", "roles": "control-plane", "instanceType": "cx32" }],
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

### `KUBEHZ_TOKEN`: claim at provision time

`KUBEHZ_TOKEN` is a `clusters:write` API token you mint in the dashboard
(**Access → API Tokens**). When it is set during `lo provision`, the cluster
is **claimed directly to your tenant at registration** — no claim code to
paste, no fingerprint step. It shows up in your dashboard already owned. Pair
it with `spec.kubehz.connectHcloudToken: true` to also wire dashboard-driven
provisioning in the same run. Without `KUBEHZ_TOKEN`, `lo provision` registers
anonymously and you [claim](#claiming) the cluster afterwards.

Deregistering only removes the platform-side registration. To remove the
in-cluster agent as well:

```bash
kubectl delete namespace kubehz-system
```

## Assessment and handover

The heartbeat agent also collects a compact **assessment** of the cluster —
Kubernetes version, datastore (`etcd`/`kine`/`unknown`), CNI, pod/service
CIDRs, StorageClasses, a PV summary grouped by provisioner, LoadBalancer and
webhook counts, node/control-plane counts. It is kubectl-only, read-only,
fail-soft, and sent **at most once per 24 h** (or when the platform requests
a refresh through the heartbeat response). The platform turns it into a
handover **feasibility** verdict.

```bash
lo kubehz assess    # pretty-print the stored assessment + feasibility
```

`assess` authenticates like `lo kubehz status` (`KUBEHZ_TOKEN` against
`spec.kubehz.apiUrl`).

### Receiving an ejected control plane

When you move a **hosted** control plane onto your own machine, the platform
exports a bundle (cluster CA, service-account keys, front-proxy CA, the
secrets-at-rest encryption key, an etcd snapshot location, and the endpoint
DNS name). Two target-side commands consume it:

```bash
# kubeadm path — run ON the fresh target node (writes /etc/kubernetes and
# /var/lib/etcd, runs kubeadm):
lo kubehz handover receive --bundle ./export-bundle [--snapshot ./snap.db] [--single-node]

# kubeone path — pre-seed only the PKI on node0, then provision as usual;
# kubeadm reuses existing CA files, preserving cluster identity:
lo kubehz handover preseed --bundle ./export-bundle --node <node0-ip>
lo provision
```

`receive` refuses a node that already carries Kubernetes state (use
`kubeadm reset` first, or `--force` if you really mean it), restores the etcd
snapshot (`--snapshot` for a pre-downloaded file; `s3://` locations need an
`aws` CLI on the node), seeds the exported PKI, runs `kubeadm init` with the
pre-seeded state, and **verifies the resulting cluster CA is byte-identical
to the bundle's** before telling you to cut DNS over. If that verify fails,
do not cut over — the restored cluster did not keep its identity.

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
`hosting` is `hosted` or `shared`, or `access != none`, and use an `https://`
URL — plain HTTP is rejected.
