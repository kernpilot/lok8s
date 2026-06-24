# cluster.lok8s.yaml — full field reference

Authoritative sources: the driver readers (`.lok8s/drivers/lo/utils/config.sh`,
`.lok8s/drivers/kubeone/config`, `.lok8s/drivers/kkp/main`, `.lok8s/drivers/capi/generate`),
all five CRDs (`operator/crds/{lo,capi,kubeone,kkp,deploy}.yaml`), and
`.lok8s/libs/{bootstrap,provision}` + `.lok8s/libs/kubehz/main`. The readers win
over the CRDs — `Lo`/`KubeOne`/`Kkp` schemas set `x-kubernetes-preserve-unknown-fields`
and enumerate only a subset.

## Common header (all kinds)

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `apiVersion` | yes | — | `cluster.lok8s.dev/v1beta1` (all kinds, incl. Deploy) |
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
    pool: 10.125.125.125-10.125.125.150    # auto-derived for *.lok8s.dev even if omitted; set it off-slot
  registries:
    tls: true                              # DEFAULT true: HTTPS registries, cert minted by the
                                           # Secret plugin's cert: generator (dev CA at CAROOT, no
                                           # mkcert binary). `false` = plain HTTP. reader-only, not in CRD
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
  oidc:                                    # Lo/KubeOne only (Capi reads no oidc — see SKILL.md §4)
    issuer: https://id.example.com
    clientID: "..."
    usernameClaim: sub                     # defaults: sub / usernamePrefix oidc: / groups / groupsPrefix oidc:
    groupsClaim: groups
    caBundle: |  <PEM>
  dns: { domainFilter: "zone1.com,zone2.com" }   # for the external-dns addon
  bootstrap: [ ... ]
  kubehz: { hosting: self, access: none }
```

## kind: KubeOne (CRD `operator/crds/kubeone.yaml` — preserve-unknown; reader `.lok8s/drivers/kubeone/config`)

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

## kind: Capi (CRD: `operator/crds/capi.yaml`; reader `.lok8s/drivers/capi/{main,generate}`)

Manifest generation targets **Hetzner Cloud only** (`spec.provider.name: hetzner`).
Canonical provider block is `spec.provider.{name,config,credentials}` (the validated
`examples/capi` spec). `spec.hcloud`/`spec.aws` are only legacy *inference* fallbacks
(`provider::detect`) — prefer `spec.provider`.

```yaml
spec:
  kubernetes: { version: "v1.31.12" }
  cluster: { domain: prod.example.com, namespace: default }
  managementCluster:                          # REQUIRED for self-hosted Capi
    domain: prod-mgmt.example.com
    local: true                               # run the CAPI mgmt cluster as a local kind cluster
  provider:
    name: hetzner                             # only hetzner (CAPH) is generated
    config:                                   # opaque; read keys: region, sshKeyName (REQUIRED),
      region: fsn1                            #   image, network.enabled, placementGroups
      sshKeyName: my-key
      image: ubuntu-24.04                     # stock image; k8s installed via cloud-init
    credentials:
      envVars: [HCLOUD_TOKEN]
      secretRef: prod-credentials             # default <metadata.name>-credentials
  controlPlane: { replicas: 3, type: cpx22 }  # odd for etcd quorum (default 1; default type cax11)
  workers:                                    # each key = its own MachineDeployment pool
    general: { replicas: 3, type: cpx22 }
  bootstrap: [ cilium, ccm ]                  # CNI + Hetzner cloud-controller-manager
  # NOTE: the Capi driver reads NO spec.oidc (no apiserver OIDC wiring — silent no-op).
  # spec.etcd / spec.gitops appear in docs but are deferred / not read by the driver.
```

## kind: Kkp (CRD `operator/crds/kkp.yaml` — preserve-unknown; reader `.lok8s/drivers/kkp/main`)

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
apiVersion: cluster.lok8s.dev/v1beta1       # Deploy shares the cluster.lok8s.dev group
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
