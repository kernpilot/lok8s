# cluster.lok8s.yaml — full field reference

Authoritative sources: the driver readers (`.lok8s/drivers/lo/utils/config.sh`,
`.lok8s/drivers/kubeone/config`, `.lok8s/drivers/kkp/main`), the Capi/Lo/Deploy
CRDs (`operator/crds/`), and `.lok8s/libs/{bootstrap,provision}` +
`.lok8s/libs/kubehz/main`. The readers win over the CRDs.

## Common header (all kinds)

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `apiVersion` | yes | — | `cluster.lok8s.dev/v1beta1` (Deploy: `deploy.lok8s.dev/v1beta1`) |
| `kind` | yes | — | `Lo` \| `KubeOne` \| `Capi` \| `Kkp` (\| `Deploy`) |
| `metadata.name` | yes | — | cluster name; also the kubeconfig filename `.kubeconfig/<name>.yaml` |
| `spec.cluster.domain` | **yes** | — | cluster FQDN |
| `spec.cluster.namespace` | no | `default` | |
| `spec.kubernetes.version` | conditional | — | quote it (`"v1.31.12"`); required for KubeOne/Capi/Kkp; Lo may omit (kind picks one) |

## kind: Lo (local kind)

```yaml
spec:
  cluster: { domain: lok8s.dev }           # only required field
  runtime: kind                            # only value
  network:                                 # auto-derived for *.lok8s.dev; REQUIRED otherwise
    name: <docker-network>                 # default = metadata.name
    cidr: 10.125.<slot>.0/24
  nodes:
    controlPlane: 1                        # 1-9
    workers: 0                             # 0-99
    hostPorts: false                       # default true only for slot 125
  loadBalancer:
    pool: 10.125.125.125-10.125.125.150    # MetalLB range; auto for *.lok8s.dev; omit block → no MetalLB
  registries:
    tls: false                             # mkcert HTTPS registries (reader-only, not in CRD)
    prefix: lok8s.local
    shared: { enabled: true }
    mirrors:                               # 'build' and 'cache' are RESERVED names — never list them
      - { name: io-docker, url: https://registry-1.docker.io }
  coredns:                                 # reader-only (not in CRD); all keys optional, all compose
    hosts:
      - { name: lok8s.dev, target: gateway }   # target: gateway → first loadBalancer.pool IP, else a literal IP
    servers: |  <raw CoreDNS server block>
    overrides: | <raw directives>          # NOTE: plural 'overrides' (docs wrongly say 'override')
    import: ./coredns
  oidc:                                    # Lo/KubeOne field names — see warning below
    issuer: https://id.example.com
    clientID: "..."
    usernameClaim: sub                     # defaults: sub / oidc: / groups / oidc:
    groupsClaim: groups
    caBundle: |  <PEM>
  dns: { domainFilter: "zone1.com,zone2.com" }   # for the external-dns addon
  bootstrap: [ ... ]
  kubehz: { hosting: self, access: none }
```

## kind: KubeOne (no CRD — reader: `.lok8s/drivers/kubeone/config`)

```yaml
spec:
  kubernetes: { version: "v1.31.12" }
  cluster: { domain: my-cluster.example.com }
  provider: { name: hetzner, configRef: hetzner.json }   # see lok8s-bare-metal skill
  network: { podSubnet: 10.244.0.0/16, serviceSubnet: 10.96.0.0/12, cni: canal }
  workers:
    pool-a: { replicas: 3, type: cx33, image: ubuntu-22.04 }   # hetzner; AWS uses ami
  datacenter: fsn1                         # hetzner
  oidc: { issuer: ..., clientID: ... }     # same field names as Lo
  dns: { domainFilter: "..." }
  addons: { enabled: false, path: ./addons }
  bootstrap: [ ... ]
```

## kind: Capi (CRD: `operator/crds/capi.yaml`)

```yaml
spec:
  kubernetes: { version: "v1.31.10" }
  cluster: { domain: prod.example.com }
  hcloud: { region: fsn1, sshKeyName: my-key }   # exactly one provider block: hcloud | hrobot | aws
  controlPlane: { replicas: 3, type: cx33 }
  workers:
    default: { replicas: 3, type: cx43 }
  oidc: { enabled: true, issuerUrl: https://id.example.com }   # ⚠️ Capi uses enabled + issuerUrl
  etcd: { encryptionSecretName: etcd-enc }
  gitops: { provider: flux, repo: ..., branch: main, path: ./clusters }
  bootstrap: [ ... ]
```

## kind: Kkp (no CRD — reader: `.lok8s/drivers/kkp/main`)

```yaml
spec:
  kubernetes: { version: "v1.29.2" }
  cluster: { domain: kkp-test.example.com }
  kkp:                                     # all three REQUIRED
    apiUrl: https://kkp.example.com
    projectId: proj-123
    datacenter: hetzner-fsn1
  provider: { name: hetzner, credentials: { envVars: [KKP_TOKEN, HCLOUD_TOKEN] } }
  workers:
    pool-a: { replicas: 3, flavor: cx33, operatingSystem: ubuntu, autoscaler: { min: 1, max: 5 } }
  bootstrap: [ ... ]
```
Env: `KKP_TOKEN`, `KKP_API_URL` (or `spec.kkp.apiUrl`), and the provider token.

## deploy.lok8s.yaml (CRD: `operator/crds/deploy.yaml`)

```yaml
apiVersion: deploy.lok8s.dev/v1beta1       # NOT cluster.lok8s.dev (a stale fixture gets this wrong)
kind: Deploy
metadata: { name: api }
spec:
  clusterRef: { domain: prod.example.com } # REQUIRED — which cluster to deploy to
  domain: api.example.com                  # optional
  namespace: api                           # optional
```
A deploy domain cannot be `lo provision`ed; use `lo build`/`lo deploy` against it.
Its KUBECONFIG resolves to the referenced cluster.

## spec.bootstrap entry forms (`.lok8s/libs/bootstrap`)

| Form | Example | Resolves to |
|------|---------|-------------|
| bare string | `cilium` | `.lok8s/addons/cilium/` |
| map (inline values) | `cilium: { encryption: { enabled: true } }` | addon + inline values (highest precedence) |
| `./` or `../` path | `./targets/networking` | `clusters/<domain>/targets/networking/` |
| absolute path | `/shared/base` | repo-root-relative |
