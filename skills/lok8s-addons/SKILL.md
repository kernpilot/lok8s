---
name: lok8s-addons
description: >-
  Use when adding or changing a lok8s addon (.lok8s/addons/<name>/), a per-cluster
  target (clusters/.../targets/<name>/), or an inline bootstrap value. Covers the
  addon vs target vs inline decision, the Helm values precedence stack, how to add
  a khelm chart addon, and the includeSelectors labels gotcha.
---

# lok8s addons, targets & inline values

## 1. Where does it go? (reach in this order: inline → addon → target)

| Home | For | Lives in |
|------|-----|----------|
| **inline bootstrap value** | a *small* per-cluster value override of an existing addon | the `spec.bootstrap` map entry: `- cilium: { ... }` |
| **framework addon** | a generic, reusable install (operator+CRDs, controller, CNI/CSI/LB chart) | `.lok8s/addons/<name>/` |
| **target** | per-cluster glue an addon can't carry: instance CRs, routes/ReferenceGrants tied to *this* Gateway+domain, `Plan`s, or oversized chart values | `clusters/.targets/<name>/` (shared, ≥2 clusters) or `clusters/<domain>/targets/<name>/` (one cluster) |

**Split rule:** most infra is *install + glue*. Put the generic atom in an addon
and the cluster-specific glue in a target; bootstrap the addon **before** the
target (CRDs/controller before CRs). Canonical splits: `cnpg-operator` addon +
`cnpg-cluster` target; `rook-ceph` addon + CephCluster target;
`system-upgrade-controller` addon + `Plan`s target. Only promote a per-cluster
target into `.targets/` once a **second** cluster consumes it.

When per-cluster chart values are too big for inline (e.g. Grafana OIDC), let the
**target** re-render the chart layering the addon's base values, and bootstrap the
target (not the bare addon) so the chart isn't rendered twice.

## 2. Addon anatomy (`.lok8s/addons/<name>/`)

| File | When | Role |
|------|------|------|
| `kustomization.yaml` | always | lists `generators:` (chart.yaml) and/or `resources:` (raw manifests) + the `labels:` block |
| `chart.yaml` | Helm addons | khelm `ChartRenderer` (presence triggers the values-stack merge) |
| `values.yaml` | Helm addons | base values, always loaded |
| `values.<kind>.yaml` | optional | driver overlay — `<kind>` ∈ `lo`/`kubeone`/`capi`/`kkp` |
| `values.<provider>.yaml` | optional | provider overlay — `<provider>` = `spec.provider.name` (e.g. `hetzner`) |
| raw manifests | optional | listed under `resources:` |

## 3. Helm values precedence (deep-merge, later wins)

```
values.yaml  <  values.<kind>.yaml  <  values.<provider>.yaml  <  inline (spec.bootstrap)
```
Rationale: provider beats driver (environment *facts* beat orchestration
*preferences*); inline beats all (explicit user intent). The merge only runs for
a bootstrap entry that resolves to `.lok8s/addons/<name>/` **with** a `chart.yaml`
— path/target entries are applied as-is, no layering.

## 4. Adding a khelm chart addon

```yaml
# .lok8s/addons/<name>/chart.yaml
apiVersion: khelm.mgoltzsche.github.com/v2   # ⚠️ .github.COM, flat fields — NOT the .io/helmChart: form some docs show
kind: ChartRenderer
metadata:
  name: <release-name>        # prefixes the rendered resources
  namespace: <target-ns>
kubeVersion: "1.31.12"        # ⚠️ set explicitly — else helm defaults to v1.20.0 and modern charts reject it
repository: https://charts.example.com/   # or oci://...
chart: <chart>
version: 1.2.3                # pin deliberately
valueFiles: [ values.yaml ]   # bootstrap rewrites this to values.merged.yaml at apply time
```
```yaml
# .lok8s/addons/<name>/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: <target-ns>
labels:
  - includeSelectors: false   # ⚠️ see below
    pairs: { lok8s.dev/name: <name>, lok8s.dev/type: system, lok8s.dev/category: networking }
generators: [ chart.yaml ]
# resources: [ namespace.yaml ]   # add raw manifests here
```
Then reference it by name in a cluster's `spec.bootstrap`.

### ⚠️ The `includeSelectors` gotcha
- `includeSelectors: false` → labels are **metadata only**. Use this for any addon
  wrapping a **third-party chart** — chart Deployments carry *immutable*
  `spec.selector` fields, and touching them makes `kubectl apply` fail.
- `includeSelectors: true` → labels also fold into `spec.selector.matchLabels`.
  Only safe for first-party/CNI manifests where you own the selector (e.g. cilium).

The `lok8s.dev/{name,type,category}` labels are read by the lok8s Tilt extension
so cluster infra isn't mistaken for a `services.yaml` service.

## 5. Verify
```bash
lo addons                  # list framework addons
lo lint <domain>           # checks every bootstrap entry resolves
lo bootstrap --domain <d>  # re-apply spec.bootstrap addons only (idempotent)
```
