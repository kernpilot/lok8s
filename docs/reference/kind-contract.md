# Driver Contract

The driver contract is lok8s's extensibility mechanism for cluster backends. Each cluster kind is implemented in `.lok8s/drivers/<kind>/main` that exports a set of required functions.

## Contract Functions

Every driver implementation must provide these four functions:

| Function | Signature | Purpose |
|----------|-----------|---------|
| `driver::provision` | `driver::provision <domain> <cluster_yaml>` | Create and configure the cluster |
| `driver::destroy` | `driver::destroy <domain> <cluster_yaml>` | Tear down the cluster |
| `driver::status` | `driver::status <domain> <cluster_yaml>` | Return cluster readiness (stdout) |
| `driver::kubeconfig` | `driver::kubeconfig <domain>` | Print path to kubeconfig (stdout) |

### Optional Functions

| Function | Signature | Purpose |
|----------|-----------|---------|
| `driver::post_provision` | `driver::post_provision <domain> <cluster_yaml>` | Post-provision hook (runs after `driver::provision`, before framework bootstrap) |

Bootstrap is **not** a driver function — it runs at the framework level
in `.lok8s/libs/bootstrap` and is identical for every driver. See the
[Addons guide](/guide/addons).

### Return Codes

`driver::provision` may return special exit codes:

| Code | Meaning |
|------|---------|
| `0` | Success — `libs/provision` continues with bootstrap, kubehz, gitops |
| `100` | Success, full lifecycle handled — skip all post-provision steps |
| Other | Failure — propagated to caller |

Return 100 is used by the Lo driver in CI remote mode: the remote VM
runs provision + bootstrap itself, so the local dispatch must skip them.

## Dispatch Mechanism

The provision system in `.lok8s/libs/provision` handles dispatch:

1. Reads `.kind` from `cluster.lok8s.yaml` (e.g. `Lo`, `Capi`)
2. Lowercases the value
3. Sources `.lok8s/drivers/<kind>/main`
4. If `--remote`: loads `spec.provider`, validates credentials
5. Calls `driver::provision` → checks return code
6. If rc=0: runs `driver::post_provision` (if defined), kubehz registration, framework `bootstrap::apply`, gitops
7. If rc=100: skips all post-provision (driver handled everything)

```bash
# cluster.lok8s.yaml
kind: Lo          # -> sources .lok8s/drivers/lo/main

kind: Capi        # -> sources .lok8s/drivers/capi/main
```

## Existing Implementations

### Lo (`drivers/lo/main`)

Local and remote clusters using Docker + kind.

**Implementation structure** (after refactor):
```
drivers/lo/
  main              driver::* contract + orchestration (~230 lines)
  utils/
    defaults.sh     constants (registry image, CIDRs, offsets)
    config.sh       lo::read_*_config, validators, spec-env export
    render.sh       kind config YAML rendering
    network.sh      Docker network lifecycle
    registries.sh   registry container lifecycle (+ registry TLS cert)
    services.sh     CoreDNS setup
    tunnel.sh       SSH tunnel + kubeconfig rewrite
    expose.sh       nginx reverse proxy
    remote.sh       remote VM provisioning + CI mode
  kind              CLI subcommand (lo kind)
  registry          CLI subcommand (lo registry)
  cluster/          templates (kind config, coredns, registry, expose)
```

