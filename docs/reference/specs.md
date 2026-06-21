# Spec Reference

lok8s uses YAML spec files to define clusters and deployments. These specs
double as Kubernetes CRDs when used with the operator.

## cluster.lok8s.yaml

Defines a cluster domain. The `kind` field determines which driver contract
handles provisioning.

### Common fields (all cluster kinds)

Every `cluster.lok8s.yaml` shares this base structure regardless of kind:

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo | KubeOne | Capi | Kkp         # driver selection
metadata:
  name: my-cluster                        # unique cluster name
spec:
  kubernetes:
    version: "v1.31.12"                   # k8s version (required)
  cluster:
    domain: example.com                   # cluster FQDN (required)
    namespace: default                    # default namespace
  provider:                               # infrastructure provider (optional for Lo)
    name: hetzner                         # provider name (matches .lok8s/providers/<name>/)
    configRef: hetzner.json               # provider config file (relative to cluster dir)
    # OR inline:
    # config: { ... }                     # opaque provider-specific config
  bootstrap:                              # cluster-infra addons (ordered, applied by framework)
    - cilium                              # bare name: .lok8s/addons/<name>/
    - ./targets/networking                # ./path: relative to cluster dir
  kubehz:                                 # kubehz platform integration (optional)
    hosting: self | hosted                # who runs the control plane
    access: none | registered | managed   # kubehz visibility
    apiUrl: https://api.kubehz.dev        # required when hosting=hosted or access!=none
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `apiVersion` | yes | — | Always `cluster.lok8s.dev/v1beta1` |
| `kind` | yes | — | Driver: `Lo`, `KubeOne`, `Capi`, `Kkp` |
| `metadata.name` | yes | — | Unique cluster name |
| `spec.kubernetes.version` | yes | — | Kubernetes version |
| `spec.cluster.domain` | yes | — | Cluster FQDN (folder name convention) |
| `spec.cluster.namespace` | no | `default` | Default namespace |
| `spec.provider.name` | no | — | Infrastructure provider |
| `spec.provider.configRef` | no | — | Provider config file path |
| `spec.provider.config` | no | — | Inline provider config (mutually exclusive with configRef) |
| `spec.bootstrap` | no | `[cilium]` | Ordered list of infra addons (default applied by framework bootstrap when omitted) |
| `spec.kubehz.hosting` | no | `self` | Control plane hosting model |
| `spec.kubehz.access` | no | `none` | kubehz platform access level |
| `spec.kubehz.apiUrl` | conditional | — | kubehz API URL |

### Provider output (standard schema)

All providers produce the same output JSON. Drivers read this — not the
cluster spec — for infrastructure details (IPs, SSH access, node topology).

```json
{
  "api": { "endpoint": "<lb_ip>", "port": 6443 },
  "access": [
    { "id": "default", "type": "ssh", "user": "root", "port": 22,
      "privateKey": "~/.ssh/key", "publicKey": "~/.ssh/key.pub" },
    { "id": "bastion", "type": "ssh", "host": "bastion.example.com",
      "user": "jump", "privateKey": "~/.ssh/bastion" },
    { "id": "dedicated", "type": "ssh", "user": "root",
      "privateKey": "~/.ssh/robot_key", "bastion": "bastion" }
  ],
  "nodes": [
    { "name": "cp-0", "role": "control-plane", "group": "cloud-cp",
      "public_ip": "1.2.3.4", "private_ip": "10.0.0.1",
      "access": "default", "ssh_user": "root", "ssh_port": 22 }
  ],
  "network": { "id": "12345", "name": "my-net", "cidr": "10.0.0.0/16" }
}
```

| Field | Description |
|-------|-------------|
| `access[].id` | Unique identifier for this access method |
| `access[].type` | Access type: `ssh`, `ssm` (AWS), `gcloud` (GCP) |
| `access[].bastion` | ID of another access entry to use as jump host |
| `nodes[].access` | References `access[].id` (default: first entry) |
| `nodes[].ssh_user/ssh_port` | Back-compat fields (derived from access) |

Drivers should read `access[]` for connection details, not `spec.ssh`.
The number of control-plane nodes is derived from
`nodes | select(.role == "control-plane") | length`, not `spec.controlPlane`.

### Kind-specific fields

Each cluster kind adds its own fields to the common base.
Kind-specific fields are documented in the sections below.

