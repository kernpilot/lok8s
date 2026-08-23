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
  values.kubeone.yaml     KubeOne/bare metal overrides (tunnel + WireGuard)
  values.hetzner.yaml     Hetzner provider overrides (optional)
  values.aws.yaml         AWS provider overrides (optional)
```

### Merge order

Later files override earlier ones. Deep-merge semantics — nested keys
are combined, not replaced.

1. `values.yaml` — base (shared across all drivers and providers)
2. `values.${kind}.yaml` — driver (`lo`, `kubeone`, `capi`, `kkp`)
3. `values.${provider}.yaml` — provider (`hetzner`, `aws`, ...)
4. Inline overrides from `spec.bootstrap` (per-cluster): `valueFiles:`
   entries first (in list order), then the inline `values:` map on top

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
| `values.${kind}.yaml` | one driver flavor | Driver-required choices (`lo` needs tunnel mode + `cluster-pool` IPAM because kind can't route; `kubeone` uses tunnel/vxlan — nodes span subnets — plus WireGuard encryption). |
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
`values:`, `valueFiles:`, `env:`, `wait:`, `dependsOn:`, and `name:` (any one of
them switches the entry to this form; otherwise the whole map is treated as
inline values, as above):

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

- **`values:`** — Helm values, deep-merged like the inline form. Chart addons
  only; setting it on a kustomize target (a `./targets/` dir with no `chart.yaml`)
  is an error.
- **`valueFiles:`** — a list of Helm values **files**, for per-cluster value
  blocks too big to inline (a gatus endpoint list, a monitoring scrape config).
  Each path resolves **relative to the cluster directory** (the one containing
  `cluster.lok8s.yaml` — the same base `./targets/...` entries resolve from);
  absolute paths pass through unchanged. The files merge in list order **between**
  the provider values and the inline `values:` map, so the full stack is
  `values.yaml` < `values.${kind}.yaml` < `values.${provider}.yaml` <
  `valueFiles:` (in order) < `values:` — the inline map stays the most explicit
  signal. Same deep-merge semantics as every other layer (nested maps combine,
  lists replace). Must be a YAML list of path strings; a missing file is a hard
  error (never silently skipped), and like `values:` it is chart-addons-only.
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
| `cilium` | Cilium CNI | `cilium/cilium` v1.20.1 |
| `metallb` | MetalLB L2 load balancer | `metallb/metallb` v0.16.1 |
| `cert-manager` | cert-manager controller + CRDs (Issuers, Certificates) | `jetstack/cert-manager` v1.21.1 |
| `cert-manager-webhook-hetzner` | Hetzner DNS-01 ACME solver webhook — **opt-in**; bootstrap *after* `cert-manager`. Only clusters that issue via Hetzner DNS-01 (e.g. Let's Encrypt on a public plane) need it; kind/dev clusters serving their Gateway from a `cert:` Secret skip it. | `cert-manager-webhook-hetzner` 0.7.0 |
| `envoy-gateway` | Envoy Gateway controller **+ the upstream Gateway API CRD bundle** — see [upgrading in place](#envoy-gateway-upgrading-in-place) before bumping the pin | `envoyproxy/gateway-helm` v1.9.0 |

### Cilium driver-specific behavior

A concrete example of the driver-layer in action — these values are
set in `values.lo.yaml` and `values.kubeone.yaml`:

| Driver | IPAM | Routing | Encryption | Why |
|--------|------|---------|------------|-----|
| Lo (kind) | `cluster-pool` | `tunnel` | off | Kind nodes are containers on ONE host kernel — no L3 routing, and encrypting loopback-adjacent traffic buys nothing |
| KubeOne | `kubernetes` (driver) → `cluster-pool` effective on Hetzner (the provider layer wins the merge) | `tunnel` (vxlan) | **WireGuard** (pod + node) | Nodes span subnets/locations (cloud subnet + bare-metal vSwitch) — native routing needs one L2 segment; traffic crosses shared infrastructure, so it ships encrypted. Mind the MTU: the `MTU` value is the RAW pod MTU and the max wire frame equals the value (measured on cilium 1.19.2; the knob is unchanged in the 1.20 chart) — so set it ≤ the smallest underlay link; the effective inner ceiling is value − 130 (see the knob's comment); running pods keep their old veth MTU until restarted |

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

Before any jump that crosses a Gateway API minor, check that no CRD's
`status.storedVersions` names a version the incoming bundle stops
serving — the apiserver rejects such an update outright:

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
   depends on the rung** — the older bundle only has to keep serving
   `v1`, and v1.5.1 already does for `TLSRoute`:

   | kind | stored after v1.9.0 | v1.5.1 serves (the v1.8.x rung) | v1.4.1 serves (the v1.7.x rung) |
   |---|---|---|---|
   | `tcproutes` | `v1alpha2`, `v1` | `v1alpha2` → **blocks** | `v1alpha2` → **blocks** |
   | `udproutes` | `v1alpha2`, `v1` | `v1alpha2` → **blocks** | `v1alpha2` → **blocks** |
   | `tlsroutes` | `v1alpha3`, `v1` | `v1`, `v1alpha2`, `v1alpha3` → passes | `v1alpha2`, `v1alpha3` → **blocks** |

   So a rollback to **v1.8.x** is blocked by `tcproutes` and `udproutes`
   only, and a rollback to **v1.7.x** is blocked by all three.

   These three kinds exist **only on the experimental channel** of the
   Gateway API, which is the channel this addon installs — check yours
   with `kubectl get crd tcproutes.gateway.networking.k8s.io -o
   jsonpath='{.metadata.annotations.gateway\.networking\.k8s\.io/channel}'`.
   On a standard-channel install the CRDs are absent and this block
   cannot arise at all.

2. **The `safe-upgrades` ValidatingAdmissionPolicy.** v1.8.1 moved
   `ValidatingAdmissionPolicy safe-upgrades.gateway.networking.k8s.io`
   out of the CRD bundle into the chart *templates*, so the addon now
   installs it. It rejects any Gateway API CRD annotated with a bundle
   version below **v1.5**, so a rollback to v1.7.x fails at admission
   until that policy and its binding are deleted. The v1.8.x rung
   (v1.5.1) passes this one.
:::

Block 1 has an escape hatch, and it is cheap **only** because these kinds
usually have no objects — most clusters route HTTP and never create a
`TCPRoute`, `UDPRoute` or `TLSRoute`. Deleting a CRD deletes every object
of that kind, so **delete the fewest CRDs the rung needs**, and **check
each one first**.

Rolling back to **v1.8.x** — two CRDs, `TLSRoute` untouched:

```console
$ kubectl get tcproutes,udproutes -A
No resources found
$ kubectl delete crd tcproutes.gateway.networking.k8s.io \
                     udproutes.gateway.networking.k8s.io
