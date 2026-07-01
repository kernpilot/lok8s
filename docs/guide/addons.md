# Bootstrap Addons

Bootstrap addons are cluster infrastructure components applied after
provisioning but before workloads deploy. They live in `.lok8s/addons/`
and are referenced by name in `spec.bootstrap`.

## Usage

```yaml
spec:
  bootstrap:
    - cilium                    # framework addon
    - metallb                   # framework addon
    - ./targets/networking      # cluster-specific target
```

Apply with `lo provision` (runs automatically after cluster creation)
or re-apply independently:

```bash
lo bootstrap --domain kubehz.dev
```

## Addon resolution

| Entry format | Resolves to |
|-------------|-------------|
| `cilium` | `.lok8s/addons/cilium/` |
| `./targets/foo` | `clusters/<domain>/targets/foo/` |
| `/absolute/path` | `/absolute/path/` |

## Provider-aware values

Each addon can ship layered Helm values files. At apply time the
framework merges them in a fixed order, then runs the chart through
khelm → kustomize.

```
.lok8s/addons/cilium/
  chart.yaml              khelm ChartRenderer
  kustomization.yaml      kustomize entry point
  values.yaml             base values (always loaded)
  values.lo.yaml          Lo/kind overrides (tunnel mode, cluster-pool IPAM)
  values.kubeone.yaml     KubeOne/bare metal overrides (native routing)
  values.hetzner.yaml     Hetzner provider overrides (optional)
  values.aws.yaml         AWS provider overrides (optional)
```

### Merge order

Later files override earlier ones. Deep-merge semantics — nested keys
are combined, not replaced.

1. `values.yaml` — base (shared across all drivers and providers)
2. `values.${kind}.yaml` — driver (`lo`, `kubeone`, `capi`, `kkp`)
3. `values.${provider}.yaml` — provider (`hetzner`, `aws`, ...)
4. Inline overrides from `spec.bootstrap` (per-cluster)

### Why this order

The four layers aren't a strict refinement hierarchy — driver and
provider are **orthogonal axes** (the same driver runs on many
providers, the same provider supports many drivers). When they
disagree the framework has to pick a winner. The rule:

> **Facts beat preferences. Narrow scope beats broad scope. Explicit
> intent beats defaults.**

