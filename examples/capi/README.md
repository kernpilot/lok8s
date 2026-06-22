# capi — Hetzner Cloud via Cluster API

The `Capi` driver runs a local kind **management** cluster and uses Cluster API
(the Hetzner provider, CAPH) to provision a **real** workload cluster on Hetzner
Cloud. Kept small to limit cost (1 control-plane + 1 worker on `cax11`);
`examples/test capi` tears it down on success.

> ⚠️ This provisions **billed** infrastructure. If you interrupt the test, check
> your Hetzner Cloud console for leftover servers/load balancers.

## Prerequisites

1. **Hetzner token** in the gitignored `.secrets/hetzner.env`:
   ```sh
   echo 'HCLOUD_TOKEN=<your-token>' > examples/capi/.secrets/hetzner.env
   ```
2. **An SSH key in your Hetzner project** named `lok8s-example` (or edit
   `spec.provider.config.sshKeyName`):
   ```sh
   hcloud ssh-key create --name lok8s-example --public-key-from-file ~/.ssh/id_ed25519.pub
   ```

## Run

```sh
examples/test capi          # provision → verify nodes Ready → tear down
# or by hand:
cd examples/capi && lo use capi-example.lok8s.dev && lo up
```
