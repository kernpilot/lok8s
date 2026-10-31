# Bootstrap Addons

Bootstrap addons are cluster infrastructure components applied after
provisioning but before workloads deploy. They live in `.lok8s/addons/`,
and you reference them by name in `spec.bootstrap`.

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

### Preview before you apply

A bootstrap re-applies every entry with `kubectl apply --server-side
--force-conflicts`, which overwrites live drift in the addons it touches.
See what would change first, and apply one entry at a time:

```bash
lo bootstrap --dry-run --render-dir /tmp/bootstrap   # render + server-side diff, apply nothing
lo bootstrap --dry-run --no-diff                     # render only, no cluster access
lo bootstrap --only cert-manager                     # apply this entry only
lo bootstrap --only cert-manager,envoy-gateway --dry-run
lo bootstrap --only cert-manager --with-restore      # also restore.d first
```

The dry run renders every entry in place, exactly as the real run does
(the same project, `HOME`, credentials and CA), applies nothing, and prints
one line per entry (`unchanged`, `differs changed=N new=M`,
`inconclusive (needs-crd)`, `inconclusive (chart dependencies not
vendored)`, `rendered`, `skipped`, `error`) plus a summary, with a
`restore.d` line first. Marks after the status: `(non-deterministic: N)`
when objects change on each render (they are listed, for example the CA and
TLS Secrets of Cilium; `--ignore-nondeterministic` diffs the entry without
them), `(non-repeatable)` when the render generated or updated secret
material that the store does not hold, or read such material that an
earlier entry of the run generated, and `(partial: KRM function skipped)`
(also for a function in a base or component the entry references), or
`(partial: a reference outside the project was not inspected)` for a
reference outside the project or a remote one, which the dry run does not
read. The
chart dependency check is Helm's own (the bash implementation needs helm
v3.3 or later on `PATH` for it). The exit code is 0 when nothing would change, 1 only when a
completed diff found a change, and 2 for all other failures.

The two implementations decide the chart dependency check the same way. The
Go implementation uses the Helm v3.21.2 library (the loader and the status
rule of `helm dependency list`). The bash implementation runs `helm
dependency list` and `helm lint` and reads only the table on stdout (helm's
log lines on stderr, for example about a symlinked subchart, are not part of
the decision). Tests pin both on the same real charts, from one table
(`internal/render/testdata/chartgate-fixtures.txt`): a symlinked subchart,
a v2 chart with `requirements.yaml`, an archive next to the subchart
directory, two archives of one dependency, a misnamed archive, and a chart, a
subchart or a dependency without a version. For each refused chart, both
print the same cause and next action. The two can still differ in
the cases below. Then one implementation is inconclusive and the other
renders a chart that khelm does not build. Neither one renders a chart
whose render writes into the chart directory.

- Without helm v3 (3.3 or a later 3.x) on `PATH`, the bash implementation
  is inconclusive.
- When helm prints no dependency table that the bash implementation can
  read, it is inconclusive.
- A v2 chart can have a `file://` dependency in `Chart.yaml` and a
  `requirements.yaml` that replaces the dependencies. Helm (and khelm) then
  read only `requirements.yaml`, but the bash implementation reads both
  files and is inconclusive.
- A helm v3 release other than v3.21.2 can give a different status for a
  dependency. Then either implementation can be the inconclusive one.