| Layer | Scope | Typical content |
|-------|-------|----------------|
| `values.yaml` | every cluster | Chart-wide defaults that must hold regardless of where the cluster runs (image registries, metric ports, namespaces). |
| `values.${kind}.yaml` | one driver flavor | Driver-required choices (`lo` needs tunnel mode + `cluster-pool` IPAM because kind can't route; `kubeone` prefers native routing on real L3 networks). |
| `values.${provider}.yaml` | one infrastructure | Environment facts the provider knows (BGP peers on Hetzner, ENI limits on AWS, loadBalancer class names). |
| inline | one cluster | Per-cluster intent you can't express elsewhere (enable Hubble for debugging, bump resource limits for a beefy node). |

Provider values win over driver values because provider entries
describe **facts about the environment** ("this cloud uses these IPs
and these API endpoints") while driver entries describe **preferences
for an orchestration flavor** ("we prefer native routing"). Getting a
fact wrong means the cluster doesn't work; getting a preference wrong
means it works sub-optimally.

Inline wins over everything because the user wrote it by hand in the
cluster spec — there's no more specific signal than that.

### Authoring guidance

- Put a value in the lowest layer where it still makes sense. If
  every Lo cluster needs it, put it in `values.lo.yaml`, not in each
  cluster spec.
- Don't duplicate the same value across multiple layers "to be safe"
  — if you change the base value later, the duplicated override will
  hide the change. Let the merge chain do its job.
- `values.${provider}.yaml` is optional. Most addons only need base +
  driver; provider-specific files are for addons that actually depend
  on cloud APIs or topology (CCM, CSI, LB controllers).

## Inline overrides

Override specific values per cluster without creating custom targets:

```yaml
spec:
  bootstrap:
    - cilium:
        encryption:
          enabled: true
        hubble:
          enabled: true
    - metallb
```

The inline config is deep-merged on top of the provider-aware defaults.

For an entry that needs more than just inline values, use the explicit map keys
`values:`, `env:`, `wait:`, `dependsOn:`, and `name:` (any one of them switches the
entry to this form; otherwise the whole map is treated as inline values, as above):

```yaml
spec:
  bootstrap:
    - cert-manager:
        wait: true            # global gate — see below
    - ccm:
        values:               # helm values (chart addons only)
          env:
            ROBOT_ENABLED: { value: "true" }
        env:                  # envsubst overrides for this entry's render
          LOK8S_USER_FOO: bar
    - cert-manager-webhook-hetzner:
        dependsOn: [cert-manager]   # wait for cert-manager's READINESS first
    - rook-ceph                     # the operator addon (.lok8s/addons/rook-ceph)
    - ./targets/rook-ceph:
        name: rook-ceph-cluster     # disambiguate from the rook-ceph addon above
        dependsOn: [rook-ceph]      # operator must be Ready before the CephCluster
```

- **`values:`** — Helm values, deep-merged like the inline form. Chart addons
  only; setting it on a kustomize target (a `./targets/` dir with no `chart.yaml`)
  is an error.
- **`env:`** — extra envsubst variables exported only while *this* entry renders.
  Name them to match the whitelist the addons reference (`LOK8S_USER_*` /
  `LOK8S_SPEC_*`), e.g. cilium's `${LOK8S_USER_API_HOST}`. Each value must be a
  scalar (`KEY: value`); a map/array value is rejected.
- **`wait:`** — global-gate flag, default `false` (see next section). Must be a
  real boolean (`true`/`false`); `yes`/`on`/`1` are rejected.
- **`dependsOn:`** — a list of entry names this entry must wait for before it
  applies (see next section). Each name is the *resolved* name of another entry:
  the map-key for a chart entry, the basename for a `./path` entry
  (`./targets/networking` → `networking`), the scalar for a bare entry, or an
  explicit `name:` override (below). Must be a YAML list of scalar names; an
  unknown name, an *ambiguous* name (one shared by two entries), or a dependency
  cycle is an error.
- **`name:`** — overrides this entry's **identifier**. By default an entry's name
  is the resolved name (map-key for a chart entry, basename for a `./path`). `name:`
  replaces it — for **both** being a `dependsOn` target *and* resolving `dependsOn`
  references — but it does **not** change which directory the addon renders from
  (still the path/map-key). Reach for it to break a basename collision: the
  `rook-ceph` addon and a `./targets/rook-ceph` target both resolve to `rook-ceph`,
  so a `dependsOn: [rook-ceph]` is ambiguous (and the target would even self-
  reference). Give the target a distinct `name: rook-ceph-cluster` and depend on
  `rook-ceph` (the addon) unambiguously. Must be a non-empty scalar matching
  `[A-Za-z0-9._-]+`; a name that duplicates another entry's name is an error.

::: danger BREAKING CHANGE — migrate before your next `lo up`
`values`, `env`, `wait`, `dependsOn`, and `name` are now **reserved keys** at the
top level of an inline map entry. Any one of them present switches the entry to the
explicit schema above. This **silently changes the meaning** of a legacy entry
whose inline Helm values *happen to use one of those names as a top-level chart
value*.

The canonical case is the hcloud CCM, whose chart takes a top-level `env:` block:

```yaml
# BEFORE — `env` was a Helm chart value (whole map = inline values)
- ccm:
    env:
      ROBOT_ENABLED: { value: "true" }

# AFTER — `env` is now the reserved envsubst key, so the line above is
#         reinterpreted as envsubst overrides (and its map value is rejected).
#         Nest the chart values under `values:`:
- ccm:
    values:
      env:
        ROBOT_ENABLED: { value: "true" }
```

The same applies to any addon whose Helm values define a top-level `values`,
`env`, `wait`, or `dependsOn` key — but they now fail **differently**, so don't
assume "no error":

- **`values:`** silently reinterprets as the reserved key — **no error**, the
  entry just renders different (probably wrong) values.
- **`env:`** reinterprets as the reserved key and a map/array value is **rejected**
  (the scalar rule above — this catches the CCM case loudly).
- **`wait:`** reinterprets as the reserved key; a *boolean* is accepted silently,
  but a **non-boolean** `wait:` (e.g. `wait: "10s"`, or a `wait:` map) now **fails
  loudly** via the boolean validation above.
- **`name:`** reinterprets as the reserved identifier key; a non-scalar or
  charset-invalid value **fails loudly**, and a valid scalar silently becomes the
  entry's identity instead of a Helm value.

There is no automatic migration, so audit every inline `spec.bootstrap` map entry
and move such keys under `values:` **before** the next `lo up` / `lo provision`.
:::

## Parallelism, gates, and dependencies

`spec.bootstrap` entries form a **dependency DAG** and apply **concurrently** by
default — independent addons (CNI, CCM, metrics-server, RBAC …) no longer wait
for each other's workloads to become Ready before the next one starts. Two keys
add ordering edges:

- **`dependsOn: [name, …]`** — a **local edge**: this entry waits only for the
  named entries' workloads to become **Ready** (not just applied) before it
  applies. Everything else still runs in parallel. Reach for this first: it lets
  independent CRD-operators fan out while still expressing the few real
  "X needs Y live" relationships.
- **`wait: true`** — a **global gate**: lok8s finishes everything before the
  gate, then applies the gate *and* waits for its workloads to be Ready, before
  **anything** after it starts. It is the heavy hammer — use it only for a true
  whole-cluster prerequisite (the CNI, the CCM), not for one downstream consumer.

```yaml
spec:
  bootstrap:
    - cilium:
        wait: true            # global gate: the CNI must be live cluster-wide
    - cert-manager            # these fan out in parallel after cilium …
    - metrics-server
    - ccm
    - cert-manager-webhook-hetzner:
        dependsOn: [cert-manager]   # … but THIS one waits for cert-manager Ready
    - ./targets/networking:
        dependsOn: [cert-manager]   # local edge, not a global gate
```

### wait vs dependsOn

| | `wait: true` | `dependsOn: [name, …]` |
|---|---|---|
| Scope | **global gate** — everything before drains; everything after waits for it | **local edge** — only this entry waits, only for its named deps |
| Waits on | the gate's own readiness | the **readiness** of each named dep |
| Use for | a cluster-wide prerequisite (CNI, CCM) | "X needs Y live" between two specific addons |
| Parallelism | serializes the whole stack at that point | preserves parallelism for everything off the edge |

Readiness waits are **selective**: an entry runs `kapply::wait_ready` only when
something depends on it — a `dependsOn` target, an entry sitting behind a gate, or
the gate itself. A **pure leaf** (nothing depends on it) just applies and is done,
skipping the health-wait entirely. So adding a `dependsOn` edge is what *makes* its
target incur a readiness wait; addons nobody waits on stay fire-and-forget.

A `dependsOn` name must resolve to **exactly one** other entry. An unknown name is
an error; so is an **ambiguous** one — a name shared by two entries (e.g. the
`rook-ceph` addon and a `./targets/rook-ceph` target both resolving to `rook-ceph`)
referenced by a `dependsOn` can't be resolved, so set an explicit `name:` on one of
them to disambiguate. (A shared basename that *nothing* depends on is only a
warning — it won't break a barrier-only config.) The resulting graph must also be
acyclic (a cycle is an error — it would deadlock).

The concurrency cap defaults to 8 and is tunable with
`LOK8S_BOOTSTRAP_PARALLEL` (set it to `1` for clean, one-at-a-time output).

### Failure handling

A bootstrap wants to apply **as much as possible**, so a failing entry does **not**
stop the whole run. When an entry fails, lok8s skips **only that entry's transitive
dependents** — the entries that `dependsOn` it (directly or through a chain), plus,
for a failed `wait: true` gate, everything positioned after the gate (a gate's
dependents are all-after). Those would fail behind the broken dependency anyway, so
each is skipped and logged:

```
bootstrap: skipping 'cnpg-cluster' — a dependency failed (cnpg-operator)
```

Everything **unrelated** to the failure keeps applying in parallel. In particular a
failed **leaf** (nothing depends on it — e.g. `gatus`, `tempo`) skips *nothing*: the
rest of the stack still applies.

The run **returns non-zero** if anything failed *or* was skipped; it returns zero
only when every entry completed cleanly. Because the failed and skipped entries are
left un-converged, re-run `lo up` / `lo provision` to reconcile once the underlying
cause is fixed.

## Framework addons

| Addon | What it installs | Chart |
|-------|-----------------|-------|
| `cilium` | Cilium CNI | `cilium/cilium` v1.19.2 |
| `metallb` | MetalLB L2 load balancer | `metallb/metallb` v0.15.3 |
| `cert-manager` | cert-manager controller + CRDs (Issuers, Certificates) | `jetstack/cert-manager` v1.20.1 |
| `cert-manager-webhook-hetzner` | Hetzner DNS-01 ACME solver webhook — **opt-in**; bootstrap *after* `cert-manager`. Only clusters that issue via Hetzner DNS-01 (e.g. Let's Encrypt on a public plane) need it; kind/dev clusters serving their Gateway from a `cert:` Secret skip it. | `cert-manager-webhook-hetzner` 0.7.0 |

### Cilium driver-specific behavior

A concrete example of the driver-layer in action — these values are
set in `values.lo.yaml` and `values.kubeone.yaml`:

| Driver | IPAM | Routing | Why |
|--------|------|---------|-----|
| Lo (kind) | `cluster-pool` | `tunnel` | Kind nodes are Docker containers — no L3 routing available |
| KubeOne | `kubernetes` | `native` | Real infrastructure — native routing, kube-proxy replacement works |

### MetalLB

MetalLB uses the `${LOK8S_SPEC_LOADBALANCER_POOL}` envsubst variable
from `spec.loadBalancer.pool` in the cluster spec. The pool range
defines the IP addresses MetalLB can assign to LoadBalancer services.

## Writing a custom addon

1. Create a directory under `.lok8s/addons/<name>/`
2. Add a `kustomization.yaml` (required)
3. For Helm charts: add `chart.yaml` (khelm ChartRenderer) + `values.yaml`
4. For raw manifests: list them in `kustomization.yaml` resources
5. Add driver/provider-specific values files as needed
6. Reference in `spec.bootstrap` by name

## Addons vs targets vs inline — where does it go?

Three homes, chosen by how reusable and how large the change is:

| Home | For | Lives in |
|------|-----|----------|
| **Framework addon** | a generic, reusable install — an operator + CRDs, a controller, a CNI/CSI/LB chart | `.lok8s/addons/<name>/` |
| **Inline `bootstrap` value** | a *small* per-cluster value override of an addon | the `spec.bootstrap` map entry |
| **Target** | per-cluster glue an addon can't carry — instance CRs, routes/ReferenceGrants tied to *this* cluster's Gateway + domain, `Plan`s, or large chart values | `clusters/.targets/<name>/` (shared) or `clusters/<domain>/targets/<name>/` (one cluster) |

Reach for them in that order: **inline first** (smallest), then an **addon** (if
it's a reusable install), then a **target** (only for real per-cluster glue).

### Split a component: install → addon, glue → target

Most infrastructure is **both** a reusable install *and* some cluster-specific
config. Don't put the whole thing in a target — split it: the addon ships the
generic atom, the target carries only the glue.

| Component | Addon (`.lok8s/addons/`) | Target (`clusters/.../targets/`) |
|-----------|--------------------------|----------------------------------|
| CloudNativePG | `cnpg-operator` (operator + CRDs) | `cnpg-cluster` (the `Cluster` CR) |
| Rook-Ceph | `rook-ceph` (operator + CRDs) | `rook-ceph` (CephCluster/pool/StorageClass) |
| system-upgrade | `system-upgrade-controller` (controller + CRD) | `system-upgrade-controller` (the `Plan`s + trigger) |
| Mailpit | `mailpit` (ns + deployment + service) | `mailpit` (HTTPRoute + ReferenceGrant) |

Bootstrap the addon **before** the target that depends on it — CRDs/controller
must exist before the CRs. When the per-cluster glue is *chart values* too large
for inline (e.g. Grafana's OIDC config), let the target re-render the chart
layering the addon's base values, and bootstrap **the target** (not the bare
addon) so the chart isn't rendered twice.

### Shared vs per-cluster targets

A target's directory placement follows how many clusters use it:

- `clusters/.targets/<name>/` — a **shared base**, used when **more than one
  cluster** needs the same glue (e.g. `networking`). Per-cluster overlays
  compose it via kustomize (`resources: [ ../../.targets/<name> ]`) and patch
  only the differences.
- `clusters/<domain>/targets/<name>/` — glue **one cluster** uses; skip the
  shared-base indirection.

Only promote a target into `.targets/` once a second cluster actually consumes
it — a single-cluster target in the shared base is needless indirection.