### Lo Spec

**Minimal form** — the framework derives everything else from the
domain (slot-parsed for `*.lok8s.dev`) and domain-independent defaults.

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  cluster:
    domain: lok8s.dev
```

**Full form** — every field explicit, equivalent to the defaults that
the minimal form would produce for slot 125.

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  kubernetes:
    version: "v1.31.4"         # Kubernetes version
  cluster:
    domain: lok8s.dev           # Cluster FQDN (required)
    namespace: default          # Default namespace
  network:                       # Docker bridge network config
    name: local                  # Bridge network name (default: metadata.name)
    cidr: "10.125.125.0/24"     # /24 slot — slot-derived from domain for *.lok8s.dev
  registries:                    # Registry mirror configuration
    tls: true                    # HTTPS registries (cert: generator-minted; default: true)
    shared:
      enabled: true              # Pull-through mirrors on a shared network (default: true)
      network:
        name: lok8s-registries   # Shared network name (default)
        cidr: "10.125.200.0/24"  # Shared network CIDR (default)
    mirrors:                     # Pull-through mirror list (default: io-* set below)
      - name: io-docker
        url: https://registry-1.docker.io
      - name: io-quay
        url: https://quay.io
      - name: io-k8s
        url: https://registry.k8s.io
      - name: io-ghcr
        url: https://ghcr.io
  nodes:                         # Kind cluster node topology
    controlPlane: 1              # Control-plane nodes (default: 1)
    workers: 0                   # Worker nodes (default: 0)
    hostPorts: true              # Bind 80/443/8080 on host (default: true for slot 125, false otherwise)
    maxConcurrentDownloads: 3    # containerd parallel image pulls (default: 3)
  loadBalancer:                  # MetalLB L2 load balancer (optional)
    pool: "10.125.125.125-10.125.125.150"  # 26 IPs; slot-derived by default for *.lok8s.dev
  runtime: kind                  # Runtime: "kind" (default; future: k3s, k0s)
  bootstrap:                     # Cluster-infra addons (ordered; default: [cilium])
    - cilium                     # framework addon: .lok8s/addons/cilium/
    - metallb                    # framework addon: .lok8s/addons/metallb/
  provider:                      # Cloud provider for remote VMs (optional, requires --remote)
    name: hetzner                # Provider name (matches .lok8s/providers/<name>/main)
    configRef: hetzner.json      # Provider config file (relative to cluster dir)
  remote:                        # Remote mode config (optional, requires --remote)
    mode: docker                 # "docker" (local orchestration, remote Docker) | "ci" (everything on VM)
    expose: true                 # Run nginx reverse proxy on remote (default: true when provider is set)
    sync:                        # Repo sync settings (CI mode only)
      path: .                   # What to rsync (default: repo root)
      exclude:                  # Rsync exclusions
        - .git
        - node_modules
        - .secrets
        - .kubeconfig
      dest: /workspace           # Remote destination (default: /workspace)
    tilt: true                   # Start Tilt on the VM after provision (CI mode, default: true)
```

#### Provider and remote mode

The `spec.provider` and `spec.remote` blocks are only activated when
`lo provision --remote` (or `lo up --remote`) is used. Without `--remote`,
they are ignored — the same spec works for both local and remote.

**Docker mode** (default): The provider creates a VM, then the local
machine runs kind/registries/bootstrap against the remote Docker via
`DOCKER_HOST=ssh://<ip>`. The API is accessed through an SSH tunnel.

**CI mode** (`mode: ci`): The repo is rsynced to the VM and `lo provision`
runs entirely on the remote. The remote handles kind, registries,
bootstrap, and optionally Tilt. The local machine sets up an SSH tunnel
for kubectl and optionally an nginx proxy for HTTPS access.

