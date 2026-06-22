# capi — Hetzner Cloud via Cluster API

The `Capi` driver provisions a **real** workload cluster on Hetzner Cloud with
Cluster API (CAPH, the Hetzner provider). With `managementCluster.local: true` it
runs the CAPI management cluster as a local kind cluster, so the only billed
infrastructure is the workload cluster itself — kept small to limit cost (1
control-plane + 1 worker on a small shared type). The kubeadm stack is installed
on a stock ubuntu image via cloud-init, so no pre-baked node image is required.

`examples/test capi` runs the whole lifecycle end to end:

1. registers a throwaway SSH key (deleted on teardown),
2. brings up a local kind management cluster and `clusterctl init`s CAPI + CAPH,
3. provisions the workload cluster on Hetzner,
4. applies the CNI (cilium) + Hetzner CCM, waits for a Ready node,
5. tears everything down — workload servers, load balancer, kind mgmt, SSH key —
   and warns if any `capi-example-*` server remains.

> ⚠️ This provisions **billed** infrastructure. The harness deprovisions it on
> teardown, but if you interrupt a run, check your Hetzner Cloud console for
> leftover servers/load balancers.
>
> Server types/locations go in and out of stock at Hetzner. If provisioning fails
> with `resource_unavailable` ("error during placement"), set
> `spec.controlPlane.type` / `spec.workers.*.type` to a type that is available in
> your region (`hcloud server-type list` + the datacenter's
> `server_types.available`).

## Prerequisites

**Hetzner token** in the gitignored `.secrets/hetzner.env`:

```sh
echo 'HCLOUD_TOKEN=<your-token>' > examples/capi/.secrets/hetzner.env
```

No pre-registered SSH key is needed — `examples/test` generates a throwaway
keypair, registers it under `spec.provider.config.sshKeyName` for the run, and
deletes it again on teardown.

## Run

```sh
examples/test capi          # kind mgmt → provision → Ready → full teardown
```
