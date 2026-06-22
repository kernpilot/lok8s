# Examples

One runnable example per cluster driver. Each is a self-contained lok8s project
(its own `clusters/<domain>/cluster.lok8s.yaml`) you can copy as a starting
point, and that `examples/test <name>` provisions, verifies, and tears down end
to end.

| Example | Driver | Cost | Provisions |
|---|---|---|---|
| [`lo`](lo/) | `Lo` (kind) | free, local | a kind cluster on a Docker bridge, with TLS |
| [`capi`](capi/) | `Capi` | **real Hetzner** | a Cluster-API workload cluster on Hetzner Cloud |
| [`kkp`](kkp/) | `Kkp` | **real Hetzner + a KKP** | a user cluster via an existing Kubermatic (KKP) |
| [`kkp-hosted`](kkp-hosted/) | `Kkp` (hosted CP) | future | placeholder until the kubehz hosted plane is GA |

## Run a test

```sh
examples/test lo            # free
examples/test capi          # needs examples/capi/.secrets/hetzner.env (HCLOUD_TOKEN)
```

The harness reuses the repo's framework + built toolchain and runs the example
as the project. **Cloud credentials live only in each example's gitignored
`.secrets/`** — never in the repo or the committed configs. `capi`/`kkp`
provision real infrastructure (and bill for it); the harness tears the cluster
down on success, but interrupt it and you may leave VMs running.