See [CLI reference — Remote clusters](cli.md#remote-clusters) for usage.

#### Default resolution

When a field is missing, the Lo driver fills it in via two layers:

1. **Domain-independent defaults** — apply to any Lo cluster:

   | Field | Default |
   |---|---|
   | `registries.shared.enabled` | `true` |
   | `registries.shared.network.name` | `lok8s-registries` |
   | `registries.shared.network.cidr` | `10.125.200.0/24` |
   | `registries.mirrors` | `io-{docker,quay,k8s,ghcr}` on the standard upstream URLs |
   | `nodes.controlPlane` | `1` |
   | `nodes.workers` | `0` |
   | `nodes.maxConcurrentDownloads` | `3` |
   | `runtime` | `kind` |
   | `bootstrap` | `[cilium]` |

2. **Slot-derived defaults** — `*.lok8s.dev` only. The slot is parsed
   from `spec.cluster.domain`: `lok8s.dev` → 125, `<n>.lok8s.dev` → n.

   | Field | Default |
   |---|---|
   | `network.name` | `metadata.name` |
   | `network.cidr` | `10.125.<slot>.0/24` |
   | `loadBalancer.pool` | `10.125.<slot>.125-10.125.<slot>.150` |
   | `nodes.hostPorts` | `true` for slot 125, `false` for every other slot |

   Non-`*.lok8s.dev` domains must supply `network.name` and
   `network.cidr` explicitly.

Build and cache registries are framework-private — they always live
on the project subnet at `.101` and `.102` and must not be listed in
`spec.registries.mirrors`.

#### Network Configuration

The `spec.network` section configures the Docker bridge network used
by kind clusters. For `*.lok8s.dev` domains, both fields are derived
from the domain and `metadata.name`; for other domains, both are
required. See [SUBNETS.md](https://github.com/kernpilot/lok8s/tree/main/tests/e2e/SUBNETS.md)
for the `10.125.0.0/16` slot plan.

| Field | Required | Description |
|-------|----------|-------------|
| `name` | no (for `*.lok8s.dev`) | Docker bridge network name (default: `metadata.name`) |
| `cidr` | no (for `*.lok8s.dev`) | `/24` CIDR for the cluster's slot (default: `10.125.<slot>.0/24`) |

Example for an alternate slot:

```yaml
spec:
  cluster:
    domain: 50.lok8s.dev
  # network.name defaults to metadata.name
  # network.cidr defaults to "10.125.50.0/24"
```

Or fully explicit:

```yaml
spec:
  network:
    name: lok8s-50
    cidr: "10.125.50.0/24"
```

#### Registries Configuration

The `spec.registries` section configures registry mirrors. Every
lok8s cluster ships with **two framework-private registries** plus
**four default pull-through mirrors**:

- **Framework-private** (`build`, `cache`) — plain filesystem registries
  owned by the framework. Always live on the project subnet at fixed
  offsets `.101` (build) and `.102` (cache), even in shared mode,
  because they hold per-project content (locally-built images for
  `build`, credentialed pre-pulls for `cache`). They get the canonical
  hostnames `lok8s.local` and `lok8s.cache` via containerd mirror
  config. **Do not list them in `spec.registries.mirrors`** — they
  ship implicitly.
- **Pull-through mirrors** (`io-docker`, `io-quay`, `io-k8s`, `io-ghcr`
  by default) — transparent caches for public upstreams. When
  `shared.enabled` is true (the default), they live on the shared
  registry network at `10.125.200.0/24` so multiple kind clusters
  reuse one cache.

| Field | Required | Description |
|-------|----------|-------------|
| `tls` | no (default `true`) | HTTPS registries with a cert minted by the Secret plugin; set `false` for plain HTTP (see [TLS](#registry-tls-mkcert) below) |
| `shared.enabled` | no (default `true`) | Pull-through mirrors live on the shared network |
| `shared.network.name` | no (default `lok8s-registries`) | Shared registry network name |
| `shared.network.cidr` | no (default `10.125.200.0/24`) | Shared registry network CIDR |
| `mirrors[].name` | yes (per entry) | Registry name (must not be `build` or `cache`) |
| `mirrors[].url` | yes (per entry) | Upstream registry URL |
| `prefix` | no (default `lok8s.local`) | Image registry prefix used for locally-built image references |

Registries are reached via raw IP (no host port mapping). The listen
port depends on the mode: **`:80`** in the default plain-HTTP mode, or
**`:443`** when `tls: true` (so a bare-IP `docker push <ip>/...`, which
the Docker client resolves to the HTTPS default port, reaches the
registry without an explicit port in the ref).

##### Registry TLS {#registry-tls-mkcert}

TLS is the **default** (`spec.registries.tls: true`; set `false` for plain
HTTP). Every registry (framework-private **and** pull-through mirrors)
serves HTTPS with a certificate minted by the
[Secret plugin](/reference/kustomize-plugins#development-certificates-cert)
(the same `cert:` generator used for application TLS), signed by your
shared dev CA at `CAROOT`. This removes the need for an
`insecure-registries` entry in the host Docker daemon configuration:

- The cert is minted into `.secrets/tls/registries/` with every
  registry's **IP** and (for framework registries) **hostname**
  (`lok8s.local`, `lok8s.cache`) as Subject Alternative Names. It is
  re-minted automatically if the IP/hostname set changes — no `mkcert`
  binary involved (the CA at `CAROOT` is created on demand).
- **Containerd inside the kind nodes** trusts the cert via an explicit CA
  file: each `hosts.toml` references a copy of the dev `rootCA.pem`
  (`CAROOT`) mounted into the node's `certs.d` tree (no `skip_verify`).
  This needs **no** `mkcert -install` — in-cluster pulls work out of the box.
- **Host `docker push`** validates against the system trust store, so the
  dev CA must be installed there with [`lo trust`](/guide/secrets); then
  pushes to `lok8s.local`/`lok8s.cache` (and raw IPs) succeed over HTTPS
  with no daemon configuration.

**Prerequisite:** the cert is minted by the Secret plugin (no mkcert), but
the **host** Docker client and `curl` only trust it once the dev CA is in
the system trust store — run [`lo trust`](/guide/secrets) once (it wraps
`mkcert -install`; `mkcert` is managed by [`b`](https://github.com/fentas/b),
`b install mkcert`). `$CAROOT/rootCA.pem` (default
`~/.local/share/mkcert/rootCA.pem`) is the trust anchor. If `tls: true`
but the Secret plugin isn't built, `lo provision`/`lo up` fail fast; if
the CA isn't trusted, the cluster still comes up but `docker push` fails
cert verification until you run `lo trust` (or another option — see
[Host push trust options](/guide/shared-registries#host-push-trust-options)).

Opting out with `tls: false` keeps plain-HTTP registries, which instead
require the registry IP range in the host's `insecure-registries`.

**Default registry set** for slot 125 (the default cluster) with
`shared.enabled: true`:

| Name | IP | Hostname | Purpose |
|------|-----|----------|---------|
| `build` | `10.125.125.101` | `lok8s.local` | Tilt push target for locally-built images |
| `cache` | `10.125.125.102` | `lok8s.cache` | Pre-pull target for `build: false` services with a configured registry |
| `io-docker` | `10.125.200.2` | `docker.io` | Pull-through mirror |
| `io-quay` | `10.125.200.3` | `quay.io` | Pull-through mirror |
| `io-k8s` | `10.125.200.4` | `registry.k8s.io` | Pull-through mirror |
| `io-ghcr` | `10.125.200.5` | `ghcr.io` | Pull-through mirror |

For other slots, replace `125.125` with `125.<slot>`. In non-shared
mode (`shared.enabled: false`), pull-throughs move to the project
subnet at `.103+`.

**Containerd wiring**: hostname → IP resolution happens via per-host `hosts.toml` files written by `lo::write_certs_d` to `clusters/<domain>/.containerd/certs.d/`. That directory is bind-mounted into every kind node via `extraMounts`, so containerd reads it on startup. Each host gets BOTH a hostname-keyed entry AND a raw-IP-keyed entry, covering both naming conventions.

#### Nodes Configuration

The `spec.nodes` section controls kind cluster node topology.

| Field | Default | Description |
|-------|---------|-------------|
| `controlPlane` | `1` | Number of control-plane nodes (1-9) |
| `workers` | `0` | Number of worker nodes (0-99) |
| `hostPorts` | `true` | Bind host ports 80/443/8080 on the first control-plane node |
| `maxConcurrentDownloads` | `3` | containerd `max_concurrent_downloads` — parallel image layer pulls per node. Matches containerd's own default. Lower to `1` if a flaky pull-through mirror returns "unexpected commit digest" under concurrent pulls; raise for faster cold starts on a reliable mirror. |

When absent, a single control-plane node is created (the kind default).

The first control-plane node gets port mappings (80, 443, 8080), the `ingress-ready=true` label, and a docker socket mount. Additional control-plane and worker nodes get the base image only.

#### Load Balancer Configuration

The `spec.loadBalancer` section installs and configures MetalLB for LoadBalancer service support. When absent, MetalLB is not installed.

| Field | Description |
|-------|-------------|
| `pool` | IP range for MetalLB L2 pool (e.g. `10.125.125.125-10.125.125.150` for slot 125) |

The pool range must be within `spec.network.cidr`. IP validation is performed at provisioning time.

MetalLB is pinned to Helm chart version `0.15.3`. An `IPAddressPool` and `L2Advertisement` are created automatically.

#### Custom DNS (`spec.coredns`)

`spec.coredns` injects per-cluster CoreDNS config. The Lo driver renders it into a `coredns-custom` ConfigMap that the base Corefile imports from `/etc/coredns/custom` (`import custom/*.server` + `import custom/*.override`). Declarative and committed, so it survives a recreate — unlike a runtime `kubectl patch`. All fields are optional and compose; when `spec.coredns` is absent the import is a no-op. Full guide: [Custom in-cluster DNS](/guide/local-dev#custom-in-cluster-dns).

| Field | Type | Description |
|-------|------|-------------|
| `hosts[]` | list of `{name, target}` | Driver generates a `name:53 { … }` server block resolving the zone `name` (its apex + every `*.name`): `A → target`, `AAAA → NODATA`, other types forwarded. `target: gateway` resolves to the **first `spec.loadBalancer.pool` IP** (where the gateway pins via the metallb annotation) — no hardcoded IP to keep in sync; otherwise a literal IP. |
| `servers` | string | Raw CoreDNS server block(s), inline (a `*.server` file). |
| `overrides` | string | Raw directives merged into the default `.:53` block (a `*.override` file). |
| `import` | string | Path (relative to the cluster dir; default `./coredns`) to a directory of raw `*.server` / `*.override` files. |

```yaml
spec:
  coredns:
    hosts:
      - name: kubehz.dev      # apex + *.kubehz.dev
        target: gateway       # = loadBalancer.pool[0], or a literal IP
```

Don't define the same zone via both `hosts` and a raw `servers`/`import` block — CoreDNS rejects duplicate zone definitions.

#### Bootstrap (Cluster-Infra Addons)

The `spec.bootstrap` field is an **ordered list** of addons the framework applies at provision time, *before* Tilt starts and *before* any workloads land. This is the framework's one ordering primitive — each entry is applied, then lok8s waits for Deployments/DaemonSets to become ready before moving to the next.

Implementation lives in `.lok8s/libs/bootstrap` and runs identically across all drivers (Lo, KubeOne, Capi, Kkp).

Use this for anything the cluster needs to be healthy before workloads can run: CNI, CSI, CCM, MetalLB, cert-manager, etc. It is **not** for application workloads — those live under `targets/` and have no framework-level ordering.

```yaml
spec:
  bootstrap:
    - cilium            # bare name → framework addon
    - metallb
    - cert-manager
    - ./custom/         # relative path → clusters/<domain>/custom/
    - /shared-base/     # absolute path → repo root /shared-base/
```

**Entry resolution:**

| Form | Resolves to |
|------|-------------|
| Bare name (`cilium`) | `.lok8s/addons/<name>/` |
| `./path` or `../path` | Relative to `clusters/<domain>/` |
| `/path` | Relative to the repo root |

Each entry must point at a kustomize-buildable directory. Framework addons ship as khelm `ChartRenderer`s (`chart.yaml` + `values.yaml`), but any kustomization works. If `spec.bootstrap` is omitted entirely, the framework defaults to `[cilium]` (every cluster needs a CNI).

Framework addons support driver- and provider-specific values files plus inline overrides. See the [Addons guide](/guide/addons#provider-aware-values) for the full precedence chain (`base < driver < provider < inline`) and authoring guidance.

**Failure modes:**

- Missing entry → bootstrap exits non-zero.
- `kustomize build` failure → exits non-zero.
- `kubectl apply` failure → exits non-zero.
- Values merge failure → exits non-zero.
- Health-wait timeout → logged as warning, non-fatal (the addon might not ship Deployments).

Running `lo lint` validates that every `spec.bootstrap` entry resolves to an existing directory.

#### IP Validation

During provisioning, all computed IPs are validated against the subnet:

- Registry IPs (sequential offsets from the project or shared subnet base)
- MetalLB pool start and end IPs (if configured)

If any IP falls outside the subnet CIDR, provisioning fails with a clear error message.

#### Registry Config (`.registries.json`)

During provisioning, the Lo driver generates a `.registries.json` file
in the cluster directory (e.g. `clusters/lok8s.dev/.registries.json`).
This JSON file is the single source of truth for all registry config —
IPs, upstream URLs, domains, container hostnames, and shared/project mode.

```json
{
  "shared": true,
  "network": { "name": "lok8s-registries", "cidr": "10.125.200.0/24" },
  "project_network": "local",
  "registries": [
    { "name": "build", "ip": "10.125.125.101", "url": "", "domain": "", "host": "lok8s.local", "type": "framework" },
    { "name": "cache", "ip": "10.125.125.102", "url": "", "domain": "", "host": "lok8s.cache", "type": "framework" },
    { "name": "io-docker", "ip": "10.125.200.2", "url": "https://registry-1.docker.io", "domain": "docker.io", "host": "", "type": "mirror" }
  ]
}
```

All registry consumers (`lo registry status`, `lo::registries`,
`lo::write_certs_d`, `lo::registry_configmap`) read from this JSON
via the `registry::*` helpers in `drivers/lo/utils/config-registry.sh`.
No global bash arrays are used.

The file is deterministic (derived from the cluster spec + slot) and
can be committed to the repo.

#### Registry ConfigMap

A `lok8s-registries` ConfigMap is created in `kube-system` during provisioning. It contains the IP, port, and (for mirrors) remote URL of each registry. This allows in-cluster tooling to discover registry mirrors programmatically.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: lok8s-registries
  namespace: kube-system
data:
  build.ip: "10.125.125.101"
  build.port: "80"
  cache.ip: "10.125.125.102"
  cache.port: "80"
  io-docker.ip: "10.125.200.2"           # shared mode
  io-docker.port: "80"
  io-docker.url: "https://registry-1.docker.io"
  # ... etc
```

Additionally, a `local-registry-hosting` ConfigMap is applied in `kube-public` per [KEP-1755](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry). Tilt reads this to auto-configure `default_registry()` so `docker_build('foo', ...)` transparently pushes to the build registry without per-user setup:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "10.125.125.101"
    hostFromClusterNetwork: "lok8s.local"
```

#### Lo Status

```yaml
status:
  phase: Provisioned            # Pending | Provisioning | Provisioned | Failed
  ready: true
  conditions:
    - type: Ready
      status: "True"
      reason: Provisioned
      message: Cluster is ready
      lastTransitionTime: "2025-01-01T00:00:00Z"
```

### KubeOne Spec

KubeOne clusters use the common fields plus `spec.provider` for infrastructure.
SSH config and node topology are defined in the provider config (e.g. `hetzner.json`),
NOT in the cluster spec. The driver reads them from `provider::output`.

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: my-prod
spec:
  kubernetes:
    version: "v1.31.12"
  cluster:
    domain: my-cluster.example.com
  provider:
    name: hetzner
    configRef: hetzner.json
  bootstrap:
    - ./targets/networking
    - ./targets/cnpg-operator
```

**Deprecated fields** (use provider config instead):
- ~~`spec.ssh`~~ → provider config `sshUser`, `sshPrivateKey`, `sshPublicKey`
- ~~`spec.controlPlane.replicas`~~ → derived from provider output node count
- ~~`spec.workers`~~ → defined in provider config (hetzner.json server entries)

These fields are still read as fallbacks for backward compatibility but
should not be used in new cluster specs.

### Capi Spec

> **Note:** Capi retains `controlPlane`, `workers`, and provider-specific
> fields (`hcloud`, `aws`) in the cluster spec because CAPI generates
> Kubernetes resources (MachineDeployments, etc.) that need these values
> directly. This is different from KubeOne where the provider handles
> node topology.

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: prod
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: prod.example.com    # Cluster FQDN (required)
    namespace: default
  managementCluster:
    domain: mgmt.example.com    # Management cluster domain (omit for SaaS)
  credentials:
    secretName: prod-credentials  # Secret name on mgmt cluster
  # Provider: exactly one of hcloud, aws
  hcloud:
    region: fsn1                # Hetzner Cloud region
    sshKeyName: my-key          # SSH key registered in Hetzner
  hrobot:                       # Optional: Hetzner bare metal
    sshKeyName: my-key
    hosts:
      - name: node-1
        serverNumber: 12345
        rootDeviceHints:
          wwn: "0x500..."
  aws:
    region: eu-central-1        # AWS region
  controlPlane:
    replicas: 3                 # Control plane nodes (default: 1)
    type: cax21                 # Machine type
  workers:                      # Worker pools (key = pool name)
    general:
      replicas: 3
      type: cax21
    gpu:
      replicas: 1
      type: ccx33
  oidc:                           # Capi uses enabled + issuerUrl;
    enabled: false                # Lo/KubeOne instead use oidc.issuer + oidc.clientID
    issuerUrl: https://auth.example.com
  etcd:
    encryptionSecretName: etcd-encryption
  bootstrap:                    # Cluster-infra addons (ordered)
    - cilium
    - ccm
    - cert-manager
  gitops:
    provider: flux              # flux | argo
    repo: https://github.com/myorg/infra.git
    branch: main                # default: main
    path: clusters/prod.example.com/artifacts
    secretRef: git-auth         # Optional: Secret for git credentials
```

#### Capi Status

```yaml
status:
  phase: Provisioned
  ready: true
  provider: hetzner
  controlPlaneEndpoint:
    host: 10.0.0.1
    port: 6443
  kubeconfig:
    secretRef: prod-kubeconfig
  targets:
    networking: Applied
    app: Applied
  gitops:
    provider: flux
    status: Bootstrapped
  conditions:
    - type: InfrastructureReady
      status: "True"
    - type: ControlPlaneReady
      status: "True"
```

## deploy.lok8s.yaml

Defines a deployment domain that targets an existing cluster.

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: api
spec:
  domain: api.example.com       # Deployment domain FQDN
  clusterRef:
    domain: prod.example.com    # Cluster to deploy to (required)
  namespace: api                # Target namespace on the cluster
```

> **Note:** Deploy CRD workload selection is being reworked. Target
> selection for Deploy specs will land alongside the `services.yaml`
> targets-map design. Until then, a Deploy spec carries only
> `clusterRef` + `namespace`.

#### Deploy Status

```yaml
status:
  phase: Deployed               # Pending | Deploying | Deployed | Failed
  targets:
    app: Applied
    jobs: Applied
  conditions:
    - type: Ready
      status: "True"
```

## services.yaml

Defines services for Tilt local development. Not a CRD -- consumed by `lo env` and the Tilt extension.

```yaml
registry:
  prefix: lok8s.local           # Image registry prefix
  branch: builds                # Image path component
  tag: latest                   # Image tag

defaults:
  build: true                   # Default per-service build flag (true = build locally + Tilt live-update)
  dockerfile: service           # "service" (lok8s.Dockerfile) or "production" (Dockerfile)

services:
  my-api:
    enabled: true               # Whether this service is active (default true)
    build: true                 # Whether to docker_build in Tilt (default: defaults.build)
    path: ./my-api              # Service source directory (default: ./<name>)
    namespace: api              # Optional: inject namespace
    dockerfile: production      # Optional: override defaults.dockerfile
    registry:                   # Optional: per-service registry override
      branch: pr-1234
      tag: abc123
    image: ghcr.io/org/img:v1   # Optional: pin to a specific image (mutually exclusive with registry:)
    watch:                      # Optional: extra files to watch
      - package.json
```

## Per-Service lok8s.yaml

Per-service Tilt build configuration. Located at `<service-path>/lok8s.yaml`.

```yaml
build:
  dockerfile: lok8s.Dockerfile  # Dockerfile path (relative to service dir)
  context: .                    # Build context (relative to service dir)
  build_args:                   # Env var names to pass as build args
    - NODE_ENV
  live_update:
    fall_back_on:
      files:
        - package.json
        - yarn.lock
    sync:
      - local_path: ./src
        remote_path: /app/src     # NOTE: remote_path (not container_path)
    run:
      cmd: npm run dev            # NOTE: cmd (not command)
    restart_container: {}       # Optional: restart after sync

ports:
  - from: 3000                  # Host port
    to: 3000                    # Container port

links:
  - url: http://localhost:3000
    name: API

workloads:                      # Optional: override detected workload names
  - my-api-deployment

tilt:
  resource_deps:                # Tilt resource dependencies
    - postgres
  labels:                       # Tilt UI labels
    - backend
  extra_resources:              # Additional k8s_resource groupings
    - name: my-api-migrations
      objects:
        - "my-api-migrate:job"
      resource_deps:
        - postgres
      labels:
        - backend
```