```

Rolling back to **v1.7.x** — all three, plus the admission policy:

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

If either `kubectl get` prints an object, **stop** — export it, or stay on
v1.9.x. Only a clean `No resources found` makes the delete safe.

Then re-pin `chart.yaml` and run `lo up` — the older bundle recreates the
CRDs you deleted at its own versions.

#### v1.9.0 behaviour changes that no diff shows

Upgrading in place is more than the CRD jump. These change what a running
gateway does without changing anything you wrote:

- **Client-forced tracing is off by default.** Upstream dropped the client
  sampling fraction from **100% to 0%**, so Envoy Gateway no longer honours
  a caller's `x-client-trace-id` forced trace. This is *not* ordinary
  sampling — `samplingRate` still defaults to 100, and traces you sample
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
  an empty `spec.clientIPDetection` used to be accepted; CEL validation now
  requires exactly one of `xForwardedFor`, `customHeader` or
  `directSourceIP`.
- **Lua `EnvoyExtensionPolicy` is opt-in**, via `extensionApis.enableLua`.
- **`SecurityPolicy.spec.mergeType` validation got stricter** — xRoute
  targets only (`HTTPRoute`, `GRPCRoute`, `TCPRoute`), rejected on a
  `Gateway`, a Gateway listener or a `ListenerSet`. That is exactly the
  shape [`sso-gate`](#sso-gate) ships, so the addon is unaffected — but a
  hand-written gateway-wide policy carrying `mergeType` must drop the
  field before it can be updated again.

### sso-gate — OIDC login in front of any service {#sso-gate}

`sso-gate` puts any HTTP service behind OIDC single sign-on **at the
gateway** — no sidecar, no change to the app. It ships one Envoy Gateway
[`SecurityPolicy`](https://gateway.envoyproxy.io/docs/tasks/security/oidc/)
that matches every `HTTPRoute` labeled `sso.lok8s.dev/protect: "true"` in
its namespace; Envoy runs the whole login flow (redirect to the issuer,
callback, session cookie, token validation) before traffic reaches the
service. Works with any spec-compliant OIDC issuer.

```yaml
spec:
  bootstrap:
    - envoy-gateway            # required: v1.8.0+ (SecurityPolicy is its CRD)
    - sso-gate: { dependsOn: [envoy-gateway] }
