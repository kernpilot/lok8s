---
name: lok8s-cluster-spec
description: >-
  Use when writing or editing a lok8s cluster spec — cluster.lok8s.yaml (kind
  Lo, KubeOne, Capi, or Kkp) or deploy.lok8s.yaml under clusters/<domain>/.
  Covers required fields per driver, the spec.bootstrap list, and the schema
  pitfalls the CRDs do not catch.
---

# Authoring lok8s cluster specs

A domain's cluster config lives at `clusters/<domain>/cluster.lok8s.yaml` (a real
cluster) or `clusters/<domain>/deploy.lok8s.yaml` (a deploy-only domain that
targets another cluster). `apiVersion` is always `cluster.lok8s.dev/v1beta1`;
`kind` selects the driver.

## 1. Pick the kind (it selects the driver)

| `kind` | driver | use for |
|--------|--------|---------|
| `Lo` | kind on local Docker | local dev clusters |
| `KubeOne` | KubeOne on VMs / bare metal | self-managed production |
| `Capi` | Cluster API | CAPI-managed clusters |
| `Kkp` | Kubermatic (KKP) | hosted control planes |

`metadata.name` is the cluster name **and** the kubeconfig filename
(`.kubeconfig/<name>.yaml`). `spec.cluster.domain` is **required** for every kind.

## 2. Minimal valid specs

```yaml
# Lo — only spec.cluster.domain is strictly required
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: local }
spec:
  cluster: { domain: lok8s.dev }
```
```yaml
# KubeOne
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: my-prod }
spec:
  kubernetes: { version: "v1.31.12" }      # quote the version
  cluster: { domain: my-cluster.example.com }
  provider: { name: hetzner, configRef: hetzner.json }
```
```yaml
# Deploy-only domain (targets another cluster; cannot be `lo provision`ed)
apiVersion: cluster.lok8s.dev/v1beta1        # all cluster kinds incl. Deploy share this group
kind: Deploy
metadata: { name: api }
spec:
  clusterRef: { domain: my-cluster.example.com }   # required
  namespace: api                                   # optional
```

For the full per-kind field tree (Lo nodes/network/registries/coredns/loadBalancer,
KubeOne workers/datacenter, Capi providers, Kkp kkp.*), read **`reference.md`**.

## 3. spec.bootstrap (the addon list)

A list applied in order. Three entry forms:

```yaml
spec:
  bootstrap:
    - cilium                       # bare string → framework addon .lok8s/addons/cilium
    - cilium:                      # MAP form → addon + inline Helm values (highest precedence)
        encryption: { enabled: true }
    - ./targets/networking         # ./ or ../ → clusters/<domain>/targets/...
    - /shared/base                 # absolute → repo root
```

Default when the key is **absent**: only `Lo` gets `[cilium]` (kind ships no CNI);
KubeOne/Capi/Kkp default to empty. An explicit `bootstrap: []` is an authoritative
opt-out. See the `lok8s-addons` skill for what to put here vs. in a target.

## 4. ⚠️ Pitfalls the CRD won't catch (the readers are authoritative)

- **No CRD ships for `KubeOne` or `Kkp`** — only `Lo`, `Capi`, `Deploy` have
  OpenAPI schemas (`operator/crds/`). KubeOne/Kkp are validated only by their bash
  readers, so don't rely on `kubectl` schema validation for them.
- **OIDC field names differ by kind**: Lo & KubeOne use `spec.oidc.issuer` +
  `spec.oidc.clientID` (+ `usernameClaim`/`groupsClaim`/`caBundle`); **Capi** uses
  `spec.oidc.enabled` + `spec.oidc.issuerUrl`. Don't mix them.
- `spec.coredns.overrides` is **plural** (the reader key); some docs say `override`.
- Many real `Lo` fields are **not in the CRD** but ARE read by the driver:
  `registries.tls`/`registries.prefix`, `nodes.hostPorts`, `coredns`, `oidc`,
  `dns.domainFilter`, `kubehz`, `remote`. Absence from the CRD ≠ unsupported.
- For non-`*.lok8s.dev` `Lo` domains you **must** set `spec.network.{name,cidr}`
  and `spec.loadBalancer.pool` (they're auto-derived only for `*.lok8s.dev`).
- `spec.kubehz`: `hosting` ∈ {self,hosted}, `access` ∈ {none,registered,managed};
  `apiUrl` is required (and must be HTTPS) when `hosting: hosted` or `access != none`.

## 5. Verify

```bash
lo lint <domain>     # validates the spec, that every bootstrap entry resolves,
                     # kustomization refs exist, and one-cluster-per-apex
```
