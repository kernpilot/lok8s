# Driver Contract

A driver creates and removes the clusters of one cluster kind. The `kind:`
field of `cluster.lok8s.yaml` selects the driver: `kind: Lo` selects the `lo`
driver. This page describes the contract of the Go `lo` binary. The
[bash contract](#the-frozen-bash-contract) of the frozen tree is at the end.

## Built-in drivers

The drivers are Go packages in the `lo` binary. `lo drivers --list` lists them.

| `kind:` | Driver | Go package | Clusters |
|---------|--------|------------|----------|
| `Lo` | `lo` | `internal/driver/lo` | kind on local Docker, or on a remote VM |
| `Capi` | `capi` | `internal/driver/capi` | Cluster API |
| `KubeOne` | `kubeone` | `internal/driver/kubeone` | KubeOne on VMs or bare metal |
| `Kkp` | `kkp` | `internal/driver/kkp` | Kubermatic (KKP) user clusters |
| `Kubehz` | `kubehz` | `internal/driver/kubehz` | spaces on the kubehz shared control plane |

Each package registers itself with `driver.Register` (`internal/driver/registry.go`).
`internal/cli/drivers.go` links all of them into the binary.

## The contract

Every driver implements the `Driver` interface (`internal/driver/driver.go`):

| Method | Purpose |
|--------|---------|
| `Provision(ctx, domain)` | Create or reconcile the cluster |
| `Destroy(ctx, domain)` | Remove the cluster |
| `Status(ctx, domain)` | Return one status word, for example `Running` or `NotFound` |
| `Kubeconfig(ctx, domain)` | Return the path of the kubeconfig under `.kubeconfig/` |

A driver can also implement two optional hooks:

| Interface | Method | Purpose |
|-----------|--------|---------|
| `Exporter` | `Export(ctx, domain)` | Set the spec-derived environment (`LOK8S_SPEC_*`) that the `spec.bootstrap` addons read. The dispatch calls it on a full provision and on `--bootstrap`, so it must be idempotent. |
| `PostProvisioner` | `PostProvision(ctx, domain)` | Side effects that need the provisioned infrastructure. The dispatch calls it on a full provision only. |

Bootstrap is **not** a driver method. The framework bootstrap in the binary
applies `spec.bootstrap` the same way for every driver. See the
[Addons guide](/guide/addons).

### Errors and exit codes

The provision dispatch handles these errors. The exit code is the code of
`lo up` and `lo provision`:

| Error | Exit code | Meaning |
|-------|-----------|---------|
| `nil` | `0` | Success. The dispatch continues with the next step. |
| `driver.ErrDeclined` | `3` | The operator declined the confirmation for real infrastructure. |
| `driver.ErrFullLifecycle` | `0` | Success. The driver did the full lifecycle, so the dispatch skips all later steps and returns success. |
| any other error | `1`, or the exit code of the failed subprocess | Failure. |

The `lo` driver returns `ErrFullLifecycle` in remote CI mode: the remote VM
runs the provision and the bootstrap itself. `ErrFullLifecycle` is the Go
form of the bash return code `100`. `lo drivers <name> provision` calls the
driver without the dispatch, so there `ErrFullLifecycle` gives exit code
`100` (`driver.ExitCode`).

## Dispatch

`lo up`, `lo provision`, `lo destroy` and `lo status` use the provision
dispatch (`internal/provision/dispatch.go`). A provision does these steps:

1. Read `clusters/<domain>/cluster.lok8s.yaml`. A domain with a
   `deploy.lok8s.yaml` stops here: `lo deploy` serves it.
2. Read `kind:` and change it to lowercase (`Lo` → `lo`).
3. Find the driver in the registry. An unknown kind stops with
   `Unknown cluster kind`.
4. Show a summary of the real infrastructure and ask for confirmation. A
   local `lo` cluster and `--force` skip this step.
5. Load and validate the provider of `spec.provider.name`. The `lo` driver
   loads it only with `--remote`.
6. Call `Provision`. On `ErrFullLifecycle`, stop with success.
7. Call `PostProvision`, if the driver has it.
8. Call `Export`, if the driver has it.
9. Register the cluster with kubehz, if `spec.kubehz` asks for it.
10. Apply the `spec.bootstrap` addons in order.
11. Publish the cluster inventory. A failure here only gives a warning.
12. Run the GitOps bootstrap, if `spec.gitops.provider` is set.

`lo provision --bootstrap` skips steps 5 to 7. It needs the kubeconfig of an
existing cluster, `.kubeconfig/<metadata.name>.yaml`.

`lo drivers <name> <command> <domain>` calls one contract method directly,
without the dispatch: no confirmation, no provider, no bootstrap. Use it to
debug a driver.

## Existing implementations

### Lo (`internal/driver/lo`)

Local and remote clusters with Docker and kind. The driver reads its
templates (kind config, CoreDNS, registry, expose) from the embedded
`drivers/lo/cluster/` assets. It writes them into `.lok8s/drivers/lo/cluster/`
on first use.

| File | Content |
|------|---------|
| `lo.go` | The contract methods and the provision order |
| `config.go`, `configregistry.go`, `defaults.go` | Spec reading, validation and defaults |
| `render.go` | The kind config |
| `network.go` | The Docker networks |
| `registries.go`, `registrytls.go`, `registryimage.go` | The registry containers and the registry TLS certificate |
| `services.go` | CoreDNS and the registry ConfigMaps |
| `remote.go`, `tunnel.go`, `expose.go` | The remote VM, the kubeconfig tunnel and the nginx reverse proxy |
| `registrycli.go` | `lo registry` |

**Provision steps (local):**
1. Read the config from the spec (network, registries, nodes, load balancer).
2. Validate the IPs against the subnets.
3. Create the Docker network of the project. With shared registries, also create the shared registry network.
4. Mint the registry TLS certificate with the Secret plugin. `registries.tls: false` skips this step.
5. Start the registry containers (build, cache, pull-through mirrors).
6. Write the containerd `certs.d/` tree. The kind nodes bind-mount it.
7. Write the apiserver authentication config, if `spec.oidc` is set.
8. Render the kind config, and create the kind cluster if it does not exist.
9. Connect the nodes to the shared registry network, at gateway priority `-1`, so the cluster network keeps the default route (Docker 28+). An older Docker gets a plain connect and an in-node `ip route replace` (see [The default route](/guide/shared-registries#the-default-route)). Every node on that network gets the route check.
10. Write the kubeconfig.
11. Apply the local-registry-hosting ConfigMap (KEP-1755).
12. Apply the registry ConfigMap and the CoreDNS config, with `spec.coredns` as the `coredns-custom` ConfigMap (see [Custom in-cluster DNS](/guide/local-dev#custom-in-cluster-dns)).

> The application wildcard TLS is **not** a driver step: declare it as a
> [`cert:` Secret](/reference/kustomize-plugins#development-certificates-cert)
> in your targets (the gateway serves it), trusted once with `lo trust`.

> Step 4 must come before steps 5 and 6: the registry containers mount the
> certificate, and `certs.d` references the dev root CA.

**Provision steps (remote, `--remote` flag):**
1. Load the provider (`spec.provider.name`) and provision the VM.
2. Wait for SSH, cloud-init and Docker on the VM.
3. **Docker mode**: set `DOCKER_HOST=ssh://<user>@<ip>`, then run the local steps against the remote Docker.
4. **CI mode**: copy the repo to the VM with rsync, run `lo provision` on the VM over SSH, set up the nginx expose and the kubeconfig tunnel, then return `ErrFullLifecycle`.

**Registry spec fields** (`spec.registries`):

| Field | Default | Description |
|-------|---------|-------------|
| `tls` | `true` | Serve registries over HTTPS (`:443`) with a cert minted by the Secret plugin (`cert:` generator); no `insecure-registries` needed. `false` = plain HTTP `:80` |
| `shared` | `false` | Opt in to shared pull-through mirrors on the dedicated network |
| `network.name` | `lok8s-registries` | Docker network name for shared mirrors |
| `network.subnet` | `10.125.200.0/24` | Subnet for the shared registry network |
| `baseIP` | required | Base IP for pull-through mirror computation (`10.125.200.0` in shared mode; project `/24` start in non-shared mode) |
| `buildIP` | `<project>.101` | Build registry IP (always on project subnet, hostname `lok8s.local`) |
| `cacheIP` | `<project>.102` | Cache registry IP (always on project subnet, hostname `lok8s.cache`) |
| `mirrors[]` | 6 default mirrors | List of registry mirrors to provision (build, cache, io-docker, io-quay, io-k8s, io-ghcr) |

**Two registry kinds**: framework-private (`build`, `cache`) live on the project subnet and never move; pull-through mirrors (`io-*`) optionally live on the shared registry network.

**Containerd wiring**: the driver writes the hostname/IP-to-registry mapping to `clusters/<domain>/.containerd/certs.d/` BEFORE `kind create cluster`. Each kind node bind-mounts that directory to `/etc/containerd/certs.d/` via `extraMounts`, so containerd reads it at startup. No post-create `docker exec` step. In plain mode each `hosts.toml` uses `server = "http://<ip>"` + `skip_verify = true`; in TLS mode it uses `server = "https://<ip>"` + `ca = "/etc/containerd/certs.d/.ca/rootCA.pem"` (a copy of the dev root CA at CAROOT placed in the bind-mounted tree).

**Destroy behavior:** `Destroy` removes only the registries of the project (build, cache). It keeps the shared pull-through mirror containers and the `lok8s-registries` network, because other projects can use them. To remove the shared registries, run `lo registry clean --shared`.

**Status output:** `Running` or `NotFound`

**Kubeconfig:** `.kubeconfig/<metadata.name>.yaml`

### Capi (`internal/driver/capi`)

Production clusters via Cluster API.

**Provision steps:**
1. Read management cluster domain from spec
2. Load management cluster kubeconfig
3. Detect the provider (`hetzner` only — generation rejects others)
4. Ensure credential Secret on management cluster
5. Generate CAPI resources from templates
6. Apply to management cluster
7. Wait for cluster to become `Provisioned`
8. Extract work cluster kubeconfig via `clusterctl`

**Status output:** the phase of the CAPI Cluster (for example `Provisioned` or `Provisioning`), `NotFound` or `Unknown`

**Kubeconfig:** `.kubeconfig/<metadata.name>.yaml`

### KubeOne (`internal/driver/kubeone`)

Production clusters via [KubeOne](https://docs.kubermatic.com/kubeone/). CLI-only (no operator hook).

**Provision steps:**
1. Resolve or generate `kubeone.yaml` manifest from cluster spec
2. Run `kubeone apply` with the manifest and Terraform state
3. Extract kubeconfig

**Status output:** `Healthy`, `Degraded`, `NotFound`, `Unknown`, or `NotProvisioned` before the first provision

**Kubeconfig:** `.kubeconfig/<metadata.name>.yaml`

### Kkp (`internal/driver/kkp`)

Production clusters via [Kubermatic Kubernetes Platform](https://github.com/kubermatic/kubermatic) (hosted control planes).

**Provision steps:**
1. Validate KKP credentials (`KKP_TOKEN`, provider credentials)
2. Read KKP config from cluster spec (`apiUrl`, `projectId`, `datacenter`)
3. Create cluster via KKP REST API (`POST /api/v2/projects/{pid}/clusters`)
4. Create machine deployments for each worker pool
5. Wait for cluster status to become `Running`
6. Extract kubeconfig via KKP API

**Status output:** `Running`, `Provisioning`, `NotFound` or `Unknown`

**Kubeconfig:** `.kubeconfig/<metadata.name>.yaml`

## Custom drivers

The driver registry of the binary is fixed at build time. A Go driver
contract for drivers outside this repo is not published yet. The Go
provision dispatch reports a kind without a Go driver as
`Unknown cluster kind`.

A custom driver in bash still works in two places:

- `lo drivers <name> …` runs `drivers/<name>/main` of the bash tree with
  argv unchanged. Run `lo assets eject bash` to put the tree into the
  project, then add your driver to it.
- A project whose `lok8s.yaml` routes `provision` to bash
  (`spec.implementation`) runs the bash dispatch below. See
  [Choosing the implementation](/reference/cli#choosing-the-implementation).

## The frozen bash contract

The frozen bash tree keeps the original contract. It applies only to the
two cases above.

A bash driver is the file `.lok8s/drivers/<kind>/main`. The dispatch
passes only the domain. A function reads the spec from
`${PATH_CLUSTERS}/<domain>/cluster.lok8s.yaml` itself.

| Function | Signature | Purpose |
|----------|-----------|---------|
| `driver::provision` | `driver::provision <domain>` | Create and configure the cluster |
| `driver::destroy` | `driver::destroy <domain>` | Tear down the cluster |
| `driver::status` | `driver::status <domain>` | Print the cluster status word (stdout) |
| `driver::kubeconfig` | `driver::kubeconfig <domain>` | Print the path of the kubeconfig (stdout) |
| `driver::export` (optional) | `driver::export <domain>` | Export the spec-derived environment for the `spec.bootstrap` addons. Runs on a full provision and on `--bootstrap`, so it must be idempotent. |
| `driver::post_provision` (optional) | `driver::post_provision <domain>` | Side effects that need the provisioned infrastructure. Runs after `driver::provision`, on a full provision only. |
| `main::driver` | argsh `:usage` table | The entry of `lo drivers <name>`. It maps `provision`, `destroy`, `status` and `kubeconfig` to the functions above. |

`driver::provision` returns `0` for success and `100` when the driver did
the full lifecycle. The bash dispatch then skips all later steps and returns
`0`. Any other code is a failure.

The bash dispatch (`provision::dispatch` in `.lok8s/libs/provision`) reads
`kind:`, changes it to lowercase, and sources `.lok8s/drivers/<kind>/main`.
It then runs the same steps as the Go dispatch.

### Example: a k3s driver that uses the k3d CLI

```bash
# .lok8s/drivers/k3s/main

main::driver() {
  local -a usage=(
    'provision@destructive'  'Provision a k3s cluster'
    'destroy@destructive'    'Destroy a k3s cluster'
    'status@readonly'        'Check cluster status'
    'kubeconfig'             'Write the kubeconfig and print its path'
  )
  :usage "k3s driver" "${@}"
  "${usage[@]}"
}

driver::provision() {
  local domain="$1"
  local cluster_yaml="${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "$cluster_yaml") || return 1

  # Create the cluster
  k3d cluster create "$cluster_name" \
    --api-port 6443 \
    --servers 1 || return 1

  # Extract kubeconfig
  driver::kubeconfig "$domain" >/dev/null
}

driver::destroy() {
  local domain="$1"
  local cluster_yaml="${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "$cluster_yaml") || return 1
  k3d cluster delete "$cluster_name"
}

driver::status() {
  local domain="$1"
  local cluster_yaml="${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "$cluster_yaml") || return 1
  if k3d cluster get "$cluster_name" >/dev/null 2>&1; then
    echo "Running"
  else
    echo "NotFound"
  fi
}

driver::kubeconfig() {
  local domain="$1"
  local cluster_yaml="${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "$cluster_yaml") || return 1
  local kubeconfig_path="${PATH_BASE}/.kubeconfig/${cluster_name}.yaml"
  mkdir -p "${PATH_BASE}/.kubeconfig"
  k3d kubeconfig get "$cluster_name" > "$kubeconfig_path" || return 1
  echo "$kubeconfig_path"
}
```

The cluster spec selects it with `kind: K3s`:

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

To call it, use `lo drivers k3s provision dev.example.com`, or route
`provision` to bash and run `lo provision --domain dev.example.com`.

### Variables available to a bash driver

A bash driver sees the variables of `main()` in `.lok8s/lo` through bash
dynamic scoping:

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

The `lo` script imports the bash libraries in `.lok8s/libs/` before it
sources a driver, so a driver can call their functions.
