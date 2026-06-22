# Examples

One example per cluster driver. Each is a self-contained lok8s project (its own
`clusters/<domain>/cluster.lok8s.yaml`) you can copy as a starting point, and
that `examples/test <name>` exercises with the repo's framework + toolchain.

| Example | Driver | Cost | E2E status |
|---|---|---|---|
| [`lo`](lo/) | `Lo` (kind) | free, local | ✅ **green** — provisions, verifies a Ready node, tears down |
| [`capi`](capi/) | `Capi` | **real Hetzner** | ✅ **green** — local kind mgmt + CAPH provisions a real 2-node Hetzner cluster (cilium + CCM), Ready, then full teardown |
| [`capi-ha`](capi-ha/) | `Capi` | **real Hetzner** | ✅ **green** — production-shaped: 3-node HA control plane + private hcloud network + two worker pools |
| [`kkp`](kkp/) | `Kkp` | **real Hetzner + a KKP** | 📄 template — needs a reachable KKP endpoint + token |
| [`kkp-hosted`](kkp-hosted/) | `Kkp` (hosted CP) | future | 📄 placeholder until the kubehz hosted plane is GA |

`lo`, `capi`, and `capi-ha` run fully end to end. `kkp` is a runnable **template**
(it needs a reachable KKP endpoint + token). For cloud drivers, `examples/test`
creates a throwaway SSH key in setup, deprovisions all infrastructure on teardown,
and warns if any `<cluster>-*` server remains.

## Run a test

```sh
examples/test lo            # free
examples/test capi          # needs examples/capi/.secrets/hetzner.env (HCLOUD_TOKEN)
```

The harness reuses the repo's framework + built toolchain and runs the example
as the project. **Cloud credentials live only in each example's gitignored
`.secrets/`** — never in the repo or the committed configs. For cloud drivers it
generates a throwaway SSH key in setup and deletes it on teardown, and warns if
any `<cluster>-*` server remains afterward. `capi` provisions real (billed)
infrastructure; the harness tears it down on success, but if you interrupt a run,
check your Hetzner console for leftover servers/load balancers.