**Provision steps (local):**
1. Read config (`lo::read_config` → network, registries, nodes, LB)
2. Validate IPs against subnets
3. Create Docker bridge network + shared registry network
4. Mint the registry TLS cert via the Secret plugin (`lo::registries_tls_cert`, default; skipped when `registries.tls: false`)
5. Start registry containers (build, cache, pull-through mirrors)
6. Write containerd `certs.d/` tree (bind-mounted into kind nodes)
7. Render + create kind cluster
8. Connect nodes to shared registry network
9. Extract kubeconfig
10. Apply local-registry-hosting ConfigMap (KEP-1755)
11. Apply registry ConfigMap + CoreDNS config (incl. `spec.coredns` →
    `coredns-custom` ConfigMap; see
    [Custom in-cluster DNS](/guide/local-dev#custom-in-cluster-dns))

> The application wildcard TLS is **not** a driver step: declare it as a
> [`cert:` Secret](/reference/kustomize-plugins#development-certificates-cert)
> in your targets (the gateway serves it), trusted once with `lo trust`.

> Step 4 must precede steps 5 and 6: the registry containers mount the
> cert, and `certs.d` references the dev root CA. The cert is minted
> before the containers start.

**Provision steps (remote, `--remote` flag):**
1. Load provider (`spec.provider.name`), provision VM
2. Wait for SSH, cloud-init, Docker on the remote
3. **Docker mode**: set `DOCKER_HOST=ssh://<ip>`, run the local steps above against remote Docker
4. **CI mode**: rsync repo to VM, run `lo provision` on VM via SSH, set up nginx expose + kubeconfig tunnel, return 100

**Registry spec fields** (`spec.registries`):

| Field | Default | Description |
|-------|---------|-------------|
| `tls` | `true` | Serve registries over HTTPS (`:443`) with a cert minted by the Secret plugin (`cert:` generator); no `insecure-registries` needed. `false` = plain HTTP `:80` |
| `shared` | `true` | Use shared pull-through mirrors on dedicated network |
| `network.name` | `lok8s-registries` | Docker network name for shared mirrors |
| `network.subnet` | `10.125.200.0/24` | Subnet for the shared registry network |
| `baseIP` | required | Base IP for pull-through mirror computation (`10.125.200.0` in shared mode; project `/24` start in non-shared mode) |
| `buildIP` | `<project>.101` | Build registry IP (always on project subnet, hostname `lok8s.local`) |
| `cacheIP` | `<project>.102` | Cache registry IP (always on project subnet, hostname `lok8s.cache`) |
| `mirrors[]` | 6 default mirrors | List of registry mirrors to provision (build, cache, io-docker, io-quay, io-k8s, io-ghcr) |

**Two registry kinds**: framework-private (`build`, `cache`) live on the project subnet and never move; pull-through mirrors (`io-*`) optionally live on the shared registry network.

**Containerd wiring**: hostname/IP-to-registry mapping is written by `lo::write_certs_d` to `clusters/<domain>/.containerd/certs.d/` BEFORE `kind create cluster`. Each kind node bind-mounts that directory to `/etc/containerd/certs.d/` via `extraMounts`, so containerd reads it at startup. No post-create `docker exec` step. In plain mode each `hosts.toml` uses `server = "http://<ip>"` + `skip_verify = true`; in TLS mode it uses `server = "https://<ip>"` + `ca = "/etc/containerd/certs.d/.ca/rootCA.pem"` (a copy of the dev root CA at CAROOT placed in the bind-mounted tree).

**Destroy behavior:** `driver::destroy` removes only per-project registries (build, cache). Shared pull-through mirror containers and the `lok8s-registries` network are **not** removed, since they may be in use by other projects. Use `lo registry clean --shared` to explicitly remove shared registries.

**Status output:** `Running` or `NotFound`

**Kubeconfig:** `.kubeconfig/<cluster-name>.yaml`

### Capi (`drivers/capi/main`)

Production clusters via Cluster API.

**Provision steps:**
1. Read management cluster domain from spec
2. Load management cluster kubeconfig
3. Detect CAPI provider (`hetzner` or `aws`)
4. Ensure credential Secret on management cluster
5. Generate CAPI resources from templates
6. Apply to management cluster
7. Wait for cluster to become `Provisioned`
8. Extract work cluster kubeconfig via `clusterctl`

**Status output:** CAPI Cluster phase (`Provisioned`, `Provisioning`, `Failed`, `NotFound`)

**Kubeconfig:** `.kubeconfig/<domain>.yaml`

### KubeOne (`drivers/kubeone/main`)

Production clusters via [KubeOne](https://docs.kubermatic.com/kubeone/). CLI-only (no operator hook).

**Provision steps:**
1. Resolve or generate `kubeone.yaml` manifest from cluster spec
2. Run `kubeone apply` with the manifest and Terraform state
3. Extract kubeconfig

**Status output:** `Ready`, `NotReady`, or `NotFound`

**Kubeconfig:** `.kubeconfig/<domain>.yaml`


### Kkp (`drivers/kkp/main`)

Production clusters via [Kubermatic Kubernetes Platform](https://github.com/kubermatic/kubermatic) (hosted control planes).

**Provision steps:**
1. Validate KKP credentials (`KKP_TOKEN`, provider credentials)
2. Read KKP config from cluster spec (`apiUrl`, `projectId`, `datacenter`)
3. Create cluster via KKP REST API (`POST /api/v2/projects/{pid}/clusters`)
4. Create machine deployments for each worker pool
5. Wait for cluster status to become `Running`
6. Extract kubeconfig via KKP API

**Status output:** KKP cluster phase (`Running`, `Creating`, `Error`, `NotFound`)

**Kubeconfig:** `.kubeconfig/<domain>.yaml`


## Writing a Custom Driver

To add a new cluster kind (e.g. `k3s`):

### 1. Create the script

```bash
# .lok8s/drivers/k3s/main

driver::provision() {
  local domain="$1" cluster_yaml="$2"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "$cluster_yaml")

  # Create the cluster
  k3d cluster create "$cluster_name" \
    --api-port 6443 \
    --servers 1

  # Extract kubeconfig
  driver::kubeconfig "$domain"
}

driver::destroy() {
  local domain="$1" cluster_yaml="$2"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "$cluster_yaml")
  k3d cluster delete "$cluster_name"
}

driver::status() {
  local domain="$1" cluster_yaml="$2"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "$cluster_yaml")
  if k3d cluster list | grep -q "$cluster_name"; then
    echo "Running"
  else
    echo "NotFound"
  fi
}

driver::kubeconfig() {
  local domain="$1"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "clusters/${domain}/cluster.lok8s.yaml")
  local kubeconfig_path="${PATH_BASE}/.kubeconfig/${cluster_name}.yaml"
  mkdir -p "${PATH_BASE}/.kubeconfig"
  k3d kubeconfig get "$cluster_name" > "$kubeconfig_path"
  echo "$kubeconfig_path"
}
```

### 2. Create the cluster spec

```yaml
# clusters/dev.example.com/cluster.lok8s.yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: K3s
metadata:
  name: dev
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: dev.example.com
  bootstrap:
    - cilium
```

### 3. Provision

```bash
lo provision dev.example.com
```

The dispatch system sources `drivers/k3s/main` automatically based on `kind: K3s`.

## Variables Available

Driver scripts have access to bash dynamic scoping from the `main()` function in `.lok8s/lo`:

| Variable | Source | Description |
|----------|--------|-------------|
| `$cluster` | `--cluster` flag / `LOK8S_CLUSTER_NAME` | Cluster name |
| `$config` | `--config` flag / `KIND_CONFIG` | Kind config path |
| `$domain` | `--domain` flag / `DOMAIN_NAME` | Domain name |
| `$path` | `--path` flag / `LOK8S_CLUSTER_PATH` | Cluster path |
| `$force` | `--force` flag | Force mode |
| `$verbose` | `--verbose` flag | Verbose mode |
| `$PATH_BASE` | Auto-detected | Project root |
| `$KUBECONFIG` | Set by `main()` | Active kubeconfig path |

Driver scripts can also source shared libraries from `.lok8s/libs/` (they are already loaded via `import` in the main `lo` script).
