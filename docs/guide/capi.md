# CAPI Clusters (Hetzner)

lok8s provisions production clusters on **Hetzner Cloud** with
[Cluster API (CAPI)](https://cluster-api.sigs.k8s.io/) and the
[Hetzner provider (CAPH)](https://github.com/syself/cluster-api-provider-hetzner).
The `Capi` driver renders CAPI/CAPH manifests from your cluster spec, applies
them to a management cluster, waits for the workload cluster, and then applies
the CNI + cloud-controller-manager.

> **Hetzner Cloud only.** Manifest generation targets hcloud (CAPH); other
> providers (AWS, Hetzner bare-metal/robot) are **not** generated — `capi::generate`
> errors for them. The cheapest, fully-working path is what this guide describes.

The two reference specs in the repo are validated end-to-end and are the
canonical examples — copy from them:

- [`examples/capi`](https://github.com/kernpilot/lok8s/tree/main/examples/capi) — a minimal 1 control-plane + 1 worker cluster.
- [`examples/capi-ha`](https://github.com/kernpilot/lok8s/tree/main/examples/capi-ha) — HA (3 control-plane) + private network + two worker pools.

## How it works

- **Management cluster.** With `managementCluster.local: true` the driver creates
  a local **kind** cluster as the CAPI management cluster (`clusterctl init` —
  CAPI `v1.13.2` + CAPH `v1.1.7`, pinned for reproducibility), so the only billed
  infrastructure is the workload cluster. Alternatively point
  `managementCluster.domain` at a cluster you already provisioned.
- **Node image.** Nodes boot a **stock `ubuntu-24.04`** image and install
  containerd + the kubeadm stack via cloud-init (`preKubeadmCommands`) — no
  pre-baked image to build.
- **Networking.** The CNI (cilium) and Hetzner CCM are applied as `spec.bootstrap`
  addons on the workload cluster after it provisions.

## Cluster Spec

```yaml
# clusters/prod.example.com/cluster.lok8s.yaml  (see examples/capi)
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: prod
spec:
  kubernetes:
    version: "v1.31.12"
  cluster:
    domain: prod.example.com
  managementCluster:
    domain: prod-mgmt.example.com
    local: true                 # run the CAPI mgmt cluster as a local kind cluster
  provider:
    name: hetzner
    config:
      region: fsn1
      sshKeyName: my-ssh-key     # an SSH key registered in your Hetzner project
      image: ubuntu-24.04        # stock image; k8s installed via cloud-init
    credentials:
      envVars:
        - HCLOUD_TOKEN
      secretRef: prod-credentials  # defaults to <metadata.name>-credentials
  controlPlane:
    replicas: 1                  # use an odd number (1, 3, 5) for etcd quorum
    type: cpx22
  workers:
    general:                     # each key is its own MachineDeployment + pool
      replicas: 2
      type: cpx22
  bootstrap:
    - cilium                     # CNI
    - ccm                        # Hetzner cloud-controller-manager
```

Every `spec.workers.<key>` becomes its own `MachineDeployment` +
`HCloudMachineTemplate`, so pools can differ in size and type independently.

> Server types go in and out of stock at Hetzner. If provisioning fails with
> `resource_unavailable` ("error during placement"), pick a type that is in stock
> in your region (`hcloud server-type list` + the datacenter's
> `server_types.available`).

## Production options

### HA control plane

Set `controlPlane.replicas` to an odd number ≥ 3 for an etcd quorum:

```yaml
  controlPlane:
    replicas: 3
    type: cpx22
```

### Anti-affinity placement groups

Opt in to spread the control plane and workers across physical hosts:

```yaml
  provider:
    config:
      placementGroups: true   # spread CP + workers (anti-affinity)
```

Off by default — a Hetzner `spread` group caps at **10 servers**, so always-on
would make larger clusters fail. When on, all worker pools share one spread group
and the control plane has its own, so keep the **total** worker count and the
control-plane count each ≤ 10.

### Private network

```yaml
  provider:
    config:
      network:
        enabled: true          # CAPH creates a private hcloud network (10.0.0.0/16)
  bootstrap:
    - cilium
    - ccm: {networking: {enabled: true}}   # CCM networking mode → private InternalIPs
```

With the network enabled and the CCM in networking mode, every node's
`InternalIP` is its private-network address and cilium's tunnel runs over the
private network. See [`examples/capi-ha`](https://github.com/kernpilot/lok8s/tree/main/examples/capi-ha)
for HA + private network + multi-pool combined.

## Provisioning

```bash
lo use prod.example.com
lo provision
```

The `Capi` driver (`.lok8s/drivers/capi/main`) then:

1. Ensures a management cluster (local kind + `clusterctl init`, or an existing one).
2. Creates the credential `Secret` on the management cluster.
3. Generates the CAPI/CAPH manifests (`capi::generate`) and applies them — retrying
   while the CAPH admission webhooks finish starting on a fresh mgmt cluster.
4. Waits for the workload `Cluster` to reach `Provisioned` (`capi::wait_ready`).
5. Extracts the workload kubeconfig and waits for its API server.
6. Creates the workload `hcloud` token secret (`driver::post_provision`), then the
   framework applies the `spec.bootstrap` addons (CNI + CCM). See [Addons](/guide/addons).

`lo down` deletes the workload `Cluster` and **blocks until CAPH has deprovisioned
the servers + load balancer** before removing the local kind management cluster, so
nothing is left billed.

## Credentials

Provide your Hetzner token via the environment; the driver stores it as a Secret
on the management cluster:

```bash
export HCLOUD_TOKEN="your-hcloud-api-token"
```

The Secret name comes from `spec.provider.credentials.secretRef` (or
`spec.credentials.secretName`), defaulting to `<metadata.name>-credentials`.

## Templates

Rendered from `.lok8s/drivers/capi/cluster/` (envsubst + an opt-in yq pass for
placement groups):

```
.lok8s/drivers/capi/cluster/
  core/
    cluster.yaml                          # Cluster (v1beta2)
    kubeadm-control-plane.yaml            # KubeadmControlPlane + cloud-init install
    machine-deployment.yaml               # MachineDeployment (per worker pool)
  providers/hetzner/
    hetzner-cluster.yaml                  # HetznerCluster (network, placement groups)
    hcloud-machine-template-controlplane.yaml
    hcloud-machine-template-worker.yaml   # per worker pool
    kubeadm-config-template.yaml          # worker KubeadmConfigTemplate + cloud-init
```

## Cluster Status

```bash
lo status prod.example.com
```

For `Capi` clusters this reports the CAPI `Cluster` phase on the management
cluster (`Provisioned`, `Provisioning`, `Failed`, `NotFound`).
