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
    agent: cronjob                       # cronjob | operator — which agent beats
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
| | `managed` | Everything `registered` does, plus kubehz's management features (healing policies, capacity watches, desired-state management) driven from the dashboard. **Subscription-gated (Supporter+)** — the platform enforces the tier once the cluster is claimed. Acting is pull-based: the in-cluster agent fetches desired state and applies it locally with the cluster's own credentials; the platform never pushes into your cluster, and per-feature execution switches let you keep acting off. **Acting also needs `agent: operator`** — the default CronJob agent reports, it does not act. |

Plus two more keys:

| Key | Default | Meaning |
|-----|---------|---------|
| `agent` | `cronjob` | Which in-cluster agent sends the heartbeats: the bash **CronJob** or the Go **live agent**. Exactly one of them beats. See [Choosing an agent](#choosing-an-agent). |
| `connectHcloudToken` | `false` | When `true` **and** a `KUBEHZ_TOKEN` is set, `lo provision` also stores your `HCLOUD_TOKEN` with the platform (encrypted at rest, used only to manage your clusters) so the dashboard can drive provisioning — worker pools, SSH keys. Without it the token is used only locally by `lo` and never sent. A read-only token is stored for account-exact pricing but leaves provisioning locked. |

`apiUrl` is required when `hosting` is `hosted` or `shared`, **or** when
`access != none`, and it
must be **HTTPS** — the agent's bearer token travels on this URL, so `lo`
refuses plain-HTTP endpoints outright.

## Upgrades and maintenance windows

Two more optional blocks under `spec.kubehz` tell the platform how far it may
upgrade your cluster without being asked, and when that work is allowed to
run:

```yaml
spec:
  kubehz:
    upgrades:
      channel: patch            # none | patch | minor — absent = patch
      defer: window             # window | immediate — absent = window
    maintenanceWindow:
      enabled: true
      daysOfWeek: [Sat]
      startTime: "02:00"        # HH:MM, in the window's timezone
      durationMinutes: 240
      timezone: Europe/Berlin
      exclusions:               # absolute freezes — a date, or a from/to range
        - "2026-12-20/2027-01-06"
```

- **`channel`** is consent, not a schedule: `patch` (the default) allows
  patch releases; `minor` allows minor releases too. `none` disables
  automatic upgrades entirely. **Not recommended**: you forgo automatic
  security patches, and it does not exempt the cluster from the
  end-of-support floor — clusters running releases that fall too far behind
  are upgraded by the platform regardless of channel.

> **Changed default.** An absent `channel` previously meant "no automatic
> upgrades"; it now means `patch`. A spec without a `channel` receives
> automatic patch upgrades once upgrade execution ships — set
> `channel: none` explicitly if you want the old behavior.
- **`defer`** decides when an allowed upgrade starts: `window` (the default)
  waits for the next maintenance window; `immediate` starts as soon as the
  upgrade is available.
- **`maintenanceWindow`** bounds platform-driven work generally, not just
  upgrades. `exclusions` are absolute freezes — nothing platform-driven runs
  inside one, except forced security/EOL upgrades.

How an upgrade lands depends on who owns the machines:

- **Nodes you own** — self-hosted clusters, and machines you registered
  yourself — are upgraded **in place**: the kubelet is swapped underneath the
  running workloads, which the swap itself does not restart. On the managed
  tier, the live agent will walk the nodes one at a time once upgrade
  execution ships.
- **Hosted worker pools** are **replaced, surge-first**: new nodes join on
  the target version before old ones drain and leave. Per-pool tuning (surge
  size and the like) rides the pool definition on the platform side, not this
  file.

`lo` checks the shape — the two enums, and that every exclusion is a
`YYYY-MM-DD` date or `from/to` range. Full semantic validation happens where
each block is consumed: for **hosted** clusters and pools, the platform API
validates its own payloads; for **self-hosted** clusters the block is the
declared input for the managed-tier agent's upgrade walk — the agent will
read it locally once upgrade execution ships, so your cluster's upgrade
behavior will never depend on the platform being reachable. Until then the
block is declarative only. Calendar
validity beyond the shape (a February 30th) is not caught by `lo` today.

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
- **The shard is the upgrade unit.** The shared control plane is upgraded as
  a whole, on the platform's schedule, and old versions are retired on that
  schedule too. Here `upgrades.channel` is a preference, not consent: an
  eager channel moves your space early in a shard's upgrade window, a
  conservative one later — but every space is migrated by the deadline.
  There is no opting out.

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

## Nodes you bring (static pools)

On a `hosting: hosted` cluster, kubehz normally provisions the workers for
you on your own Hetzner account. A **static pool** is the other option: a
worker pool whose machines are yours — bare metal, a second cloud, a virtual
machine that already runs. Nothing provisions them and nothing rotates them.
You install a kubelet, run one join command, and the node appears in your
cluster next to the pools kubehz manages. One cluster can hold both kinds.

Declare the pool with `kind: static` in the dashboard, then work from the
machine itself:

```bash
lo kubehz node join --pool metal      # mint a ticket and join THIS machine
lo kubehz node status                 # the nodes you brought, and the slot count
lo kubehz node remove --name box-1    # take a node out and free its slot
```

::: tip Cluster-level commands are unchanged
`lo kubehz join` mints a join ticket for a **Space** (`hosting: shared`), and
`lo kubehz deregister` removes a whole cluster. The `node` group is the
node-level surface, so a verb never touches more than its name says.
:::

### `lo kubehz node join`

Run it **as root on the machine you are adding**. It mints a join ticket from
the platform, then runs the `kubeadm join` line the platform composes. The
platform composes that line because the join address and the CA fingerprints
are properties of a control plane you do not run.

| Flag | Default | Meaning |
|------|---------|---------|
| `--pool` | the one pool this cluster's nodes share | The `kind: static` pool to join. Name it for the first node of a pool, or when the cluster holds more than one. |
| `--name` | this machine's short hostname, lowercased | The node name. A DNS label: lowercase letters, digits and dashes. |
| `--cluster-id` | the active domain's cluster | The platform cluster id (`cl-…`). Set it to reach a cluster your local specs do not describe. |
| `--node-ip` | — | The address other nodes reach this machine on. Add it when the machine's default-route address is not the one the cluster can dial. |
| `--kubelet-version` | read from this machine | The kubelet version to declare. The platform refuses a version more than two minors behind the control plane. |
| `--print-only` | off | Mint the ticket, print the command, and run nothing. |

The command refuses before it mints anything when `kubeadm` is absent, or when
the shell is not root. Use `--print-only` to mint on one machine and join on
another.

::: warning Re-run with `sudo -E`, not plain `sudo`
`kubeadm join` must run as root, but the join reads `KUBEHZ_TOKEN` and the
`PATH_*` lo environment first. Plain `sudo` starts a clean environment and
drops both, so the mint would fail to authenticate. Use `sudo -E lo kubehz node
join …` to carry your environment through, or mint with `--print-only` as your
own user and run the printed `kubeadm join` line under root separately.
:::

**Requirements on the machine:** a container runtime, plus `kubelet`,
`kubeadm` and `kubectl` from a supported Kubernetes release. The machine must
reach the join endpoint on TCP 6443 and the node tunnel on TCP 8088. A machine
behind NAT or CGNAT needs `--node-ip` with an address the cluster can reach.

**The ticket is a credential.** It is bound to one node name and to ten
minutes, it works once, and its plaintext exists only in that one response —
a lost ticket is minted again, never recovered. A second mint for the same
name revokes the first.

### `lo kubehz node status`

Lists the nodes you brought, with the slot count against the cluster's
static-node limit:

```
Cluster: cl-1234abcd (acme.example.org)
Nodes:   2/20

  NAME                     POOL             STATUS     JOINED
  box-1                    metal            Ready      2026-08-30T10:00:00Z
  box-2                    metal            Joining    -
```

Nodes in kubehz-provisioned pools are not listed here — those appear as pool
counts on the cluster itself. A warning appears while the control plane has
published no join address: a mint is refused until it does.

### `lo kubehz node remove`

Removes one node from the cluster and frees its slot at once. The platform
cordons the node, then deletes it from your cluster.

Removal takes the **membership**, never the hardware. Pods on the machine are
not evicted first, because the machine is yours and asking for it back is not
a reason for the platform to move your workloads. Drain the node yourself when
you want a clean handover. Run `kubeadm reset` on the machine before you join
it anywhere again.

::: warning A dark node still holds its slot
A node that stops reporting keeps its slot until it is removed. The platform
notifies you, and removes an unacknowledged dark node after 30 days. Use
`lo kubehz node remove` when you already know the machine is gone.
:::

### When a join is refused

Every refusal names what to do next. The four you are most likely to meet:

| Refusal | What it means |
|---------|---------------|
| Static pools not enabled | The feature rolls out account by account. Ask kubehz support to enable it. |
| The kubelet is too old | A node must run a kubelet no more than two minor versions below the control plane. Upgrade the kubelet. |
| No free slot | The cluster is at its static-node limit. Remove a node you no longer use, or ask support to raise the limit. |
| No join address yet | The control plane publishes its join address and CA fingerprints after the cluster declares its first `kind: static` pool. Check `lo kubehz node status`, then try again. |

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

## Choosing an agent

Dashboard visibility comes from an in-cluster agent. lok8s ships two, and
`spec.kubehz.agent` picks which one sends the heartbeats:

| | `cronjob` (default) | `operator` |
|---|---|---|
| Workload | CronJob `kubehz-heartbeat` | Deployment `kubehz-live-agent` |
| What it is | A shell script in a stock `alpine/k8s` image | The Go agent from the public [kubehz-agent](https://github.com/kernpilot/kubehz-agent) repo |
| Cadence | Every 5 minutes | Within seconds of a change, and at least every minute |
| Reports | Node status, control-plane component health, certificate expiry, the 24 h [assessment](#assessment-and-handover) | Nodes with capacity and instance type, pod counts by phase, warning events, machine-controller failures, your addon inventory |
| Can act | No | Yes — worker scaling, self-healing and worker upgrades, when the platform authorizes them |
| Costs | ~50 m CPU for a few seconds every 5 minutes | ~25 m CPU and 64 Mi resident, always on |

Deploy either one with:

```bash
lo use my-cluster.example.com   # point the kubeconfig at the cluster
lo kubehz deploy                # applies the agent your spec names
lo kubehz deploy --dry-run      # print the manifests instead
```

Change `spec.kubehz.agent` and re-run `lo kubehz deploy` to switch. The command
applies the agent you named and stops the other one — differently in each
direction. Switching to `operator` **keeps** the CronJob and silences it: it
still bootstraps and enrolls the identity Secret, and it reads the
`KUBEHZ_HEARTBEAT_OWNER` marker before every beat. Switching to `cronjob`
**deletes** the live agent's Deployment, because the Go agent reads no marker
and beats whenever it runs.

::: warning Exactly one agent may beat
The platform keeps the **latest** heartbeat and nothing older. The two agents
report different fields, so if both beat, each one erases what the other
reported: your live view would empty itself every five minutes.

You do not have to police this as long as you switch with `lo kubehz deploy`.
`spec.kubehz.agent` holds one value, so a spec cannot ask for both; the command
applies, silences and deletes in an order that leaves no gap in either
direction, and it stops rather than continues if any step fails; and the
cluster itself carries the decision as `KUBEHZ_HEARTBEAT_OWNER` in the
`kubehz-agent-config` ConfigMap, which the CronJob reads before every beat.

That marker is a **one-way** interlock, and it is worth knowing which way. It
can silence the CronJob lok8s ships — that manifest reads the marker before
every beat, so applying it by hand while the live agent owns the heartbeat
starts no second producer. A CronJob you write yourself reads nothing and
beats. It cannot silence the live agent either: the Go agent has no
equivalent switch and beats whenever it runs, so what stops it is removing its
Deployment — which is exactly what `lo kubehz deploy` does before it re-arms
the CronJob. Apply the live agent's Deployment by hand while the marker says
`cronjob` and you *will* have two producers. Let the command do the switching.
:::

### What the live agent gives you

With `agent: operator`, and once you [claim](#claiming) the cluster:

- **Live view.** Node capacity and instance type, pod counts by phase, recent
  warning events, and machine-controller provisioning failures, pushed within
  seconds of a change. Pod *names* and namespaces stay in your cluster unless
  you opt in.
- **Addon updates in `kubectl`.** The agent reports your lok8s
  [ClusterInventory](./inventory.md) and writes the platform's answer back onto
  it, so `kubectl get clusterinventory cluster -o yaml` shows which addons have
  a newer version. No dashboard needed.
- **Worker scaling, self-healing and worker upgrades**, when **all** of these
  hold: your `access` is `managed`, your tenant tier allows acting
  (Supporter+), and the platform has the feature switched on. The agent asks
  the platform what it should do and obeys the answer. Nothing local turns
  acting on — there is no flag for it — and without `access: managed` no acting
  permission is granted at all.

Acting always runs with **your** cluster's own credentials. kubehz holds no
credential for your cluster and cannot connect into it; the agent pulls, and
`kubectl delete namespace kubehz-system` ends it.

::: tip What you give up for now
While the live agent owns the heartbeat, the platform does not receive
control-plane component health, certificate expiry, or the 24 h assessment —
the live agent does not collect them yet, so `lo kubehz assess` will go stale.
Set `agent: cronjob` and re-run `lo kubehz deploy` to get them back.
:::

### Permissions the live agent holds

The base install is read-only. It can `get`, `list` and `watch` nodes, pods and
events, read your `ClusterInventory`, and `get` one Secret by name — its own
identity Secret. It has one write: `patch` on `clusterinventories/status`, which
is where the addon-update answer lands. It cannot read pod logs, exec into a
pod, or read any other Secret.

With `access: managed` it also gets, in `kube-system` only:

- `patch` on `machinedeployments.cluster.k8s.io` — two fields: the replica count
  (scaling) and the kubelet version (worker upgrades). No `create`, no `delete`.
- `get`/`list`/`watch` **and `delete`** on `machines.cluster.k8s.io`. The
  `delete` verb exists for one reason: self-healing removes an unhealthy worker
  Machine so your MachineSet rebuilds it. It is the sharpest permission the
  agent holds. Remove that one verb and self-healing fails closed with a
  reported error, while everything else keeps working.
- `delete` on pods, cluster-wide. This one exists only to finish a deletion the
  cluster cannot finish itself: when a healed node is truly dead, the pods stuck
  `Terminating` on it can never confirm their eviction, so the machine sits in
  deletion and the server keeps billing you. The agent force-deletes exactly
  those pods, once per machine, and only while the node is still unreachable.

Every one of these is documented at the rule in
[`rbac-managed.yaml`](https://github.com/kernpilot/kubehz-agent/blob/56ccd9b370066b2b581bd97733e988a856df8857/deploy/managed/rbac-managed.yaml),
and lok8s ships that file byte-identical to the agent's own repo — the
permissions you grant are the ones the public source documents. That link
points at the exact commit lok8s vendored, which is what makes
"byte-identical" checkable: the SHA-256 of both vendored files is recorded in
`.lok8s/libs/kubehz/manifests/live-agent/UPSTREAM.sha256`, and
`sha256sum --check UPSTREAM.sha256` from that directory verifies it.

The image is `ghcr.io/kernpilot/kubehz-agent`, pinned by digest and signed. To
check what you are about to run:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/kernpilot/kubehz-agent/\.github/workflows/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/kernpilot/kubehz-agent@sha256:<the digest in the manifest>
```

## The heartbeat agent

The default agent is a CronJob `kubehz-heartbeat` in the `kubehz-system`
namespace that runs **every five minutes** and POSTs a status snapshot to
`/api/clusters/<domain>/heartbeat`. It also owns the cluster's identity, which
is why it stays deployed even when the live agent takes over the heartbeat: it
creates the identity Secret and enrolls it, and the live agent only reads that
Secret. Properties worth knowing:

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

Deploy it with `lo kubehz deploy`, which reads `spec.kubehz.agent`, substitutes
your `apiUrl` and cluster domain, and applies the result with your current
kubeconfig:

```bash
lo use my-cluster.example.com
lo kubehz deploy
```

On its first run the agent creates its identity Secret, self-registers, and
starts beating. Then read the claim code with `lo kubehz claim-code` and paste
it into the dashboard to [claim](#claiming) the cluster.

The manifests ship with lok8s as kustomize directories —
`.lok8s/libs/kubehz/manifests/agent/` (CronJob) and
`.lok8s/libs/kubehz/manifests/live-agent/` (live agent, base and managed) — so
you can read exactly what `deploy` applies, or render it first with
`lo kubehz deploy --dry-run`.

## Status and deregistration

```bash
lo kubehz status        # local spec + live platform status for the domain
lo kubehz deregister    # remove the cluster from the platform
```

`status` prints the configured hosting/access/apiUrl, then reads your
registry row on the platform: the cluster's live status, its id, and the
last heartbeat. `deregister` deletes the platform registration: it resolves
the cluster id from your registry and sends `DELETE /api/clusters/<id>`
(and runs automatically during `lo destroy` when `access != none`). Both
look the cluster up through the tenant registry, so both need
`KUBEHZ_TOKEN` set to a `clusters:write` token of the owning tenant —
without it, `status` reports the HTTP refusal and `deregister` refuses
without deleting anything.

### `KUBEHZ_TOKEN`: claim at provision time

`KUBEHZ_TOKEN` is a `clusters:write` API token you mint in the dashboard
(**Access → API Tokens**). When it is set during `lo provision`, the cluster
is **claimed directly to your tenant at registration** — no claim code to
paste, no fingerprint step. It shows up in your dashboard already owned. Pair
it with `spec.kubehz.connectHcloudToken: true` to also wire dashboard-driven
provisioning in the same run. Without `KUBEHZ_TOKEN`, `lo provision` registers
anonymously and you [claim](#claiming) the cluster afterwards.

Deregistering deletes the platform cluster record and, when `HCLOUD_TOKEN`
is set, the `kubehz-claim-<domain>` SSH key from your Hetzner Cloud
account. The in-cluster agents survive — to remove them as well:

```bash
kubectl delete namespace kubehz-system
kubectl delete clusterrole,clusterrolebinding -l app.kubernetes.io/part-of=kubehz
kubectl -n kube-system delete role,rolebinding -l app.kubernetes.io/part-of=kubehz
```

All three are needed. Both agents run in `kubehz-system`, but neither keeps all
of its RBAC there: the cluster-scoped roles go in the second command, and both
agents also hold a namespaced `Role`/`RoleBinding` in **`kube-system`** — the
CronJob's pod-list and assessment reads (`kubehz-agent`), and, at
`access: managed`, the MachineDeployment and Machine grants
(`kubehz-live-agent-machinedeployments`). Those survive the first two commands.

After the three, nothing labelled `app.kubernetes.io/part-of=kubehz` is left:

```bash
kubectl get clusterrole,clusterrolebinding -l app.kubernetes.io/part-of=kubehz
kubectl get role,rolebinding -A -l app.kubernetes.io/part-of=kubehz
```

Both should print `No resources found`. If you moved the worker pools out of
`kube-system` with `KUBEHZ_MD_NAMESPACE`, the managed `Role` moved with them —
the `-A` query above finds it wherever it is.

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

**No heartbeats, and you set `agent: operator`?** Read the live agent instead:

```bash
kubectl -n kubehz-system get deploy kubehz-live-agent
kubectl -n kubehz-system logs deploy/kubehz-live-agent
```

An `ImagePullBackOff` means the node cannot reach `ghcr.io` — mirror the image
and override the registry in the manifest, keeping the digest. A `401` means the
agent-token was rejected: run `lo kubehz re-enroll`. If the Deployment is missing
entirely, run `lo kubehz deploy` again.

To confirm the CronJob really has stood down:

```bash
kubectl -n kubehz-system get configmap kubehz-agent-config \
  -o jsonpath='{.data.KUBEHZ_HEARTBEAT_OWNER}'   # must print: operator
```

If that prints `cronjob` while the live agent is running, both are beating. Run
`lo kubehz deploy` to settle it.

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
