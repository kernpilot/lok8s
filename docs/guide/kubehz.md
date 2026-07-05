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
| | `registered` | The cluster is announced to kubehz at provision time and can send heartbeats — read-only dashboard visibility. |
| | `managed` | `registered` plus the kubehz operator (a `kubehz-operator` Deployment with its own write RBAC, `kubehz-managed`) may act on the cluster. |

`apiUrl` is required when `hosting: hosted` **or** `access != none`, and it
must be **HTTPS** — bearer tokens and fingerprints travel on this URL, so
`lo` refuses plain-HTTP endpoints outright.

## Registration

When `access` is `registered` or `managed`, the last `lo provision` step
registers the cluster with the platform. The same call is available
standalone:

```bash
lo kubehz register
```

Registration is a **public, unauthenticated** POST to
`/api/clusters/register` carrying exactly two things:

- the cluster **domain** (which becomes its platform identity), and
- an SSH-key **MD5 fingerprint** (Hetzner exposes MD5 fingerprints, so lok8s
  uses MD5, not SHA256).

This creates a **pending** cluster on the platform. Registration proves
nothing by itself and needs no token — ownership is proven later, at claim
time. If the API is unreachable, provisioning warns and continues without
the integration. Where the fingerprint comes from depends on the
environment:

### The claim key (default on Hetzner)

When `HCLOUD_TOKEN` is available — which is every hcloud provisioning
environment, including the [CI spinup workflow](./deployment.md#provisioning-from-ci)
— lok8s plants a **dedicated claim key** in your Hetzner Cloud project:

1. It generates an ephemeral ed25519 keypair locally.
2. It uploads only the **public** half to your hcloud project as an SSH key
   named `kubehz-claim-<domain>`, and registers that key's MD5 fingerprint
   with the platform.
3. It **discards the private key** immediately. The key is never used for
   SSH — it exists solely so its fingerprint is readable in *your* Hetzner
   console.

That fingerprint is your **claim ticket**: it lives inside your hcloud
account, where only you can read it. lok8s therefore does **not** print it
— not to the terminal, not to CI job summaries:

```
kubehz: cluster 'my-cluster.example.com' registered (pending).
  To claim it: open your Hetzner Cloud console → Security → SSH keys,
  copy the fingerprint of 'kubehz-claim-my-cluster.example.com' (or: hcloud ssh-key list),
  and paste it at your kubehz dashboard's /claim page.
```

Re-registering is idempotent: an existing `kubehz-claim-<domain>` key is
reused as-is, never rotated — the ticket you read from the console stays
valid.

### Server-key fallback (no `HCLOUD_TOKEN`)

Without `HCLOUD_TOKEN` (non-hcloud contexts), or if planting the claim key
fails, lok8s falls back to the cluster's own SSH-key fingerprint: `KubeOne`
reads the SSH public key from the provider output (falling back to
`spec.hcloud.sshPublicKeyFile`), `Capi` resolves `spec.hcloud.sshKeyName`
via the hcloud API, and `Lo` clusters — which have no SSH keys — use a
`lo:<domain>` identifier instead. A server-key fingerprint is public (safe
in logs), so the CLI prints it — and claiming it goes through the
[token-verification path](#token-verification-fallback-fingerprints):

```
kubehz: cluster 'my-cluster.example.com' registered (pending). Claim it in the dashboard:
  fingerprint: MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99
  note: no HCLOUD_TOKEN in this environment — claiming this cluster will use
  the token-verification path (fingerprint + your Hetzner Cloud API token).
```

## Claiming

A pending cluster is attached to your account through the dashboard
**Claim** page. What you paste there depends on which fingerprint was
registered.

### With a claim key

The fingerprint of `kubehz-claim-<domain>` works as a **secret ticket
inside your hcloud account** — being able to read it proves you own the
project the cluster was provisioned into. No hcloud token is handed to the
platform:

1. Open your Hetzner Cloud console → **Security → SSH keys** and copy the
   fingerprint of `kubehz-claim-<domain>` (or run `hcloud ssh-key list`).
2. Paste it at the dashboard **Claim** page.

Treat that fingerprint like a password while the cluster is pending:
**never share or post it** — anyone who knows it can claim the cluster.
It is **one-time use**: the claim consumes it. After a successful claim you
may delete the `kubehz-claim-<domain>` key from your hcloud project — the
platform never uses it again.

### Token verification (fallback fingerprints)

A server-key fingerprint is public, so knowing it proves nothing. Claiming
one takes two inputs:

1. the **MD5 fingerprint** printed at registration, and
2. **your own Hetzner Cloud API token** — used once to verify that the SSH
   key behind the fingerprint lives in your hcloud account. The token is
   used transiently for that lookup and is **never stored**.

You can reproduce a server-key fingerprint locally at any time:

```bash
ssh-keygen -E md5 -lf ~/.ssh/id_ed25519.pub
```

## The heartbeat agent

Dashboard visibility comes from a small in-cluster agent: a CronJob
`kubehz-heartbeat` in the `kubehz-system` namespace that runs **every five
minutes** and POSTs a status snapshot to
`/api/clusters/<domain>/heartbeat`. Properties worth knowing:

- **Outbound-only.** The agent pushes; the platform never connects into
  your cluster and holds no credentials for it.
- **Read-only RBAC.** The `kubehz-agent` ServiceAccount can only read
  cluster state: `get`/`list` on nodes, namespaces, componentstatuses and
  CSRs, pods in `kube-system` only (a namespaced Role, for control-plane
  health), and `get` on the `/readyz` + `/version` API paths. Nothing
  grants write access.
- **Domain-keyed.** The payload's `clusterId` is the cluster domain — the
  same identity used at registration.
- **Hardened.** Stock `registry.k8s.io/kubectl` image, non-root, read-only
  root filesystem, all capabilities dropped.

The snapshot contains the Kubernetes version, node list (name/readiness/
role, capped at 20 nodes), certificate expiry, and control-plane component
health:

```json
{
  "clusterId": "my-cluster.example.com",
  "timestamp": "2026-07-04T12:00:00Z",
  "kubernetes": { "version": "v1.31.8" },
  "nodes": [{ "name": "cp-1", "status": "True", "roles": "control-plane" }],
  "components": [
    { "name": "apiserver", "status": "Healthy" },
    { "name": "etcd", "status": "Healthy" },
    { "name": "scheduler", "status": "Healthy" },
    { "name": "controller-manager", "status": "Healthy" }
  ],
  "certificates": { "expiresAt": "2027-06-15T00:00:00Z" }
}
```

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

A `4xx` from the API in those logs usually means the cluster is not
registered (or was deregistered) — re-run `lo kubehz register`.

**Registration failed during provisioning?** It is non-fatal by design;
the cluster is fully functional without it. Re-run `lo kubehz register`
once the API is reachable.

**Fingerprint mismatch at claim time?** For a claim-key registration, the
value must match the `kubehz-claim-<domain>` key **exactly as your Hetzner
console shows it** — copy it fresh from **Security → SSH keys** (or
`hcloud ssh-key list`); if the key was deleted before claiming, re-run
`lo kubehz register` to plant a new one. For a token-verification claim,
the platform matches the fingerprint against the SSH keys in *your* hcloud
account: verify the key that provisioned the cluster is uploaded there, and
compare against `ssh-keygen -E md5 -lf <key>.pub` (the `MD5:` prefix is
accepted and normalized away).

**`spec.kubehz.apiUrl is required` / HTTPS errors?** Set `apiUrl` whenever
`hosting: hosted` or `access != none`, and use an `https://` URL — plain
HTTP is rejected.