The dry run prevents accidental side effects of legitimate addon code and
configuration. It is not a security boundary: a malicious addon can still
execute code during a dry run, and the real bootstrap runs the same addon
with full credentials. lo and its built-in generators never apply anything
and never persist new state in a dry run. Executable addon code (an exec
plugin other than lok8s' Secret and ChartRenderer, a KRM exec or container
function, a remote base, a `helmCharts` chart that is not in `chartHome`, a
build of chart dependencies, a helm plugin, a `bash:` secrets generator) is
refused unless you pass `--allow-generators`, which runs it as trusted, with
the side effects of a real run. Helm caches may be populated. An entry's
`env:` goes only to the render and cannot set lo's own variables
(`LOK8S_BOOTSTRAP_*`, `BOOTSTRAP_*`, `bootstrap_*`, `PATH_SECRETS`,
`KUBECONFIG`, `KUSTOMIZE_PLUGIN_HOME`, `HELM_PLUGINS`). `--render-dir` holds
the rendered manifests and diffs, Secrets in plaintext (mode 0700).
`--only` leaves `restore.d` out unless you add `--with-restore`. The full
description, with the table of what a dry run refuses and what it does not
prevent, is in [Deployment](deployment.md#preview-first-dry-run). Every flag
here also exists on `lo provision --bootstrap`.

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
  values.kubeone.yaml     KubeOne/bare metal overrides (tunnel + WireGuard)
  values.hetzner.yaml     Hetzner provider overrides (optional)
  values.aws.yaml         AWS provider overrides (optional)
```

### Merge order

Later files override earlier ones. Deep-merge semantics: nested keys
combine instead of replace.

1. `values.yaml`: base (shared across all drivers and providers)
2. `values.${kind}.yaml`: driver (`lo`, `kubeone`, `capi`, `kkp`)
3. `values.${provider}.yaml`: provider (`hetzner`, `aws`, ...)
4. Inline overrides from `spec.bootstrap` (per-cluster): `valueFiles:`
   entries first (in list order), then the inline `values:` map on top

### Why this order

The four layers are not a strict refinement hierarchy: driver and
provider are **orthogonal axes** (the same driver runs on many
providers, the same provider supports many drivers). When they
disagree the framework has to pick a winner. The rule:

> **Facts beat preferences. Narrow scope beats broad scope. Explicit
> intent beats defaults.**

| Layer | Scope | Typical content |
|-------|-------|----------------|
| `values.yaml` | every cluster | Chart-wide defaults that must hold regardless of where the cluster runs (image registries, metric ports, namespaces). |
| `values.${kind}.yaml` | one driver flavor | Driver-required choices (`lo` needs tunnel mode + `cluster-pool` IPAM because kind can't route; `kubeone` uses tunnel/vxlan (nodes span subnets) plus WireGuard encryption). |
| `values.${provider}.yaml` | one infrastructure | Environment facts the provider knows (BGP peers on Hetzner, ENI limits on AWS, loadBalancer class names). |
| inline | one cluster | Per-cluster intent you can't express elsewhere (enable Hubble for debugging, bump resource limits for a beefy node). |

Provider values win over driver values because provider entries
describe **facts about the environment** ("this cloud uses these IPs
and these API endpoints") while driver entries describe **preferences
for an orchestration flavor** ("we prefer native routing"). Getting a
fact wrong means the cluster doesn't work; getting a preference wrong
means it works sub-optimally.

Inline wins over everything because the user wrote it by hand in the
cluster spec: there is no more specific signal than that.

### Authoring guidance

- Put a value in the lowest layer where it still makes sense. If
  every Lo cluster needs it, put it in `values.lo.yaml`, not in each
  cluster spec.
- Do not duplicate the same value across multiple layers "to be safe".
  If you change the base value later, the duplicated override hides
  the change. Let the merge chain do its job.
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

lok8s deep-merges the inline config on top of the provider-aware defaults.

For an entry that needs more than just inline values, use the explicit map keys
`values:`, `valueFiles:`, `env:`, `wait:`, `dependsOn:`, and `name:`. Any one of
them switches the entry to this form. Otherwise lok8s treats the whole map as
inline values, as above:

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
    - gatus:
        valueFiles:           # helm values FILES, relative to the cluster dir
          - ./targets/gatus/values.dev.yaml
    - cert-manager-webhook-hetzner:
        dependsOn: [cert-manager]   # wait for cert-manager's READINESS first
    - rook-ceph                     # the operator addon (.lok8s/addons/rook-ceph)
    - ./targets/rook-ceph:
        name: rook-ceph-cluster     # disambiguate from the rook-ceph addon above
        dependsOn: [rook-ceph]      # operator must be Ready before the CephCluster
```

- **`values:`** sets Helm values, deep-merged like the inline form. Chart addons
  only: setting it on a kustomize target (a `./targets/` dir with no `chart.yaml`)
  is an error.
- **`valueFiles:`** takes a list of Helm values **files**, for per-cluster value
  blocks too big to inline (a gatus endpoint list, a monitoring scrape config).
  Each path resolves **relative to the cluster directory** (the one that contains
  `cluster.lok8s.yaml`, the same base `./targets/...` entries resolve from).
  Absolute paths pass through unchanged. The files merge in list order **between**
  the provider values and the inline `values:` map, so the full stack is
  `values.yaml` < `values.${kind}.yaml` < `values.${provider}.yaml` <
  `valueFiles:` (in order) < `values:`. The inline map stays the most explicit
  signal. Same deep-merge semantics as every other layer (nested maps combine,
  lists replace). It must be a YAML list of path strings. A missing file is a hard
  error (never silently skipped), and like `values:` it is chart-addons-only.
- **`env:`** adds extra envsubst variables, exported only while *this* entry
  renders. Name them to match the whitelist the addons reference (`LOK8S_USER_*` /
  `LOK8S_SPEC_*`), e.g. cilium's `${LOK8S_USER_API_HOST}`. Each value must be a
  scalar (`KEY: value`); a map/array value is an error.
- **`wait:`** is the global-gate flag, default `false` (see next section). It must
  be a real boolean (`true`/`false`); `yes`/`on`/`1` are errors.
- **`dependsOn:`** lists the entry names this entry must wait for before it
  applies (see next section). Each name is the *resolved* name of another entry:
  the map-key for a chart entry, the basename for a `./path` entry
  (`./targets/networking` → `networking`), the scalar for a bare entry, or an
  explicit `name:` override (below). It must be a YAML list of scalar names; an
  unknown name, an *ambiguous* name (one shared by two entries), or a dependency
  cycle is an error.
- **`name:`** overrides this entry's **identifier**. By default an entry's name
  is the resolved name (map-key for a chart entry, basename for a `./path`). `name:`
  replaces it for **both** roles (being a `dependsOn` target *and* resolving
  `dependsOn` references), but it does **not** change which directory the addon
  renders from (still the path/map-key). Reach for it to break a basename
  collision: the `rook-ceph` addon and a `./targets/rook-ceph` target both
  resolve to `rook-ceph`, so a `dependsOn: [rook-ceph]` is ambiguous (and the
  target would even self-reference). Give the target a distinct
  `name: rook-ceph-cluster` and depend on `rook-ceph` (the addon) unambiguously.
  It must be a non-empty scalar that matches `[A-Za-z0-9._-]+`; a name that
  duplicates another entry's name is an error.

::: danger BREAKING CHANGE — migrate before your next `lo up`
`values`, `valueFiles`, `env`, `wait`, `dependsOn`, and `name` are now **reserved
keys** at the top level of an inline map entry. Any one of them present switches
the entry to the explicit schema above. This **silently changes the meaning** of a legacy entry
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
`env`, `wait`, or `dependsOn` key. But they now fail **differently**, so do not
assume "no error":

- **`values:`** silently reinterprets as the reserved key: **no error**, the
  entry just renders different (probably wrong) values.
- **`env:`** reinterprets as the reserved key and a map/array value is **rejected**
  (the scalar rule above, which catches the CCM case loudly).
- **`wait:`** reinterprets as the reserved key; a *boolean* passes silently,
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
default: independent addons (CNI, CCM, metrics-server, RBAC …) no longer wait
for each other's workloads to become Ready before the next one starts. Two keys
add ordering edges:

- **`dependsOn: [name, …]`** is a **local edge**: this entry waits only for the
  named entries' workloads to become **Ready** (not just applied) before it
  applies. Everything else still runs in parallel. Reach for this first: it lets
  independent CRD-operators fan out while still expressing the few real
  "X needs Y live" relationships.
- **`wait: true`** is a **global gate**: lok8s finishes everything before the
  gate, then applies the gate *and* waits for its workloads to be Ready, before
  **anything** after it starts. It is the heavy hammer: use it only for a true
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
| Scope | **global gate**: everything before drains; everything after waits for it | **local edge**: only this entry waits, only for its named deps |
| Waits on | the gate's own readiness | the **readiness** of each named dep |
| Use for | a cluster-wide prerequisite (CNI, CCM) | "X needs Y live" between two specific addons |
| Parallelism | serializes the whole stack at that point | preserves parallelism for everything off the edge |

Readiness waits are **selective**: an entry runs `kapply::wait_ready` only when
something depends on it (a `dependsOn` target, an entry behind a gate, or
the gate itself). A **pure leaf** (nothing depends on it) just applies and is done,
skipping the health-wait entirely. So adding a `dependsOn` edge is what *makes* its
target incur a readiness wait; addons nobody waits on stay fire-and-forget.

A `dependsOn` name must resolve to **exactly one** other entry. An unknown name is
an error, and so is an **ambiguous** one: a name shared by two entries. (E.g. the
`rook-ceph` addon and a `./targets/rook-ceph` target both resolve to `rook-ceph`,
so a `dependsOn` cannot resolve that name.) Set an explicit `name:` on one of
them to disambiguate. (A shared basename that *nothing* depends on is only a
warning: it does not break a barrier-only config.) The resulting graph must also be
acyclic (a cycle is an error because it would deadlock).

The concurrency cap defaults to 8 and is tunable with
`LOK8S_BOOTSTRAP_PARALLEL` (set it to `1` for clean, one-at-a-time output).
On `lo-full` the entries apply one at a time regardless (the in-process
render puts each entry's `env:` overlay in the process environment while it
renders; `lo -v` says so); `LO_RENDER=exec` restores the parallel run.

### Failure handling

A bootstrap wants to apply **as much as possible**, so a failing entry does **not**
stop the whole run. When an entry fails, lok8s skips **only that entry's transitive
dependents**: the entries that `dependsOn` it (directly or through a chain), plus,
for a failed `wait: true` gate, everything positioned after the gate (a gate's
dependents are all-after). Those would fail behind the broken dependency anyway, so
lok8s skips and logs each one:

```
bootstrap: skipping 'cnpg-cluster' — a dependency failed (cnpg-operator)
```

Everything **unrelated** to the failure keeps applying in parallel. In particular a
failed **leaf** (nothing depends on it, e.g. `gatus`, `tempo`) skips *nothing*: the
rest of the stack still applies.

The run **returns non-zero** if any entry failed *or* was skipped; it returns zero
only when every entry completed cleanly. The failed and skipped entries stay
un-converged, so re-run `lo up` / `lo provision` to reconcile once you fix the
underlying cause.

## Framework addons

| Addon | What it installs | Chart |
|-------|-----------------|-------|
| `cilium` | Cilium CNI | `cilium/cilium` v1.20.4 |
| `metallb` | MetalLB L2 load balancer | `metallb/metallb` v0.16.1 |
| `cert-manager` | cert-manager controller + CRDs (Issuers, Certificates) | `jetstack/cert-manager` v1.21.3 |
| `cert-manager-webhook-hetzner` | Hetzner DNS-01 ACME solver webhook. **Opt-in**: bootstrap *after* `cert-manager`. Only clusters that issue via Hetzner DNS-01 (e.g. Let's Encrypt on a public plane) need it; kind/dev clusters serving their Gateway from a `cert:` Secret skip it. | `cert-manager-webhook-hetzner` 0.7.0 |
| `envoy-gateway` | Envoy Gateway controller **+ the upstream Gateway API CRD bundle**. See [upgrading in place](#envoy-gateway-upgrading-in-place) before you bump the pin | `envoyproxy/gateway-helm` v1.9.3 |
| `rook-ceph` | Rook-Ceph operator + CRDs, and the ceph-csi-operator work orders that yield the RBD `CSIDriver`. The CSI node plugin runs with a [narrowed ClusterRole](#rook-ceph) | `rook-release/rook-ceph` v1.20.6, `ceph-csi-drivers` 1.0.4 |
| `rook-ceph-csi-kms` | **Opt-in**, after `rook-ceph`: gives the CSI node plugin back the Secret / ServiceAccount-token grants that KMS-encrypted volumes and `mounter: rbd-nbd` need. See [rook-ceph](#rook-ceph) | raw manifests |
| `redis-operator` | The OT-CONTAINER-KIT Redis operator + CRDs (`Redis`, `RedisReplication`, `RedisSentinel`, `RedisCluster`). It runs with [narrowed RBAC](#redis-operator), and nothing aggregates into the built-in `admin` role. The CRs go in a target | `ot-container-kit/redis-operator` 0.26.1 |
| `redis-operator-chart-rbac` | **Opt-in, not recommended**, after `redis-operator`: binds the chart's broad ClusterRole rules to the operator again (without the aggregation). See [redis-operator](#redis-operator) | raw manifests |
| `system-upgrade-controller` | Rancher's system-upgrade-controller. It runs [without cluster-admin](#system-upgrade-controller), with both images pinned by digest. The `Plan`s go in a target | `rancher/system-upgrade-controller` 110.0.0 |
| `system-upgrade-controller-cluster-admin` | **Opt-in, not recommended**, after `system-upgrade-controller`: binds the controller to cluster-admin again. See [system-upgrade-controller](#system-upgrade-controller) | raw manifests |

### Cilium driver-specific behavior

A concrete example of the driver layer in action. These values live
in `values.lo.yaml` and `values.kubeone.yaml`:

| Driver | IPAM | Routing | Encryption | Why |
|--------|------|---------|------------|-----|
| Lo (kind) | `cluster-pool` | `tunnel` | off | Kind nodes are containers on ONE host kernel: no L3 routing, and encrypting loopback-adjacent traffic buys nothing |
| KubeOne | `kubernetes` (driver) → `cluster-pool` effective on Hetzner (the provider layer wins the merge) | `tunnel` (vxlan) | **WireGuard** (pod + node) | Nodes span subnets/locations (cloud subnet + bare-metal vSwitch): native routing needs one L2 segment; traffic crosses shared infrastructure, so it ships encrypted. Mind the MTU: the `MTU` value is the RAW pod MTU and the max wire frame equals the value (measured on cilium 1.19.2; the knob is unchanged in the 1.20 chart). Set it ≤ the smallest underlay link; the effective inner ceiling is value − 130 (see the knob's comment); running pods keep their old veth MTU until restarted |

### MetalLB

MetalLB uses the `${LOK8S_SPEC_LOADBALANCER_POOL}` envsubst variable
from `spec.loadBalancer.pool` in the cluster spec. The pool range
defines the IP addresses MetalLB can assign to LoadBalancer services.

### envoy-gateway — upgrading in place {#envoy-gateway-upgrading-in-place}

The `envoy-gateway` addon installs the Envoy Gateway controller **and the
upstream Gateway API CRD bundle**, because that bundle ships inside the
chart. khelm inflates chart CRDs into the kustomize output, so the usual
"`helm upgrade` skips CRDs" escape hatch does not apply here: bumping the
chart pin **applies new Gateway API CRDs to your cluster**, in the same
apply as the controller.

That makes the pin in `.lok8s/addons/envoy-gateway/chart.yaml` a
cluster-wide change, not an image bump. The versions move together:

| envoy-gateway | Gateway API | notes |
|---|---|---|
| v1.7.1 | v1.4.1 | no `SecurityPolicy.mergeType` |
| v1.8.3 | v1.5.1 | `mergeType` added; standard `ListenerSet`, experimental `XListenerSet` dropped |
| v1.9.0 | v1.6.1 | `TCPRoute`/`UDPRoute`/`TLSRoute` gain `v1` **and move their storage version to it** |
| v1.9.3 | v1.6.1 | the same bundle; read [upgrading to v1.9.3](#envoy-gateway-v1-9-3-hazards) first |

Before any jump that crosses a Gateway API minor, check that no CRD's
`status.storedVersions` names a version the incoming bundle stops
serving. The apiserver rejects such an update outright:

```console
$ kubectl get crd -o json | jq -r '.items[]
    | select(.spec.group|test("gateway"))
    | "\(.metadata.name) stored=\(.status.storedVersions|join(","))"'
```

#### Rolling back from v1.9.0 is blocked

::: danger You cannot simply re-pin an older version
Two independent blocks stop a downgrade, and the first one applies **as
soon as v1.9.0 has reconciled once**:

1. **`storedVersions`.** v1.9.0 moves the storage version of `TCPRoute`,
   `UDPRoute` and `TLSRoute` to `v1`, so the apiserver starts recording
   `v1` alongside the old version:

   ```
   tcproutes  storedVersions ["v1alpha2","v1"]
   udproutes  storedVersions ["v1alpha2","v1"]
   tlsroutes  storedVersions ["v1alpha3","v1"]
   ```

   A CRD update may not drop a version still listed in
   `status.storedVersions`. **Which of the three actually blocks you
   depends on the rung**. The older bundle only has to keep serving
   `v1`, and v1.5.1 already does for `TLSRoute`:

   | kind | stored after v1.9.0 | v1.5.1 serves (the v1.8.x rung) | v1.4.1 serves (the v1.7.x rung) |
   |---|---|---|---|
   | `tcproutes` | `v1alpha2`, `v1` | `v1alpha2` → **blocks** | `v1alpha2` → **blocks** |
   | `udproutes` | `v1alpha2`, `v1` | `v1alpha2` → **blocks** | `v1alpha2` → **blocks** |
   | `tlsroutes` | `v1alpha3`, `v1` | `v1`, `v1alpha2`, `v1alpha3` → passes | `v1alpha2`, `v1alpha3` → **blocks** |

   So only `tcproutes` and `udproutes` block a rollback to **v1.8.x**,
   and all three block a rollback to **v1.7.x**.

   These three kinds exist **only on the experimental channel** of the
   Gateway API, which is the channel this addon installs. Check yours
   with `kubectl get crd tcproutes.gateway.networking.k8s.io -o
   jsonpath='{.metadata.annotations.gateway\.networking\.k8s\.io/channel}'`.
   On a standard-channel install the CRDs are absent and this block
   cannot arise at all.

2. **The `safe-upgrades` ValidatingAdmissionPolicy.** v1.8.1 moved
   `ValidatingAdmissionPolicy safe-upgrades.gateway.networking.k8s.io`
   out of the CRD bundle into the chart *templates*, so the addon now
   installs it. It rejects any Gateway API CRD annotated with a bundle
   version below **v1.5**, so a rollback to v1.7.x fails at admission
   until you delete that policy and its binding. The v1.8.x rung
   (v1.5.1) passes this one.
:::

Block 1 has an escape hatch, and it is cheap **only** because these kinds
usually have no objects: most clusters route HTTP and never create a
`TCPRoute`, `UDPRoute` or `TLSRoute`. Deleting a CRD deletes every object
of that kind, so **delete the fewest CRDs the rung needs**, and **check
each one first**.

To roll back to **v1.8.x**, delete two CRDs and leave `TLSRoute` untouched:

```console
$ kubectl get tcproutes,udproutes -A
No resources found
$ kubectl delete crd tcproutes.gateway.networking.k8s.io \
                     udproutes.gateway.networking.k8s.io
```

To roll back to **v1.7.x**, delete all three, plus the admission policy:

```console
$ kubectl get tcproutes,udproutes,tlsroutes -A
No resources found
$ kubectl delete crd tcproutes.gateway.networking.k8s.io \
                     udproutes.gateway.networking.k8s.io \
                     tlsroutes.gateway.networking.k8s.io
$ kubectl delete validatingadmissionpolicybinding \
      safe-upgrades.gateway.networking.k8s.io
$ kubectl delete validatingadmissionpolicy \
      safe-upgrades.gateway.networking.k8s.io
```

If either `kubectl get` prints an object, **stop**: export it, or stay on
v1.9.x. Only a clean `No resources found` makes the delete safe.

Then re-pin `chart.yaml` and run `lo up`: the older bundle recreates the
CRDs you deleted at its own versions.

#### v1.9.0 behaviour changes that no diff shows

Upgrading in place is more than the CRD jump. These change what a running
gateway does without changing anything you wrote:

- **Client-forced tracing is off by default.** Upstream dropped the client
  sampling fraction from **100% to 0%**, so Envoy Gateway no longer honours
  a caller's `x-client-trace-id` forced trace. This is *not* ordinary
  sampling: `samplingRate` still defaults to 100, and traces you sample
  yourself are unaffected. Only client-*forced* traces stop. The addon
  deliberately does **not** pin this: the knob belongs to your own
  `EnvoyProxy`, the addon ships no tracing config at all, and a shared
  gateway that lets any caller force a full trace is not a default worth
  restoring for everyone. If you relied on it, opt back in explicitly:

  ```yaml
  apiVersion: gateway.envoyproxy.io/v1alpha1
  kind: EnvoyProxy
  metadata:
    name: tracing-opt-in
    namespace: envoy-gateway-system
  spec:
    telemetry:
      tracing:
        # a FRACTION, not a percentage: numerator is required,
        # denominator defaults to 100. 100/100 = the pre-1.9 behaviour.
        clientSamplingFraction:
          numerator: 100
        # `tracing` requires a provider — point this at your own collector.
        provider:
          type: OpenTelemetry
          backendRefs:
            - name: otel-collector
              namespace: monitoring
              port: 4317
  ```

  Reference the `EnvoyProxy` from your `GatewayClass`
  (`spec.parametersRef`) for it to take effect.

- **`clientIPDetection: {}` is now rejected.** A `ClientTrafficPolicy` with
  an empty `spec.clientIPDetection` used to pass. CEL validation now
  requires exactly one of `xForwardedFor`, `customHeader` or
  `directSourceIP`.
- **Lua `EnvoyExtensionPolicy` is opt-in**, via `extensionApis.enableLua`.
- **`SecurityPolicy.spec.mergeType` validation got stricter**: xRoute
  targets only (`HTTPRoute`, `GRPCRoute`, `TCPRoute`), rejected on a
  `Gateway`, a Gateway listener or a `ListenerSet`. That is exactly the
  shape [`sso-gate`](#sso-gate) ships, so the addon is unaffected. But a
  hand-written gateway-wide policy that carries `mergeType` must drop the
  field before you can update it again.

#### Upgrading to v1.9.3 {#envoy-gateway-v1-9-3-hazards}

v1.9.3 ships the same Gateway API bundle as v1.9.0, so neither rollback
block above applies between the two. The upgrade still changes what the
running gateway does, and three of the items below need work **before**
you bump the pin. The [Envoy Gateway v1.9.1 release
notes](https://gateway.envoyproxy.io/news/releases/notes/v1.9.1/) have the
details.

- **From v1.9.0 only: a short TLS outage when the new controller
  starts.** v1.9.1 reverts the `0s` SDS and RDS initial-fetch timeout of
  v1.9.0 to Envoy's 15 s default. That changes the SDS configuration of
  every listener and cluster. The proxies that still run when the new
  controller starts hit envoy issue
  [#47309](https://github.com/envoyproxy/envoy/issues/47309). About 15 s
  after the push, their TLS listeners go active without certificates, and
  TLS backends (the global rate limit service included) lose theirs. The
  pods still report Ready. A re-pushed Secret (for example, a rotated
  certificate) restores what uses it; a new pod restores everything.

  On one 2-replica gateway, every TLS handshake failed for about 13 s.
  The proxy log showed `initial fetch timed out for
  type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret`.
  The outage ended when the new proxy pods were Ready.

  The upstream mitigation replaces all proxy pods at once. Set it
  **before** the upgrade, and keep room for twice the replicas. It does
  not prevent the outage, so plan a short one:

  ```yaml
  apiVersion: gateway.envoyproxy.io/v1alpha1
  kind: EnvoyProxy
  metadata:
    name: my-proxy          # the EnvoyProxy your GatewayClass references
    namespace: envoy-gateway-system
  spec:
    provider:
      type: Kubernetes
      kubernetes:
        envoyDeployment:
          strategy:
            type: RollingUpdate
            rollingUpdate:
              maxSurge: 100%
              maxUnavailable: 0
  ```

- **[`sso-gate`](#sso-gate) users sign in again once.** v1.9.1 encrypts
  OAuth2/OIDC session cookies with AES-256-GCM (CVE-2026-47775), and
  sessions from the old proxies are not accepted. The change is in the
  default bootstrap. An `EnvoyProxy` whose `spec.bootstrap` is of type
  `Replace` (the default when no type is given) does not get it. For such
  a proxy, add `envoy.reloadable_features.oauth2_use_gcm_encryption: true`
  and `envoy.reloadable_features.oauth2_legacy_cbc_decrypt_compat: false`
  to a `layered_runtime` static layer.
- **The default proxy image trusts compromised CAs.** The v1.9.3 default,
  `distroless-v1.39.5`, still ships the unpatched CA bundle, which trusts
  the roots of the compromised eMudhra intermediate CAs ([envoy v1.39.6
  release notes](https://github.com/envoyproxy/envoy/releases/tag/v1.39.6)).
  The addon ships no `EnvoyProxy`, so set `distroless-v1.39.6` in your own,
  under `spec.provider.kubernetes.envoyDeployment.container.image` (a tag,
  or a tag plus `@sha256:` digest). Remove the pin when the default
  catches up.
- **Fix every non-https OIDC issuer first.** The `SecurityPolicy` CRD now
  checks `spec.oidc.provider.issuer` against
  `^https://[^/?#@]+(/[^?#]*)?$`, so it rejects any other issuer on the
  next apply. The v1.9.3 controller also rejects such an issuer when it
  translates the policy, so the protected routes stop working as soon as
  the new controller runs.

### sso-gate — OIDC login in front of any service {#sso-gate}

`sso-gate` puts any HTTP service behind OIDC single sign-on **at the
gateway**: no sidecar, no change to the app. It ships one Envoy Gateway
[`SecurityPolicy`](https://gateway.envoyproxy.io/docs/tasks/security/oidc/)
that matches every `HTTPRoute` labeled `sso.lok8s.dev/protect: "true"` in
its namespace. Envoy runs the whole login flow (redirect to the issuer,
callback, session cookie, token validation) before traffic reaches the
service. It works with any spec-compliant OIDC issuer.

```yaml
spec:
  bootstrap:
    - envoy-gateway            # required: v1.8.0+ (SecurityPolicy is its CRD)
    - sso-gate: { dependsOn: [envoy-gateway] }
```

(Without `envoy-gateway` the `SecurityPolicy` CRD does not exist: the
apply retries a few times and then fails that bootstrap entry loudly.
The rest of the DAG continues.)

Then, per service you want protected:

```yaml
# on the service's HTTPRoute
metadata:
  labels:
    sso.lok8s.dev/protect: "true"
```

Routes without the label stay public; removing the label makes a route
public again. A labeled route with a broken policy (unpatched issuer,
missing client Secret) answers **500: the gate fails closed, never
open**. So configure these three things *before* you label any route
(see the header of `.lok8s/addons/sso-gate/kustomization.yaml` for
copy-paste patches):

1. **Issuer + client ID**: patch `SecurityPolicy/sso-gate` from a
   consuming target.
2. **Client secret**: a Secret `sso-gate-client` (key `client-secret`)
   in the policy's namespace, e.g. via the secrets plugin.
3. **Redirect URI**: register `https://<each-protected-host>/oauth2/callback`
   at your issuer.

One sharp edge: `targetSelectors` matches routes in the **same namespace**
as the policy. That is the selector's default behavior, and this addon ships
it with the `default` namespace. Envoy Gateway can widen it with
`targetSelectors[].namespaces`, but that needs a `ReferenceGrant` in every
target namespace. For routes elsewhere it is simpler to layer a second
copy with a kustomize `namespace:` transform.

Note this is about the **policy and the routes it selects**. The Gateway
those routes attach to may sit in a third namespace: merging follows the
route's attachment hierarchy, not namespaces.

#### SSO must ADD to your gateway's guards, not replace them

::: danger A route-level SecurityPolicy REPLACES the Gateway's one
Envoy Gateway resolves overlapping `SecurityPolicy` objects by
**specificity**, and the most specific one wins **entirely**. A policy on
an `HTTPRoute` therefore replaces the policy on that route's `Gateway`:
wholesale, not field by field. If your Gateway carries an IP allowlist, an
`authorization` deny-by-default, JWT or CORS, an unmerged route-level SSO
policy **deletes all of it** for every route you label. The route ends up
*more* reachable than before you protected it.

The only signal is an `Overridden` condition on the Gateway policy, which
names the routes it lost, and which nothing reads unless you look:

```console
$ kubectl get securitypolicy gateway-guard -o yaml
...
    - type: Overridden
      status: "True"
      message: 'This policy is being overridden by other securityPolicies
        for these routes: [default/internal-dashboard]'
```
:::

`sso-gate` ships **`mergeType: StrategicMerge`** for exactly this reason,
so a labeled route **keeps** the gateway-wide guards and **gains** OIDC on
top. When it takes effect the Gateway policy stops reporting the route
under `Overridden` and reports it under `Merged` instead:

```console
    - type: Merged
      status: "True"
      message: 'This policy is being merged by other securityPolicies
        for these routes: [default/internal-dashboard]'
```

::: warning The merge cuts BOTH ways
Merging is not only "the route keeps what it had". A labeled route now
**inherits the Gateway policy's rules as well**, and those rules did not
apply to it before.

So the breaking direction is real. If your Gateway policy carries an IP
allowlist, or `authorization.defaultAction: Deny` with allow rules that
never mentioned this route, then a route that serves fine today over SSO
starts to return **403** the moment you label it. The login succeeds, and
the inherited authorization refuses the request afterwards. Nothing is
misconfigured; the route is simply subject to gateway-wide rules for the
first time.

Before labeling a route on a gateway that has its own `SecurityPolicy`,
read that policy and confirm the route's callers are inside whatever it
allows. `Merged: True` on the Gateway policy means the inheritance is
live, not that it is harmless.
:::

Three consequences:

- **It needs `envoy-gateway` v1.8.0 or later**, the release that added
  `mergeType`. On an older CRD there is no such field, and how that fails
  depends on how you apply: the apiserver **rejects** a **server-side**
  apply (`kubectl apply --server-side`, and every GitOps controller) with
  `.spec.mergeType: field not declared in schema`, while a client-side
  apply drops the field **silently** and leaves you with the replace
  behaviour. Check the CRD before you trust the field:

  ```console
  $ JP='{.spec.versions[?(@.name=="v1alpha1")]'
  $ JP="${JP}.schema.openAPIV3Schema.properties.spec.properties.mergeType.type}"
  $ kubectl get crd securitypolicies.gateway.envoyproxy.io -o jsonpath="${JP}"
  string     # empty output = your Envoy Gateway is too old
  ```

  (Select the version by name, not by `versions[0]`: the order of that
  list is not a contract. Running this for you from `lo doctor` is
  [kernpilot/lok8s#141](https://github.com/kernpilot/lok8s/issues/141).)

- **Upgrade Envoy Gateway before the policy reconciles.** Under `lo up`
  that ordering comes free from `dependsOn: [envoy-gateway]`, and a
  rejected `sso-gate` would only skip *its own* dependents (a failing
  addon never stops unrelated ones; see [Failure
  handling](#failure-handling)). Under **GitOps** it is different: Flux
  and Argo reconcile the rendered set as a unit, so one object rejected
  for `.spec.mergeType: field not declared in schema` fails the whole
  sync. Sequence the gateway upgrade ahead of it there.

- **`mergeType` is legal only on a child target**: `HTTPRoute`,
  `GRPCRoute` or `TCPRoute`. Envoy Gateway rejects it on a `Gateway`, a
  Gateway listener or a `ListenerSet`, because there is no parent to merge
  into. Never copy the field onto a gateway-wide policy.

The trap is not specific to this addon. **Any** route-level
`SecurityPolicy` you write replaces the Gateway's: set `mergeType` on it,
or first check what the Gateway policy was doing for that route. `lo
audit` flags the combination (a gateway-wide default-Deny plus a
route-level policy with no `mergeType`) under `exposed-endpoints`.

To prove the merge on a running gateway, read the generated Envoy config
rather than the policy status. The route entries under a protected host's
virtual host must carry **both** filters:

```console
$ POD=$(kubectl -n envoy-gateway-system get pod -o name \
    -l gateway.envoyproxy.io/owning-gateway-name=<your-gateway> | head -n1)
$ kubectl -n envoy-gateway-system port-forward "${POD}" 19000:19000 & PF=$!
$ curl -s 'localhost:19000/config_dump?resource=dynamic_route_configs' \
  | jq -r '.configs[].route_config.virtual_hosts[]
           | . as $v
           | (($v.routes // []) | map((.typed_per_filter_config // {}) | keys)
              | add // [] | unique) as $f
           | "\($v.name)\trbac=\($f | any(startswith("envoy.filters.http.rbac")))"
           + "\toidc=\($f | any(test("oauth2")))"'

default/gw/https/app_example_com      rbac=true   oidc=false
default/gw/https/internal_example_com rbac=true   oidc=true    # merged

$ kill "${PF}"
```

`rbac=false oidc=true` on a protected host is the bug: the login went on
and the gateway's guard came off. Note the filter order: Envoy runs
`oauth2` **before** `rbac`, so an unauthenticated request from a blocked
source goes to the issuer first and gets refused after it returns.
Access stays gated, but the service's existence is no longer hidden.

### rook-ceph: least privilege for the CSI node plugin {#rook-ceph}

The `rook-ceph` addon installs the Rook operator and the ceph-csi-operator
"work orders" (the `Driver`, the plugin ServiceAccounts and their RBAC).

The CSI **node plugin** is a DaemonSet that tolerates every taint, so it
runs on every node, control planes included. Its ClusterRole is therefore
what an attacker holds after a takeover of any one node.

The upstream `ceph-csi-drivers` chart grants that role ten rules (RBD and
NVMe-oF) or eight (CephFS). Five of them serve volume encryption with an
external KMS, and the addon removes them:

| Rule | What ceph-csi uses it for |
|---|---|
| `secrets` get/list/watch | the KMS passphrase Secret, the Vault tenant token, the `rbd-nbd` volume healer, and the block-volume usage metrics (see the exception below) |
| `configmaps` get | the KMS configuration ConfigMap, the per-tenant Vault config |
| `serviceaccounts` get | the tenant ServiceAccount (`vaulttenantsa`) |
| `serviceaccounts/token` create | a `TokenRequest` for that ServiceAccount: **mints a token for any ServiceAccount in the cluster** |
| `tokenreviews` create | the Vault token-reviewer setup |

A plain, unencrypted RBD or CephFS mount uses none of them: kubelet passes
the Ceph user credentials to the plugin inside the CSI request. The
remaining rules (`nodes` get, `persistentvolumes` and `volumeattachments`
get/list, `persistentvolumeclaims` get, `events`) stay. The plugin reads
them on every start, and none of them returns a credential.

One exception is not about encryption. `NodeGetVolumeStats` on a
`volumeMode: Block` RBD volume reads the controller-publish Secret to
report the used bytes (ceph-csi `internal/rbd/nodeserver.go`,
`blockNodeGetVolumeStats`).

With the narrowed role that read fails on the volume **stats** of block
volumes, not on their mount. Kubelet logs the error and the volume keeps
working. A cluster that needs usage metrics for block volumes bootstraps
`rook-ceph-csi-kms` (below).

The removal is a guarded JSON patch in the addon's `kustomization.yaml`.
A `test` on the complete rule precedes each `remove`. A chart bump that
reorders, merges or widens a rule therefore **fails the render** instead
of removing the wrong rule.

When you bump `ceph-csi-drivers`, compare the indices with the chart's
`templates/*-nodeplugin-cr-rbac.yaml` and adjust the patch. Then refresh
the fixture the render test reads (`internal/addons/testdata/rook-ceph/`,
its header says how).

The patches cover the RBD, CephFS and NVMe-oF node plugin roles (the NFS
role carries `nodes get` only). Each target names the driver name the
addon pins (`rook-ceph.<type>.csi.ceph.com`) and the chart default
(`<type>.csi.ceph.com`). A target for a driver that is off is a no-op and
takes effect when a render turns the driver on.

::: warning Upgrade note: the CephFS and NVMe-oF driver names are now pinned
Only the addon's own `csi-drivers.yaml` selects the drivers. Inline
`values:` in `spec.bootstrap` reach `chart.yaml` (the Rook operator chart),
not this generator. So a cluster that runs CephFS or NVMe-oF through this
addon does it from a fork of the addon, or from a custom target with its
own `csi-drivers.yaml`, and that copy keeps its own driver names when it
rebases on this version.

The driver name is the CSI identity: the `CSIDriver` object, the plugin
workloads, the kubelet registration and `spec.csi.driver` of every PV,
which is immutable. The addon now pins `rook-ceph.cephfs.csi.ceph.com`
(before: the chart default `cephfs.csi.ceph.com`) and
`rook-ceph.nvmeof.csi.ceph.com` (before: `nvmeof.csi.ceph.com`). For
CephFS the pinned name is the one Rook writes into its own StorageClass and
VolumeSnapshotClass (`<operatorNamespace>.cephfs.csi.ceph.com`).

When you rebase your copy:

- **Keep the default name**: the patch targets it too, so the role is
  narrowed and nothing else changes.
- **Adopt the pinned name**: the CSI identity changes. Point your
  StorageClasses and VolumeSnapshotClasses at the new name, migrate the
  PVs (new PVC + copy), then delete the old `Driver` and its classes. Do
  not delete a `Driver` while a PV still uses it.

The patch acts on the render only. A live old `Driver` that is no longer
in the render keeps its broad node plugin ClusterRole, and the bootstrap
does not prune. After the migration delete that `Driver` and its RBAC, or
narrow the old role by hand with the same patch.
:::

#### The controller plugin

The CSI **controller plugin** (ceph-csi plus the external-provisioner,
-attacher, -resizer and -snapshotter sidecars) gets the chart's full
upstream ClusterRole as well: 29 rules for RBD and NVMe-oF, 27 for CephFS,
16 for NFS. The addon removes four of them:

| Rule | What uses it |
|---|---|
| `secrets` get/list/watch | the sidecars `get` the StorageClass-referenced secret. ceph-csi itself does a `get` for the controller publish and unpublish credentials and for KMS Secrets. Nothing lists or watches |
| `configmaps` get | KMS only |
| `serviceaccounts` get | KMS only (`vaulttenantsa`) |
| `serviceaccounts/token` create | KMS only: **mints a token for any ServiceAccount**, from the controller pod |

NFS loses its single cluster-wide `secrets get` the same way. The addon
keeps `tokenreviews` and `subjectaccessreviews` create: they are not
escalation paths, and the snapshot-metadata sidecar needs them when a
`SnapshotMetadataService` exists. Every PV, PVC, snapshot and attachment
rule stays.

What the controller side needs comes back namespaced: a `Role` in the
Rook namespace that grants `get` on Rook's provisioner secrets by name
(`rook-csi-rbd-provisioner`, `rook-csi-cephfs-provisioner`, the secrets the
StorageClass `provisioner`, `controller-expand` and VolumeSnapshotClass
`snapshotter` parameters reference), bound to the controller plugin
ServiceAccount of every driver under both names it can have. The node
secrets (`rook-csi-<type>-node`) stay out: kubelet reads them, not this
plugin. A `VolumeGroupSnapshotClass` or `VolumeReplicationClass` that
names another secret needs its own grant the same way.

::: warning StorageClasses with their own secret
A StorageClass whose controller-side secret has another name, or lives
outside the Rook namespace, loses every controller call that needs the
secret: provision and delete (reclaim), attach and detach, expand and
snapshot. The sidecar gets a 403 from the API and a new PVC stays
`Pending`. Mounted volumes keep working: a mounted filesystem does not
touch the API. Add a `Role` + `RoleBinding` in that
namespace from your own target (the cephfs subject for CephFS classes):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ceph-csi-ctrlplugin-secrets
  namespace: <secret namespace>
rules:
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [<secret name>]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ceph-csi-ctrlplugin-secrets
  namespace: <secret namespace>
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: ceph-csi-ctrlplugin-secrets }
subjects:
  - kind: ServiceAccount
    name: rook-ceph-rbd-csi-ceph-com-ctrlplugin-sa
    namespace: rook-ceph
  - kind: ServiceAccount
    name: rook-ceph-cephfs-csi-ceph-com-ctrlplugin-sa
    namespace: rook-ceph
```
:::

::: warning Upgrade note: re-apply the bootstrap, delete the COSI binding once
The narrowed ClusterRole and the new Role arrive in the same bootstrap
apply (`lo up`, `lo provision` or `lo bootstrap`). Until the RoleBinding
lands, a provisioner call in those seconds gets a 403. The sidecars retry
with backoff, so a PVC from that window binds a little later. Nothing
restarts.

The bootstrap applies without prune, so the COSI binding below stays on an
existing cluster. A cluster without the COSI driver deletes it once, by
hand, and checks the far end:

```console
$ kubectl delete clusterrolebinding objectstorage-provisioner-role-binding
$ kubectl auth can-i get secrets -n kube-system \
    --as=system:serviceaccount:rook-ceph:objectstorage-provisioner
no
```
:::

The rook-ceph chart also ships a `ClusterRoleBinding` for the Ceph COSI
driver ServiceAccount (`objectstorage-provisioner`: `secrets` get, create,
update and delete in every namespace). The addon deploys no
`CephCOSIDriver` and no pod runs as that ServiceAccount, so the addon drops
the binding from its render (`$patch: delete`). A new cluster never gets
it, and an existing cluster deletes it once (above). A cluster that runs the
COSI driver re-adds it from its own target:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: objectstorage-provisioner-role-binding
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: objectstorage-provisioner-role }
subjects:
  - kind: ServiceAccount
    name: objectstorage-provisioner
    namespace: rook-ceph
```

#### When you need the grants back

Three ceph-csi features read those objects through the Kubernetes API:

- **encrypted volumes with a KMS**: a StorageClass with `encrypted: "true"`
  and an `encryptionKMSID` (Vault, `vaulttokens`, `vaulttenantsa`, the
  metadata KMS, ...)
- **`mounter: rbd-nbd`**: after a plugin restart the volume healer re-reads
  each attached volume's node-stage Secret to re-map the device
- **usage metrics for `volumeMode: Block` volumes** (the exception above)

For those clusters bootstrap the opt-in `rook-ceph-csi-kms` addon after
`rook-ceph`. It binds the five removed rules to the RBD node plugin and
controller plugin ServiceAccounts again (a KMS runs on both sides: the
controller creates the key, the node unlocks it), so the effective
permissions equal the upstream chart's:

```yaml
spec:
  bootstrap:
    - rook-ceph
    - rook-ceph-csi-kms:
        dependsOn: [rook-ceph]
```

::: warning This restores the every-node escalation path
`rook-ceph-csi-kms` restores the upstream grants, not the minimum a KMS
needs. With it, root on any node that runs the plugin, and the controller
pod, can read every Secret and mint a token for any ServiceAccount again.
Enable it only for one of the three features above, and track the
narrower, per-feature grants in [kernpilot/lok8s#265](https://github.com/kernpilot/lok8s/issues/265).
:::

The binding names the RBD ServiceAccounts only (node plugin and controller
plugin). A cluster that also runs the CephFS driver with encryption adds
both CephFS ServiceAccounts from a patch in its own target: CephFS
initializes encryption on the controller side, so the node subject alone
restores only half.

```yaml
patches:
  - target: { kind: ClusterRoleBinding, name: rook-ceph-csi-nodeplugin-kms-crb }
    patch: |-
      - op: add
        path: /subjects/-
        value:
          kind: ServiceAccount
          name: rook-ceph-cephfs-csi-ceph-com-nodeplugin-sa
          namespace: rook-ceph
      - op: add
        path: /subjects/-
        value:
          kind: ServiceAccount
          name: rook-ceph-cephfs-csi-ceph-com-ctrlplugin-sa
          namespace: rook-ceph
```

A fork that keeps the chart-default driver names uses
`cephfs-csi-ceph-com-nodeplugin-sa` and `cephfs-csi-ceph-com-ctrlplugin-sa`.
NVMe-oF with encryption adds `rook-ceph-nvmeof-csi-ceph-com-nodeplugin-sa`
and `rook-ceph-nvmeof-csi-ceph-com-ctrlplugin-sa` the same way.

#### Verify it

```console
$ SA=system:serviceaccount:rook-ceph:rook-ceph-rbd-csi-ceph-com-nodeplugin-sa
$ kubectl auth can-i create serviceaccounts/token --as="${SA}"
no
$ kubectl auth can-i get secrets --as="${SA}" --all-namespaces
no
$ kubectl auth can-i list volumeattachments --as="${SA}"
yes
```

Both `no` lines read `yes` on a cluster that bootstraps `rook-ceph-csi-kms`.

### system-upgrade-controller: no cluster-admin {#system-upgrade-controller}

The `system-upgrade-controller` addon installs Rancher's
system-upgrade-controller (SUC). SUC runs each `Plan` as one Job per node.
The Rancher chart binds the controller's ServiceAccount
(`cattle-system/system-upgrade-controller`) to `cluster-admin`. The addon
deletes that binding from its render and binds four roles instead
(`rbac.yaml`). A comment on each rule names the source line that needs it.
`internal/addons/suc_rbac_test.go` holds the same list of calls, written by
hand from the source, and fails when a rule grants more or less than those
calls need. The list is not a runtime proof: a call that the list misses,
the test misses too.

| Role | Rules | What uses them |
|---|---|---|
| ClusterRole `lok8s-system-upgrade-controller` | `customresourcedefinitions` list, create; get and update on `plans.upgrade.cattle.io` | the controller creates or updates its Plan CRD on every start. The chart ships no CRD |
| | `namespaces` get on `kube-system` | the cluster ID, read on start |
| | `nodes` list, watch, update | the node cache; the Plan label and the uncordon after a Job completes |
| | `jobs` list | the apply engine lists the Plan Jobs in all namespaces (see below) |
| Role `lok8s-system-upgrade-controller` (`cattle-system`) | `plans` get, list, watch; `plans/status` update | the Plan cache and the Plan status. The controller never writes a Plan's spec |
| | `jobs` get, watch, create, patch, delete | the Plan Jobs (the list comes from the ClusterRole) |
| | `secrets` list, watch | the Secret cache. Every Plan needs it, not only a Plan with `spec.secrets`: the controller handles no Plan before the cache syncs |
| | `leases` create; get and update on `system-upgrade-controller` | leader election |
| | `events` create, patch | events on Plans |
| Role `lok8s-system-upgrade-controller-events` (`default`) | `events` create, patch | events on the node, which land in `default` |
| ClusterRole `lok8s-system-upgrade-controller-drainer` | see [Cordon and drain](#cordon-and-drain) | the cordon and drain containers of a Plan's Job |

Each object has the prefix `lok8s-`. Upstream SUC manifests and the
Rancher chart use the names `system-upgrade`, `system-upgrade-drainer`,
`system-upgrade-controller`, `system-upgrade-controller-drainer` and
`system-upgrade-controller-psp`. A binding's roleRef cannot change, so a
binding that reused one of those names would fail to apply on a cluster
that still carries the old one.

The controller also reads API discovery. No rule in the addon grants it:
the default ClusterRoleBinding `system:discovery` gives it to every
authenticated user. A cluster that removes that binding breaks the
controller.

#### What the ServiceAccount can still do

RBAC sees one ServiceAccount, not the pod that uses its token. The
controller pod, and every Plan Job that runs as
`system-upgrade-controller`, get all four roles together. With that token
a holder can:

- read the content of every Secret in `cattle-system` (list, watch);
- create Jobs in `cattle-system` with any pod template and any
  ServiceAccount of that namespace. The pods can be privileged and mount
  host paths, unless an admission policy stops them. SUC itself mounts the
  host root into its upgrade pods. Root on a node, and the credentials on
  that node, stay in reach;
- change every Node (`update`, and `patch` through the drainer): labels,
  taints, `spec.unschedulable`. RBAC cannot limit the update to the fields
  SUC writes;
- through the drainer: list and get every pod in every namespace (a list
  returns the full pod spec, environment values included), get every
  DaemonSet, and evict or delete any pod;
- list every Job in the cluster. The controller lists with a label
  selector, but the grant has none;
- create any CustomResourceDefinition, and change the Plan CRD;
- update Plan status in `cattle-system`; create Leases there and
  get/update the `system-upgrade-controller` Lease; create/patch Events in
  `cattle-system` and `default`. The Lease create allows any name: RBAC
  cannot limit a create by name, only the get and the update.

What it lost against `cluster-admin`: every write to RBAC (roles and
bindings), impersonation, token requests for other ServiceAccounts,
Secret reads outside `cattle-system`, pod `create`, `exec` and `attach`,
and every write outside Nodes, pods (delete and evict), Jobs, Leases,
events, the Plan status and CRDs. This narrows what a stolen token can do
in one step. It does not make host-level escalation impossible: see the
Job item above.

#### Cordon and drain

The controller never cordons or drains a node. A Plan with `spec.cordon` or
`spec.drain` gets an init container that runs `kubectl cordon` or
`kubectl drain` as the Plan's ServiceAccount (`spec.serviceAccountName`).
For a Plan that runs as the controller's ServiceAccount, the addon binds the
ClusterRole `lok8s-system-upgrade-controller-drainer` (ClusterRoleBinding of
the same name):

| Plan feature | Rules it needs |
|---|---|
| `spec.cordon` | `nodes` get, patch |
| `spec.drain` | `nodes` get, patch; `pods` get, list; `pods/eviction` create |
| `spec.drain` with `ignoreDaemonSets` (the default) | also `daemonsets` get |
| `spec.drain.disableEviction: true` | also `pods` delete |

These rules apply to every node and to pods in every namespace, and the
controller's token holds them too. When your Plans use their own
ServiceAccount, delete the ClusterRoleBinding
`lok8s-system-upgrade-controller-drainer` from your copy of the addon
(`.lok8s/addons/system-upgrade-controller/rbac.yaml`) and from the cluster,
and bind the ClusterRole to that ServiceAccount in the target that holds
the Plans:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: os-upgrade-drainer
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: lok8s-system-upgrade-controller-drainer }
subjects:
  - kind: ServiceAccount
    name: <the Plan's ServiceAccount>
    namespace: cattle-system
```

A Plan without `spec.cordon` and `spec.drain` needs none of these rules.
A Plan author picks `spec.serviceAccountName` freely, so a separate
ServiceAccount is no trust boundary against a Plan author: an admission
policy has to limit who writes Plans.

::: warning Upgrade note: update the ejected copy, delete the old binding once
`lo` ejects an addon into `.lok8s/addons/<name>/` on first use and never
overwrites it. A project that bootstrapped this addon before runs
`lo assets update addons/system-upgrade-controller` (or takes `rbac.yaml`,
the `patches:` block, the kustomization without its `namespace:` line and
the image pins into its modified copy by hand).

The bootstrap applies without prune. A cluster that ran an earlier version
of the addon keeps `ClusterRoleBinding/system-upgrade-controller` (roleRef
`cluster-admin`), and the controller keeps cluster-admin through it. Apply
the bootstrap first (`lo up`, `lo provision` or `lo bootstrap`): it adds the
four new bindings. Then delete the old binding and check the far end:

```console
$ kubectl delete clusterrolebinding system-upgrade-controller
$ SA=system:serviceaccount:cattle-system:system-upgrade-controller
$ kubectl auth can-i get secrets -n kube-system --as="${SA}"
no
$ kubectl auth can-i update nodes --as="${SA}"
yes
$ kubectl auth can-i update leases/system-upgrade-controller -n cattle-system --as="${SA}"
yes
```

The `can-i` lines are spot checks. Until now cluster-admin hid any call
the new roles miss. Restart the controller, run one Plan that does what
your Plans do (cordon, drain, Secrets), and look for `forbidden` in the
log:

```console
$ kubectl -n cattle-system rollout restart deployment/system-upgrade-controller
$ kubectl -n cattle-system logs deployment/system-upgrade-controller | grep -i forbidden
```

A cluster that bootstraps `system-upgrade-controller-cluster-admin` skips
the delete: that addon owns the binding. A GitOps apply with prune removes
the old binding itself when the binding is in its inventory. Do not delete
it before the new bindings exist: the controller then loses its access,
logs `forbidden` errors and handles no Plan until they arrive.
:::

#### When you need cluster-admin back

The opt-in addon `system-upgrade-controller-cluster-admin` applies the
chart's binding again, unchanged:

```yaml
spec:
  bootstrap:
    - system-upgrade-controller
    - system-upgrade-controller-cluster-admin:
        dependsOn: [system-upgrade-controller]
```

::: danger Not recommended
With it, the controller pod and every Plan Job that runs as its
ServiceAccount can do anything in the cluster: read every Secret, start a
pod on any node, change RBAC. Use it only when a Plan's upgrade container
calls the Kubernetes API for more than cordon and drain, or when a newer
controller logs `forbidden` errors before the addon follows. For the Plan
case, a better fix is a ServiceAccount of its own with the grants it needs.
:::

To opt out again, removing the entry from `spec.bootstrap` is not enough:
the bootstrap applies without prune, so the binding stays. Delete it too:

```console
$ kubectl delete clusterrolebinding system-upgrade-controller
```

#### Images

`values.yaml` pins both images by digest: the controller
(`rancher/system-upgrade-controller:v0.20.1@sha256:…`) and the kubectl
image of the cordon and drain containers
(`rancher/kuberlr-kubectl:v8.1.1@sha256:…`). The container runtime pulls by
the digest, so a re-pushed tag changes nothing. A mirror that you set
through `global.cattle.systemDefaultRegistry` must keep the digests. On a
bump, change the tag and the digest together, then refresh the render
fixture (`internal/addons/testdata/system-upgrade-controller/`, its header
says how). The kubectl image has no Windows variant, and the chart uses it
for Windows nodes too: a cordon or drain on a Windows node still fails to
pull it. The images a Plan names for its own containers are not pinned
here.

### redis-operator: narrowed RBAC, no aggregation into admin {#redis-operator}

The `redis-operator` addon installs the OT-CONTAINER-KIT Redis operator
(chart 0.26.1, operator v0.26.0). The chart binds the operator's
ServiceAccount (`redis-operator/redis-operator`) to its ClusterRole
`redis-operator`. That ClusterRole has two problems:

- It grants create, delete, get, list, patch, update and watch on
  namespaces, Secrets, ConfigMaps, pods, `pods/exec`, Services, PVCs and
  events in every namespace, and get on every non-resource URL. The
  operator calls much less (see below).
- It carries the label `rbac.authorization.k8s.io/aggregate-to-admin`.
  Kubernetes then copies all of its rules into the built-in ClusterRole
  `admin`. Every subject with a RoleBinding to `admin` in a namespace can
  then delete that namespace and read and write the Secrets there through
  the copied rules.

The addon deletes the chart's ClusterRole and ClusterRoleBinding from its
render and binds three roles instead (`rbac.yaml`). No object of the addon
carries an `aggregate-to-*` label. A comment on each rule names the source
line that needs it. `internal/addons/redis_operator_rbac_test.go` holds the
same list of calls, written by hand from the operator source, and fails
when a rule grants more or less than those calls need, when an object of
the addon aggregates into a built-in role, and when a role grants a verb on
namespaces. The list is not a runtime proof: a call that the list misses,
the test misses too.

| Role | Rules | What uses them |
|---|---|---|
| ClusterRole `lok8s-redis-operator` | `redis`, `redisclusters`, `redisreplications`, `redissentinels` list, watch, update | the CR caches; the finalizer add and remove (an update of the CR). The operator never creates, patches or deletes a CR |
| | `redisclusters/status`, `redisreplications/status` update | the status of a RedisCluster and a RedisReplication |
| | `redisreplications/finalizers` update | the owner reference with `blockOwnerDeletion` on the Sentinel Service and StatefulSet of a RedisReplication (checked when the admission plugin `OwnerReferencesPermissionEnforcement` is on) |
| | `secrets` get | password and TLS Secrets, read by name. No list, no watch, no write |
| | `pods` get, list, patch | the Redis pods; the patch writes the role label |
| | `services` get, list, watch, create, update, delete | the Services of each CR. The operator never deletes a Service; `delete` is for an update that changes a Service's owner references, which the admission plugin `OwnerReferencesPermissionEnforcement` allows only with delete. Without it, a RedisReplication stops at its resources step on such a cluster |
| | `statefulsets` get, list, watch, create, update, delete | the StatefulSets of each CR; the delete recreates one |
| | `persistentvolumeclaims` list, update, delete | the PVC resize; the delete when a CR without `storage.keepAfterDelete` goes away |
| | `poddisruptionbudgets` get, create, update, delete | the PDBs of each CR |
| | `events` create, patch | RedisCluster events; leader-election events |
| ClusterRole `lok8s-redis-operator-exec` | `pods/exec` create | `redis-cli` in a RedisCluster pod (create, scale, rebalance). Only a RedisCluster needs it |
| Role `lok8s-redis-operator-leader-election` (`redis-operator`) | `leases` create; get and update on `6cab913b.redis.opstreelabs.in` | leader election |

The rules are cluster-wide because the chart runs the operator without
`WATCH_NAMESPACE`: it handles a Redis CR in any namespace. The call list
holds for the chart's defaults (`--leader-elect`, no `WATCH_NAMESPACE`, no
webhooks, no `--metrics-secure`). The test reads these from the chart
render and fails when one changes, for example through
`redisOperator.watchNamespace` or `redisOperator.webhook` in an inline
override.

Each object has the prefix `lok8s-`. The chart uses the name
`redis-operator` for its ClusterRole and ClusterRoleBinding. A binding's
roleRef cannot change, so a binding that reused that name would fail to
apply on a cluster that still carries the old one.

The operator also reads API discovery. No rule in the addon grants it: the
default ClusterRoleBinding `system:discovery` gives it to every
authenticated user.

#### What the ServiceAccount can still do

A cluster-wide operator that manages workloads keeps a large blast radius.
With the operator's token a holder can, in every namespace:

- read every Secret whose name it knows (get). It cannot list them;
- create, change and delete StatefulSets. A StatefulSet can carry any pod
  template and any ServiceAccount of its namespace, so a holder can start
  pods that run as any ServiceAccount and mount any Secret, unless an
  admission policy stops them. This is the largest remaining path;
- list, get and patch pods (a list returns the full pod spec, environment
  values included);
- create, change and delete Services and PodDisruptionBudgets, and create
  and patch events;
- list, resize and delete PVCs. A PVC delete can destroy data;
- run any command in any container of any pod (`pods/exec`), as long as the
  binding `lok8s-redis-operator-exec` exists;
- update every Redis CR and the status of RedisClusters and
  RedisReplications.

What it lost against the chart's ClusterRole: every verb on namespaces
and ConfigMaps; every Secret verb except get; get, create, patch and
delete on the Redis CRs, and the status and finalizers grants that the
operator does not use; pod create, update, delete and watch; patch on
Services and StatefulSets; PVC create, get, watch and patch; PDB list,
watch and patch; event get, list, watch, update and delete; every Lease
verb outside `redis-operator`; every `pods/exec` verb except create
(`get` on `pods/exec` also allows exec over a websocket on older API
servers); CRD reads; get on all non-resource URLs; and the copy of its
rules into `admin`.

#### pods/exec: on by default, for RedisCluster only {#redis-operator-exec}

The addon keeps `pods/exec` create bound by default (ClusterRole and
ClusterRoleBinding `lok8s-redis-operator-exec`), so that a RedisCluster
keeps working after the update. **Exec stays enabled by default: until you
remove the binding, the exec grant is not reduced.** The operator's token
can still run any command in any container of any pod.

Only a RedisCluster needs exec: the operator runs `redis-cli` in its pods
to create the cluster, add and remove nodes, and rebalance the slots.
Redis, RedisReplication and RedisSentinel never use it.

Exec is a part of the `redis-operator` addon, not an addon of its own:
`spec.bootstrap` applies exactly the entries it lists, and lok8s has no
addon part that is on by default and that a project can switch off by name
([#316](https://github.com/kernpilot/lok8s/issues/316)). To remove exec,
change your ejected copy:

1. Make sure that no RedisCluster exists:

   ```console
   $ kubectl get redisclusters.redis.redis.opstreelabs.in -A
   No resources found
   ```

2. In `.lok8s/addons/redis-operator/rbac.yaml`, delete the
   ClusterRoleBinding document `lok8s-redis-operator-exec`. Keep the
   ClusterRole: without a binding it grants nothing.
3. Apply the bootstrap, then delete the binding in the cluster. The
   bootstrap applies without prune, so step 2 alone leaves it there:

   ```console
   $ kubectl delete clusterrolebinding lok8s-redis-operator-exec
   ```

4. Migration from the chart's binding: `ClusterRole/redis-operator` grants
   `pods/exec` too. Until you delete it and its binding (the upgrade note
   below), the operator keeps exec through them.
5. Verify:

   ```console
   $ SA=system:serviceaccount:redis-operator:redis-operator
   $ NS=<a namespace with Redis CRs>
   $ kubectl auth can-i create pods --subresource=exec -n "${NS}" --as="${SA}"
   no
   $ kubectl get clusterrolebinding -o json \
       | jq -r '.items[]
           | select(any(.subjects[]?; .kind == "ServiceAccount"
               and .name == "redis-operator"
               and .namespace == "redis-operator"))
           | .metadata.name'
   lok8s-redis-operator
   ```

   Write the subresource with `--subresource=exec`: kubectl reads
   `pods/exec` as the pod named `exec`, and then prints `no` while exec
   is still allowed. The last command lists every ClusterRoleBinding of
   the operator. `lok8s-redis-operator-exec`, `redis-operator` and
   `lok8s-redis-operator-chart-rbac` (the opt-in addon) must not be in it:
   each one grants exec.

Later releases: your edit makes the copy locally modified.
`lo assets update addons/redis-operator` then refuses and names the file,
so an update never binds exec again by itself. Take a later release into
your copy by hand and keep the binding out. `lo assets update --force`
replaces your copy and binds exec again: repeat steps 2 and 3 after it. In
the other direction, no release removes the binding from a copy that has
it.

A RedisCluster that you add later needs exec back first: put the
ClusterRoleBinding document back into `rbac.yaml` (or run
`lo assets update addons/redis-operator --force`), apply the bootstrap,
then create the RedisCluster. Without exec, the operator logs `forbidden`
for `pods/exec` and the cluster does not form. `lo doctor` does not warn
about this yet ([#315](https://github.com/kernpilot/lok8s/issues/315)).

::: warning Upgrade note: update the ejected copy, delete the old ClusterRole and binding once
`lo` ejects an addon into `.lok8s/addons/<name>/` on first use and never
overwrites it. A project that bootstrapped this addon before runs
`lo assets diff addons/redis-operator`, then
`lo assets update addons/redis-operator`. A copy without a `.lo-origin`
marker (a vendored copy) refuses the update: when the diff shows only the
changes of this release, run `lo assets update addons/redis-operator --force`.
A modified copy takes `rbac.yaml` and the `patches:` block by hand.

The bootstrap applies without prune. A cluster that ran an earlier version
of the addon keeps `ClusterRole/redis-operator` (with the label
`aggregate-to-admin`) and `ClusterRoleBinding/redis-operator`. The operator
keeps its broad rules through them, and `admin` keeps the copied rules.
Apply the bootstrap first (`lo up`, `lo provision` or `lo bootstrap`): it
adds the three new bindings. Then delete the old objects and check the far
end:

```console
$ kubectl delete clusterrolebinding redis-operator
$ kubectl delete clusterrole redis-operator
$ SA=system:serviceaccount:redis-operator:redis-operator
$ kubectl auth can-i delete namespaces --as="${SA}"
no
$ kubectl auth can-i list secrets -n kube-system --as="${SA}"
no
$ kubectl auth can-i update leases/6cab913b.redis.opstreelabs.in \
    -n redis-operator --as="${SA}"
yes
```

The removal from `admin` is asynchronous. The delete of the labelled
ClusterRole queues the clusterrole-aggregation controller, which then
writes the rules of `admin` again, usually within seconds. Until it does,
`admin` keeps the copied rules. Poll until no copied rule is left:

```console
$ Q='[.rules[]
     | select(((.apiGroups // []) | index("redis.redis.opstreelabs.in"))
       or (((.resources // []) | index("namespaces"))
           and (.verbs | index("delete"))))]
     | length'
$ for i in $(seq 1 24); do
    n=unknown
    if json=$(kubectl get clusterrole admin -o json); then
      n=$(jq "${Q}" <<<"${json}") || n=unknown
    fi
    echo "admin: ${n} rule(s) left"
    [ "${n}" = 0 ] && break
    [ "${i}" -lt 24 ] && sleep 5
  done
admin: 0 rule(s) left
$ [ "${n}" = 0 ] || echo "timeout: admin still has ${n} copied rule(s)" >&2
```

`unknown` means the read failed (kubectl or jq), not 0. After 24 tries
(about 2 minutes) the last line reports the timeout.

The built-in `admin` grants no namespace delete and no
`redis.redis.opstreelabs.in` rule by itself. If the count stays above 0,
one of two things holds:

- another ClusterRole still aggregates such a rule into `admin`;
- the clusterrole-aggregation controller has not run: it is part of
  kube-controller-manager, so check that kube-controller-manager runs and
  has a leader, and read its log.

List the ClusterRoles that aggregate into the built-in roles, and read
their rules:

```console
$ kubectl get clusterrole -l rbac.authorization.k8s.io/aggregate-to-admin=true
$ kubectl get clusterrole -l rbac.authorization.k8s.io/aggregate-to-edit=true
$ kubectl get clusterrole -l rbac.authorization.k8s.io/aggregate-to-view=true
```

A clean `admin` does not prove that a subject lost the rights: another
binding can grant the same verbs (a legitimate one, or one you did not
expect). Check a subject with `kubectl auth can-i --as=<subject>`. The
`can-i` lines above are spot checks. Until now the broad rules hid any
call that the new roles miss. Restart the operator, let it reconcile
every Redis CR, and look for `forbidden` in the log:

```console
$ kubectl -n redis-operator rollout restart deployment/redis-operator
$ kubectl -n redis-operator logs deployment/redis-operator | grep -i forbidden
```

A GitOps apply with prune removes the old objects itself when they are in
its inventory. Do not delete them before the new bindings exist: the
operator then loses its access and handles no CR until they arrive.
:::

#### When you need the chart's rules back

The opt-in addon `redis-operator-chart-rbac` binds the rules of the
chart's ClusterRole to the operator again, under the name
`lok8s-redis-operator-chart-rbac`. It does not restore the label
`aggregate-to-admin`:

```yaml
spec:
  bootstrap:
    - redis-operator
    - redis-operator-chart-rbac:
        dependsOn: [redis-operator]
```

::: danger Not recommended
With it, the operator's token can again create and delete namespaces,
read and write every Secret and ConfigMap, and exec into every pod. Use it
only when a newer operator logs `forbidden` errors before the addon
follows.
:::

To opt out again, removing the entry from `spec.bootstrap` is not enough:
the bootstrap applies without prune. Delete the objects too:

```console
$ kubectl delete clusterrolebinding lok8s-redis-operator-chart-rbac
$ kubectl delete clusterrole lok8s-redis-operator-chart-rbac
```

If namespace admins must manage Redis CRs, do not use the opt-in addon for
that. Give them a ClusterRole in a target with only the
`redis.redis.opstreelabs.in` resources they need.

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
| **Framework addon** | a generic, reusable install: an operator + CRDs, a controller, a CNI/CSI/LB chart | `.lok8s/addons/<name>/` |
| **Inline `bootstrap` value** | a *small* per-cluster value override of an addon | the `spec.bootstrap` map entry |
| **Target** | per-cluster glue an addon can't carry: instance CRs, routes/ReferenceGrants tied to *this* cluster's Gateway + domain, `Plan`s, or large chart values | `clusters/.targets/<name>/` (shared) or `clusters/<domain>/targets/<name>/` (one cluster) |

Reach for them in that order: **inline first** (smallest), then an **addon** (if
it's a reusable install), then a **target** (only for real per-cluster glue).

### Split a component: install → addon, glue → target

Most infrastructure is **both** a reusable install *and* some cluster-specific
config. Do not put the whole thing in a target. Split it: the addon ships the
generic atom, the target carries only the glue.

| Component | Addon (`.lok8s/addons/`) | Target (`clusters/.../targets/`) |
|-----------|--------------------------|----------------------------------|
| CloudNativePG | `cnpg-operator` (operator + CRDs) | `cnpg-cluster` (the `Cluster` CR) |
| Rook-Ceph | `rook-ceph` (operator + CRDs) | `rook-ceph` (CephCluster/pool/StorageClass) |
| system-upgrade | `system-upgrade-controller` (controller + CRD) | `system-upgrade-controller` (the `Plan`s + trigger) |
| Mailpit | `mailpit` (ns + deployment + service) | `mailpit` (HTTPRoute + ReferenceGrant) |

Bootstrap the addon **before** the target that depends on it: CRDs/controller
must exist before the CRs. When the per-cluster glue is *chart values* too large
for inline (e.g. Grafana's OIDC config), let the target re-render the chart
on top of the addon's base values, and bootstrap **the target** (not the bare
addon) so the chart does not render twice.

### Shared vs per-cluster targets

A target's directory placement follows how many clusters use it:

- `clusters/.targets/<name>/`: a **shared base**, used when **more than one
  cluster** needs the same glue (e.g. `networking`). Per-cluster overlays
  compose it via kustomize (`resources: [ ../../.targets/<name> ]`) and patch
  only the differences.
- `clusters/<domain>/targets/<name>/`: glue **one cluster** uses; skip the
  shared-base indirection.

Only promote a target into `.targets/` once a second cluster actually consumes
it: a single-cluster target in the shared base is needless indirection.
