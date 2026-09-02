# Concepts

lok8s rests on a small set of core concepts that stay consistent from local development to production.

## Two Concerns

lok8s cleanly separates two concerns:

| Concern | Handled By | Config File |
|---------|-----------|-------------|
| **Cluster creation** | lok8s operator / `lo` CLI | `cluster.lok8s.yaml` |
| **Cluster content** | kustomize + targets | `kustomization.yaml` per target |

These never mix. The operator (or CLI) creates clusters; kustomize builds what runs on them.

## Domains

Everything in lok8s is keyed by FQDN (Fully Qualified Domain Name). There are two types of domains.

### The default `lok8s.dev` domain

Every lok8s install ships with `clusters/lok8s.dev/`, a preconfigured
cluster domain that runs on a local Docker bridge with valid TLS out of
the box. **You do not need your own domain to get started**:

```bash
lo use lok8s.dev
lo up    # kind cluster on bridge 10.125.0.0/16, *.lok8s.dev dev TLS (cert: generator)
```

`lok8s.dev` is a real DNS zone owned by the lok8s project. It resolves
to the local bridge subnet, so `*.lok8s.dev` works across machines
without `/etc/hosts` edits. The dev TLS is a [`cert:` Secret](/reference/kustomize-plugins#development-certificates-cert)
signed by your local CA; trust it once per machine with `lo trust`.

If you run **multiple lok8s projects in parallel**, use numbered
subdomain shards: `*.1.lok8s.dev`, `*.2.lok8s.dev`, ..., `*.100.lok8s.dev`.
Each shard maps to a distinct bridge subnet so projects don't collide.

You can always **bring your own FQDN** alongside (or instead of)
`lok8s.dev`. See the FQDN convention below.

### Cluster Domains

A cluster domain has a `cluster.lok8s.yaml` spec. **One cluster = one
FQDN = one folder.** The folder name under `clusters/` MUST match the
`spec.cluster.domain` field: that is how lok8s identifies the cluster.

The cluster's `spec.cluster.domain` should be the **k8s API endpoint
hostname**, not the user-facing brand domain. For example:

- A local kind cluster on your laptop that serves `*.example.com` via
  `/etc/hosts` → folder `example.com/`, `domain: example.com`
- A production cluster whose k8s API LB is `cluster.example.in.net`
  but which serves public traffic on `app.example.com`, `api.example.com` →
  folder `cluster.example.in.net/`, `domain: cluster.example.in.net`

You declare the user-facing service hostnames (`app.*`, `api.*`, etc.)
**explicitly per service** in your Envoy/Ingress routes. lok8s does not
derive them from `spec.cluster.domain`. This separation lets you run
multiple clusters that serve the same brand domain in different
environments (local dev vs staging vs production) without folder
collisions.

This convention falls out naturally:

```
clusters/
├── example.com/                  # local dev (kind, claims example.com locally)
│   └── cluster.lok8s.yaml        #   spec.cluster.domain: example.com
└── cluster.example.in.net/       # production (KubeOne, k8s API LB)
    └── cluster.lok8s.yaml        #   spec.cluster.domain: cluster.example.in.net
```

Both clusters can run identical workloads that expose identical service
hostnames. The cluster identity stays distinct, and the served domains
are routing concerns.

The canonical "minimal" Lo cluster spec is just a kind and a domain:
the framework derives network, registries, nodes, loadBalancer,
runtime, and bootstrap from the domain (slot-derived for `*.lok8s.dev`)
and from domain-independent defaults. See the
[Specs reference](/reference/specs) for the full defaulting table.

```yaml
# clusters/lok8s.dev/cluster.lok8s.yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  cluster:
    domain: lok8s.dev
```

Override any field explicitly when you want to. The defaults only
fill in what is missing.

### One cluster per plane — NOT per subdomain or service

A `cluster.lok8s.yaml` is **one physical cluster**, normally **one per
plane** (local dev, staging, production). It is *not* one-per-subdomain
and *not* one-per-service. The hostnames a plane serves
(`app.example.com`, `api.example.com`, `auth.example.com`, even a platform
UI like `kkp.example.com`) are **routing + `targets/` inside that one
cluster** (Envoy/HTTPRoutes), never their own `cluster.lok8s.yaml`.

```
clusters/
├── example.com/            # the dev cluster              ✅
├── cluster.example.in.net/ # the prod cluster             ✅
├── app.example.com/        # ❌ a subdomain is NOT a cluster
├── api.example.com/        # ❌ ditto
└── kkp.example.com/        # ❌ KKP is a TARGET inside the cluster, not a cluster
```

If a plane needs a platform (KKP, a registry, monitoring), add it as a
**target** in that plane's `spec.bootstrap` (`./targets/kkp`), not a
second cluster. The *only* place a separate spec is legitimate is
provisioning a **managed/tenant** cluster as a product output (a
`kind: Kkp`/`kind: Capi` spec that represents a **customer's** cluster).
That is a different concern from your own platform plane, and it is never
a per-subdomain folder of your own apex domain.

> Recurring trap: agents/tools "tidying" sometimes invent
> `clusters/<sub>.<apex>/cluster.lok8s.yaml` per subdomain. They do not
> belong. Consolidate to the one apex-domain cluster and express the rest
> as targets/routing. (`lo lint` should flag >1 cluster spec per apex.)

### Dev mirrors prod

A dev cluster should mirror its production counterpart's spec: **same
`spec.kubernetes.version`**, same bootstrap/targets. It differs **only**
where the infrastructure genuinely must (single control-plane node,
MetalLB vs a cloud LB, mkcert vs ACME, PROXY-protocol optional). A drifted
field like a different K8s version is a bug, not a convenience: dev then
does not validate what prod runs. (Concretely: a platform that requires
K8s ≥ 1.32 silently cannot run on a dev cluster pinned to 1.31. So you
must bump dev to the prod version, which for a kind cluster means a
recreate.)

### Deployment Domains

A deployment domain has a `deploy.lok8s.yaml` spec with a `clusterRef` pointing to a cluster domain. It deploys content to another domain's cluster.

```yaml
# clusters/api.example.com/deploy.lok8s.yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: api
spec:
  clusterRef:
    domain: example.com
```

> **Note:** Deploy CRD workload selection is under rework: target
> selection will land alongside the `services.yaml` targets-map
> design. For now, a Deploy spec only carries a `clusterRef`.

### Active Domain

The `lo use` command sets the active domain, stored in `clusters/.active`. Most commands default to the active domain when you pass no domain argument.

```bash
lo use lok8s.dev        # set active domain
lo use                  # show active domain and list all domains
```

The full resolution order for every command is `--domain` flag > `DOMAIN_NAME`
environment variable > `clusters/.active` > `lok8s.dev`. The env var outranks
`.active` (the same layering as `KUBECONFIG` vs a kubectl context: an exported
variable is a more explicit act than previously persisted state). If you set
both and they disagree, `lo` prints a one-line notice that names which one won.

## Directory Layout

The top level separates framework and user content: `.lok8s/`
holds framework code, `clusters/` holds user cluster definitions.
Override `PATH_CLUSTERS` to point at a different directory (e.g. a
parallel project tree) without touching the framework.

```
.lok8s/                 # framework (flat tree, all framework-owned)
  lo                    # CLI entrypoint
  libs/                 # shared bash libraries (bootstrap, build, deploy, ...)
  utils/                # helpers (verbose, ip, types, ...)
  addons/               # framework-level cluster addons (cilium, metallb, ...)
  drivers/              # cluster-architecture drivers
    lo/
      main              #   Lo driver (kind)
      cluster/          #   runtime templates
    capi/
      main              #   CAPI driver
    kubeone/
    kkp/
  providers/            # physical infra providers
    hetzner/
  tilt/                 # framework-shipped Tilt extension
    Tiltfile

clusters/               # user cluster definitions
  .active               # runtime state: active domain (gitignored)
  lok8s.dev/            # cluster domain (local dev)
    cluster.lok8s.yaml  # cluster spec
    targets/            # kustomize source directories (the workload plane)
      platform/
        kustomization.yaml
      apps/
        kustomization.yaml
    artifacts/          # built output (gitignored)
      platform/
        artifacts.yaml
      apps/
        artifacts.yaml
      kustomization.yaml  # auto-generated top-level (refs each target)
    .kubeconfig/        # runtime kubeconfigs (gitignored)
    .containerd/        # runtime containerd certs.d tree (gitignored)
  example.com/          # cluster domain (production)
    cluster.lok8s.yaml
    targets/
    artifacts/
```

## Atoms and Molecules

lok8s splits the deployment model into two layers, like React's atoms
and molecules:

- **Addons (atoms)**: reusable third-party artifacts (charts, raw
  manifests, kustomize bases). Framework addons live at
  `.lok8s/addons/<name>/` and work across all drivers/providers.
  Cilium, MetalLB, cert-manager, storage drivers: the building blocks.
  See [Addons](/guide/addons) for the full handbook.
- **Targets (molecules)**: user-named kustomize directories that
  compose one or more addons plus domain-specific resources. Targets
  can reference any kustomize base: a framework addon, a local service,
  or a remote repo. They're the unit of "what runs on this cluster."

**Source**: `clusters/<domain>/targets/<target>/kustomization.yaml`
**Output**: `clusters/<domain>/artifacts/<target>/artifacts.yaml`

## Two Deployment Planes

A cluster's content lives on two distinct planes, handled by different
mechanisms:

### Plane A — Cluster Infrastructure (`spec.bootstrap`)

Things the cluster itself needs to be usable: CNI, CSI, CCM, MetalLB,
cert-manager CRDs. You express them in the cluster spec as an ordered
list of addons. The framework (`.lok8s/libs/bootstrap`) applies them
during provisioning: the **same code path for every driver**, **before**
Tilt starts and **before** any workloads land.

```yaml
spec:
  bootstrap:
    - cilium                  # framework addon → .lok8s/addons/cilium/
    - metallb                 # framework addon → .lok8s/addons/metallb/
    - ./targets/networking    # your own target → clusters/<domain>/targets/networking/
```

Entry resolution:

- **Bare name** (`cilium`) → `.lok8s/addons/<name>/`
- **`./path`** / **`../path`** → relative to `clusters/<domain>/`
- **`/path`** → relative to repo root

Entries form a **dependency DAG**, not a strict sequence: an entry
waits for its `dependsOn:` targets (and its own `wait:` health gates)
and otherwise applies in parallel with its siblings. List order is only
the default edge for entries without a `dependsOn:`. This is where "deploy
order" earns its keep (you cannot safely apply MetalLB before the CNI
runs) without serializing the independent addons.

If you omit `spec.bootstrap`, the framework defaults to `[cilium]`
(every cluster needs a CNI). Framework addons support driver- and
provider-specific values files plus inline overrides. See
[Addons](/guide/addons) for the full precedence chain.

### Plane B — Workloads (`targets/`)

Everything else: your applications, platform services, monitoring, etc.
Lives under `clusters/<domain>/targets/` with one kustomization per
target. **There is no framework-level ordering primitive**: kubectl handles
in-manifest ordering, Tilt handles live runtime dependencies at the
resource level via `resource_deps`, and GitOps engines translate their
own ordering primitives.

The domain kustomization composes its targets into ONE
`clusters/<domain>/artifacts.yaml` (`lo build`). Deploy everything
(`lo deploy`) or a labelled subset (`lo deploy -l lok8s.dev/name=<target>`).

## Cluster Kinds (Drivers)

The cluster kind determines how lok8s creates a cluster. Each kind is
a **driver** at `.lok8s/drivers/<kind>/main` that follows
the [driver contract](/reference/kind-contract).

| Kind | Purpose | Runtime |
|------|---------|---------|
| **Lo** | Local and CI clusters | Docker + kind |
| **Capi** | Production clusters | Cluster API |
| **KubeOne** | Production clusters | KubeOne CLI |
| **Kkp** | Managed clusters | Kubermatic KKP API |

The `kind` field in `cluster.lok8s.yaml` selects the driver:

```yaml
kind: Lo        # sources .lok8s/drivers/lo/main
kind: Capi      # sources .lok8s/drivers/capi/main
kind: KubeOne   # sources .lok8s/drivers/kubeone/main
```

## Infrastructure Providers

Drivers that provision cloud infrastructure delegate to a **provider**
at `.lok8s/providers/<name>/main`. The provider handles VMs, networks,
load balancers, firewalls: everything the driver needs to install
Kubernetes on.

```yaml
spec:
  provider:
    name: hetzner
    config:                    # opaque — provider-specific
      region: fsn1
      cluster_name: prod
```

The relationship is **many-to-many** in principle, but today **Hetzner** is
the implemented provider: CAPI via CAPH, KubeOne natively (see the table
below for the planned AWS provider). Lo can optionally use a provider for
remote clusters (`lo up --remote`). Without `--remote`, Lo runs locally
with no provider.

Every provider produces a **standard output** after provisioning:
a JSON inventory of servers, API endpoint, and network info. Drivers
read this inventory to build their own config (KubeOne → tfjson,
CAPI → Machine templates). The standard output is the contract that
makes drivers and providers independently pluggable.

| Provider | Cloud | Status |
|----------|-------|--------|
| **hetzner** | Hetzner Cloud | Implemented |
| **aws** | AWS | Planned |

See [Specs reference — Provider](/reference/specs#default-resolution) for
the full spec shape.

## Build and Deploy Pipeline

```
lo up <domain>
 ├─ provision               driver creates the cluster (kind/CAPI)
 ├─ bootstrap               apply the spec.bootstrap addon DAG
 │                          (dependsOn/wait gates, parallel otherwise)
 └─ tilt up                 Tilt reads services.yaml, builds targets,
                            applies with service-enable filters, wires
                            image swaps / live reload

lo build                     domain kustomization build
 └─ clusters/<domain>/artifacts.yaml   (ONE artifact, targets composed)

lo deploy [-l key=value]     domain artifact apply
 ├─ extract + apply CRDs (server-side, wait Established)
 ├─ apply remaining resources (optionally label-scoped)
 └─ scoped wait for the manifest's own workloads
```

- `lo up` is the one-shot dev flow: provision + bootstrap + tilt.
- `lo build` and `lo deploy` are the headless primitives: CI uses them
  directly, Tilt wraps them with live-reload.
- No ordering between targets: if you need it, express it in Tilt
  (`resource_deps=`) or in the resources themselves. Cluster-infra
  ordering lives in `spec.bootstrap`, not in the workload plane.

## Service Configuration

For local development with Tilt, you configure services in two layers:

- `services.yaml`: committed base config (which services exist, registry config)
- `services.<config>.yaml`: personal overrides (gitignored, enable/disable services)

Each service has a `lok8s.yaml` in its directory that defines how to build it (Dockerfile, live_update, ports).