```

(Without `envoy-gateway` the `SecurityPolicy` CRD does not exist: the
apply retries a few times and then fails that bootstrap entry loudly —
the rest of the DAG continues.)

Then, per service you want protected:

```yaml
# on the service's HTTPRoute
metadata:
  labels:
    sso.lok8s.dev/protect: "true"
```

Routes without the label stay public; removing the label makes a route
public again. A labeled route with a broken policy (unpatched issuer,
missing client Secret) answers **500 — the gate fails closed, never
open** — so configure these three things *before* labeling any route
(see the header of `.lok8s/addons/sso-gate/kustomization.yaml` for
copy-paste patches):

1. **Issuer + client ID** — patch `SecurityPolicy/sso-gate` from a
   consuming target.
2. **Client secret** — a Secret `sso-gate-client` (key `client-secret`)
   in the policy's namespace, e.g. via the secrets plugin.
3. **Redirect URI** — register `https://<each-protected-host>/oauth2/callback`
   at your issuer.

One sharp edge: `targetSelectors` matches routes in the **same namespace**
as the policy (shipped: `default`) — that is the default and what this
addon ships. Envoy Gateway can widen it with
`targetSelectors[].namespaces`, but that needs a `ReferenceGrant` in every
target namespace; for routes elsewhere it is simpler to layer a second
copy with a kustomize `namespace:` transform.

Note this is about the **policy and the routes it selects**. The Gateway
those routes attach to may sit in a third namespace — merging follows the
route's attachment hierarchy, not namespaces.

#### SSO must ADD to your gateway's guards, not replace them

::: danger A route-level SecurityPolicy REPLACES the Gateway's one
Envoy Gateway resolves overlapping `SecurityPolicy` objects by
**specificity**, and the most specific one wins **entirely**. A policy on
an `HTTPRoute` therefore replaces the policy on that route's `Gateway` —
wholesale, not field by field. If your Gateway carries an IP allowlist, an
`authorization` deny-by-default, JWT or CORS, an unmerged route-level SSO
policy **deletes all of it** for every route you label. The route ends up
*more* reachable than before you protected it.

The only signal is an `Overridden` condition on the Gateway policy, which
names the routes it lost — and which nothing reads unless you look:

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

So the breaking direction is real: if your Gateway policy carries an IP
allowlist, or `authorization.defaultAction: Deny` with allow rules that
never mentioned this route, then a route that serves fine today over SSO
starts returning **403** the moment it is labeled — the login succeeds and
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
  depends on how you apply: a **server-side** apply (`kubectl apply
  --server-side`, and every GitOps controller) is **rejected** with
  `.spec.mergeType: field not declared in schema`, while a client-side
  apply drops the field **silently** and leaves you with the replace
  behaviour. Check the CRD before trusting the field:

  ```console
  $ JP='{.spec.versions[?(@.name=="v1alpha1")]'
  $ JP="${JP}.schema.openAPIV3Schema.properties.spec.properties.mergeType.type}"
  $ kubectl get crd securitypolicies.gateway.envoyproxy.io -o jsonpath="${JP}"
  string     # empty output = your Envoy Gateway is too old
  ```

  (Select the version by name, not by `versions[0]` — the order of that
  list is not a contract. Running this for you from `lo doctor` is
  [kernpilot/lok8s#141](https://github.com/kernpilot/lok8s/issues/141).)

- **Upgrade Envoy Gateway before the policy reconciles.** Under `lo up`
  that ordering comes free from `dependsOn: [envoy-gateway]`, and a
  rejected `sso-gate` would only skip *its own* dependents — a failing
  addon never stops unrelated ones (see [Failure
  handling](#failure-handling)). Under **GitOps** it is different: Flux
  and Argo reconcile the rendered set as a unit, so one object rejected
  for `.spec.mergeType: field not declared in schema` fails the whole
  sync. Sequence the gateway upgrade ahead of it there.

- **`mergeType` is legal only on a child target** — `HTTPRoute`,
  `GRPCRoute` or `TCPRoute`. Envoy Gateway rejects it on a `Gateway`, a
  Gateway listener or a `ListenerSet`, because there is no parent to merge
  into. Never copy the field onto a gateway-wide policy.

The trap is not specific to this addon. **Any** route-level
`SecurityPolicy` you write replaces the Gateway's: set `mergeType` on it,
or first check what the Gateway policy was doing for that route. `lo
audit` flags the combination — a gateway-wide default-Deny plus a
route-level policy with no `mergeType` — under `exposed-endpoints`.

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
and the gateway's guard came off. Note the filter order — Envoy runs
`oauth2` **before** `rbac`, so an unauthenticated request from a blocked
source is redirected to the issuer first and refused after it returns.
Access is still gated; the service's existence is no longer hidden.

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
