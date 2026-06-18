# e2e subnet allocation

Each e2e scenario gets its own `/24` slot under the parent
`10.125.0.0/16` so they don't collide with the default dev cluster
or with each other.

## Slot allocation

| Slot | Domain          | Scenario          | Purpose                                    |
|------|-----------------|-------------------|--------------------------------------------|
| 125  | `lok8s.dev`     | (default dev)     | Reserved — primary local dev cluster       |
| 126  | `126.lok8s.dev` | `no-services`     | Provision-only smoke; no workloads         |
| 127  | `127.lok8s.dev` | `single-local-build` | Tilt build → push → deploy roundtrip    |
| 128  | `128.lok8s.dev` | `cache-mode`      | `build:false` cache pre-pull path          |
| 129  | `129.lok8s.dev` | `remote-lo`       | Lo on remote Hetzner VM — docker mode (E2E_REMOTE=1) |
| 130  | `130.lok8s.dev` | `remote-ci`       | Lo on remote Hetzner VM — CI mode (E2E_REMOTE=1) |
| 200  | (shared)        | (shared mirrors)  | Pull-through registry network — all clusters share this |

## Per-slot layout

Within slot `<n>` the `/24` subnet `10.125.<n>.0/24` is split:

```
  10.125.<n>.1                   docker bridge gateway
  10.125.<n>.101                 build registry  (lok8s.local)
  10.125.<n>.102                 cache registry  (lok8s.cache)
  10.125.<n>.103-106             io-* mirrors (when shared: false)
  10.125.<n>.107-124             reserved
  10.125.<n>.125-150             MetalLB pool (26 IPs)
  10.125.<n>.151-254             kind nodes (docker IPAM)
```

When `spec.registries.shared: true` (the default), the io-* mirrors
move to the shared registry network at `10.125.200.0/24` and are
shared across all clusters.

## DNS

Each slot relies on `*.<n>.lok8s.dev` resolving to its slot subnet
via the porkbun zone (`lok8s.dev`). Without the wildcard DNS, mkcert
certificates fail and any in-cluster service references break in
confusing ways late in the run. The `e2e::require_dns` helper in
`lib/helpers.bash` checks this up front and skips the scenario if
the DNS isn't provisioned.
