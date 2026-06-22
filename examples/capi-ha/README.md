# capi-ha — production-shaped Hetzner cluster (HA + private network + multi-pool)

A heavier counterpart to [`../capi`](../capi/) that exercises three more
capabilities of the `Capi` (CAPH) driver in one cluster:

- **HA control plane** — 3 control-plane nodes (etcd quorum), spread across
  physical hosts with a `spread` placement group (anti-affinity).
- **Private network** — `spec.provider.config.network.enabled: true` makes CAPH
  create a private hcloud network and attach every node + the load balancer to
  it. The CCM runs in **networking mode** (`ccm: {networking: {enabled: true}}`),
  so each node's `InternalIP` is its private address and cilium's tunnel runs
  over the private network.
- **Multiple worker pools** — `general` and `apps`, each its own
  `MachineDeployment` (and its own `HCloudMachineTemplate`), so pools can differ
  in size/type independently.

Like [`../capi`](../capi/) it uses a local kind management cluster and installs
the kubeadm stack on a stock ubuntu image via cloud-init — so the only billed
infrastructure is the workload cluster.

> ⚠️ This provisions **billed** infrastructure: 3 control-plane + 2 worker VMs +
> 1 load balancer + a private network. The harness deprovisions it all on
> teardown (and warns if any `capi-ha-example-*` server remains), but if you
> interrupt a run, check your Hetzner Cloud console.
>
> Server types go in and out of stock — if provisioning fails with
> `resource_unavailable`, pick an in-stock type (`hcloud server-type list` + the
> datacenter's `server_types.available`) for `controlPlane.type` / `workers.*.type`.

## Prerequisites

**Hetzner token** in the gitignored `.secrets/hetzner.env`:

```sh
echo 'HCLOUD_TOKEN=<your-token>' > examples/capi-ha/.secrets/hetzner.env
```

The SSH key is created (throwaway) and deleted by `examples/test`.

## Run

```sh
examples/test capi-ha       # kind mgmt → HA provision → all nodes Ready → teardown
```

## Tuning

- **Scale the control plane**: keep `controlPlane.replicas` odd (1, 3, 5) for etcd quorum.
- **Add/resize worker pools**: add keys under `spec.workers` (each becomes its own
  pool); set per-pool `replicas` and `type`. All workers share one `spread`
  placement group (Hetzner caps a spread group at 10 servers).
- **Disable the private network**: drop `provider.config.network` (or set
  `enabled: false`) and the `ccm` networking override — that gives you the same
  shape as [`../capi`](../capi/).
